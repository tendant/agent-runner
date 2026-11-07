//go:build unix

package sandbox

import (
	"io/fs"
	"syscall"
)

// fileID names a file on its device, for counting hard links once.
type fileID struct {
	dev, ino uint64
	links    uint64
}

func (f fileID) key() fileID { return fileID{dev: f.dev, ino: f.ino} }

// allocated is the disk a file takes: its blocks (512 bytes each, by
// stat(2)), and its identity for hard links.
func allocated(info fs.FileInfo) (int64, fileID, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return info.Size(), fileID{}, false
	}
	return int64(st.Blocks) * 512, fileID{dev: uint64(st.Dev), ino: uint64(st.Ino), links: uint64(st.Nlink)}, true
}
