package isobox

import (
	"fmt"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// gvisorUnreachable returns an error naming the first directory on the way to
// dir that a gVisor sandbox cannot enter. Inside, the agent is uid 0 / gid 0
// with no capabilities (no CAP_DAC_OVERRIDE), so it gets a file's owner bits
// only where the host owner is root, its group bits only where the group is
// root, and the "other" bits everywhere else.
func gvisorUnreachable(dir string) error {
	p, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil // Launch reports a missing root on its own
	}
	for d := p; ; d = filepath.Dir(d) {
		var st unix.Stat_t
		if err := unix.Stat(d, &st); err != nil {
			return nil
		}
		x := st.Mode & 0o001
		switch {
		case st.Uid == 0:
			x = st.Mode & 0o100
		case st.Gid == 0:
			x = st.Mode & 0o010
		}
		if x == 0 {
			return fmt.Errorf("gVisor sandbox cannot enter %s (owner uid %d, gid %d, mode %04o) on the way to %s: "+
				"inside, the agent is root without capabilities, so every parent of the runner's data dirs must be "+
				"owned by root or searchable by others (e.g. chmod o+x %s)", d, st.Uid, st.Gid, st.Mode&0o7777, dir, d)
		}
		if d == "/" || d == "." {
			return nil
		}
	}
}
