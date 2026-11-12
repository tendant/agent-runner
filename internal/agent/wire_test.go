package agent

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// Every Session field crosses the wire: a field added to Session without a
// SessionWire counterpart would silently stay behind in the runner.
func TestSessionWireCoversSession(t *testing.T) {
	local := map[string]bool{"mu": true, "notify": true, "stopCh": true, "redact": true} // the process's own
	renamed := map[string]string{"PlanJSON": "Plan", "ReviewJSON": "Review", "stopRequested": "StopRequested", "execEventSeq": "ExecEventSeq"}
	wire := reflect.TypeOf(SessionWire{})
	st := reflect.TypeOf(Session{})
	for i := 0; i < st.NumField(); i++ {
		name := st.Field(i).Name
		if local[name] {
			continue
		}
		if r, ok := renamed[name]; ok {
			name = r
		}
		if _, ok := wire.FieldByName(name); !ok {
			t.Errorf("Session.%s has no SessionWire field", st.Field(i).Name)
		}
	}
}

type wirePlan struct {
	Steps []string `json:"steps"`
}

func wireDecoder(field string, raw json.RawMessage) (any, error) {
	var p wirePlan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func fullSession() *Session {
	at := time.Unix(1_800_000_000, 0).UTC()
	done := at.Add(time.Minute)
	return &Session{
		notify:               make(chan struct{}, 1),
		ID:                   "agent-1",
		Source:               "stream",
		ConvID:               "c_1",
		Message:              "do it",
		Messages:             []Message{{Role: "user", Content: "do it", Time: at}},
		Paths:                []string{"repo"},
		Author:               "claude-agent",
		CommitMessagePrefix:  "[agent]",
		MaxIterations:        5,
		MaxTotalSeconds:      600,
		Status:               SessionStatusRunning,
		CurrentIteration:     2,
		SuccessfulIterations: 1,
		Iterations:           []IterationResult{{Iteration: 1, Status: IterationStatusSuccess, Output: "ok", Prompt: "the prompt", CostUSD: 0.5}},
		StartedAt:            at,
		CompletedAt:          &done,
		Error:                "e",
		Warnings:             []string{"w"},
		Notes:                []string{"n"},
		Eval:                 &EvalSpec{Overrides: map[string]string{"agent.md": "x"}},
		EvalResults:          []EvalResult{{}},
		Pushed:               []PushedCommit{{}},
		CallbackURL:          "https://example.com/hook",
		TraceID:              "trace",
		WorkspacePath:        "/ws",
		ElapsedSeconds:       60,
		TotalCostUSD:         0.5,
		ConsecutiveFailures:  1,
		OutputFiles:          []OutputFile{{Name: "a.txt", Data: []byte("hello"), ContentType: "text/plain"}},
		CompletedSteps:       []string{"1"},
		PlanStepCount:        2,
		PlanJSON:             &wirePlan{Steps: []string{"one", "two"}},
		ReviewJSON:           &wirePlan{Steps: []string{"review"}},
		TaskWorkspace:        "/task",
		TaskFeedback:         "more",
		TurnApproval:         true,
		TurnStatus:           "needs_input",
		TurnQuestion:         "which?",
		TurnSummary:          "half",
		TurnDecisions:        []string{"d"},
		UnsavedWork:          "1 file",
		stopRequested:        true,
		AgentEvents:          []ExecEvent{{Seq: 3, Kind: "tool_start", Text: "bash", At: at}},
		execEventSeq:         3,
		LogLines:             []string{"line"},
	}
}

// A session survives Wire → JSON → NewSessionFromWire whole, hidden fields
// (prompts, file contents, the turn's state) and typed plan included.
func TestSessionWireRoundTrip(t *testing.T) {
	orig := fullSession()
	w1, err := orig.Wire()
	if err != nil {
		t.Fatal(err)
	}
	// The sample sets every field, so the round trip checks them all.
	v := reflect.ValueOf(*w1)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsZero() {
			t.Errorf("fullSession leaves SessionWire.%s unset", v.Type().Field(i).Name)
		}
	}
	data, err := json.Marshal(w1)
	if err != nil {
		t.Fatal(err)
	}
	var got SessionWire
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	s, err := NewSessionFromWire(&got, wireDecoder)
	if err != nil {
		t.Fatal(err)
	}
	w2, err := s.Wire()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(w1, w2) {
		a, _ := json.MarshalIndent(w1, "", " ")
		b, _ := json.MarshalIndent(w2, "", " ")
		t.Fatalf("round trip changed the session:\n%s\n---\n%s", a, b)
	}
	if p, ok := s.PlanJSON.(*wirePlan); !ok || len(p.Steps) != 2 {
		t.Errorf("plan decoded as %T %v", s.PlanJSON, s.PlanJSON)
	}
	if s.Iterations[0].Prompt != "the prompt" || string(s.OutputFiles[0].Data) != "hello" {
		t.Error("hidden fields lost")
	}
}

// Applying a report never takes back a stop the owner asked for, and
// refuses another session's state.
func TestApplyWireKeepsStopAndChecksID(t *testing.T) {
	s := &Session{ID: "agent-1", notify: make(chan struct{}, 1), stopRequested: true}
	if err := s.ApplyWire(&SessionWire{ID: "agent-1", Status: SessionStatusRunning}, nil); err != nil {
		t.Fatal(err)
	}
	if !s.stopRequested {
		t.Error("a report undid the stop")
	}
	if err := s.ApplyWire(&SessionWire{ID: "agent-2"}, nil); err == nil {
		t.Error("applied another session's state")
	}
}
