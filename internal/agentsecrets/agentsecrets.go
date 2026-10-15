// Package agentsecrets keeps the secrets agent users give their agents
// (a Gitee token, a Stripe key), apart from agent-runner's own (.env:
// model keys, GIT_TOKEN, bot tokens). They live in DATA_DIR/secrets.json,
// are set with /secret, and reach every agent session as environment
// variables, sandboxed or not: they are for the agent, so the sandbox does
// not hide them. Agents are told each one's name and description, never
// its value. A name agent-runner reads itself is refused, so the two kinds
// cannot mix.
package agentsecrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Secret is one agent secret.
type Secret struct {
	Name        string `json:"-"`
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// Store is the secrets file. Safe for concurrent use in one process.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns the store at path (DATA_DIR/secrets.json).
func New(path string) *Store { return &Store{path: path} }

// Path is the secrets file.
func (s *Store) Path() string { return s.path }

// List returns the secrets by name. A missing file is no secrets.
func (s *Store) List() ([]Secret, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.read()
	if err != nil {
		return nil, err
	}
	out := make([]Secret, 0, len(m))
	for name, sec := range m {
		sec.Name = name
		out = append(out, sec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Env is the secrets as KEY=VALUE, for an agent session's environment.
func (s *Store) Env() ([]Secret, []string, error) {
	list, err := s.List()
	if err != nil {
		return nil, nil, err
	}
	return list, EnvOf(list), nil
}

// EnvOf is list as KEY=VALUE, leaving out any name agent-runner reads.
func EnvOf(list []Secret) []string {
	env := make([]string, 0, len(list))
	for _, sec := range list {
		if CheckName(sec.Name) != nil { // a hand-edited file: never pass a runner name
			continue
		}
		env = append(env, sec.Name+"="+sec.Value)
	}
	return env
}

// Set adds or replaces a secret.
func (s *Store) Set(name, value, description string) error {
	if err := CheckName(name); err != nil {
		return err
	}
	if value == "" {
		return errors.New("empty value")
	}
	if strings.ContainsAny(value, "\x00\n") {
		return errors.New("a value cannot contain a newline or NUL")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.read()
	if err != nil {
		return err
	}
	m[name] = Secret{Value: value, Description: strings.TrimSpace(description)}
	return s.write(m)
}

// Remove deletes a secret, reporting whether it existed.
func (s *Store) Remove(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.read()
	if err != nil {
		return false, err
	}
	if _, ok := m[name]; !ok {
		return false, nil
	}
	delete(m, name)
	return true, s.write(m)
}

func (s *Store) read() (map[string]Secret, error) {
	m := map[string]Secret{}
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
		return m, nil
	}
	if err != nil {
		return nil, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return m, nil
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", s.path, err)
	}
	return m, nil
}

func (s *Store) write(m map[string]Secret) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".secrets.json.tmp-*") // 0600
	if err != nil {
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		os.Remove(tmp.Name()) //nolint:errcheck
		return err
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

// runnerPrefixes are agent-runner's own setting families, and variables
// that change how git or the dynamic linker behave.
var runnerPrefixes = []string{
	"AGENT_", "FAST_LLM_", "STREAM_", "TELEGRAM_", "WECHAT_", "SCHEDULER_", "OTEL_",
	"MEMORY_", "GIT_", "JOB_", "VALIDATION_", "ANTHROPIC_", "OPENAI_", "CLAUDE_",
	"CODEX_", "PI_", "DEEPSEEK_", "XDG_", "LD_", "DYLD_",
}

// runnerNames are single names agent-runner reads, or sets for every agent
// process (proxies, HOME, ...).
var runnerNames = map[string]bool{
	"API_KEY": true, "API_BIND": true, "DATA_DIR": true, "INSTANCE": true, "LOGS_ROOT": true,
	"STATE_ROOT": true, "TMP_ROOT": true, "OUTPUTS_ROOT": true, "UPLOADS_ROOT": true,
	"REPO_CACHE_ROOT": true, "ALLOWED_PROJECTS": true, "ANALYZER_TIMEOUT_SECONDS": true,
	"CLEANUP_STALE_JOBS": true, "LOG_RETENTION_DAYS": true, "TRACING_ENABLED": true,
	"WELCOME_ENABLED": true, "IS_SANDBOX": true,
	"PATH": true, "HOME": true, "USER": true, "SHELL": true, "TMPDIR": true, "LANG": true, "TERM": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true, "ALL_PROXY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true, "NODE_OPTIONS": true, "NODE_EXTRA_CA_CERTS": true,
}

// CheckName refuses a name that is not an environment variable name, or one
// agent-runner reads or sets itself.
func CheckName(name string) error {
	if !namePattern.MatchString(name) {
		return fmt.Errorf("%q: use upper-case letters, digits and _ (e.g. GITEE_TOKEN)", name)
	}
	if runnerNames[name] {
		return fmt.Errorf("%s is agent-runner's own setting, not an agent secret", name)
	}
	for _, p := range runnerPrefixes {
		if strings.HasPrefix(name, p) {
			return fmt.Errorf("%s: names starting with %s are agent-runner's own; pick another (e.g. GITEE_TOKEN)", name, p)
		}
	}
	return nil
}
