package executor

import (
	"context"
	"slices"
	"testing"
)

func TestWithSessionEnv(t *testing.T) {
	ctx := WithSessionEnv(context.Background(), []string{"GITEE_TOKEN=t"})
	cmd, release, err := resolveLauncher(ctx, nil).Command(ctx, LaunchSpec{Name: "true", ExtraEnv: []string{"X=1"}})
	if err != nil {
		t.Fatal(err)
	}
	release()
	if !slices.Contains(cmd.Env, "GITEE_TOKEN=t") || !slices.Contains(cmd.Env, "X=1") {
		t.Errorf("env %v", cmd.Env)
	}
	if _, confined := LauncherFrom(ctx); confined {
		t.Error("session env on the host is not a sandbox")
	}
	sl := &SandboxLauncher{ScratchDir: "/s"}
	ctx = WithSessionEnv(WithLauncher(context.Background(), sl), []string{"A=b"})
	if l, ok := LauncherFrom(ctx); !ok || l != Launcher(sl) {
		t.Error("the sandbox launcher must still be found under the wrapper")
	}
	if scratchDir(ctx, nil) != "/s" {
		t.Error("scratch dir through the wrapper")
	}
	if WithSessionEnv(context.Background(), nil) != context.Background() {
		t.Error("no env: ctx unchanged")
	}
	env := sl.env(LaunchSpec{Name: "pi", ExtraEnv: []string{"GITEE_TOKEN=t"}})
	if !slices.Contains(env, "GITEE_TOKEN=t") {
		t.Errorf("sandbox env %v", env)
	}
}
