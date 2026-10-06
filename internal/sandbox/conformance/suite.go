// Package conformance is the shared backend test suite. A backend may claim a
// capability only if every test the registry lists for it passes (Report.Proven).
// Tests that are not implemented, skipped, or failing all count as unproven.
package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
)

// Env is the host fixture a backend is constructed against. Roots maps the
// logical roots to real host directories. Outside is writable by the test
// process but granted to nobody.
type Env struct {
	Roots   map[string]string
	Outside string
	Secret  string // marker written to Home/.ssh/id_rsa
}

// Host maps a logical path to its host path.
func (e Env) Host(logical string) string {
	best, hp := "", ""
	for root, h := range e.Roots {
		if (logical == root || strings.HasPrefix(logical, root+"/")) && len(root) > len(best) {
			best, hp = root, h
		}
	}
	if best == "" {
		return logical
	}
	return filepath.Join(hp, strings.TrimPrefix(logical, best))
}

// Factory builds the backend under test for a fixture.
type Factory func(Env) sandbox.Backend

// Status of one test.
type Status string

const (
	Pass   Status = "pass"
	Fail   Status = "fail"
	Skip   Status = "skip"   // not applicable on this host (missing tool)
	NotRun Status = "notrun" // suite does not implement it yet
)

// Result is one test outcome.
type Result struct {
	Status Status `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// Report holds results keyed by test ID (the IDs in sandbox.Registry).
type Report struct {
	Backend string            `json:"backend"`
	Host    string            `json:"host"`
	Results map[string]Result `json:"results"`
}

// Proven reports whether every registry test for c passed.
func (r Report) Proven(c sandbox.CapabilityID) bool {
	d, ok := sandbox.Registry[c]
	if !ok || len(d.Tests) == 0 {
		return false
	}
	for _, id := range d.Tests {
		if r.Results[id].Status != Pass {
			return false
		}
	}
	return true
}

// ProvenSet lists all capabilities the report proves.
func (r Report) ProvenSet() []sandbox.CapabilityID {
	var out []sandbox.CapabilityID
	for _, c := range sandbox.CapabilityIDs() {
		if r.Proven(c) {
			out = append(out, c)
		}
	}
	return out
}

// ErrSkip marks a test as not applicable on this host.
var ErrSkip = errors.New("skip")

type x struct {
	b   sandbox.Backend
	env Env
	ctx context.Context
}

// procEnv is the complete environment given to probes.
func (t *x) procEnv() []string {
	return []string{
		"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		"HOME=" + t.env.Host(sandbox.RootHome),
		"TMPDIR=" + t.env.Host(sandbox.RootTmp),
	}
}

func (t *x) baseSpec() sandbox.SandboxSpec {
	return sandbox.SandboxSpec{
		Version: sandbox.SpecVersion,
		Filesystem: sandbox.FilesystemPolicy{
			Grants: []sandbox.PathGrant{
				{Path: sandbox.RootWorkspace, Access: sandbox.ReadWrite},
				{Path: sandbox.RootTmp, Access: sandbox.ReadWrite},
				{Path: sandbox.RootHome, Access: sandbox.ReadOnly},
			},
			Deny: []string{sandbox.RootHome + "/.ssh"},
		},
		Network: sandbox.NetworkPolicy{Egress: sandbox.EgressNone},
	}
}

// exec runs one probe in a fresh sandbox and returns combined output.
func (t *x) exec(spec sandbox.SandboxSpec, script string) (string, error) {
	return t.execEnv(spec, script, nil)
}

func (t *x) execEnv(spec sandbox.SandboxSpec, script string, extra []string) (string, error) {
	var out bytes.Buffer
	ps, err := t.b.Prepare(t.ctx, spec)
	if err != nil {
		return "", fmt.Errorf("prepare: %w", err)
	}
	defer ps.Destroy(context.Background())
	_, err = ps.Run(t.ctx, sandbox.ProcSpec{
		Args: []string{"sh", "-c", script}, Dir: sandbox.RootWorkspace,
		Env: append(t.procEnv(), extra...), Stdout: &out, Stderr: &out,
	})
	return out.String(), err
}

func need(tools ...string) error {
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			return fmt.Errorf("%w: %s not installed", ErrSkip, tool)
		}
	}
	return nil
}

type testCase struct {
	id  string
	run func(*x) error
}

// listener accepts and counts TCP connections on 127.0.0.1.
func listener() (port int, accepted *atomic.Int64, stop func(), err error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, nil, err
	}
	accepted = new(atomic.Int64)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			c.Close()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port, accepted, func() { l.Close() }, nil
}

func tcpProbe(host string, port int) string {
	return fmt.Sprintf(`bash -c 'exec 3<>/dev/tcp/%s/%d' 2>&1; echo rc=$?`, host, port)
}

func cases() []testCase {
	return []testCase{
		{"fs.ssh_unreadable", func(t *x) error {
			out, _ := t.exec(t.baseSpec(), "cat "+t.env.Host(sandbox.RootHome+"/.ssh/id_rsa")+" 2>&1")
			if strings.Contains(out, t.env.Secret) {
				return errors.New("read SSH credential")
			}
			return nil
		}},
		{"fs.symlink_escape", func(t *x) error {
			ws := t.env.Host(sandbox.RootWorkspace)
			out, _ := t.exec(t.baseSpec(), "ln -s "+t.env.Host(sandbox.RootHome+"/.ssh/id_rsa")+" "+ws+"/link 2>&1; cat "+ws+"/link 2>&1")
			if strings.Contains(out, t.env.Secret) {
				return errors.New("symlink read denied file")
			}
			return nil
		}},
		{"fs.no_write_outside_workspace", func(t *x) error {
			ws, outside := t.env.Host(sandbox.RootWorkspace), t.env.Outside
			t.exec(t.baseSpec(), fmt.Sprintf("echo ok > %s/pos 2>&1; echo x > %s/neg 2>&1", ws, outside))
			if _, err := os.Stat(filepath.Join(ws, "pos")); err != nil {
				return errors.New("positive control failed: cannot write workspace")
			}
			if _, err := os.Stat(filepath.Join(outside, "neg")); err == nil {
				return errors.New("wrote outside granted paths")
			}
			return nil
		}},
		{"fs.virtual_paths", func(t *x) error {
			out, _ := t.exec(t.baseSpec(), "test -d /workspace && echo VIRT_OK")
			if !strings.Contains(out, "VIRT_OK") {
				return errors.New("/workspace not visible inside sandbox")
			}
			return nil
		}},
		{"env.no_credentials", func(t *x) error {
			os.Setenv("ANTHROPIC_API_KEY", t.env.Secret)
			os.Setenv("AWS_SECRET_ACCESS_KEY", t.env.Secret)
			defer os.Unsetenv("ANTHROPIC_API_KEY")
			defer os.Unsetenv("AWS_SECRET_ACCESS_KEY")
			out, _ := t.exec(t.baseSpec(), "env")
			if strings.Contains(out, t.env.Secret) {
				return errors.New("host credentials inherited")
			}
			return nil
		}},
		{"proc.daemon_killed", func(t *x) error {
			if err := need("python3", "pgrep"); err != nil {
				return err
			}
			marker := fmt.Sprintf("sleep 4%06d", time.Now().UnixNano()%1000000)
			defer exec.Command("pkill", "-f", marker).Run()
			py := fmt.Sprintf(`import os
if os.fork()==0:
    os.setsid()
    os.execvp('sleep',['sleep','%s'])`, strings.TrimPrefix(marker, "sleep "))
			t.exec(t.baseSpec(), "python3 -c \""+strings.ReplaceAll(py, `"`, `\"`)+"\" >/dev/null 2>&1 </dev/null; sleep 0.3")
			for i := 0; i < 20; i++ {
				if exec.Command("pgrep", "-f", marker).Run() != nil {
					return nil
				}
				time.Sleep(150 * time.Millisecond)
			}
			return errors.New("daemonized child survived teardown")
		}},
		{"proc.cross_run_signal", func(t *x) error {
			if err := need("pkill"); err != nil {
				return err
			}
			n := fmt.Sprintf("4.%06d", time.Now().UnixNano()%1000000)
			done := make(chan string, 1)
			go func() { o, _ := t.exec(t.baseSpec(), "sleep "+n+"; echo done_A"); done <- o }()
			time.Sleep(1500 * time.Millisecond)
			t.exec(t.baseSpec(), "pkill -f 'sleep "+n+"' 2>&1; true")
			select {
			case o := <-done:
				if !strings.Contains(o, "done_A") {
					return errors.New("run B signalled run A")
				}
				return nil
			case <-time.After(20 * time.Second):
				return errors.New("run A never finished")
			}
		}},
		{"net.none_loopback", func(t *x) error {
			if err := need("bash"); err != nil {
				return err
			}
			port, n, stop, err := listener()
			if err != nil {
				return err
			}
			defer stop()
			t.exec(t.baseSpec(), tcpProbe("127.0.0.1", port))
			if n.Load() > 0 {
				return errors.New("reached host loopback service")
			}
			return nil
		}},
		{"net.none_raw_ip", func(t *x) error { return denyDial(t, "1.1.1.1", 443) }},
		{"net.none_metadata", func(t *x) error { return denyDial(t, "169.254.169.254", 80) }},
		{"net.outbound_connect", func(t *x) error {
			if err := need("bash"); err != nil {
				return err
			}
			port, n, stop, err := listener()
			if err != nil {
				return err
			}
			defer stop()
			s := t.baseSpec()
			s.Network.Egress = sandbox.EgressOutbound
			t.exec(s, tcpProbe("127.0.0.1", port))
			if n.Load() == 0 {
				return errors.New("outbound connect failed")
			}
			return nil
		}},
		{"net.no_listen", func(t *x) error {
			if err := need("python3"); err != nil {
				return err
			}
			s := t.baseSpec()
			s.Network.Egress = sandbox.EgressOutbound
			out, _ := t.exec(s, `python3 -c "import socket;s=socket.socket();s.bind(('127.0.0.1',0));s.listen(1);print('LISTEN_OK')" 2>&1`)
			if strings.Contains(out, "LISTEN_OK") {
				return errors.New("listener created")
			}
			return nil
		}},
		{"res.timeout", func(t *x) error {
			s := t.baseSpec()
			s.Resources.TimeoutSec, s.Resources.GraceSec = 1, 1
			start := time.Now()
			t.exec(s, "sleep 30; echo FINISHED")
			if time.Since(start) > 15*time.Second {
				return fmt.Errorf("took %v", time.Since(start))
			}
			return nil
		}},
		{"res.memory", func(t *x) error {
			if err := need("python3"); err != nil {
				return err
			}
			s := t.baseSpec()
			s.Resources.MemoryBytes = 128 << 20
			out, _ := t.exec(s, `python3 -c "b=bytearray(700*1024*1024);b[::4096]=b'x'*len(b[::4096]);print('ALLOC_OK')" 2>&1`)
			if strings.Contains(out, "ALLOC_OK") {
				return errors.New("allocated 700MiB under 128MiB cap")
			}
			return nil
		}},
		{"res.pids", func(t *x) error {
			if err := need("python3"); err != nil {
				return err
			}
			s := t.baseSpec()
			s.Resources.PIDs = 16
			py := "import os,time\ntry:\n for i in range(100):\n  if os.fork()==0:\n   time.sleep(3);os._exit(0)\n print('UNLIMITED')\nexcept OSError:\n print('LIMITED')"
			out, _ := t.exec(s, "python3 -c \""+strings.ReplaceAll(py, `"`, `\"`)+"\" 2>&1")
			if !strings.Contains(out, "LIMITED") || strings.Contains(out, "UNLIMITED") {
				return errors.New("process cap not enforced")
			}
			return nil
		}},
	}
}

func denyDial(t *x, host string, port int) error {
	if err := need("bash"); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(t.ctx, 5*time.Second)
	defer cancel()
	tt := *t
	tt.ctx = ctx
	out, err := tt.exec(t.baseSpec(), tcpProbe(host, port))
	if err == nil && strings.Contains(out, "rc=0") {
		return fmt.Errorf("connected to %s:%d", host, port)
	}
	return nil // refused, unreachable or hung: not connected
}

// pending are registry tests whose cases need later steps (gateway, supervisor,
// launcher/proxy). They are reported NotRun so their capabilities stay unproven.
var pending = map[string]string{
	"net.restricted_allow": "needs egress gateway (step 8)", "net.restricted_deny": "needs egress gateway (step 8)",
	"net.controlled_dns": "needs controlled DNS (step 8)", "res.cpu": "not implemented", "res.disk": "needs disk quota (step 8)",
	"res.output": "needs launcher output cap (step 7)", "res.deadline_runner_crash": "needs supervisor (step 10)",
	"rt.orphan_recovery": "needs supervisor (step 10)", "kernel.isolation": "not implemented",
	// Non-registry safety tests required by the design.
	"policy.immutable_from_tools": "needs request channel (step 7)", "ctl.endpoint_unreachable": "needs request channel (step 7)",
	"model.byte_rate_limits": "needs model proxy (step 7)",
}

// Run executes the suite against the backend built by f.
func Run(ctx context.Context, name string, f Factory) (Report, error) {
	base, err := os.MkdirTemp("", "sbx-conf-")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(base)
	env := Env{Roots: map[string]string{}, Outside: filepath.Join(base, "outside"), Secret: fmt.Sprintf("CONF-SECRET-%d", time.Now().UnixNano())}
	for _, r := range [][2]string{{sandbox.RootWorkspace, "ws"}, {sandbox.RootHome, "home"}, {sandbox.RootTmp, "tmp"}} {
		p, _ := filepath.EvalSymlinks(base) // macOS /var -> /private/var
		env.Roots[r[0]] = filepath.Join(p, r[1])
		if err := os.MkdirAll(env.Roots[r[0]], 0o755); err != nil {
			return Report{}, err
		}
	}
	env.Outside = filepath.Join(filepath.Dir(env.Roots[sandbox.RootWorkspace]), "outside")
	if err := os.MkdirAll(env.Outside, 0o755); err != nil {
		return Report{}, err
	}
	ssh := filepath.Join(env.Roots[sandbox.RootHome], ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		return Report{}, err
	}
	if err := os.WriteFile(filepath.Join(ssh, "id_rsa"), []byte(env.Secret+"\n"), 0o600); err != nil {
		return Report{}, err
	}

	rep := Report{Backend: name, Host: hostName(), Results: map[string]Result{}}
	for id, why := range pending {
		rep.Results[id] = Result{NotRun, why}
	}
	for _, c := range cases() {
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		t := &x{b: f(env), env: env, ctx: cctx}
		err := c.run(t)
		cancel()
		switch {
		case err == nil:
			rep.Results[c.id] = Result{Status: Pass}
		case errors.Is(err, ErrSkip):
			rep.Results[c.id] = Result{Skip, err.Error()}
		default:
			rep.Results[c.id] = Result{Fail, err.Error()}
		}
	}
	return rep, nil
}

func hostName() string {
	out, _ := exec.Command("uname", "-sm").Output()
	return strings.TrimSpace(string(out))
}

// JSON renders the report as evidence.
func (r Report) JSON() []byte {
	b, _ := json.MarshalIndent(r, "", "  ")
	return b
}
