// Package gitsafe builds git commands the runner executes on the HOST inside
// agent-writable directories. A sandboxed agent can edit .git/config and hooks;
// without these overrides the runner's own `git status`/`commit` would run
// agent-chosen programs outside the sandbox (core.fsmonitor, hooks, ssh/pager/
// external-diff commands). Command-line -c settings take precedence over repo
// config, so they neutralize those vectors.
package gitsafe

import (
	"context"
	"os/exec"
)

// hardening are -c overrides applied to every runner-side git invocation.
var hardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "core.sshCommand=ssh",
	"-c", "core.pager=cat",
	"-c", "core.editor=true",
	"-c", "core.askPass=",
	"-c", "diff.external=",
	"-c", "protocol.ext.allow=never",
	"-c", "protocol.file.allow=user",
	"-c", "gc.auto=0",
	"-c", "maintenance.auto=false",
	"-c", "receive.denyCurrentBranch=refuse",
}

// Args returns args prefixed with the hardening overrides.
func Args(args ...string) []string {
	out := make([]string, 0, len(hardening)+len(args))
	out = append(out, hardening...)
	return append(out, args...)
}

// Command is exec.Command("git", ...) with hardening applied.
func Command(args ...string) *exec.Cmd { return exec.Command("git", Args(args...)...) }

// CommandContext is exec.CommandContext(ctx, "git", ...) with hardening applied.
func CommandContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "git", Args(args...)...)
}
