package gitsafe

import "testing"

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://sites:2879578495d5a8@git.memochat.ai/agent/r.git": "https://sites:xxxxx@git.memochat.ai/agent/r.git",
		"https://ghp_abc123@github.com/o/r.git":                    "https://xxxxx@github.com/o/r.git",
		"https://github.com/o/r.git":                               "https://github.com/o/r.git",
		"git@github.com:o/r.git":                                   "git@github.com:o/r.git",
		"ssh://git@github.com/o/r.git":                             "ssh://git@github.com/o/r.git",
		"/srv/repos/r":                                             "/srv/repos/r",
		"":                                                         "",
	} {
		if got := RedactURL(in); got != want {
			t.Errorf("RedactURL(%q) = %q, want %q", in, got, want)
		}
	}
	if !HasCredential("https://u:p@h/r") || HasCredential("https://h/r") {
		t.Error("HasCredential")
	}
}
