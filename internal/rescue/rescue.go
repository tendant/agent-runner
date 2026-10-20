// Package rescue keeps a workspace's unsaved work before the workspace is
// deleted: commits on no remote (a git bundle), files that differ from
// their repo's last commit or were never committed, and files outside any
// repo. Work in a repo nobody registered (so nobody pushes it), commits
// whose push failed, and edits never committed would otherwise go with the
// workspace, silently.
//
// The workspace is the agent's, so git runs here only in ways an agent's
// repo config can't turn into running its programs: hardened (gitsafe),
// pinned to the repo's .git, and never status/diff (which run its clean
// filters and textconv): changed files are found by hashing them against
// `git ls-tree`.
package rescue

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/agent-runner/agent-runner/internal/gitsafe"
)

const (
	maxFileBytes  = 20 << 20  // a single file larger than this is listed, not kept
	maxTotalBytes = 200 << 20 // all kept files together
	gitTimeout    = time.Minute
)

// heavyDirs are regenerated, not written: never kept.
var heavyDirs = map[string]bool{
	"node_modules": true, ".venv": true, "venv": true, "__pycache__": true, ".next": true,
	".nuxt": true, "dist": true, "build": true, "target": true, ".cache": true, ".gradle": true,
	".turbo": true, ".pytest_cache": true, ".mypy_cache": true, "coverage": true,
}

// RepoReport is what was kept of one repo.
type RepoReport struct {
	Path     string   // relative to the workspace
	Commits  int      // commits on no remote, in Bundle
	Bundle   string   // the bundle file, "" for none
	Changed  []string // files kept that differ from the last commit or were never committed
	Deleted  []string // files in the last commit that are gone
	Remote   string   // origin's URL, for restoring
	Complete bool     // the bundle has the whole history (no remote to build on)
}

// Report is what Save kept.
type Report struct {
	Dir       string // where; "" when there was nothing to keep
	Repos     []RepoReport
	Loose     []string // files outside any repo
	Skipped   []string // files too large to keep
	Truncated bool     // the size limit stopped the copying
	bytes     int64
}

// Empty reports whether nothing was kept.
func (r Report) Empty() bool { return r.Dir == "" }

// Summary is one line for a chat warning.
func (r Report) Summary() string {
	var parts []string
	for _, rp := range r.Repos {
		var what []string
		if rp.Commits > 0 {
			what = append(what, fmt.Sprintf("%d unpushed commit(s)", rp.Commits))
		}
		if n := len(rp.Changed) + len(rp.Deleted); n > 0 {
			what = append(what, fmt.Sprintf("%d uncommitted file change(s)", n))
		}
		parts = append(parts, rp.Path+": "+strings.Join(what, ", "))
	}
	if len(r.Loose) > 0 {
		parts = append(parts, fmt.Sprintf("%d file(s) outside any repo", len(r.Loose)))
	}
	s := strings.Join(parts, "; ")
	if r.Truncated {
		s += " (size limit reached: not everything was kept)"
	}
	return s
}

// Save keeps workspace's unsaved work under root/label (made unique), and
// reports it; nothing is written when there is none. workspace is a session
// or task directory: its workspace/ subdirectory (the agent's) is scanned.
func Save(workspace, root, label string) (Report, error) {
	scan := workspace
	if fi, err := os.Stat(filepath.Join(workspace, "workspace")); err == nil && fi.IsDir() {
		scan = filepath.Join(workspace, "workspace")
	}
	if _, err := os.Stat(scan); err != nil {
		return Report{}, nil
	}
	dest := filepath.Join(root, label+"-"+time.Now().UTC().Format("20060102-150405"))
	r := &Report{}
	s := &saver{dest: dest, r: r}

	var err error
	if fi, e := os.Lstat(filepath.Join(scan, ".git")); e == nil && fi.IsDir() {
		s.repo(scan, ".") // the agent's folder is itself the repo
	} else {
		err = s.walk(scan)
	}
	if err != nil {
		return *r, err
	}
	if len(r.Repos) == 0 && len(r.Loose) == 0 {
		os.RemoveAll(dest)
		return Report{}, nil
	}
	r.Dir = dest
	return *r, s.readme()
}

// walk keeps the repos and loose files under scan.
func (s *saver) walk(scan string) error {
	r := s.r
	return filepath.WalkDir(scan, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(scan, p)
		if d.IsDir() {
			if p == scan {
				return nil
			}
			if heavyDirs[d.Name()] || d.Name() == ".git" {
				return filepath.SkipDir
			}
			if filepath.Dir(rel) == "." && strings.HasPrefix(d.Name(), "_") {
				return filepath.SkipDir // the runner's (_send/, ...)
			}
			if fi, err := os.Lstat(filepath.Join(p, ".git")); err == nil && fi.IsDir() {
				s.repo(p, rel)
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Dir(rel) == "." && strings.HasPrefix(d.Name(), "_") {
			return nil // _progress.json, _schedule.json
		}
		if d.Type().IsRegular() {
			if s.keep(p, filepath.Join("files", rel)) {
				r.Loose = append(r.Loose, rel)
			}
		}
		return nil
	})
}

type saver struct {
	dest string
	r    *Report
}

// keep copies src to dest/rel, within the size limits; false when not kept.
func (s *saver) keep(src, rel string) bool {
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		return false
	}
	if fi.Size() > maxFileBytes {
		s.r.Skipped = append(s.r.Skipped, rel)
		return false
	}
	if s.r.bytes+fi.Size() > maxTotalBytes {
		s.r.Truncated = true
		return false
	}
	out := filepath.Join(s.dest, rel)
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return false
	}
	in, err := os.Open(src)
	if err != nil {
		return false
	}
	defer in.Close()
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	n, err := io.Copy(f, in)
	f.Close()
	if err != nil {
		os.Remove(out)
		return false
	}
	s.r.bytes += n
	return true
}

// repo keeps a git repo's unpushed commits and uncommitted files.
func (s *saver) repo(dir, rel string) {
	rp := RepoReport{Path: rel}
	safe := gitsafe.CheckRepo(dir) == nil
	tracked := map[string]string{} // path -> blob id at HEAD
	sha256Repo := false
	if safe {
		rp.Remote = strings.TrimSpace(s.git(dir, "remote", "get-url", "origin"))
		rp.Remote = gitsafe.RedactURL(rp.Remote)
		if n, err := strconv.Atoi(strings.TrimSpace(s.git(dir, "rev-list", "--count", "--branches", "--not", "--remotes"))); err == nil && n > 0 {
			rp.Commits = n
			bundle := filepath.Join(s.dest, "bundles", strings.ReplaceAll(rel, string(filepath.Separator), "__")+".bundle")
			os.MkdirAll(filepath.Dir(bundle), 0o700)
			args := []string{"bundle", "create", bundle, "--branches"}
			if s.git(dir, "remote") != "" {
				args = append(args, "--not", "--remotes")
			} else {
				rp.Complete = true
			}
			if _, err := s.gitErr(dir, args...); err == nil {
				rp.Bundle = bundle
			}
		}
		for _, line := range strings.Split(s.git(dir, "ls-tree", "-r", "-z", "--full-tree", "HEAD"), "\x00") {
			// "<mode> <type> <id>\t<path>"
			meta, path, ok := strings.Cut(line, "\t")
			f := strings.Fields(meta)
			if !ok || len(f) != 3 || f[1] != "blob" {
				continue
			}
			tracked[path] = f[2]
			sha256Repo = len(f[2]) == 64
		}
	}
	seen := map[string]bool{}
	filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != dir && (d.Name() == ".git" || heavyDirs[d.Name()]) {
				return filepath.SkipDir
			}
			if p != dir {
				if fi, err := os.Lstat(filepath.Join(p, ".git")); err == nil && fi.IsDir() {
					return filepath.SkipDir // a nested repo: not this one's
				}
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		path, _ := filepath.Rel(dir, p)
		path = filepath.ToSlash(path)
		seen[path] = true
		if id, ok := tracked[path]; ok && blobID(p, sha256Repo) == id {
			return nil // as committed
		}
		if s.keep(p, filepath.Join("repos", rel, path)) {
			rp.Changed = append(rp.Changed, path)
		}
		return nil
	})
	for path := range tracked {
		if !seen[path] {
			if _, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path))); err != nil {
				rp.Deleted = append(rp.Deleted, path)
			}
		}
	}
	sort.Strings(rp.Deleted)
	if rp.Commits > 0 || len(rp.Changed) > 0 || len(rp.Deleted) > 0 {
		s.r.Repos = append(s.r.Repos, rp)
	}
}

// blobID is git's object id for a file's content ("" when unreadable).
func blobID(path string, sha256Repo bool) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	var h hash.Hash = sha1.New()
	if sha256Repo {
		h = sha256.New()
	}
	fmt.Fprintf(h, "blob %d\x00", fi.Size())
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *saver) git(dir string, args ...string) string {
	out, _ := s.gitErr(dir, args...)
	return out
}

// gitErr runs a hardened git command pinned to dir's own .git.
func (s *saver) gitErr(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := gitsafe.CommandContext(ctx, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), gitsafe.RepoEnv(dir)...), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	cmd.WaitDelay = 5 * time.Second
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	if _, isExit := err.(*exec.ExitError); err != nil && !isExit {
		return "", err
	}
	return out.String(), err
}

// readme writes what was kept and how to restore it.
func (s *saver) readme() error {
	var b strings.Builder
	b.WriteString("Unsaved work kept by agent-runner before deleting a workspace.\n\n")
	for _, rp := range s.r.Repos {
		fmt.Fprintf(&b, "## %s", rp.Path)
		if rp.Remote != "" {
			fmt.Fprintf(&b, " (origin %s)", rp.Remote)
		}
		b.WriteString("\n")
		if rp.Bundle != "" {
			rel, _ := filepath.Rel(s.dest, rp.Bundle)
			fmt.Fprintf(&b, "- %d commit(s) on no remote: %s\n", rp.Commits, rel)
			if rp.Complete {
				fmt.Fprintf(&b, "  restore: git clone %s <dir>\n", rel)
			} else {
				fmt.Fprintf(&b, "  restore in a clone of the repo: git fetch %s/%s 'refs/heads/*:refs/heads/rescued/*'\n", s.dest, rel)
			}
		}
		if len(rp.Changed) > 0 {
			fmt.Fprintf(&b, "- %d file(s) changed or never committed, as they were: repos/%s/\n", len(rp.Changed), rp.Path)
			b.WriteString("  restore: copy them over a checkout (cp -R repos/" + rp.Path + "/. <checkout>/)\n")
		}
		if len(rp.Deleted) > 0 {
			fmt.Fprintf(&b, "- deleted since the last commit: %s\n", strings.Join(rp.Deleted, ", "))
		}
		b.WriteString("\n")
	}
	if len(s.r.Loose) > 0 {
		fmt.Fprintf(&b, "## Files outside any repo: files/ (%d)\n\n", len(s.r.Loose))
	}
	if len(s.r.Skipped) > 0 {
		fmt.Fprintf(&b, "Too large to keep (over %dMB): %s\n", maxFileBytes>>20, strings.Join(s.r.Skipped, ", "))
	}
	if s.r.Truncated {
		fmt.Fprintf(&b, "The %dMB limit was reached: some files were not kept.\n", maxTotalBytes>>20)
	}
	if err := os.MkdirAll(s.dest, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(s.dest, "README.txt"), []byte(b.String()), 0o600)
}

// Prune removes rescues older than maxAge.
func Prune(root string, maxAge time.Duration) {
	entries, err := os.ReadDir(root)
	if err != nil || maxAge <= 0 {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		if fi, err := e.Info(); err == nil && e.IsDir() && fi.ModTime().Before(cutoff) {
			os.RemoveAll(filepath.Join(root, e.Name()))
		}
	}
}
