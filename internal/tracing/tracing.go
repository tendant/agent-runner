// Package tracing wires OpenTelemetry for agent-runner. With TRACING_ENABLED
// unset the global tracer is the SDK's no-op, so span calls throughout the
// engine cost nothing; with it set, spans go to an OTLP collector named by
// the standard OTEL_EXPORTER_OTLP_* environment variables.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

// Config selects the exporter. Endpoint and protocol fall back to the
// OTEL_EXPORTER_OTLP_ENDPOINT / OTEL_EXPORTER_OTLP_PROTOCOL env vars the
// exporters read themselves, so an empty Config with Enabled=true is a
// valid "use the standard env" setup.
type Config struct {
	Enabled     bool
	ServiceName string // default "agent-runner"
	Protocol    string // "grpc" or "http/protobuf" (default), also OTEL_EXPORTER_OTLP_PROTOCOL
	Version     string
}

// Init installs the global tracer provider. The returned shutdown flushes
// pending spans; call it on exit. Disabled tracing returns a no-op
// shutdown and leaves the no-op global provider in place.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		return func(context.Context) error { return nil }, nil
	}
	name := cfg.ServiceName
	if name == "" {
		name = "agent-runner"
	}

	var client otlptrace.Client
	proto := strings.ToLower(cfg.Protocol)
	if proto == "" {
		proto = strings.ToLower(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"))
	}
	switch proto {
	case "grpc":
		client = otlptracegrpc.NewClient()
	case "", "http/protobuf", "http":
		client = otlptracehttp.NewClient()
	default:
		return nil, fmt.Errorf("tracing: unsupported protocol %q (grpc or http/protobuf)", proto)
	}
	exp, err := otlptrace.New(ctx, client)
	if err != nil {
		return nil, fmt.Errorf("tracing: exporter: %w", err)
	}

	// resource.New (not Merge with resource.Default) so the SDK's own
	// semconv schema version never conflicts with the one imported here.
	attrs := []attribute.KeyValue{semconv.ServiceName(name)}
	if cfg.Version != "" {
		attrs = append(attrs, semconv.ServiceVersion(cfg.Version))
	}
	res, err := resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithFromEnv(), // OTEL_RESOURCE_ATTRIBUTES
		resource.WithHost(),
		resource.WithProcessPID(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: resource: %w", err)
	}

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp, sdktrace.WithBatchTimeout(2*time.Second)),
		sdktrace.WithResource(res),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("tracing: export error", "error", err)
	}))
	slog.Info("tracing enabled", "service", name, "protocol", proto,
		"endpoint", firstNonEmpty(os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"), "default"))

	return func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return tp.Shutdown(ctx)
	}, nil
}

// Tracer is the one tracer the engine uses.
func Tracer() trace.Tracer { return otel.Tracer("agent-runner") }

// Start opens a span under ctx.
func Start(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return Tracer().Start(ctx, name, trace.WithAttributes(attrs...))
}

// End closes span, recording err as the span status when non-nil.
func End(span trace.Span, err error) {
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
	}
	span.End()
}

// Fail marks span as errored with msg without ending it.
func Fail(span trace.Span, msg string) {
	span.SetStatus(codes.Error, msg)
}

// TraceID returns the hex trace id of the span in ctx, or "" when ctx has
// no recording span — the value to put on a session so logs, the API
// response and the trace backend can be joined.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
