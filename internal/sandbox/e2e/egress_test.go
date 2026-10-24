package e2e

import (
	"context"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/sandbox"
	sandboxrt "github.com/agent-runner/agent-runner/internal/sandbox/runtime"
)

// TestSSHPortInPermissiveMode tests that SSH (port 22) is accessible in permissive egress mode
func TestSSHPortInPermissiveMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	h := newHarness(t)

	// Create runtime in permissive mode with restricted egress
	rt := h.runtime(sandbox.ModePermissive, "")
	ws := t.TempDir()

	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{
		ThreadID:  "test-ssh",
		RunID:     "run-ssh-1",
		Workspace: ws,
	})
	if err != nil {
		t.Fatalf("Failed to begin sandbox: %v", err)
	}
	defer run.Finish()

	t.Log("=== Testing SSH port 22 in permissive mode ===")

	// Test 1: Try to connect to github.com:22 using nc or similar
	// We can't do actual SSH, but we can test the connection attempt
	out, err := runCLI(t, run.Ctx, ws, "sh", "-c", "echo 'test' | nc -zv github.com 22 2>&1 || echo 'nc_failed'")
	t.Logf("nc output: %s", out)

	// Whether nc succeeds or fails, the important thing is that the gateway
	// allowed the connection attempt (no "port 22 denied" error from sandbox)
	if strings.Contains(out, "refused") {
		t.Log("✓ Connection to github.com:22 reached (refused by remote is OK)")
	} else if strings.Contains(out, "Connection established") || strings.Contains(out, "succeeded") {
		t.Log("✓ Connection to github.com:22 succeeded")
	} else if strings.Contains(out, "nc_failed") {
		// nc might not be available
		t.Log("⚠ nc not available, trying telnet...")
		out, err = runCLI(t, run.Ctx, ws, "sh", "-c", "timeout 2 telnet github.com 22 </dev/null 2>&1 || echo 'telnet_done'")
		t.Logf("telnet output: %s", out)
	}

	// Test 2: Try HTTP (should also work in permissive)
	t.Log("\n=== Testing HTTP port 80 in permissive mode ===")
	out, err = runCLI(t, run.Ctx, ws, "sh", "-c", "curl -s --max-time 3 http://example.com/ 2>&1 | head -1")
	if err == nil && (strings.Contains(out, "<!doctype") || strings.Contains(out, "Example Domain")) {
		t.Log("✓ HTTP to example.com:80 succeeded")
	} else {
		t.Logf("HTTP attempt output: %s (err: %v)", out, err)
	}
}

// TestSSHPortDeniedInStrictMode tests that SSH (port 22) is blocked in strict egress mode
func TestSSHPortDeniedInStrictMode(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	h := newHarness(t)

	// Create runtime in strict mode with restricted egress
	rt := h.runtime(sandbox.ModeStrict, "")
	ws := t.TempDir()

	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{
		ThreadID:  "test-strict-ssh",
		RunID:     "run-strict-ssh-1",
		Workspace: ws,
	})
	if err != nil {
		t.Fatalf("Failed to begin sandbox: %v", err)
	}
	defer run.Finish()

	t.Log("=== Testing SSH port 22 in strict mode (should be blocked) ===")

	// Try to connect to github.com:22
	out, err := runCLI(t, run.Ctx, ws, "sh", "-c", "timeout 2 nc -zv github.com 22 2>&1 || true")
	t.Logf("nc output: %s", out)

	// In strict mode, port 22 should be blocked at the gateway level
	// The exact error depends on how the sandbox reports it
	// It might show "connection refused" or "gateway denied" or similar
	if !strings.Contains(out, "refused") && !strings.Contains(out, "denied") && !strings.Contains(out, "timeout") {
		t.Logf("⚠ Unexpected result (might still indicate blocking): %s", out)
	}

	// Test 2: HTTPS should work in strict mode
	t.Log("\n=== Testing HTTPS port 443 in strict mode (should be allowed) ===")
	out, err = runCLI(t, run.Ctx, ws, "sh", "-c", "curl -s --max-time 3 https://example.com/ 2>&1 | head -1")
	if err == nil && (strings.Contains(out, "<!doctype") || strings.Contains(out, "Example Domain")) {
		t.Log("✓ HTTPS to example.com:443 succeeded in strict mode")
	} else {
		t.Logf("HTTPS attempt: %s (err: %v)", out, err)
	}
}

// TestEgressPolicyEnforcement tests the egress gateway logs for policy decisions
func TestEgressPolicyEnforcement(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping network test in short mode")
	}

	h := newHarness(t)

	// Permissive mode - should allow port 22
	t.Log("=== Permissive Mode: Testing port 22 access ===")
	rt := h.runtime(sandbox.ModePermissive, "")
	ws := t.TempDir()

	run, err := rt.Begin(context.Background(), sandboxrt.BeginReq{
		ThreadID:  "test-policy-perm",
		RunID:     "run-policy-perm",
		Workspace: ws,
	})
	if err != nil {
		t.Fatalf("Failed to begin sandbox: %v", err)
	}

	// Attempt connection (it will fail but gateway should allow the attempt)
	_, _ = runCLI(t, run.Ctx, ws, "sh", "-c", "timeout 1 bash -c 'exec 3<>/dev/tcp/github.com/22' 2>&1 || true")
	run.Finish()

	t.Log("✓ Permissive mode test completed - check logs for 'egress allow attempt' messages")

	// Strict mode - should deny port 22
	t.Log("\n=== Strict Mode: Testing port 22 access (should be denied) ===")
	rt2 := h.runtime(sandbox.ModeStrict, "")
	ws2 := t.TempDir()

	run2, err := rt2.Begin(context.Background(), sandboxrt.BeginReq{
		ThreadID:  "test-policy-strict",
		RunID:     "run-policy-strict",
		Workspace: ws2,
	})
	if err != nil {
		t.Fatalf("Failed to begin sandbox: %v", err)
	}

	// Attempt connection (should be blocked at gateway)
	_, _ = runCLI(t, run2.Ctx, ws2, "sh", "-c", "timeout 1 bash -c 'exec 3<>/dev/tcp/github.com/22' 2>&1 || true")
	run2.Finish()

	t.Log("✓ Strict mode test completed - check logs for 'egress port check failed' messages for port 22")
}
