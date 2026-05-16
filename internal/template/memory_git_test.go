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
			got := injectToken(tc.remote, tc.token)
			if got != tc.want {
				t.Errorf("injectToken(%q, %q) = %q, want %q", tc.remote, tc.token, got, tc.want)
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

	// Init with remoteA.
	res, err := InitMemoryGit(memDir, remoteA, MemoryGitCreds{})
	if err != nil {
		t.Fatalf("InitMemoryGit(remoteA): %v", err)
	}
	if !res.Pushed {
		t.Error("expected push to remoteA to succeed")
	}

	// Switch to remoteB (empty).
	res2, err := InitMemoryGit(memDir, remoteB, MemoryGitCreds{})
	if err != nil {
		t.Fatalf("InitMemoryGit(remoteB): %v", err)
	}
	if !res2.RemoteChanged {
		t.Error("expected RemoteChanged=true after URL update")
	}
	if !res2.Pushed {
		t.Error("expected push to remoteB to succeed")
	}

	// Verify remoteB actually has the commit.
	out, err := exec.Command("git", "-C", remoteB, "log", "--oneline").Output()
	if err != nil {
		t.Fatalf("git log on remoteB: %v", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 {
		t.Error("remoteB has no commits after URL-change push")
	}
}
