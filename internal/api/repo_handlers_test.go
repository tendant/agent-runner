package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/repos"
)

func TestHandleRepos(t *testing.T) {
	env := setupTestEnv(t)
	repos.AllowLocalPaths = true
	bare := filepath.Join(t.TempDir(), "site.git")
	exec.Command("git", "init", "-q", "--bare", "-b", "main", bare).Run()

	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
		w := httptest.NewRecorder()
		env.handlers.HandleRepos(w, req)
		return w
	}
	if w := do(http.MethodPost, "/repos", `{"url":"`+bare+`","push":false}`); w.Code != http.StatusCreated {
		t.Fatalf("post: %d %s", w.Code, w.Body)
	}
	w := do(http.MethodGet, "/repos", "")
	var got struct{ Repos []repos.Repo }
	json.Unmarshal(w.Body.Bytes(), &got)
	if len(got.Repos) != 1 || got.Repos[0].Name != "site" || got.Repos[0].Push {
		t.Errorf("list: %s", w.Body)
	}
	if w := do(http.MethodPost, "/repos", `{"url":"https://a:b@x.com/o/r.git"}`); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "password") {
		t.Errorf("refused URL: %d %s", w.Code, w.Body)
	}
	if w := do(http.MethodDelete, "/repos/site", ""); w.Code != http.StatusNoContent {
		t.Errorf("delete: %d", w.Code)
	}
	if w := do(http.MethodDelete, "/repos/site", ""); w.Code != http.StatusNotFound {
		t.Errorf("delete again: %d", w.Code)
	}
}

func TestHandleRunRefusesReadOnlyRepo(t *testing.T) {
	env := setupTestEnv(t)
	repos.AllowLocalPaths = true
	repos.New(env.handlers.config.ReposFile).Put(repos.Repo{Name: "docs", URL: filepath.Join(t.TempDir(), "docs.git"), Push: false})
	w := postJSON(env.handlers.HandleRun, map[string]any{"project": "docs", "instruction": "x", "paths": []string{"src/"}})
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "read-only") {
		t.Errorf("got %d %s", w.Code, w.Body)
	}
}
