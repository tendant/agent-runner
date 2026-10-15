package agentsecrets

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data", "secrets.json")
	s := New(path)
	if list, err := s.List(); err != nil || len(list) != 0 {
		t.Fatalf("missing file: %v %v", list, err)
	}
	if err := s.Set("GITEE_TOKEN", "tok-1", "Gitee API for oam-server"); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("STRIPE_API_KEY", "sk", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Set("GITEE_TOKEN", "tok-2", "Gitee API"); err != nil { // replace
		t.Fatal(err)
	}
	list, env, err := s.Env()
	if err != nil || len(list) != 2 || list[0].Name != "GITEE_TOKEN" || list[0].Description != "Gitee API" {
		t.Fatalf("list %+v %v", list, err)
	}
	if strings.Join(env, " ") != "GITEE_TOKEN=tok-2 STRIPE_API_KEY=sk" {
		t.Errorf("env %v", env)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	if ok, err := s.Remove("STRIPE_API_KEY"); !ok || err != nil {
		t.Errorf("remove: %v %v", ok, err)
	}
	if ok, _ := s.Remove("STRIPE_API_KEY"); ok {
		t.Error("removed twice")
	}
	for _, bad := range []struct{ name, value string }{{"GIT_TOKEN", "x"}, {"lower", "x"}, {"OK_NAME", ""}, {"OK_NAME", "a\nb"}} {
		if err := s.Set(bad.name, bad.value, ""); err == nil {
			t.Errorf("Set(%q, %q) must fail", bad.name, bad.value)
		}
	}
}

// A runner name put into the file by hand is never passed to agents.
func TestEnvSkipsRunnerNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	os.WriteFile(path, []byte(`{"GIT_TOKEN":{"value":"runner"},"GITEE_TOKEN":{"value":"user"}}`), 0o600)
	_, env, err := New(path).Env()
	if err != nil || len(env) != 1 || env[0] != "GITEE_TOKEN=user" {
		t.Errorf("env %v %v", env, err)
	}
}

// Every environment variable agent-runner reads is refused as an agent
// secret name. Fails when a new setting is added without a matching rule.
func TestCheckNameCoversRunnerSettings(t *testing.T) {
	read := regexp.MustCompile(`(?:env[A-Za-z]*|Getenv|LookupEnv)\("([A-Z][A-Z0-9_]+)"`)
	root := filepath.Join("..", "..")
	seen := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (d.Name() == "e2e" || d.Name() == "conformance" || strings.HasPrefix(d.Name(), ".") && p != root) {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		b, _ := os.ReadFile(p)
		for _, m := range read.FindAllStringSubmatch(string(b), -1) {
			seen[m[1]] = true
		}
		return nil
	})
	if len(seen) < 50 {
		t.Fatalf("found only %d settings: the scan is broken", len(seen))
	}
	for name := range seen {
		if CheckName(name) == nil {
			t.Errorf("%s is read by agent-runner but allowed as an agent secret", name)
		}
	}
	for _, ok := range []string{"GITEE_TOKEN", "GH_TOKEN", "STRIPE_API_KEY", "AWS_ACCESS_KEY_ID", "DATABASE_URL"} {
		if err := CheckName(ok); err != nil {
			t.Errorf("%s should be allowed: %v", ok, err)
		}
	}
}
