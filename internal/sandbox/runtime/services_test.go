package runtime

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/agent-runner/agent-runner/internal/executor"
	"github.com/agent-runner/agent-runner/internal/sandbox"
	"github.com/tendant/agentbox/pkg/agentbox/audit"
)

// echoUpstream is a TCP service that answers each line with "echo: line".
func echoUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc := bufio.NewScanner(c)
				for sc.Scan() {
					c.Write([]byte("echo: " + sc.Text() + "\n"))
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// dialService connects as the agent would: through the forwarder's socket
// (Linux) or straight to the loopback port (macOS).
func dialService(t *testing.T, run *Run, name string) net.Conn {
	t.Helper()
	s := run.services
	var c net.Conn
	var err error
	if sock, ok := s.sockets[name]; ok {
		c, err = net.Dial("unix", sock)
	} else {
		c, err = net.Dial("tcp", s.inSandbox[name])
	}
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// A granted service is reached through its own listener, which relays to its
// one upstream; the agent finds it in its environment; each connection is
// on the record, with the service's name; a configured service the run's
// policy does not grant is blank in its environment.
func TestServicesRelayAndRecord(t *testing.T) {
	bothPaths(t, func(t *testing.T) {
		up := echoUpstream(t)
		svcs := []sandbox.Service{{Name: "pg", Upstream: up}, {Name: "cache", Upstream: "127.0.0.1:1"}}
		rt, read, _ := auditRT(t, sandbox.ModePermissive, nil, Config{Services: svcs})
		// The run keeps only pg.
		layer := sandbox.Layer{Name: sandbox.LayerRun, Spec: sandbox.SandboxSpec{Version: sandbox.SpecVersion,
			Network: sandbox.NetworkPolicy{Services: []sandbox.Service{svcs[0]}}}}
		run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", Workspace: t.TempDir(), Layers: []sandbox.Layer{layer}})
		if err != nil {
			t.Fatal(err)
		}
		defer run.Finish()
		if got := run.Spec.Network.Services; len(got) != 1 || got[0].Name != "pg" {
			t.Fatalf("granted services %v", got)
		}

		c := dialService(t, run, "pg")
		c.SetDeadline(time.Now().Add(5 * time.Second))
		c.Write([]byte("select 1\n"))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || line != "echo: select 1\n" {
			t.Fatalf("relayed %q %v", line, err)
		}
		c.Close()

		// The agent's environment.
		l, _ := executor.LauncherFrom(run.Ctx)
		env := strings.Join(l.(*executor.SandboxLauncher).EnvForce, "\n")
		addr := run.services.inSandbox["pg"]
		host, port, _ := net.SplitHostPort(addr)
		for _, want := range []string{"AGENT_SERVICE_PG_ADDR=" + addr, "AGENT_SERVICE_PG_HOST=" + host, "AGENT_SERVICE_PG_PORT=" + port, "AGENT_SERVICE_CACHE_ADDR=\n"} {
			if !strings.Contains(env+"\n", want) {
				t.Errorf("env lacks %q:\n%s", want, env)
			}
		}
		if _, sockets := run.services.sockets["pg"]; sockets {
			if run.services.routes()[addr] == "" || len(run.services.hostPorts()) != 0 {
				t.Errorf("Linux: routes %v ports %v", run.services.routes(), run.services.hostPorts())
			}
		} else if len(run.services.hostPorts()) != 1 || run.services.routes() != nil {
			t.Errorf("macOS: routes %v ports %v", run.services.routes(), run.services.hostPorts())
		}

		// The record: the policy names the service, and the connection is on it.
		uhost, uport, _ := net.SplitHostPort(up)
		var found bool
		for _, e := range read() {
			if e.Type == audit.TypeNetworkAllowed {
				var d audit.NetworkDetail
				json.Unmarshal(e.Detail, &d)
				found = d.Service == "pg" && d.Destination == uhost && uport == itoa(d.Port) && d.Protocol == "tcp"
			}
			if e.Type == audit.TypePolicySnapshot && !strings.Contains(string(e.Detail), `"name":"pg"`) {
				t.Errorf("snapshot without the service: %s", e.Detail)
			}
		}
		if !found {
			t.Error("the connection is not on the record")
		}
	})
}

// Finishing the run closes its listeners and cuts open connections.
func TestServicesEndWithTheRun(t *testing.T) {
	bothPaths(t, func(t *testing.T) {
		up := echoUpstream(t)
		rt, _, _ := auditRT(t, sandbox.ModePermissive, nil, Config{Services: []sandbox.Service{{Name: "pg", Upstream: up}}})
		run, err := rt.Begin(context.Background(), BeginReq{RunID: "r1", Workspace: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		c := dialService(t, run, "pg")
		c.Write([]byte("hi\n"))
		bufio.NewReader(c).ReadString('\n')
		done := make(chan struct{})
		go func() { run.Finish(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Finish hung on an open service connection")
		}
		c.SetDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Error("the connection outlived the run")
		}
		if sock, ok := run.services.sockets["pg"]; ok {
			if _, err := net.Dial("unix", sock); err == nil {
				t.Error("the service socket outlived the run")
			}
		} else if _, err := net.Dial("tcp", run.services.inSandbox["pg"]); err == nil {
			t.Error("the service port outlived the run")
		}
	})
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
