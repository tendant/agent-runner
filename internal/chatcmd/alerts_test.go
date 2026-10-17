package chatcmd

import (
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/config"
)

func TestHandleAlerts(t *testing.T) {
	c := &Commander{cfg: &config.Config{MemoryDir: t.TempDir()}}
	if got := c.handleAlerts(""); !strings.Contains(got, "Without an alert policy") {
		t.Fatalf("no policy: %s", got)
	}
	if got := c.handleAlerts("set CI on main: fix it"); !strings.HasPrefix(got, "ok") {
		t.Fatalf("set: %s", got)
	}
	if got := c.handleAlerts(""); !strings.Contains(got, "CI on main: fix it") {
		t.Fatalf("show: %s", got)
	}
	if got := c.handleAlerts("clear"); !strings.HasPrefix(got, "ok") {
		t.Fatalf("clear: %s", got)
	}
	if got := c.handleAlerts(""); !strings.Contains(got, "Without an alert policy") {
		t.Fatalf("after clear: %s", got)
	}
}
