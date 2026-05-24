package main

import (
	"bufio"
	"strings"
	"testing"
)

func TestReadToken_TypedLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("hello\n"))
	tok, err := readToken(r)
	if err != nil {
		t.Fatal(err)
	}
	if tok.text != "hello" || tok.paste {
		t.Errorf("expected typed line 'hello', got %+v", tok)
	}
}

func TestReadToken_EmptyEnter(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\n"))
	tok, _ := readToken(r)
	if tok.text != "" || tok.paste {
		t.Errorf("expected empty typed line, got %+v", tok)
	}
}

func TestReadToken_BracketedPaste_SingleLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\x1b[200~hello world\x1b[201~"))
	tok, _ := readToken(r)
	if !tok.paste {
		t.Fatal("expected paste=true")
	}
	if tok.text != "hello world" {
		t.Errorf("expected 'hello world', got %q", tok.text)
	}
}

func TestReadToken_BracketedPaste_MultiLine(t *testing.T) {
	r := bufio.NewReader(strings.NewReader("\x1b[200~line1\nline2\nline3\x1b[201~"))
	tok, _ := readToken(r)
	if !tok.paste {
		t.Fatal("expected paste=true")
	}
	if tok.text != "line1\nline2\nline3" {
		t.Errorf("unexpected text: %q", tok.text)
	}
}

func TestReadToken_BracketedPaste_WithPrefixText(t *testing.T) {
	// text typed before paste marker on same line
	r := bufio.NewReader(strings.NewReader("prefix\x1b[200~pasted\x1b[201~"))
	tok, _ := readToken(r)
	if !tok.paste {
		t.Fatal("expected paste=true")
	}
	if !strings.Contains(tok.text, "prefix") || !strings.Contains(tok.text, "pasted") {
		t.Errorf("expected prefix and pasted content, got %q", tok.text)
	}
}

func TestReadToken_BracketedPaste_AutoNewlineConsumed(t *testing.T) {
	// Terminals often append \n after \x1b[201~; it must NOT become a typed Enter.
	input := "\x1b[200~pasted\x1b[201~\ntyped\n"
	r := bufio.NewReader(strings.NewReader(input))

	tok1, _ := readToken(r)
	if !tok1.paste || tok1.text != "pasted" {
		t.Errorf("tok1: expected paste 'pasted', got %+v", tok1)
	}

	// Next token should be the typed line, not a spurious empty Enter.
	tok2, _ := readToken(r)
	if tok2.paste || tok2.text != "typed" {
		t.Errorf("tok2: expected typed 'typed', got %+v", tok2)
	}
}

func TestReadToken_MultipleTokens(t *testing.T) {
	// The \n after \x1b[201~ is consumed automatically; only 2 tokens expected.
	input := "\x1b[200~pasted line1\nline2\x1b[201~\ntyped line\n"
	r := bufio.NewReader(strings.NewReader(input))

	tok1, _ := readToken(r)
	if !tok1.paste || tok1.text != "pasted line1\nline2" {
		t.Errorf("tok1: expected paste with 'pasted line1\\nline2', got %+v", tok1)
	}

	tok2, _ := readToken(r)
	if tok2.paste || tok2.text != "typed line" {
		t.Errorf("tok2: expected typed 'typed line', got %+v", tok2)
	}
}
