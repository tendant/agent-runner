package agentsecrets

import "testing"

func TestRedactor(t *testing.T) {
	t.Setenv("FAKE_RUNNER_TOKEN", "runner-token-12345")
	t.Setenv("SOME_SETTING", "not-a-secret-value")
	r := NewRedactor([]Secret{{Name: "GITEE_TOKEN", Value: "gitee-abcdef123"}, {Name: "SHORT", Value: "abc"}})
	got := r.Redact("pushed with gitee-abcdef123, runner runner-token-12345, abc, not-a-secret-value")
	want := "pushed with [redacted:GITEE_TOKEN], runner [redacted:FAKE_RUNNER_TOKEN], abc, not-a-secret-value"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
	var nilR *Redactor
	if nilR.Redact("x") != "x" {
		t.Error("nil redactor")
	}
}
