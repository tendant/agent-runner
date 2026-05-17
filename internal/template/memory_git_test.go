package template

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInjectToken(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		token  string
		want   string
	}{
		{
			name:   "empty token returns remote unchanged",
			remote: "https://git.example.com/repo.git",
			token:  "",
			want:   "https://git.example.com/repo.git",
		},
		{
			name:   "injects token into clean https URL",
			remote: "https://git.example.com/repo.git",
			token:  "mytoken",
			want:   "https://oauth2:mytoken@git.example.com/repo.git",
		},
		{
			name:   "injects token into clean http URL",
			remote: "http://git.example.com/repo.git",
			token:  "mytoken",
			want:   "http://oauth2:mytoken@git.example.com/repo.git",
		},
		{
			name:   "skips injection when URL already has credentials",
			remote: "https://sites:existingtoken@git.example.com/repo.git",
			token:  "newtoken",
			want:   "https://sites:existingtoken@git.example.com/repo.git",
		},
		{
			name:   "SSH URL returned unchanged",
			remote: "git@github.com:org/repo.git",
			token:  "mytoken",
			want:   "git@github.com:org/repo.git",
		},
		{
			name:   "empty remote returns empty",
			remote: "",
			token:  "mytoken",
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := InjectToken(tc.remote, tc.token)
			if got != tc.want {
				t.Errorf("InjectToken(%q, %q) = %q, want %q", tc.remote, tc.token, got, tc.want)
			}
		})
	}
}

func TestInitMemoryGit_URLChange(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	remoteA := t.TempDir()
	remoteB := t.TempDir()
	for _, r := range []string{remoteA, remoteB} {
		if err := exec.Command("git", "init", "--bare", r).Run(); err != nil {
			t.Fatalf("git init --bare %s: %v", r, err)
		}
	}

	memDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(memDir, "note.md"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	// Init with remoteA — should configure remote but NOT push.
	res, err := InitMemoryGit(memDir, remoteA, MemoryGitCreds{})
	if err != nil {
		t.Fatalf("InitMemoryGit(remoteA): %v", err)
	}
	if !res.RemoteChanged {
		t.Error("expected RemoteChanged=true on first init")
	}
	// Push explicitly.
	if err := CommitAndPushMemory(memDir, MemoryGitCreds{}); err != nil {
		t.Fatalf("CommitAndPushMemory(remoteA): %v", err)
	}

	// Switch to remoteB — remote should be updated, no implicit push.
	res2, err := InitMemoryGit(memDir, remoteB, MemoryGitCreds{})
	if err != nil {
		t.Fatalf("InitMemoryGit(remoteB): %v", err)
	}
	if !res2.RemoteChanged {
		t.Error("expected RemoteChanged=true after URL update")
	}

	// Push explicitly to remoteB.
	if err := CommitAndPushMemory(memDir, MemoryGitCreds{}); err != nil {
		t.Fatalf("CommitAndPushMemory(remoteB): %v", err)
	}

	// Verify remoteB actually has the commit.
	out, err := exec.Command("git", "-C", remoteB, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log on remoteB: %v", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Error("remoteB has no commits after explicit push")
	}
}

func TestPullMemory_DivergedLocal(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	remote := t.TempDir()
	if err := exec.Command("git", "init", "--bare", remote).Run(); err != nil {
		t.Fatalf("git init --bare: %v", err)
	}

	// Set up local repo A — initial commit, push to remote.
	repoA := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoA, "a.md"), []byte("from A"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InitMemoryGit(repoA, remote, MemoryGitCreds{}); err != nil {
		t.Fatalf("InitMemoryGit(repoA): %v", err)
	}

	// Clone remote into repo B and push a new commit — remote is now ahead of A.
	repoB := t.TempDir()
	if err := exec.Command("git", "clone", remote, repoB).Run(); err != nil {
		t.Fatalf("git clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoB, "b.md"), []byte("from B"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "user.email", "b@local"},
		{"config", "user.name", "B"},
		{"add", "-A"},
		{"commit", "-m", "from B"},
		{"push", "origin", "HEAD"},
	} {
		if err := exec.Command("git", append([]string{"-C", repoB}, args...)...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	// Add a local commit to A (diverged from remote).
	if err := os.WriteFile(filepath.Join(repoA, "local.md"), []byte("local only"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", "local only"},
	} {
		if err := exec.Command("git", append([]string{"-C", repoA}, args...)...).Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}

	// PullMemory should rebase A's local commit on top of B's remote commit.
	if _, err := PullMemory(repoA, MemoryGitCreds{}); err != nil {
		t.Fatalf("PullMemory: %v", err)
	}

	// Both files should be present after rebase.
	for _, name := range []string{"a.md", "b.md", "local.md"} {
		if _, err := os.Stat(filepath.Join(repoA, name)); err != nil {
			t.Errorf("expected %s to exist after pull --rebase: %v", name, err)
		}
	}
}
