package agentbox

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The forwarder relays a TCP connection to a Unix socket while it runs its
// command, and exits with the command's status.
func TestForwarder(t *testing.T) {
	dir, err := os.MkdirTemp("", "fw")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	sock := dir + "/m.sock"
	ul, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ul.Close()
	go func() {
		for {
			c, err := ul.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			fmt.Fprintf(c, "ECHO %s", line)
			c.Close()
		}
	}()
	// A free port.
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := l.Addr().String()
	l.Close()

	self, _ := os.Executable()
	script := fmt.Sprintf(`exec 3<>/dev/tcp/%s/%s; echo hi >&3; cat <&3; exit 7`, strings.Split(addr, ":")[0], strings.Split(addr, ":")[1])
	cmd := exec.Command(self, fwdArgs(map[string]string{addr: sock}, []string{"/bin/bash", "-c", script})...)
	out, err := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 7 {
		t.Errorf("exit %d (%v), want the command's 7: %s", cmd.ProcessState.ExitCode(), err, out)
	}
	if !strings.Contains(string(out), "ECHO hi") {
		t.Errorf("relayed: %q", out)
	}
}

func TestForwarderRejectsBadArgs(t *testing.T) {
	for _, args := range [][]string{nil, {"a=b"}, {"a=b", "x", "cmd"}, {"nosep", "--", "true"}} {
		if _, err := runForwarder(args); err == nil {
			t.Errorf("%v: want an error", args)
		}
	}
}
