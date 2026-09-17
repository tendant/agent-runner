package executor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Claude Code's --output-format stream-json emits one JSON object per line:
// a "system"/"init" envelope, "assistant" messages (text and tool_use
// blocks), "user" messages carrying tool_result blocks, and a final
// "result" line with the outcome, cost and duration. streamParser turns
// that into progress events while it runs and an ExecutionResult at the end,
// so the iteration loop sees each tool call as it happens instead of one
// opaque blob when the process exits.

// streamLine is the subset of every stream-json envelope we look at.
type streamLine struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
	Message struct {
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	// result-line fields
	IsError        bool    `json:"is_error"`
	Result         string  `json:"result"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	CostUSD        float64 `json:"cost_usd"` // older CLIs
	DurationMS     int     `json:"duration_ms"`
	NumTurns       int     `json:"num_turns"`
	APIErrorStatus *int    `json:"api_error_status"`
	Denials        []struct {
		ToolName string `json:"tool_name"`
	} `json:"permission_denials"`
	// Legacy --output-format json object ({"result":…,"error":…}) has no
	// "type"; Error is its error field.
	Error string `json:"error"`
}

// contentBlock is one element of an assistant/user message's content array.
type contentBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

const (
	maxEventText  = 240
	maxResultText = 200
)

// streamParser consumes stream-json lines, emitting events through onEvent
// (nil is fine) and accumulating the final result plus a compact transcript
// for the audit log.
type streamParser struct {
	onEvent    func(kind EventKind, text string)
	toolNames  map[string]string // tool_use id → name, for tool_end
	transcript strings.Builder
	plain      strings.Builder // non-JSON stdout, for CLIs that don't speak stream-json
	final      *streamLine
	sawLine    bool
}

// maxPlainBytes bounds how much non-JSON stdout is retained.
const maxPlainBytes = 256 * 1024

func newStreamParser(onEvent func(EventKind, string)) *streamParser {
	return &streamParser{onEvent: onEvent, toolNames: map[string]string{}}
}

func (p *streamParser) emit(kind EventKind, text string) {
	fmt.Fprintf(&p.transcript, "[%s] %s\n", kind, text)
	if p.onEvent != nil {
		p.onEvent(kind, text)
	}
}

// consume reads r to EOF, one line at a time. Lines can be very large (a
// Read of a big file comes back inside one tool_result), so this uses an
// unbounded reader rather than bufio.Scanner.
func (p *streamParser) consume(r io.Reader) error {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			p.handleLine(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (p *streamParser) handleLine(raw []byte) {
	raw = []byte(strings.TrimSpace(string(raw)))
	if len(raw) == 0 {
		return
	}
	var line streamLine
	if raw[0] != '{' || json.Unmarshal(raw, &line) != nil {
		if p.plain.Len() < maxPlainBytes {
			p.plain.Write(raw)
			p.plain.WriteByte('\n')
		}
		return
	}
	p.sawLine = true
	switch line.Type {
	case "assistant":
		for _, b := range p.blocks(line.Message.Content) {
			switch b.Type {
			case "text":
				if t := strings.TrimSpace(b.Text); t != "" {
					p.emit(EventText, truncateText(t, maxEventText))
				}
			case "tool_use":
				p.toolNames[b.ID] = b.Name
				p.emit(EventToolStart, summarizeToolUse(b.Name, b.Input))
			}
		}
	case "user":
		for _, b := range p.blocks(line.Message.Content) {
			if b.Type != "tool_result" {
				continue
			}
			name := p.toolNames[b.ToolUseID]
			if name == "" {
				name = "tool"
			}
			delete(p.toolNames, b.ToolUseID)
			if b.IsError {
				p.emit(EventToolEnd, name+" error: "+truncateText(firstLine(resultText(b.Content)), maxResultText))
			} else {
				p.emit(EventToolEnd, name+" ok")
			}
		}
	case "result":
		l := line
		p.final = &l
		for _, d := range l.Denials {
			p.emit(EventWarning, "permission denied for tool "+d.ToolName)
		}
	case "":
		// A bare {"result":…} object is the legacy --output-format json
		// shape (older CLIs, wrapper scripts, test mocks). Accept it as the
		// final line so those keep working unchanged.
		if strings.Contains(string(raw), `"result"`) || line.Error != "" {
			l := line
			if l.Error != "" && !l.IsError {
				l.IsError = true
				l.Result = l.Error
			}
			p.final = &l
		}
	}
}

// blocks decodes a message content field, which is an array of blocks or —
// for plain user text — a bare string.
func (p *streamParser) blocks(raw json.RawMessage) []contentBlock {
	if len(raw) == 0 || raw[0] != '[' {
		return nil
	}
	var out []contentBlock
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// result builds the ExecutionResult from the final result line. ok is
// false if no result line arrived (the process died mid-stream).
func (p *streamParser) result() (*ExecutionResult, bool) {
	if p.final == nil {
		return nil, false
	}
	f := p.final
	cost := f.TotalCostUSD
	if cost == 0 {
		cost = f.CostUSD
	}
	res := &ExecutionResult{
		Output:     f.Result,
		CostUSD:    cost,
		DurationMS: f.DurationMS,
	}
	if f.IsError || (f.Subtype != "" && f.Subtype != "success") {
		msg := f.Result
		if msg == "" {
			msg = f.Subtype
		}
		if f.APIErrorStatus != nil {
			msg = fmt.Sprintf("%s (api status %d)", msg, *f.APIErrorStatus)
		}
		if f.Subtype != "" && f.Subtype != "success" && !strings.Contains(msg, f.Subtype) {
			msg = f.Subtype + ": " + msg
		}
		res.Error = fmt.Errorf("CLAUDE_ERROR: %s", msg)
	}
	fmt.Fprintf(&p.transcript, "[result] %s cost=$%.4f turns=%d\n%s\n", f.Subtype, cost, f.NumTurns, f.Result)
	return res, true
}

// summarizeToolUse renders a tool call as one short line: the tool name
// plus the one input field a human would want to see.
func summarizeToolUse(name string, input json.RawMessage) string {
	var in map[string]any
	_ = json.Unmarshal(input, &in)
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := in[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var detail string
	switch name {
	case "Bash":
		detail = firstLine(pick("command"))
	case "Read", "Write", "Edit", "MultiEdit", "NotebookEdit":
		detail = pick("file_path", "notebook_path", "path")
	case "Grep":
		detail = pick("pattern")
		if p := pick("path"); p != "" {
			detail += " in " + p
		}
	case "Glob":
		detail = pick("pattern")
	case "Task", "Agent":
		detail = pick("description", "prompt")
	case "WebFetch":
		detail = pick("url")
	case "WebSearch":
		detail = pick("query")
	case "Skill":
		detail = pick("skill", "name")
	default:
		detail = pick("command", "path", "file_path", "query", "url", "pattern", "description", "prompt")
		if detail == "" && len(input) > 0 && string(input) != "{}" {
			detail = string(input)
		}
	}
	if detail == "" {
		return name
	}
	return name + ": " + truncateText(detail, maxEventText)
}

// resultText flattens a tool_result content field: a string, or an array
// of {type:text,text} blocks.
func resultText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		_ = json.Unmarshal(raw, &s)
		return s
	}
	var blocks []contentBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return string(raw)
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i] + " …"
	}
	return s
}

func truncateText(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
