package clisetup

import (
	"fmt"
	"os"
	"os/exec"
	goruntime "runtime"
	"strings"
)

// The isobox build the sandbox needs, as pinned in
// .github/workflows/sandbox.yml (a test keeps them in step): the tendant
// fork, upstream plus the fix keeping /bin in gVisor's scoped-read views.
const (
	IsoboxRepo = "https://github.com/tendant/isobox"
	IsoboxRef  = "2145756b9672559615f1605d4c2cec8a19011cf7"
)

// IsoboxInstallHelp says how to install the pinned isobox: built as the
// user (Go lives there), installed with sudo where root finds it (gVisor
// runs agent-runner as root, and root's PATH has no ~/go/bin).
func IsoboxInstallHelp() string {
	return "install it: `git clone " + IsoboxRepo + " && cd isobox && git checkout " + IsoboxRef[:7] +
		" && go build -o isobox ./cmd/isobox && sudo install -m 0755 isobox /usr/local/bin/` " +
		"(or set AGENT_SANDBOX_ISOBOX to the binary's absolute path; AGENT_SANDBOX=off runs agents without the sandbox)"
}

// SandboxScopedReads parses AGENT_SANDBOX_READ: "scoped" limits reads to an
// allowlist, "host" leaves the host readable minus denied paths. Unset:
// scoped on Linux (where the sandbox is uid 0 and production runs), host
// elsewhere.
func SandboxScopedReads(v string) (bool, error) {
	switch v {
	case "scoped":
		return true, nil
	case "host":
		return false, nil
	case "":
		return goruntime.GOOS == "linux", nil
	}
	return false, fmt.Errorf("AGENT_SANDBOX_READ: bad value %q (want scoped|host)", v)
}

// SandboxGVisor reports whether runs use gVisor: asked for, or left to
// isobox on Linux (where it picks gVisor).
func SandboxGVisor(backend string) bool {
	return backend == "gvisor" || (backend == "" && goruntime.GOOS == "linux")
}

// lookPath and euid are vars so tests can fake a host.
var (
	lookPath = exec.LookPath
	euid     = os.Geteuid
)

// SandboxProblems lists what keeps the sandbox from running on this host,
// each with its fix; nil when the sandbox is off or nothing is missing.
func SandboxProblems(mode, isobox, backend string) []string {
	if mode == "" || mode == "off" {
		return nil
	}
	var out []string
	if isobox == "" {
		isobox = "isobox"
	}
	if _, err := lookPath(isobox); err != nil {
		out = append(out, fmt.Sprintf("isobox not found (%s): %s", isobox, IsoboxInstallHelp()))
	}
	if SandboxGVisor(backend) {
		if _, err := lookPath("runsc"); err != nil {
			out = append(out, "runsc (gVisor) not found: install release 20261005.0 (https://gvisor.dev/docs/user_guide/install/)")
		}
		var missing []string
		for _, tool := range []string{"ip", "sysctl", "iptables"} {
			if _, err := lookPath(tool); err != nil {
				missing = append(missing, tool)
			}
		}
		if len(missing) > 0 {
			out = append(out, "missing "+strings.Join(missing, ", ")+" (isobox builds the gVisor sandbox's network with them): "+
				"install iproute2, procps and iptables")
		}
		if euid() != 0 {
			out = append(out, "gVisor needs agent-runner to run as root")
		}
	}
	return out
}

// SandboxStatus is the /status line for the sandbox.
func SandboxStatus(mode, isobox, backend, read string) string {
	if mode == "" || mode == "off" {
		return "off"
	}
	b := backend
	if b == "" {
		b = "auto"
		if SandboxGVisor(backend) {
			b = "auto (gvisor)"
		}
	}
	reads := "host reads"
	if scoped, err := SandboxScopedReads(read); err != nil {
		reads = err.Error()
	} else if scoped {
		reads = "scoped reads"
	}
	line := fmt.Sprintf("%s, %s, %s", mode, b, reads)
	if p := SandboxProblems(mode, isobox, backend); len(p) > 0 {
		return line + " ✗ every run will be rejected:\n  - " + strings.Join(p, "\n  - ")
	}
	return line + " ✓"
}
