package tracing

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// installRecorder swaps in an in-memory exporter for the test's lifetime.
func installRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		tp.Shutdown(context.Background())
	})
	return rec
}

func TestToolSpans_PairsStartAndEnd(t *testing.T) {
	rec := installRecorder(t)
	ctx, parent := Start(context.Background(), "agent.iteration")
	ts := NewToolSpans(ctx)

	ts.Start("Bash: go test ./...")
	ts.Start("Read: /w/main.go")
	ts.End("Read error: File does not exist.")
	ts.End("Bash ok")
	parent.End()

	ended := rec.Ended()
	if len(ended) != 3 {
		t.Fatalf("expected 3 ended spans, got %d", len(ended))
	}
	// Read ended first, then Bash, then the parent.
	read, bash := ended[0], ended[1]
	if read.Name() != "agent.tool" || bash.Name() != "agent.tool" {
		t.Errorf("tool spans should be named agent.tool: %s %s", read.Name(), bash.Name())
	}
	if read.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("tool span should be a child of the iteration span")
	}
	if read.Status().Code != codes.Error {
		t.Error("errored tool should set error status")
	}
	if bash.Status().Code == codes.Error {
		t.Error("ok tool should not set error status")
	}
	var sawName, sawErr bool
	for _, a := range read.Attributes() {
		if a.Key == "tool.name" && a.Value.AsString() == "Read" {
			sawName = true
		}
		if a.Key == "tool.error" && a.Value.AsString() == "File does not exist." {
			sawErr = true
		}
	}
	if !sawName || !sawErr {
		t.Errorf("missing tool.name/tool.error attributes: %v", read.Attributes())
	}
}

func TestToolSpans_SameToolFIFO(t *testing.T) {
	rec := installRecorder(t)
	ts := NewToolSpans(context.Background())
	ts.Start("Bash: first")
	ts.Start("Bash: second")
	ts.End("Bash error: boom")
	ts.End("Bash ok")
	ended := rec.Ended()
	if len(ended) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(ended))
	}
	// The first-opened Bash span should carry the error (FIFO).
	for _, a := range ended[0].Attributes() {
		if a.Key == "tool.input" && a.Value.AsString() != "first" {
			t.Errorf("FIFO violated: errored span input=%q", a.Value.AsString())
		}
	}
}

func TestToolSpans_CloseEndsDangling(t *testing.T) {
	rec := installRecorder(t)
	ts := NewToolSpans(context.Background())
	ts.Start("Bash: hangs")
	ts.End("Nope ok") // unmatched end is ignored
	ts.Close()
	ended := rec.Ended()
	if len(ended) != 1 || ended[0].Status().Code != codes.Error {
		t.Errorf("dangling span should be ended with error status, got %d spans", len(ended))
	}
	if len(ts.open) != 0 {
		t.Error("Close should clear open spans")
	}
}

func TestInit_DisabledIsNoop(t *testing.T) {
	shutdown, err := Init(context.Background(), Config{})
	if err != nil || shutdown == nil {
		t.Fatalf("disabled tracing should return a no-op shutdown: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Errorf("no-op shutdown errored: %v", err)
	}
	if TraceID(context.Background()) != "" {
		t.Error("no span → empty trace id")
	}
}

func TestInit_RejectsUnknownProtocol(t *testing.T) {
	if _, err := Init(context.Background(), Config{Enabled: true, Protocol: "carrier-pigeon"}); err == nil {
		t.Error("expected error for unknown protocol")
	}
}

func TestInit_ExportsOverOTLPHTTP(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/traces" {
			select {
			case got <- r.Header.Get("Content-Type"):
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", srv.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "true")

	prev := otel.GetTracerProvider()
	t.Cleanup(func() { otel.SetTracerProvider(prev) })
	shutdown, err := Init(context.Background(), Config{Enabled: true, ServiceName: "test"})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	ctx, span := Start(context.Background(), "agent.session")
	if TraceID(ctx) == "" {
		t.Error("enabled tracing should yield a trace id")
	}
	span.End()
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	select {
	case ct := <-got:
		if ct != "application/x-protobuf" {
			t.Errorf("unexpected content type %q", ct)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no spans reached the OTLP endpoint")
	}
}
