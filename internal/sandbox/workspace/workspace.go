// Package workspace implements the Thread workspace lifecycle around a run:
//
//	Thread workspace --Stage--> run copy (sandbox rw) --Export--> Thread workspace
//
// A run starts from a content-addressed revision of the Thread workspace. On
// completion the sandbox's changes become a validated diff. If the Thread
// workspace moved since the base revision (or validation fails) the diff is NOT
// applied and NOT lost: it is stored in durable run-scoped quarantine.
package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry describes one path in a snapshot.
type Entry struct {
	Hash string `json:"hash,omitempty"` // sha256 of content (regular files)
	Mode uint32 `json:"mode"`
	Link string `json:"link,omitempty"` // symlink target
	Dir  bool   `json:"dir,omitempty"`
}

// Manifest is a content-addressed snapshot.
type Manifest struct {
	Files    map[string]Entry `json:"files"`
	Revision string           `json:"revision"`
}

// Snapshot walks root without following symlinks. Special files (sockets,
// devices, fifos) make the snapshot fail: they have no place in a workspace.
func Snapshot(root string) (Manifest, error) {
	m := Manifest{Files: map[string]Entry{}}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			m.Files[rel] = Entry{Dir: true, Mode: uint32(info.Mode().Perm())}
		case info.Mode()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err != nil {
				return err
			}
			m.Files[rel] = Entry{Link: t}
		case info.Mode().IsRegular():
			h, err := hashFile(p)
			if err != nil {
				return err
			}
			m.Files[rel] = Entry{Hash: h, Mode: uint32(info.Mode().Perm())}
		default:
			return fmt.Errorf("special file %s (%s) not allowed in workspace", rel, info.Mode().Type())
		}
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	m.Revision = revision(m.Files)
	return m, nil
}

func hashFile(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func revision(files map[string]Entry) string {
	keys := make([]string, 0, len(files))
	for k := range files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		e := files[k]
		fmt.Fprintf(h, "%s\x00%s\x00%o\x00%s\x00%v\n", k, e.Hash, e.Mode, e.Link, e.Dir)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Op is a change kind.
type Op string

const (
	Add    Op = "add"
	Modify Op = "modify"
	Delete Op = "delete"
)

// Change is one path-level difference.
type Change struct {
	Path string `json:"path"`
	Op   Op     `json:"op"`
}

// Diff lists changes turning base into after, sorted by path.
func Diff(base, after Manifest) []Change {
	var out []Change
	for p, e := range after.Files {
		b, ok := base.Files[p]
		switch {
		case !ok:
			out = append(out, Change{p, Add})
		case b != e:
			if b.Dir && e.Dir {
				continue // directory mode-only noise
			}
			out = append(out, Change{p, Modify})
		}
	}
	for p := range base.Files {
		if _, ok := after.Files[p]; !ok {
			out = append(out, Change{p, Delete})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// Stage copies the Thread workspace into runDir (which must not exist or be
// empty) and returns the base manifest the copy started from.
func Stage(threadDir, runDir string) (Manifest, error) {
	base, err := Snapshot(threadDir)
	if err != nil {
		return Manifest{}, err
	}
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		return Manifest{}, err
	}
	paths := make([]string, 0, len(base.Files))
	for p := range base.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		if err := copyEntry(threadDir, runDir, p, base.Files[p]); err != nil {
			return Manifest{}, err
		}
	}
	return base, nil
}

func copyEntry(srcRoot, dstRoot, rel string, e Entry) error {
	dst := filepath.Join(dstRoot, filepath.FromSlash(rel))
	switch {
	case e.Dir:
		return os.MkdirAll(dst, 0o755)
	case e.Link != "":
		_ = os.Remove(dst)
		return os.Symlink(e.Link, dst)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(filepath.Join(srcRoot, filepath.FromSlash(rel)))
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".stage-*")
	if err != nil {
		return err
	}
	if _, err := io.Copy(tmp, in); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Chmod(tmp.Name(), fs.FileMode(e.Mode)); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), dst) // atomic replace; never writes through a link
}

// SafePath rejects traversal and git internals-style escapes in a change path.
func SafePath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || filepath.Clean(p) != p || p == ".." || strings.HasPrefix(p, "../") || strings.Contains(p, "\x00") {
		return fmt.Errorf("unsafe path %q", p)
	}
	return nil
}
