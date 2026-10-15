package chatcmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	"github.com/agent-runner/agent-runner/internal/repos"
)

const repoUsage = "usage: /repo add <url> [name=NAME] [secret=SECRET] [branch=BRANCH] [push=false] · /repo list · /repo set <name> key=value ... · /repo remove <name>"

// handleRepo manages the repo registry (internal/repos, REPOS_DESIGN.md):
// the repos every session works on, with the /secret each logs in with.
func (c *Commander) handleRepo(arg string) string {
	fields := strings.Fields(arg)
	if len(fields) == 0 {
		return c.handleRepoList()
	}
	reg := repos.New(c.cfg.ReposFile)
	switch strings.ToLower(fields[0]) {
	case "list":
		return c.handleRepoList()
	case "add":
		if len(fields) < 2 {
			return "error: " + repoUsage
		}
		rp := repos.Repo{URL: fields[1], Name: repos.NameFromURL(fields[1]), Push: true}
		if err := applyRepoOptions(&rp, fields[2:]); err != nil {
			return "error: " + err.Error()
		}
		if _, exists, _ := reg.Get(rp.Name); exists {
			return fmt.Sprintf("error: %s is already registered (/repo set %s ... to change it)", rp.Name, rp.Name)
		}
		return c.saveRepo(reg, rp, "added")
	case "set":
		if len(fields) < 3 {
			return "error: " + repoUsage
		}
		rp, ok, err := reg.Get(fields[1])
		if err != nil {
			return "error: " + err.Error()
		}
		if !ok {
			return "error: no repo named " + fields[1]
		}
		if err := applyRepoOptions(&rp, fields[2:]); err != nil {
			return "error: " + err.Error()
		}
		if rp.Name != fields[1] {
			return "error: a repo cannot be renamed; remove it and add it again"
		}
		return c.saveRepo(reg, rp, "updated")
	case "remove", "rm":
		if len(fields) != 2 {
			return "error: " + repoUsage
		}
		ok, err := repos.Unregister(reg, fields[1], c.cfg.RepoCacheRoot)
		if err != nil {
			return "error: " + err.Error()
		}
		if !ok {
			return "no repo named " + fields[1]
		}
		return "ok: " + fields[1] + " removed; sessions no longer get it"
	}
	return "error: " + repoUsage
}

// saveRepo registers the repo (fetching it first) and describes it.
func (c *Commander) saveRepo(reg *repos.Registry, rp repos.Repo, verb string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := repos.Register(ctx, reg, rp, c.cfg.RepoCacheRoot, agentsecrets.New(c.cfg.SecretsFile)); err != nil {
		return "error: " + err.Error()
	}
	return fmt.Sprintf("ok: %s %s — %s", rp.Name, verb, describeRepo(rp))
}

func applyRepoOptions(rp *repos.Repo, opts []string) error {
	for _, o := range opts {
		k, v, ok := strings.Cut(o, "=")
		if !ok {
			return fmt.Errorf("%q: options are key=value (name, secret, branch, push)", o)
		}
		switch strings.ToLower(k) {
		case "name":
			rp.Name = v
		case "secret":
			rp.Secret = v
		case "branch":
			rp.Branch = v
		case "push":
			switch strings.ToLower(v) {
			case "true", "yes", "on":
				rp.Push = true
			case "false", "no", "off":
				rp.Push = false
			default:
				return fmt.Errorf("push=%s: true or false", v)
			}
		case "url":
			rp.URL = v
		default:
			return fmt.Errorf("unknown option %s (name, url, secret, branch, push)", k)
		}
	}
	return nil
}

func describeRepo(rp repos.Repo) string {
	branch := rp.Branch
	if branch == "" {
		branch = "default branch"
	}
	login := "public (no secret)"
	if rp.Secret != "" {
		login = "as " + rp.User() + " with $" + rp.Secret
	}
	push := "pushed"
	if !rp.Push {
		push = "read-only"
	}
	return fmt.Sprintf("%s, %s, %s, %s", rp.URL, branch, login, push)
}

func (c *Commander) handleRepoList() string {
	list, err := repos.New(c.cfg.ReposFile).List()
	if err != nil {
		return "error: " + err.Error()
	}
	if len(list) == 0 {
		return "no repos (add one: /repo add <url> [secret=NAME])"
	}
	var b strings.Builder
	b.WriteString("Repos in every session:\n")
	for _, rp := range list {
		fmt.Fprintf(&b, "- %s: %s\n", rp.Name, describeRepo(rp))
	}
	return strings.TrimSpace(b.String())
}
