package tracing

import (
	"context"
	"strings"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// ToolSpans turns the executor's tool_start/tool_end event pairs into
// child spans of whatever context is current. Events carry no call id,
// only "<Tool>: <detail>" / "<Tool> ok|error: …", so a tool_end closes the
// oldest open span with that tool name — exact for sequential tool use,
// and the right shape (same count, same names) when a backend runs a few
// tools concurrently.
type ToolSpans struct {
	mu     sync.Mutex
	parent context.Context
	open   map[string][]trace.Span // tool name → open spans, oldest first
}

// NewToolSpans returns a recorder whose spans are parented to parent
// until SetParent changes it.
func NewToolSpans(parent context.Context) *ToolSpans {
	return &ToolSpans{parent: parent, open: map[string][]trace.Span{}}
}

// SetParent switches the parent for spans opened from now on — the engine
// calls it with each iteration's span context.
func (t *ToolSpans) SetParent(ctx context.Context) {
	t.mu.Lock()
	t.parent = ctx
	t.mu.Unlock()
}

// Start opens a span for a tool_start event text.
func (t *ToolSpans) Start(text string) {
	name, detail := splitTool(text)
	if name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	_, span := Tracer().Start(t.parent, "agent.tool", trace.WithAttributes(
		attribute.String("tool.name", name),
		attribute.String("tool.input", detail),
	))
	t.open[name] = append(t.open[name], span)
}

// End closes the span for a tool_end event text.
func (t *ToolSpans) End(text string) {
	name, rest := splitTool(text)
	if name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	spans := t.open[name]
	if len(spans) == 0 {
		return
	}
	span := spans[0]
	if len(spans) == 1 {
		delete(t.open, name)
	} else {
		t.open[name] = spans[1:]
	}
	if strings.HasPrefix(rest, "error") {
		span.SetAttributes(attribute.String("tool.error", strings.TrimSpace(strings.TrimPrefix(rest, "error:"))))
		Fail(span, "tool error")
	}
	span.End()
}

// Close ends any spans still open (a prompt that died mid-tool).
func (t *ToolSpans) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for name, spans := range t.open {
		for _, s := range spans {
			Fail(s, "unfinished")
			s.End()
		}
		delete(t.open, name)
	}
}

// splitTool parses "<Tool>: detail" or "<Tool> ok" / "<Tool> error: …".
func splitTool(text string) (name, rest string) {
	name, rest, _ = strings.Cut(strings.TrimSpace(text), " ")
	name = strings.TrimSuffix(name, ":")
	return name, strings.TrimSpace(rest)
}
