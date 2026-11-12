package tracing

import (
	"context"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func recorder(t *testing.T) *tracetest.SpanRecorder {
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(prev); tp.Shutdown(context.Background()) })
	return rec
}

// spanText is every text an exported span carries.
func spanText(s sdktrace.ReadOnlySpan) string {
	var b strings.Builder
	b.WriteString(s.Name() + "\n" + s.Status().Description + "\n")
	kvs := append([]attribute.KeyValue(nil), s.Attributes()...)
	for _, e := range s.Events() {
		b.WriteString(e.Name + "\n")
		kvs = append(kvs, e.Attributes...)
	}
	for _, kv := range kvs {
		b.WriteString(kv.Value.Emit() + "\n")
	}
	return b.String()
}

// Every text a span under a Redaction is given is redacted, on that span
// and on its descendants, whichever way it is set.
func TestRedactionCoversSpans(t *testing.T) {
	rec := recorder(t)
	ctx, r := WithRedaction(context.Background())
	r.Set(func(s string) string { return strings.ReplaceAll(s, "s3cr3t", "[redacted]") })

	ctx, root := Start(ctx, "agent.session", attribute.String("session.message", "deploy s3cr3t"))
	root.SetAttributes(attribute.StringSlice("paths", []string{"a", "s3cr3t"}), attribute.Int("n", 3))
	_, child := Start(ctx, "child s3cr3t")
	child.SetStatus(codes.Error, "failed with s3cr3t")
	child.RecordError(errors.New("error: s3cr3t"))
	child.AddEvent("event s3cr3t", trace.WithAttributes(attribute.String("detail", "s3cr3t")))
	child.End()
	tools := NewToolSpans(ctx)
	tools.Start("Bash: curl -H 'token: s3cr3t'")
	tools.End("Bash error: s3cr3t denied")
	Fail(root, "s3cr3t")
	root.End()

	ended := rec.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans", len(ended))
	}
	for _, s := range ended {
		text := spanText(s)
		if strings.Contains(text, "s3cr3t") || !strings.Contains(text, "[redacted]") {
			t.Errorf("span %q:\n%s", s.Name(), text)
		}
	}
	// Non-string attributes are kept as they were.
	for _, kv := range ended[2].Attributes() {
		if kv.Key == "n" && kv.Value.AsInt64() != 3 {
			t.Errorf("n = %v", kv.Value)
		}
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Error("the context lost its span")
	}
}

// Without a Redaction, or before its redactor is set, text passes as is.
func TestRedactionUnset(t *testing.T) {
	rec := recorder(t)
	_, plain := Start(context.Background(), "plain", attribute.String("k", "s3cr3t"))
	plain.End()
	ctx, _ := WithRedaction(context.Background())
	_, early := Start(ctx, "early", attribute.String("k", "s3cr3t"))
	early.End()
	for _, s := range rec.Ended() {
		if !strings.Contains(spanText(s), "s3cr3t") {
			t.Errorf("span %q changed without a redactor", s.Name())
		}
	}
}
