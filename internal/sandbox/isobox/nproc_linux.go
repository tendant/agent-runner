package isobox

import "golang.org/x/sys/unix"

func setNproc(n uint64) error {
	return unix.Setrlimit(unix.RLIMIT_NPROC, &unix.Rlimit{Cur: n, Max: n})
}
