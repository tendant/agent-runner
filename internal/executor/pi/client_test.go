package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestMain re-execs the test binary as a fake pi process when GO_WANT_FAKE_PI
// is set, so Client tests exercise real pipes and process lifecycle.
func TestMain(m *testing.M) {
	if os.Getenv("GO_WANT_FAKE_PI") == "1" {
		fakePi()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakePi speaks just enough of the RPC protocol for the tests, with behavior
// variants selected by FAKE_PI_MODE.
func fakePi() {
	mode := os.Getenv("FAKE_PI_MODE")
	emit := func(format string, args ...any) {
		fmt.Fprintf(os.Stdout, format+"\n", args...)
	}
	settle := func() {
		emit(`{"type":"tool_execution_start","toolName":"bash"}`)
		emit(`{"type":"tool_execution_end","toolName":"bash"}`)
		emit(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"fallback text"}],"usage":{"input":982,"output":15,"cost":{"input":0.0003,"output":0.00002,"total":0.05}}}}`)
		emit(`{"type":"agent_settled"}`)
	}

	if mode == "malformed" {
		emit(`this is not json`)
	}

	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 1024*1024), 10*1024*1024)
	for in.Scan() {
		var req struct {
			ID        string `json:"id"`
			Type      string `json:"type"`
			Message   string `json:"message"`
			Cancelled bool   `json:"cancelled"`
		}
		if json.Unmarshal(in.Bytes(), &req) != nil {
			continue
		}
		switch req.Type {
		case "prompt":
			emit(`{"type":"response","id":%q,"command":"prompt","success":true}`, req.ID)
			switch mode {
			case "die":
				os.Exit(1)
			case "ui":
				emit(`{"type":"extension_ui_request","id":"ui-1"}`)
			case "hang-abort":
				// settle only on abort
			default:
				settle()
			}
		case "abort":
			emit(`{"type":"response","id":%q,"command":"abort","success":true}`, req.ID)
			emit(`{"type":"agent_settled"}`)
		case "get_last_assistant_text":
			if mode == "nolast" {
				emit(`{"type":"response","id":%q,"command":"get_last_assistant_text","success":false,"error":"unavailable"}`, req.ID)
			} else {
				emit(`{"type":"response","id":%q,"command":"get_last_assistant_text","success":true,"data":{"text":"authoritative text"}}`, req.ID)
			}
		case "extension_ui_response":
			if mode == "ui" && req.Cancelled {
				settle()
			}
		}
	}
}

func startFake(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := Start(Options{
		Binary:    os.Args[0],
		Workspace: t.TempDir(),
		Env:       []string{"GO_WANT_FAKE_PI=1", "FAKE_PI_MODE=" + mode},
	})
	if err != nil {
		t.Fatalf("start fake pi: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPromptSettles(t *testing.T) {
	c := startFake(t, "")
	res, err := c.Prompt(context.Background(), "do the thing")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.Text != "authoritative text" {
		t.Errorf("text = %q, want authoritative text", res.Text)
	}
	if res.CostUSD != 0.05 {
		t.Errorf("cost = %v, want 0.05", res.CostUSD)
	}

	kinds := map[string]bool{}
	for {
		select {
		case ev := <-c.Events():
			kinds[ev.Kind] = true
			continue
		default:
		}
		break
	}
	for _, want := range []string{"prompt_start", "tool_start", "tool_end", "settled"} {
		if !kinds[want] {
			t.Errorf("missing event %q (got %v)", want, kinds)
		}
	}
}

func TestPromptTextFallback(t *testing.T) {
	c := startFake(t, "nolast")
	res, err := c.Prompt(context.Background(), "x")
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if res.Text != "fallback text" {
		t.Errorf("text = %q, want fallback text from message_end", res.Text)
	}
}

func TestMalformedLineSkipped(t *testing.T) {
	c := startFake(t, "malformed")
	if _, err := c.Prompt(context.Background(), "x"); err != nil {
		t.Fatalf("prompt after malformed line: %v", err)
	}
}

func TestUIRequestAutoCancelled(t *testing.T) {
	// The fake settles ONLY after receiving a cancelled:true ui response, so
	// this passing proves the auto-cancel reply is sent.
	c := startFake(t, "ui")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Prompt(ctx, "x"); err != nil {
		t.Fatalf("prompt blocked on ui dialog: %v", err)
	}
}

func TestProcessDeath(t *testing.T) {
	c := startFake(t, "die")
	_, err := c.Prompt(context.Background(), "x")
	if !errors.Is(err, ErrDead) {
		t.Fatalf("err = %v, want ErrDead", err)
	}
	// A dead client refuses further prompts.
	if _, err := c.Prompt(context.Background(), "again"); !errors.Is(err, ErrDead) {
		t.Fatalf("second prompt err = %v, want ErrDead", err)
	}
}

func TestTimeoutAbortsGracefully(t *testing.T) {
	c := startFake(t, "hang-abort")
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Prompt(ctx, "x")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want DeadlineExceeded", err)
	}
	// The fake acks abort and settles, so we should be well under the 5s
	// kill grace.
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("abort took %v, expected fast graceful settle", elapsed)
	}
}
