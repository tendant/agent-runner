package wechat

import (
	"path/filepath"
	"testing"
)

func TestAllowedSender(t *testing.T) {
	b := &Bot{ownerFile: filepath.Join(t.TempDir(), "wechat-owner")}
	if !b.allowedSender("alice") {
		t.Fatal("the first user binds the bot")
	}
	if b.allowedSender("mallory") {
		t.Error("another user must be refused")
	}
	if !b.allowedSender("alice") {
		t.Error("the owner stays allowed")
	}
	listed := &Bot{allowed: []string{"bob"}, ownerFile: filepath.Join(t.TempDir(), "o")}
	if !listed.allowedSender("bob") || listed.allowedSender("alice") {
		t.Error("WECHAT_ALLOWED_USERS decides when set")
	}
}
