//go:build !unix

package sandbox

import "io/fs"

type fileID struct{ links uint64 }

func (f fileID) key() fileID { return f }

// allocated falls back to the file's length where blocks are not reported.
func allocated(info fs.FileInfo) (int64, fileID, bool) {
	if info.IsDir() {
		return 0, fileID{}, false
	}
	return info.Size(), fileID{}, false
}
