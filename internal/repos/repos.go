// Package repos is the registry of repositories agents work on
// (REPOS_DESIGN.md): which ones, from where, with which of the user's
// secrets, on which branch, and whether agent-runner pushes them. It lives in
// DATA_DIR/repos.json and is managed with /repo and /repos, not .env: the
// work is the users', agent-runner's own config is not.
package repos

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
	gitpkg "github.com/agent-runner/agent-runner/internal/git"
)

// Repo is one registered repository.
type Repo struct {
	Name   string `json:"name"`
	URL    string `json:"url"`              // as given, never with a password
	Secret string `json:"secret,omitempty"` // the /secret with its token; "" for a public repo
	Branch string `json:"branch,omitempty"` // "" = the remote's default branch
	Push   bool   `json:"push"`
}

// Registry is the repos file. Safe for concurrent use in one process.
type Registry struct {
	path string
	mu   sync.Mutex
}

// New returns the registry at path (DATA_DIR/repos.json).
func New(path string) *Registry { return &Registry{path: path} }

// List returns the repos by name. A missing file is no repos.
func (r *Registry) List() ([]Repo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.read()
}

// Get returns the repo named name.
func (r *Registry) Get(name string) (Repo, bool, error) {
	list, err := r.List()
	if err != nil {
		return Repo{}, false, err
	}
	for _, rp := range list {
		if rp.Name == name {
			return rp, true, nil
		}
	}
	return Repo{}, false, nil
}

// Put adds or replaces a repo after checking it.
func (r *Registry) Put(rp Repo) error {
	if err := rp.Check(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.read()
	if err != nil {
		return err
	}
	out := list[:0]
	for _, x := range list {
		if x.Name != rp.Name {
			out = append(out, x)
		}
	}
	return r.write(append(out, rp))
}

// Remove deletes a repo, reporting whether it existed.
func (r *Registry) Remove(name string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	list, err := r.read()
	if err != nil {
		return false, err
	}
	out := list[:0]
	for _, x := range list {
		if x.Name != name {
			out = append(out, x)
		}
	}
	if len(out) == len(list) {
		return false, nil
	}
	return true, r.write(out)
}

func (r *Registry) read() ([]Repo, error) {
	data, err := os.ReadFile(r.path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var list []Repo
	if len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("%s: %w", r.path, err)
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	return list, nil
}

func (r *Registry) write(list []Repo) error {
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(r.path), ".repos.json.tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	return os.Rename(tmp.Name(), r.path)
}

var (
	namePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	branchPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	scpLike       = regexp.MustCompile(`^[A-Za-z0-9._-]+@[A-Za-z0-9.-]+:[A-Za-z0-9._/~-]+$`)
)

// AllowLocalPaths lets tests register a repo by its absolute path.
var AllowLocalPaths bool

// Check validates a repo entry.
func (rp Repo) Check() error {
	if !namePattern.MatchString(rp.Name) || strings.Contains(rp.Name, "..") || strings.HasPrefix(rp.Name, "_") {
		return fmt.Errorf("repo name %q: letters, digits, '.', '-', '_'", rp.Name)
	}
	if rp.Branch != "" && (!branchPattern.MatchString(rp.Branch) || strings.Contains(rp.Branch, "..")) {
		return fmt.Errorf("branch %q is not a branch name", rp.Branch)
	}
	if rp.Secret != "" {
		if err := agentsecrets.CheckName(rp.Secret); err != nil {
			return fmt.Errorf("secret: %w", err)
		}
	}
	return CheckURL(rp.URL)
}

// CheckURL accepts https:// and ssh:// URLs and scp-like git@host:path,
// never with a password in them.
func CheckURL(raw string) error {
	if scpLike.MatchString(raw) || AllowLocalPaths && filepath.IsAbs(raw) {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || strings.Trim(u.Path, "/") == "" {
		return fmt.Errorf("%q is not a repository URL (https://host/org/repo.git or git@host:org/repo.git)", raw)
	}
	switch u.Scheme {
	case "https", "ssh":
	case "http":
		return errors.New("http:// would send the token in the clear; use https://")
	default:
		return fmt.Errorf("%s:// URLs are not supported; use https:// or ssh://", u.Scheme)
	}
	if _, ok := u.User.Password(); ok {
		return errors.New("the URL has a password in it: give the token as a /secret instead (secret=NAME)")
	}
	return nil
}

// NameFromURL is the default repo name: the URL's last path element without
// ".git".
func NameFromURL(raw string) string {
	p := raw
	if u, err := url.Parse(raw); err == nil && u.Path != "" && !scpLike.MatchString(raw) {
		p = u.Path
	} else if i := strings.LastIndex(raw, ":"); i >= 0 {
		p = raw[i+1:]
	}
	return strings.TrimSuffix(filepath.Base(strings.TrimRight(p, "/")), ".git")
}

// IsSSH reports an ssh:// or scp-like URL.
func (rp Repo) IsSSH() bool {
	return strings.HasPrefix(rp.URL, "ssh://") || scpLike.MatchString(rp.URL)
}

// User is the login name for HTTPS: the URL's, else "oauth2" (accepted with
// a token by GitLab, Gitea and GitHub; hosts that need the real name, such as
// Gitee, get it in the URL).
func (rp Repo) User() string {
	if u, err := url.Parse(rp.URL); err == nil && u.User != nil && u.User.Username() != "" {
		return u.User.Username()
	}
	return "oauth2"
}

// Env variables carrying the login to the credential helper below: names a
// /secret cannot take (AGENT_ is agent-runner's).
const (
	userEnv     = "AGENT_RUNNER_GIT_USER"
	passwordEnv = "AGENT_RUNNER_GIT_PASSWORD"
)

// Auth is how agent-runner's own git commands in this repo log in: a
// credential helper given on the command line (over any the repo's config
// names), reading the token from that one command's environment. Nothing is
// written to git config, a URL or a log. A repo without a secret gets no
// helper and no prompt.
func (rp Repo) Auth(secrets *agentsecrets.Store) (gitpkg.Auth, error) {
	a := gitpkg.Auth{Env: []string{"GIT_TERMINAL_PROMPT=0"}}
	if rp.Secret == "" {
		return a, nil
	}
	if rp.IsSSH() {
		return a, fmt.Errorf("%s: ssh URLs with a secret are not supported yet; use the https:// URL", rp.Name)
	}
	token, err := lookup(secrets, rp.Secret)
	if err != nil {
		return a, fmt.Errorf("%s: %w", rp.Name, err)
	}
	a.Args = []string{"-c", "credential.helper=", "-c",
		`credential.helper=!f() { test "$1" = get || exit 0; echo "username=$` + userEnv + `"; echo "password=$` + passwordEnv + `"; }; f`}
	a.Env = append(a.Env, userEnv+"="+rp.User(), passwordEnv+"="+token)
	return a, nil
}

func lookup(secrets *agentsecrets.Store, name string) (string, error) {
	if secrets == nil {
		return "", fmt.Errorf("secret %s is not set (/secret set %s ...)", name, name)
	}
	list, err := secrets.List()
	if err != nil {
		return "", err
	}
	for _, s := range list {
		if s.Name == name {
			return s.Value, nil
		}
	}
	return "", fmt.Errorf("secret %s is not set (/secret set %s ...)", name, name)
}
