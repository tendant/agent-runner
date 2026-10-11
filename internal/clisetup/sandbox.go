package clisetup

import (
	"fmt"
	"os/exec"
	goruntime "runtime"
	"strings"
)

// AgentboxInstallHelp says how an admin sets a Linux host up for the
// sandbox: agentbox's installer puts runsc (gVisor) and the AppArmor
// profiles Ubuntu 23.10+ needs in place, with sudo; agent-runner itself
// never runs as root.
const AgentboxInstallHelp = "an admin installs it once: `git clone https://github.com/tendant/agentbox && cd agentbox && make install` " +
	"(runsc and, on Ubuntu 23.10+, its AppArmor profiles; `make check` verifies the host); AGENT_SANDBOX=off runs agents without the sandbox"

// SandboxScopedReads parses AGENT_SANDBOX_READ. Reads are always scoped: the
// sandbox sees the system's directories, its own roots and the configured
// paths (AGENT_SANDBOX_READ_ALLOW), nothing else of the host.
func SandboxScopedReads(v string) (bool, error) {
	switch v {
	case "", "scoped":
		return true, nil
	case "host":
		return false, fmt.Errorf("AGENT_SANDBOX_READ=host is no longer supported: the sandbox reads only what it is given (add paths with AGENT_SANDBOX_READ_ALLOW)")
	}
	return false, fmt.Errorf("AGENT_SANDBOX_READ: bad value %q (want scoped)", v)
}

// SandboxBackendName is the agentbox backend this OS uses.
func SandboxBackendName() string {
	switch goruntime.GOOS {
	case "linux":
		return "gvisor"
	case "darwin":
		return "seatbelt"
	}
	return "none"
}

// lookPath and goos are vars so tests can fake a host.
var (
	lookPath = exec.LookPath
	goos     = goruntime.GOOS
)

// SandboxProblems lists what keeps the sandbox from running on this host,
// each with its fix; nil when the sandbox is off or nothing is missing.
func SandboxProblems(mode, backend string) []string {
	if mode == "" || mode == "off" {
		return nil
	}
	var out []string
	if backend != "" && backend != "agentbox" {
		out = append(out, fmt.Sprintf("AGENT_SANDBOX_BACKEND=%s is no longer supported: isobox was removed, agentbox is the sandbox (unset AGENT_SANDBOX_BACKEND)", backend))
	}
	switch goos {
	case "linux":
		if _, err := lookPath("/usr/local/bin/runsc"); err != nil {
			if _, err := lookPath("runsc"); err != nil {
				out = append(out, "runsc (gVisor) not found: "+AgentboxInstallHelp)
			}
		}
	case "darwin":
		if _, err := lookPath("/usr/bin/sandbox-exec"); err != nil {
			out = append(out, "/usr/bin/sandbox-exec not found: the macOS sandbox (Seatbelt) needs it")
		}
	default:
		out = append(out, "no sandbox on "+goos+": agentbox runs on Linux (gVisor) and macOS (Seatbelt)")
	}
	return out
}

// SandboxStatus is the /status line for the sandbox.
func SandboxStatus(mode, backend, read string) string {
	if mode == "" || mode == "off" {
		return "off"
	}
	line := fmt.Sprintf("%s, agentbox (%s)", mode, SandboxBackendName())
	p := SandboxProblems(mode, backend)
	if _, err := SandboxScopedReads(read); err != nil {
		p = append(p, err.Error())
	}
	if len(p) > 0 {
		return line + " ✗ every run will be rejected:\n  - " + strings.Join(p, "\n  - ")
	}
	return line + " ✓"
}
