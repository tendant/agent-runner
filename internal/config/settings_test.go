package config

import (
	"os"
	"regexp"
	"testing"
)

// Every setting config.go reads, and every one .env.example documents, is
// one /config set accepts: a setting added without it would be refused.
func TestSettingsCoverConfigAndExample(t *testing.T) {
	src, err := os.ReadFile("config.go")
	if err != nil {
		t.Fatal(err)
	}
	read := regexp.MustCompile(`(?:env[A-Za-z]*|Getenv|LookupEnv)\(\s*"([A-Z][A-Z0-9_]+)"`)
	for _, m := range read.FindAllStringSubmatch(string(src), -1) {
		if !Settings[m[1]] {
			t.Errorf("config.go reads %s: add it to Settings", m[1])
		}
	}
	alias := regexp.MustCompile(`envAlias\(([^)]*)\)`)
	name := regexp.MustCompile(`"([A-Z][A-Z0-9_]+)"`)
	for _, m := range alias.FindAllStringSubmatch(string(src), -1) {
		for _, n := range name.FindAllStringSubmatch(m[1], -1) {
			if !Settings[n[1]] {
				t.Errorf("config.go reads %s: add it to Settings", n[1])
			}
		}
	}
	example, err := os.ReadFile("../../.env.example")
	if err != nil {
		t.Fatal(err)
	}
	documented := regexp.MustCompile(`(?m)^#\s?([A-Z][A-Z0-9_]+)=`)
	for _, m := range documented.FindAllStringSubmatch(string(example), -1) {
		if m[1] == "MYPROVIDER_API_KEY" { // a placeholder for a provider of your own
			continue
		}
		if !Settings[m[1]] {
			t.Errorf(".env.example documents %s: add it to Settings", m[1])
		}
	}
}

func TestCheckSetting(t *testing.T) {
	for _, ok := range []string{"AGENT_CLI", "DEEPSEEK_API_KEY", "STREAM_BOT_TOKEN"} {
		if err := CheckSetting(ok); err != nil {
			t.Errorf("%s refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"APP_ENV", "STRIPE_API_KEY", "GIT_TOKEN"} {
		if err := CheckSetting(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if !NeedsRestart("STREAM_SERVER_URL") || !NeedsRestart("API_BIND") || NeedsRestart("AGENT_CLI") || NeedsRestart("DEEPSEEK_API_KEY") {
		t.Error("NeedsRestart misclassifies")
	}
}

func TestUnsetEnvLocal(t *testing.T) {
	path := t.TempDir() + "/.env.local"
	os.WriteFile(path, []byte("A=1\nAGENT_CLI=claude\nB=2\n"), 0600)
	if found, err := unsetEnvLocalFile(path, "AGENT_CLI"); !found || err != nil {
		t.Fatalf("unset = %v, %v", found, err)
	}
	if got, _ := os.ReadFile(path); string(got) != "A=1\nB=2\n" {
		t.Fatalf("file = %q", got)
	}
	if found, _ := unsetEnvLocalFile(path, "AGENT_CLI"); found {
		t.Fatal("unset twice found it")
	}
}
