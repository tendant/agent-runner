package agentbox

import (
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
)

// fwdMode is the argv[1] that turns agent-runner's binary into the model
// channel's forwarder inside an agentbox sandbox:
//
//	agent-runner __agent-runner-fwd 127.0.0.1:4801=/run/agentbox/a.sock[,...] -- CMD [ARGS...]
//
// The sandbox has no network, so the per-run model proxies listen on Unix
// sockets mounted into it; the agent CLIs expect http://127.0.0.1:<port>.
// The forwarder listens there, relays each connection to its socket, and
// runs CMD as its child: it lives exactly as long as the CLI.
const fwdMode = "__agent-runner-fwd"

func init() {
	if len(os.Args) > 1 && os.Args[1] == fwdMode {
		code, err := runForwarder(os.Args[2:])
		if err != nil {
			fmt.Fprintln(os.Stderr, "agent-runner forwarder:", err)
			os.Exit(127)
		}
		os.Exit(code)
	}
}

// fwdArgs builds the forwarder's argv after the binary: routes map an
// in-sandbox address to a socket path in the sandbox.
func fwdArgs(routes map[string]string, cmd []string) []string {
	var r []string
	for addr, sock := range routes {
		r = append(r, addr+"="+sock)
	}
	sortStrings(r)
	return append([]string{fwdMode, strings.Join(r, ","), "--"}, cmd...)
}

func runForwarder(args []string) (int, error) {
	if len(args) < 3 || args[1] != "--" {
		return 0, errors.New("usage: ADDR=SOCKET[,...] -- CMD [ARGS...]")
	}
	for _, route := range strings.Split(args[0], ",") {
		addr, sock, ok := strings.Cut(route, "=")
		if !ok {
			return 0, fmt.Errorf("bad route %q", route)
		}
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return 0, err
		}
		go serve(ln, sock)
	}
	c := exec.Command(args[2], args[3:]...)
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		return 127, err
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() {
		for s := range sigs {
			c.Process.Signal(s)
		}
	}()
	err := c.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), nil
		}
		return ee.ExitCode(), nil
	}
	return 127, err
}

func serve(ln net.Listener, sock string) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			u, err := net.Dial("unix", sock)
			if err != nil {
				return
			}
			defer u.Close()
			done := make(chan struct{}, 2)
			go func() { io.Copy(u, c); done <- struct{}{} }()
			go func() { io.Copy(c, u); done <- struct{}{} }()
			<-done
		}()
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
