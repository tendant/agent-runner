package runtime

import (
	"context"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

// Service ports (AGENT_SANDBOX_SERVICES, NetworkPolicy.Services): each
// service the run's policy grants gets a listener of its own, reached the
// way the model channels are (no network needed): on Linux a Unix socket
// mounted into the sandbox, served at 127.0.0.1:<port> inside it by the
// forwarder; on macOS a port on the host's loopback the sandbox may connect
// to. Each connection to it is relayed to the service's one upstream, so
// granting a service opens that host:port and nothing else, and is on the
// audit record. The agent finds it at AGENT_SERVICE_<NAME>_ADDR.

// serviceDialTimeout bounds connecting to an upstream.
const serviceDialTimeout = 10 * time.Second

// serviceRun is a run's service listeners.
type serviceRun struct {
	sockDir   string            // Linux: the services' Unix sockets
	inSandbox map[string]string // name -> address the agent connects to
	sockets   map[string]string // Linux: name -> socket path on the host
	ports     []int             // macOS: the listeners' loopback ports
	services  []sandbox.Service
	lns       []net.Listener
	wg        sync.WaitGroup
	mu        sync.Mutex
	conns     map[net.Conn]bool
}

// startServices listens for each service the spec grants. reserved are
// in-sandbox addresses already taken (the model channels).
func (r *Runtime) startServices(spec sandbox.SandboxSpec, runID, threadID string, reserved map[string]bool) (*serviceRun, error) {
	if len(spec.Network.Services) == 0 {
		return nil, nil
	}
	s := &serviceRun{inSandbox: map[string]string{}, services: spec.Network.Services, conns: map[net.Conn]bool{}}
	if modelSockets {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		d, err := os.MkdirTemp(base, "ars-") // short: socket paths are limited
		if err != nil {
			return nil, err
		}
		s.sockDir, s.sockets = d, map[string]string{}
	}
	taken := map[string]bool{"127.0.0.1:3128": true} // the egress proxy's forwarder address
	for a := range reserved {
		taken[a] = true
	}
	for i, sv := range spec.Network.Services {
		var ln net.Listener
		var err error
		if s.sockDir != "" {
			sock := filepath.Join(s.sockDir, "svc-"+sv.Name+".sock")
			if ln, err = net.Listen("unix", sock); err == nil {
				s.sockets[sv.Name] = sock
				s.inSandbox[sv.Name] = sandboxServiceAddr(sv, i, taken)
			}
		} else if ln, err = net.Listen("tcp", "127.0.0.1:0"); err == nil {
			s.inSandbox[sv.Name] = ln.Addr().String()
			s.ports = append(s.ports, ln.Addr().(*net.TCPAddr).Port)
		}
		if err != nil {
			s.close()
			return nil, err
		}
		s.lns = append(s.lns, ln)
		sv := sv
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			first := true // a service's first connection is logged
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				s.track(c, true)
				go s.relay(r, c, sv, runID, threadID, first)
				first = false
			}
		}()
	}
	return s, nil
}

// sandboxServiceAddr is the address inside a gVisor sandbox (its own
// loopback) the forwarder serves service sv at: its upstream's port when
// free, so the agent's tools find a database at its usual port, else one
// from 15000.
func sandboxServiceAddr(sv sandbox.Service, i int, taken map[string]bool) string {
	_, port, _ := net.SplitHostPort(sv.Upstream)
	a := "127.0.0.1:" + port
	for n := 15000 + i; taken[a]; n++ {
		a = "127.0.0.1:" + strconv.Itoa(n)
	}
	taken[a] = true
	return a
}

func (s *serviceRun) track(c net.Conn, add bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if add {
		s.conns[c] = true
	} else {
		delete(s.conns, c)
	}
}

// relay connects c to sv's upstream and copies both ways until either side
// closes. Each connection is recorded; a failure to reach the upstream is
// logged, as it is not a policy decision.
func (s *serviceRun) relay(r *Runtime, c net.Conn, sv sandbox.Service, runID, threadID string, first bool) {
	defer s.track(c, false)
	defer c.Close()
	host, port, _ := net.SplitHostPort(sv.Upstream)
	p, _ := strconv.Atoi(port)
	r.audit(runID, audit.TypeNetworkAllowed, audit.NetworkDetail{Destination: host, Port: p, Protocol: "tcp", Service: sv.Name})
	if first {
		r.emit(Event{Kind: "sandbox.service_connected", RunID: runID, ThreadID: threadID, Detail: sv.Name + " " + sv.Upstream})
	}
	ctx, cancel := context.WithTimeout(context.Background(), serviceDialTimeout)
	u, err := (&net.Dialer{}).DialContext(ctx, "tcp", sv.Upstream)
	cancel()
	if err != nil {
		slog.Warn("sandbox service: upstream unreachable", "run", runID, "service", sv.Name, "upstream", sv.Upstream, "error", err)
		return
	}
	s.track(u, true)
	defer s.track(u, false)
	defer u.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(u, c); halfClose(u); done <- struct{}{} }()
	go func() { io.Copy(c, u); halfClose(c); done <- struct{}{} }()
	<-done
	<-done
}

// halfClose ends the write side of c, when it has one, so the far end sees
// EOF while the other direction finishes.
func halfClose(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

// routes maps the forwarder's in-sandbox addresses to the services' sockets
// (Linux); nil otherwise.
func (s *serviceRun) routes() map[string]string {
	if s == nil || s.sockDir == "" {
		return nil
	}
	out := map[string]string{}
	for name, a := range s.inSandbox {
		out[a] = s.sockets[name]
	}
	return out
}

// hostPorts are the services' loopback ports (macOS); nil otherwise.
func (s *serviceRun) hostPorts() []int {
	if s == nil {
		return nil
	}
	return s.ports
}

// env is where the agent finds each service.
func (s *serviceRun) env() []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, sv := range s.services {
		out = append(out, ServiceEnv(sv.Name, s.inSandbox[sv.Name])...)
	}
	return out
}

// close stops the listeners and ends the run's connections.
func (s *serviceRun) close() {
	if s == nil {
		return
	}
	for _, ln := range s.lns {
		ln.Close()
	}
	s.mu.Lock()
	for c := range s.conns {
		c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	if s.sockDir != "" {
		os.RemoveAll(s.sockDir)
	}
}

// ServiceEnv is service name's variables for the agent, at addr (host:port):
// AGENT_SERVICE_<NAME>_ADDR, _HOST and _PORT. Empty values when addr is ""
// (a service the run's policy does not grant).
func ServiceEnv(name, addr string) []string {
	k := "AGENT_SERVICE_" + strings.ToUpper(name)
	host, port, _ := net.SplitHostPort(addr)
	return []string{k + "_ADDR=" + addr, k + "_HOST=" + host, k + "_PORT=" + port}
}
