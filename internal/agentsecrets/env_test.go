package agentsecrets

import (
	"path/filepath"
	"testing"
)

func TestLoadPlainAndSecrets(t *testing.T) {
	dir := t.TempDir()
	sec, env := filepath.Join(dir, "secrets.json"), filepath.Join(dir, "env.json")
	New(sec).Set("GITEE_TOKEN", "t0k", "Gitee")
	New(sec).Set("BOTH", "secret-value", "")
	New(env).Set("APP_ENV", "staging", "deploy target")
	New(env).Set("BOTH", "plain-value", "")
	plain, secrets, err := Load(sec, env)
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 1 || plain[0].Name != "APP_ENV" || plain[0].Value != "staging" {
		t.Errorf("plain = %+v; a name in both files is a secret", plain)
	}
	if len(secrets) != 2 {
		t.Errorf("secrets = %+v", secrets)
	}
	if plain, _, err := Load(sec, filepath.Join(dir, "missing.json")); err != nil || len(plain) != 0 {
		t.Errorf("missing env file: %v %v", plain, err)
	}
}

func TestLooksLikeCredential(t *testing.T) {
	for _, v := range []string{
		"ghp_" + "abcdefghijklmnopqrstuvwxyz0123", "github_pat_11ABCDEFG0123456789_abcdefghij",
		"sk-proj-abcdefghijklmnop1234", "sk_live_abcdefghijklmnop1234", "xoxb-1234567890-abcdef",
		"AKIAABCDEFGHIJKLMNOP", "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ1XzEifQ.abcdefghijklmnop", "postgres://app:hunter2@db:5432/app",
	} {
		if !LooksLikeCredential(v) {
			t.Errorf("%q not taken for a credential", v)
		}
	}
	for _, v := range []string{"staging", "https://api.example.com", "postgres://db:5432/app", "8080", "true", "sk-1"} {
		if LooksLikeCredential(v) {
			t.Errorf("%q taken for a credential", v)
		}
	}
}
