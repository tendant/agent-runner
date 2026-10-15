package telegram

import (
	"errors"
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
