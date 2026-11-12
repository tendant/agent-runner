package tracing

import (
	"context"
	"errors"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// Redaction removes a session's secret values from its spans before they
// leave the process: every span Start opens under a context carrying it
// redacts its name, string attributes, status and events. A session sets the
// redactor once its secrets are loaded (Set); until then text passes as is,
// so nothing secret should be put on a span before that.
type Redaction struct {
	mu sync.RWMutex
	fn func(string) string
}

type redactionKey struct{}

// WithRedaction returns ctx carrying a new, empty Redaction.
func WithRedaction(ctx context.Context) (context.Context, *Redaction) {
	r := &Redaction{}
	return context.WithValue(ctx, redactionKey{}, r), r
}

// Set installs the redactor; nil removes it.
func (r *Redaction) Set(fn func(string) string) {
	r.mu.Lock()
	r.fn = fn
	r.mu.Unlock()
}

func (r *Redaction) redact(s string) string {
	r.mu.RLock()
	fn := r.fn
	r.mu.RUnlock()
	if fn == nil || s == "" {
		return s
	}
	return fn(s)
}

func (r *Redaction) attrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, len(in))
	for i, kv := range in {
		switch kv.Value.Type() {
		case attribute.STRING:
			out[i] = kv.Key.String(r.redact(kv.Value.AsString()))
		case attribute.STRINGSLICE:
			ss := kv.Value.AsStringSlice()
			for j := range ss {
				ss[j] = r.redact(ss[j])
			}
			out[i] = kv.Key.StringSlice(ss)
		default:
			out[i] = kv
		}
	}
	return out
}

func (r *Redaction) eventOptions(opts []trace.EventOption) []trace.EventOption {
	cfg := trace.NewEventConfig(opts...)
	out := []trace.EventOption{trace.WithAttributes(r.attrs(cfg.Attributes())...), trace.WithTimestamp(cfg.Timestamp())}
	if cfg.StackTrace() {
		out = append(out, trace.WithStackTrace(true))
	}
	return out
}

func redactionFrom(ctx context.Context) *Redaction {
	r, _ := ctx.Value(redactionKey{}).(*Redaction)
	return r
}

// redactingSpan passes every text a span is given through its Redaction.
type redactingSpan struct {
	trace.Span
	r *Redaction
}

func (s redactingSpan) SetName(name string) { s.Span.SetName(s.r.redact(name)) }
func (s redactingSpan) SetAttributes(kv ...attribute.KeyValue) {
	s.Span.SetAttributes(s.r.attrs(kv)...)
}
func (s redactingSpan) SetStatus(code codes.Code, desc string) {
	s.Span.SetStatus(code, s.r.redact(desc))
}
func (s redactingSpan) AddEvent(name string, opts ...trace.EventOption) {
	s.Span.AddEvent(s.r.redact(name), s.r.eventOptions(opts)...)
}
func (s redactingSpan) RecordError(err error, opts ...trace.EventOption) {
	if err == nil {
		return
	}
	s.Span.RecordError(errors.New(s.r.redact(err.Error())), s.r.eventOptions(opts)...)
}
