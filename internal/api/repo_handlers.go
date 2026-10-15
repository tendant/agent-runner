package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

// HandleRepos serves the repo registry (REPOS_DESIGN.md), as /repo does in
// chat: GET /repos lists, POST /repos {url, name?, secret?, branch?, push?}
// registers (fetching first), DELETE /repos/{name} removes.
func (h *Handlers) HandleRepos(w http.ResponseWriter, r *http.Request) {
	reg := repos.New(h.config.ReposFile)
	name := strings.Trim(strings.TrimPrefix(r.URL.Path, "/repos"), "/")
	switch {
	case r.Method == http.MethodGet && name == "":
		list, err := reg.List()
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if list == nil {
			list = []repos.Repo{}
		}
		h.writeJSON(w, http.StatusOK, map[string]any{"repos": list})
	case r.Method == http.MethodPost && name == "":
		var req struct {
			URL    string `json:"url"`
			Name   string `json:"name"`
			Secret string `json:"secret"`
			Branch string `json:"branch"`
			Push   *bool  `json:"push"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			h.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
			return
		}
		rp := repos.Repo{URL: req.URL, Name: req.Name, Secret: req.Secret, Branch: req.Branch, Push: req.Push == nil || *req.Push}
		if rp.Name == "" {
			rp.Name = repos.NameFromURL(rp.URL)
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
		defer cancel()
		if err := repos.Register(ctx, reg, rp, h.config.RepoCacheRoot, agentsecrets.New(h.config.SecretsFile)); err != nil {
			h.writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.writeJSON(w, http.StatusCreated, rp)
	case r.Method == http.MethodDelete && name != "":
		ok, err := repos.Unregister(reg, name, h.config.RepoCacheRoot)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		if !ok {
			h.writeError(w, http.StatusNotFound, "no repo named "+name)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		h.writeError(w, http.StatusMethodNotAllowed, "GET /repos, POST /repos, DELETE /repos/{name}")
	}
}
