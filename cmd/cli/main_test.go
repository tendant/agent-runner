package main

import (
	"bufio"
	"strings"
	"testing"
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
