package repos

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
	"github.com/agent-runner/agent-runner/internal/gitsafe"
)

// The cache holds one bare clone per repo (REPO_CACHE_ROOT/<name>.git),
// refreshed by fetching with the repo's own login and never written to from
// a workspace. Workspaces are cloned from it.

// CachePath is a repo's bare clone in the cache.
func CachePath(cacheRoot, name string) string { return filepath.Join(cacheRoot, name+".git") }

var cacheLocks sync.Map // name -> *sync.Mutex

func cacheLock(name string) *sync.Mutex {
	m, _ := cacheLocks.LoadOrStore(name, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// Sync makes the repo's cache current: a bare clone if there is none, else
// a fetch of every branch (pruned). Errors name the repo and, for a login
// failure, its secret.
func Sync(ctx context.Context, rp Repo, cacheRoot string, secrets *agentsecrets.Store) error {
	auth, err := rp.Auth(secrets)
	if err != nil {
		return err
	}
	mu := cacheLock(rp.Name)
	mu.Lock()
	defer mu.Unlock()
	cache := CachePath(cacheRoot, rp.Name)
	if _, err := os.Stat(filepath.Join(cache, "HEAD")); err != nil {
		if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
			return err
		}
		os.RemoveAll(cache) //nolint:errcheck // a partial clone from before
		if err := run(ctx, "", auth, "clone", "--bare", "--quiet", rp.URL, cache); err != nil {
			os.RemoveAll(cache) //nolint:errcheck
			return rp.explain("clone", err)
		}
		return nil
	}
	if err := run(ctx, cache, gitpkg.Auth{}, "remote", "set-url", "origin", rp.URL); err != nil {
		return err
	}
	if err := run(ctx, cache, auth, "fetch", "--quiet", "--prune", "origin", "+refs/heads/*:refs/heads/*"); err != nil {
		return rp.explain("fetch", err)
	}
	// The cache's HEAD names the default branch as it was at the clone. If
	// that branch is gone (the remote renamed it, or the URL changed),
	// follow the remote's: a checkout would otherwise be an empty repo.
	if run(ctx, cache, gitpkg.Auth{}, "rev-parse", "--verify", "--quiet", "HEAD") != nil {
		if err := repointHead(ctx, rp, cache, auth); err != nil {
			return rp.explain("finding the default branch", err)
		}
	}
	return nil
}

// repointHead sets the cache's HEAD to the remote's default branch.
func repointHead(ctx context.Context, rp Repo, cache string, auth gitpkg.Auth) error {
	cmd := gitsafe.CommandContext(ctx, append(append([]string{}, auth.Args...), "ls-remote", "--symref", "origin", "HEAD")...)
	cmd.Dir = cache
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), auth.Env...)
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(out), "\n") {
		if ref, ok := strings.CutPrefix(line, "ref: "); ok {
			ref, _, _ = strings.Cut(ref, "\t")
			if strings.HasPrefix(ref, "refs/heads/") {
				return run(ctx, cache, gitpkg.Auth{}, "symbolic-ref", "HEAD", ref)
			}
		}
	}
	if strings.TrimSpace(string(out)) == "" {
		return nil // an empty repo: no branch yet, nothing to point at
	}
	return fmt.Errorf("%s does not say what its default branch is", rp.URL)
}

// Checkout puts a working copy of the repo at dst, cloned from its cache
// (synced first) on its branch, with origin pointing at the repo's URL. The
// copy shares no files with the cache. Its git config holds no credential.
func Checkout(ctx context.Context, rp Repo, cacheRoot, dst string, secrets *agentsecrets.Store) error {
	if err := Sync(ctx, rp, cacheRoot, secrets); err != nil {
		return err
	}
	args := []string{"clone", "--quiet", "--no-hardlinks"}
	if rp.Branch != "" {
		args = append(args, "--branch", rp.Branch)
	}
	mu := cacheLock(rp.Name)
	mu.Lock()
	err := run(ctx, "", gitpkg.Auth{}, append(args, CachePath(cacheRoot, rp.Name), dst)...)
	mu.Unlock()
	if err != nil {
		os.RemoveAll(dst) //nolint:errcheck
		return fmt.Errorf("%s: checkout: %w", rp.Name, err)
	}
	return run(ctx, dst, gitpkg.Auth{}, "remote", "set-url", "origin", rp.URL)
}

// Ops is ops logging in as this repo.
func (rp Repo) Ops(ops *gitpkg.Operations, secrets *agentsecrets.Store) (*gitpkg.Operations, error) {
	auth, err := rp.Auth(secrets)
	if err != nil {
		return nil, err
	}
	return ops.WithAuth(auth), nil
}

func (rp Repo) explain(what string, err error) error {
	msg := err.Error()
	if rp.Secret != "" && (strings.Contains(msg, "Authentication failed") || strings.Contains(msg, "could not read Username") ||
		strings.Contains(msg, "403") || strings.Contains(msg, "401")) {
		return fmt.Errorf("%s: %s refused: check /env secret %s and the user name in the URL (%s)", rp.Name, what, rp.Secret, rp.User())
	}
	if rp.Secret == "" && strings.Contains(msg, "could not read Username") {
		return fmt.Errorf("%s: %s needs a login: add a token with /env secret NAME ... and secret=NAME", rp.Name, what)
	}
	return fmt.Errorf("%s: %s: %w", rp.Name, what, err)
}

func run(ctx context.Context, dir string, auth gitpkg.Auth, args ...string) error {
	cmd := gitsafe.CommandContext(ctx, append(append([]string{}, auth.Args...), args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "GIT_TERMINAL_PROMPT=0"), auth.Env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// HasBranch reports whether the repo's cache has branch rp.Branch.
func HasBranch(ctx context.Context, rp Repo, cacheRoot string) bool {
	return run(ctx, CachePath(cacheRoot, rp.Name), gitpkg.Auth{}, "rev-parse", "--verify", "--quiet", "refs/heads/"+rp.Branch) == nil
}

// Register checks rp, fetches it with its login (so a wrong URL, branch or
// token is reported now, not in the next session) and saves it.
func Register(ctx context.Context, reg *Registry, rp Repo, cacheRoot string, secrets *agentsecrets.Store) error {
	if err := rp.Check(); err != nil {
		return err
	}
	if err := Sync(ctx, rp, cacheRoot, secrets); err != nil {
		return err
	}
	if rp.Branch != "" && !HasBranch(ctx, rp, cacheRoot) {
		return fmt.Errorf("%s has no branch %s", rp.Name, rp.Branch)
	}
	return reg.Put(rp)
}

// Unregister removes a repo and its cache.
func Unregister(reg *Registry, name, cacheRoot string) (bool, error) {
	ok, err := reg.Remove(name)
	if ok {
		os.RemoveAll(CachePath(cacheRoot, name)) //nolint:errcheck
	}
	return ok, err
}
