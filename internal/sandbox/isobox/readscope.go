package isobox

import (
	"os"
	"path/filepath"
	"runtime"
)

// systemReadable are the host paths programs need under scoped reads: the
// system's binaries, libraries, configuration and certificates. Missing ones
// are dropped. The user's home is not among them.
func systemReadable() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/usr", "/bin", "/sbin", "/System", "/Library", "/opt", "/Applications",
			"/private/etc", "/etc", "/private/var/db", "/var/db", "/dev"}
	}
	return []string{"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32", "/etc", "/opt",
		"/run/systemd/resolve"} // /etc/resolv.conf links there on systemd hosts
}

// systemDeny are system files no agent needs, read-denied in both read
// modes: a gVisor sandbox is uid 0, so root-owned 0600/0640 files would
// otherwise be readable (password hashes, sudoers, SSH host keys).
func systemDeny() []string {
	var out []string
	for _, p := range []string{"/etc/shadow", "/etc/shadow-", "/etc/gshadow", "/etc/gshadow-",
		"/etc/sudoers", "/etc/sudoers.d", "/etc/security/opasswd", "/etc/master.passwd"} {
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	keys, _ := filepath.Glob("/etc/ssh/ssh_host_*key*")
	return append(out, keys...)
}
