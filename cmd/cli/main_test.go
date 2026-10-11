package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// readUntilPasteEnd is the low-level primitive still worth testing.

func TestReadUntilPasteEnd_Simple(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello\x1b[201~rest"))
	got, _ := readUntilPasteEnd(r)
	if got != "hello" {
		t.Errorf("expected 'hello', got %q", got)
	}
	// "rest" should remain unread
	remaining, _ := r.ReadString('\n')
	if !strings.HasPrefix(remaining, "r") {
		t.Errorf("expected 'rest' still in reader, got %q", remaining)
	}
}

func TestReadUntilPasteEnd_MultiLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("line1\nline2\nline3\x1b[201~"))
	got, _ := readUntilPasteEnd(r)
	if got != "line1\nline2\nline3" {
		t.Errorf("expected multi-line content, got %q", got)
	}
}

func TestCollectPaste_SingleLine(t *testing.T) {
	// Input after \x1b[200~ has been consumed: paste content + end marker + trailing \n
	r := bufio.NewReader(strings.NewReader("hello world\x1b[201~\n"))
	var acc []string
	line := collectPaste(r, nil, &acc)
	if string(line) != "hello world" {
		t.Errorf("expected 'hello world' as remaining line, got %q", line)
	}
	if len(acc) != 0 {
		t.Errorf("expected empty acc for single-line paste, got %v", acc)
	}
}

func TestCollectPaste_MultiLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("line1\nline2\nline3\x1b[201~\n"))
	var acc []string
	line := collectPaste(r, nil, &acc)
	if string(line) != "line3" {
		t.Errorf("expected last line 'line3' as current line, got %q", line)
	}
	if len(acc) != 2 || acc[0] != "line1" || acc[1] != "line2" {
		t.Errorf("expected acc=[line1,line2], got %v", acc)
	}
}

func TestCollectPaste_PreservesPriorTypedText(t *testing.T) {
	// User typed "prefix " before pasting
	r := bufio.NewReader(strings.NewReader("pasted\x1b[201~\n"))
	var acc []string
	line := collectPaste(r, []byte("prefix "), &acc)
	// typed prefix should be saved to acc, last paste line is new line
	if len(acc) != 1 || acc[0] != "prefix " {
		t.Errorf("expected acc=[\"prefix \"], got %v", acc)
	}
	if string(line) != "pasted" {
		t.Errorf("expected 'pasted' as current line, got %q", line)
	}
}

// fakeStream serves one session's SSE stream: the done event right away, or
// only after a stop request when hold is set. It counts stop requests.
func fakeStream(t *testing.T, hold bool) (url string, stops *atomic.Int32) {
	stops = new(atomic.Int32)
	stopped := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/stop") {
			if stops.Add(1) == 1 {
				close(stopped)
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.(http.Flusher).Flush()
		if hold {
			select {
			case <-stopped:
			case <-r.Context().Done(): // the client hung up
				return
			}
		}
		fmt.Fprint(w, "event: done\ndata: {\"status\":\"completed\"}\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv.URL, stops
}

func TestFinishedSessionIsNotStopped(t *testing.T) {
	url, stops := fakeStream(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	if err := streamSession(ctx, url, "", "agent-1"); err != nil {
		t.Fatal(err)
	}
	cancel() // what the REPL does after every session
	time.Sleep(100 * time.Millisecond)
	if n := stops.Load(); n != 0 {
		t.Errorf("a finished session got %d stop requests", n)
	}
}

func TestCtrlCStopsTheSession(t *testing.T) {
	url, stops := fakeStream(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }() // Ctrl+C
	done := make(chan error, 1)
	go func() { done <- streamSession(ctx, url, "", "agent-1") }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream did not end after the stop")
	}
	if n := stops.Load(); n != 1 {
		t.Errorf("Ctrl+C sent %d stop requests, want 1", n)
	}
}
