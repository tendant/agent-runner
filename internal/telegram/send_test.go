package telegram

import (
	"errors"
	"strings"
	"testing"
)

func TestPlainTextResend(t *testing.T) {
	if !isParseError(errors.New("Bad Request: can't parse entities: Can't find end of the entity starting at byte offset 12")) {
		t.Error("parse errors must be recognised")
	}
	if isParseError(errors.New("Forbidden: bot was blocked by the user")) {
		t.Error("other errors are not parse errors")
	}
	if got := plainText("**ok:** GITEE\\_TOKEN set, use `$GITEE_TOKEN`"); got != "ok: GITEE_TOKEN set, use $GITEE_TOKEN" {
		t.Errorf("plain: %q", got)
	}
}

func TestSplitMessage(t *testing.T) {
	long := strings.Repeat("line of text\n", 1000) // 13000 runes
	parts := splitMessage(long, telegramMaxLen)
	if len(parts) < 4 {
		t.Fatalf("%d parts", len(parts))
	}
	total := 0
	for _, p := range parts {
		if n := len([]rune(p)); n > telegramMaxLen {
			t.Errorf("part of %d runes", n)
		}
		total += len(p)
	}
	if total < len(long)-len(parts) {
		t.Error("text lost")
	}
	if got := splitMessage("short", telegramMaxLen); len(got) != 1 {
		t.Error("short message")
	}
}
