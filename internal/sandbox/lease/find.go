package lease

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// FindByArgvTag returns processes whose command line contains the sandbox tag
// ("sbx."+tag, see ProcSpec.Tag). It lets the supervisor find a run's process
// group when the runner died before recording pids.
func FindByArgvTag(tag string) []ProcRef {
	if tag == "" {
		return nil
	}
	needle := "sbx." + tag
	out, err := exec.Command("ps", "-axo", "pid=,command=").Output()
	if err != nil {
		return nil
	}
	self := os.Getpid()
	var refs []ProcRef
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		i := strings.IndexByte(line, ' ')
		if i < 0 || !strings.Contains(line[i:], needle) {
			continue
		}
		pid, err := strconv.Atoi(line[:i])
		if err != nil || pid == self {
			continue
		}
		refs = append(refs, ProcRef{PID: pid, Start: ProcStart(pid)})
	}
	return refs
}
