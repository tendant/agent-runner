// Package chatlog keeps what was said in each chat, as it was said, for
// longer than a thread keeps it (internal/thread compacts a thread's history
// and forgets idle threads). A run is given its own chat's earlier threads
// to look back on: a copy in `_chats/` of its working directory.
//
// Layout: Root/<chat>/<thread>/<YYYY-MM-DD>.md, one Markdown file per thread
// per day, so retention drops whole days. <chat> is the scope a run may see:
// an agent-stream channel, or a Telegram/WeChat chat (whose one thread is
// the chat itself).
//
// Only messages that reach a thread are recorded: chat commands (/env
// secret, /config set, ...) never do, so no secret is ever written here.
package chatlog

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DirName is the folder a run's copy of its chat is put in, in the
// agent's working directory.
const DirName = "_chats"

const dayLayout = "2006-01-02"

// sweepEvery is how often Append drops days past the retention.
const sweepEvery = time.Hour

// Entry is one message of a thread.
type Entry struct {
	Time    time.Time
	Speaker string // as the agent should read it ("User", "You (the assistant)", ...)
	Content string
}

// Archive appends threads' messages under Root and drops days older than
// Retention (none when non-positive).
type Archive struct {
	Root      string
	Retention time.Duration

	mu        sync.Mutex
	lastSweep time.Time
}

// Append records e in thread's file for e's day, under chat ("" = the
// thread is its own chat).
func (a *Archive) Append(chat, thread string, e Entry) error {
	if thread == "" {
		return errors.New("chatlog: no thread")
	}
	if chat == "" {
		chat = thread
	}
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.lastSweep) > sweepEvery {
		a.lastSweep = time.Now()
		a.sweepLocked(a.lastSweep)
	}

	day := e.Time.Local()
	dir := filepath.Join(a.Root, SafeName(chat), SafeName(thread))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, day.Format(dayLayout)+".md")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	var b strings.Builder
	if info, err := f.Stat(); err == nil && info.Size() == 0 {
		fmt.Fprintf(&b, "# %s, %s\n\n", thread, day.Format("Monday 2 January 2006"))
	}
	fmt.Fprintf(&b, "### %s %s\n\n%s\n\n", day.Format("15:04"), e.Speaker, strings.TrimSpace(e.Content))
	_, err = f.WriteString(b.String())
	return err
}

// Sweep drops the days older than the retention, and the folders that
// leaves empty.
func (a *Archive) Sweep(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastSweep = now
	a.sweepLocked(now)
}

func (a *Archive) sweepLocked(now time.Time) {
	if a.Retention <= 0 {
		return
	}
	cutoff := now.Add(-a.Retention).Local().Format(dayLayout)
	chats, _ := os.ReadDir(a.Root)
	for _, c := range chats {
		if !c.IsDir() {
			continue
		}
		chatDir := filepath.Join(a.Root, c.Name())
		threads, _ := os.ReadDir(chatDir)
		for _, t := range threads {
			if !t.IsDir() {
				continue
			}
			threadDir := filepath.Join(chatDir, t.Name())
			days, _ := os.ReadDir(threadDir)
			for _, d := range days {
				// Day files sort as dates: older than the cutoff's day goes.
				if day, ok := strings.CutSuffix(d.Name(), ".md"); ok && day < cutoff {
					if err := os.Remove(filepath.Join(threadDir, d.Name())); err != nil {
						slog.Warn("chatlog: could not remove an expired day", "path", filepath.Join(threadDir, d.Name()), "error", err)
					}
				}
			}
			os.Remove(threadDir) // only if now empty
		}
		os.Remove(chatDir) // only if now empty
	}
}

// ChatOf is the chat folder thread is recorded under in root, "" when it
// has no record. A thread recorded both on its own and in a chat (messages
// from before its chat was known) belongs to the chat.
func ChatOf(root, thread string) string {
	name := SafeName(thread)
	matches, _ := filepath.Glob(filepath.Join(root, "*", name))
	chat := ""
	for _, m := range matches {
		if info, err := os.Stat(m); err != nil || !info.IsDir() {
			continue
		}
		if c := filepath.Base(filepath.Dir(m)); chat == "" || c != name {
			chat = c
		}
	}
	return chat
}

// CopyChat replaces dst with a copy of every thread recorded in the same
// chat as thread, and returns the folder name thread itself has in it; ""
// (and no dst) when the thread has no record.
func CopyChat(root, thread, dst string) (string, error) {
	if err := os.RemoveAll(dst); err != nil {
		return "", err
	}
	chat := ChatOf(root, thread)
	if chat == "" {
		return "", nil
	}
	src := filepath.Join(root, chat)
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		return copyFile(p, out)
	})
	if err != nil {
		return "", err
	}
	return SafeName(thread), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// SafeName is key as a file name: letters, digits, dash and underscore kept,
// anything else replaced with an underscore.
func SafeName(key string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, key)
}
