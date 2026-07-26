// Package pi speaks the pi coding agent's RPC protocol: one long-lived
// `pi --mode rpc --no-session` process per client, LF-delimited JSON on
// stdin/stdout. The package knows nothing about agent-runner's executor
// interfaces; it exposes its own Client/Result/Event types.
//
// Protocol notes (pi docs/rpc.md):
//   - Every command is acknowledged by a {"type":"response"} line carrying
//     the command's id and success flag.
//   - A prompt is finished when the agent_settled event arrives — not
//     turn_end, since one prompt may span several turns, retries, and
//     compactions.
//   - extension_ui_request dialogs block pi until answered; a headless
//     client must reply with cancelled:true or the run hangs forever.
package pi

import "encoding/json"

// request is a client→pi command line.
type request struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Message string `json:"message,omitempty"`
}

// uiResponse cancels an extension_ui_request dialog.
type uiResponse struct {
	Type      string          `json:"type"`
	ID        json.RawMessage `json:"id"`
	Cancelled bool            `json:"cancelled"`
}

// line is the lenient decode envelope for everything pi prints on stdout.
// Unknown event types simply carry a Type we don't dispatch on.
type line struct {
	Type    string          `json:"type"`
	ID      json.RawMessage `json:"id,omitempty"`
	Command string          `json:"command,omitempty"`
	Success *bool           `json:"success,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data,omitempty"`

	// Event payload fields, all optional across event kinds.
	ToolName string          `json:"toolName,omitempty"`
	Name     string          `json:"name,omitempty"`
	Message  json.RawMessage `json:"message,omitempty"`
	Usage    json.RawMessage `json:"usage,omitempty"`
	Text     string          `json:"text,omitempty"`
}

// idKey normalizes a raw JSON id ("1" or 1) to a map key.
func idKey(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

// extractText pulls a string out of a response's data field, which may be a
// bare string or an object with a text field.
func extractText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return obj.Text
	}
	return ""
}

// extractMessageText pulls role and concatenated text content out of a
// message payload. Content may be a bare string or a list of typed blocks.
func extractMessageText(raw json.RawMessage) (role, text string) {
	if len(raw) == 0 {
		return "", ""
	}
	var msg struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return "", ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return msg.Role, s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "" || b.Type == "text" {
				text += b.Text
			}
		}
	}
	return msg.Role, text
}

// extractCost pulls a dollar cost out of a usage payload, trying the field
// names pi might use. Returns 0 when absent — cost is best-effort.
func extractCost(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var u struct {
		Cost      float64 `json:"cost"`
		CostUSD   float64 `json:"costUSD"`
		TotalCost float64 `json:"totalCost"`
	}
	if json.Unmarshal(raw, &u) != nil {
		return 0
	}
	for _, c := range []float64{u.Cost, u.CostUSD, u.TotalCost} {
		if c > 0 {
			return c
		}
	}
	return 0
}

// extractToolName pulls the tool name from a tool_execution_* event.
func (l *line) extractToolName() string {
	if l.ToolName != "" {
		return l.ToolName
	}
	return l.Name
}
