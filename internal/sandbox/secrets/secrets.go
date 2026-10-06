// Package secrets turns trusted-policy SecretGrants into sandbox inputs.
// Secrets come only from a runner-side Provider named by policy; the sandbox or
// its tools can neither add nor enumerate them. Delivery prefers an inherited
// file descriptor, then a 0600 temp file, then an environment variable.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// Provider is the trusted secret store. It is never exposed to the sandbox.
type Provider interface {
	Get(name string) (string, bool)
}

// MapProvider is an in-memory Provider (tests, and values loaded by the runner).
type MapProvider map[string]string

func (m MapProvider) Get(n string) (string, bool) { v, ok := m[n]; return v, ok }

// EnvProvider reads SANDBOX_SECRET_<NAME> from the RUNNER's environment. It
// never reads arbitrary variables, so developer credentials (SSH agent, cloud
// keys, model keys) are not reachable by naming them in policy.
type EnvProvider struct{ Lookup func(string) (string, bool) }

func (e EnvProvider) Get(name string) (string, bool) {
	lk := e.Lookup
	if lk == nil {
		lk = os.LookupEnv
	}
	return lk("SANDBOX_SECRET_" + strings.ToUpper(strings.NewReplacer("-", "_", ".", "_").Replace(name)))
}

// Options control delivery.
type Options struct {
	// Host maps a logical path to its host path (for file delivery).
	Host func(logical string) (string, error)
	// FileDir is a runner-owned 0700 directory for backing files whose logical
	// target must be materialized (file delivery writes to the host path of the
	// grant target; this is used for FD temp files).
	FileDir string
	// Supported lists delivery methods the backend can honour. Empty = file, env.
	Supported []sandbox.SecretDelivery
	// NoDowngrade turns a weaker-than-requested delivery into an error.
	NoDowngrade bool
}

// Delivery is what a run needs to pass to the sandbox.
type Delivery struct {
	Env        []string   // KEY=VALUE, appended to the complete sandbox env
	ExtraFiles []*os.File // inherited FDs; entry i is fd 3+i in the child
	Downgrades []string   // "name: fd->env" notes for events (no values)
	cleanup    []string
	values     map[string]string
}

// Cleanup removes materialized files and closes FDs.
func (d *Delivery) Cleanup() {
	for _, f := range d.ExtraFiles {
		f.Close()
	}
	for _, p := range d.cleanup {
		os.Remove(p)
	}
}

// Redact replaces any delivered secret value in s.
func (d *Delivery) Redact(s string) string { return redact(s, d.values) }

func redact(s string, vals map[string]string) string {
	names := make([]string, 0, len(vals))
	for n := range vals {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return len(vals[names[i]]) > len(vals[names[j]]) })
	for _, n := range names {
		if v := vals[n]; len(v) >= 6 {
			s = strings.ReplaceAll(s, v, "[REDACTED:"+n+"]")
		}
	}
	return s
}

var order = []sandbox.SecretDelivery{sandbox.DeliverFD, sandbox.DeliverFile, sandbox.DeliverEnv}

func rank(m sandbox.SecretDelivery) int {
	for i, x := range order {
		if x == m {
			return i
		}
	}
	return -1
}

// Deliver resolves every grant. Missing secrets fail closed.
func Deliver(grants []sandbox.SecretGrant, p Provider, o Options) (*Delivery, error) {
	sup := map[sandbox.SecretDelivery]bool{}
	supported := o.Supported
	if len(supported) == 0 {
		supported = []sandbox.SecretDelivery{sandbox.DeliverFile, sandbox.DeliverEnv}
	}
	for _, s := range supported {
		sup[s] = true
	}
	d := &Delivery{values: map[string]string{}}
	fail := func(err error) (*Delivery, error) { d.Cleanup(); return nil, err }
	for _, g := range grants {
		v, ok := p.Get(g.Name)
		if !ok {
			return fail(fmt.Errorf("secret %q is not available to the runner", g.Name))
		}
		d.values[g.Name] = v
		method := g.Delivery
		for rank(method) >= 0 && !sup[method] {
			if o.NoDowngrade {
				return fail(fmt.Errorf("secret %q: delivery %s unsupported and downgrade disabled", g.Name, method))
			}
			next := rank(method) + 1
			if next >= len(order) {
				return fail(fmt.Errorf("secret %q: no supported delivery", g.Name))
			}
			d.Downgrades = append(d.Downgrades, fmt.Sprintf("%s: %s->%s", g.Name, method, order[next]))
			method = order[next]
		}
		switch method {
		case sandbox.DeliverFD:
			f, err := fdFile(o.FileDir, v)
			if err != nil {
				return fail(err)
			}
			d.ExtraFiles = append(d.ExtraFiles, f)
			name := g.Target
			if name == "" {
				name = strings.ToUpper(g.Name) + "_FD"
			}
			d.Env = append(d.Env, fmt.Sprintf("%s=%d", name, 2+len(d.ExtraFiles)))
		case sandbox.DeliverFile:
			target := g.Target
			if target == "" || o.Host == nil {
				return fail(fmt.Errorf("secret %q: file delivery needs a target and a path mapper", g.Name))
			}
			hp, err := o.Host(target)
			if err != nil {
				return fail(err)
			}
			if err := os.MkdirAll(filepath.Dir(hp), 0o700); err != nil {
				return fail(err)
			}
			if err := os.WriteFile(hp, []byte(v), 0o600); err != nil {
				return fail(err)
			}
			d.cleanup = append(d.cleanup, hp)
		case sandbox.DeliverEnv:
			if g.Target == "" {
				return fail(fmt.Errorf("secret %q: env delivery needs a target", g.Name))
			}
			d.Env = append(d.Env, g.Target+"="+v)
		default:
			return fail(errors.New("unknown delivery"))
		}
	}
	return d, nil
}

// fdFile returns a read-only handle on an unlinked 0600 temp file holding v.
func fdFile(dir, v string) (*os.File, error) {
	w, err := os.CreateTemp(dir, ".secret-*")
	if err != nil {
		return nil, err
	}
	name := w.Name()
	defer os.Remove(name) // unlinked as soon as we hold the fd
	if err := os.Chmod(name, 0o600); err != nil {
		w.Close()
		return nil, err
	}
	if _, err := w.WriteString(v); err != nil {
		w.Close()
		return nil, err
	}
	w.Close()
	return os.Open(name)
}

// EnvNames returns only the variable names of an environment, for logging.
func EnvNames(env []string) []string {
	out := make([]string, 0, len(env))
	for _, e := range env {
		out = append(out, strings.SplitN(e, "=", 2)[0])
	}
	return out
}
