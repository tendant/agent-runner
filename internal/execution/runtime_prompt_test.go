package execution

import (
	"strings"
	"testing"
)

func TestRuntimeInstructions(t *testing.T) {
	host := runtimeInstructions(runtimeFacts{MemoryDir: "/data/memory"})
	box := runtimeInstructions(runtimeFacts{Sandboxed: true, MemoryDir: "/state/home/s1/memory", MemoryCopy: true})

	for _, want := range []string{"git pull --rebase", "push it to origin", "/data/memory", "several people", "do not edit them", "_send/"} {
		if !strings.Contains(host, want) {
			t.Errorf("unsandboxed: missing %q:\n%s", want, host)
		}
	}
	if strings.Contains(host, "your own copy") || strings.Contains(host, "model API keys never are") {
		t.Errorf("unsandboxed sessions must not get the sandbox's rules:\n%s", host)
	}
	for _, want := range []string{"do not `git push`", "the runner pushes", "/state/home/s1/memory", "your own copy", "model API keys never are"} {
		if !strings.Contains(box, want) {
			t.Errorf("sandboxed: missing %q:\n%s", want, box)
		}
	}
	if strings.Contains(box, "push it to origin") {
		t.Errorf("a sandboxed agent must not be told to push:\n%s", box)
	}
	if strings.Contains(runtimeInstructions(runtimeFacts{}), "### Memory") {
		t.Error("no memory section without a memory dir")
	}
}
