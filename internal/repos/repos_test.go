package repos

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-runner/agent-runner/internal/agentsecrets"
)

func TestRegistry(t *testing.T) {
	r := New(filepath.Join(t.TempDir(), "repos.json"))
	if list, err := r.List(); err != nil || len(list) != 0 {
		t.Fatalf("empty: %v %v", list, err)
	}
	a := Repo{Name: "oam-server", URL: "https://lei@gitee.com/org/oam-server.git", Secret: "GITEE_TOKEN", Push: true}
	b := Repo{Name: "docs", URL: "git@github.com:org/docs.git", Secret: "GH_DEPLOY_KEY"}
	for _, rp := range []Repo{a, b} {
		if err := r.Put(rp); err != nil {
			t.Fatal(err)
		}
	}
	a.Branch = "develop"
	if err := r.Put(a); err != nil { // replace
		t.Fatal(err)
	}
	list, _ := r.List()
	if len(list) != 2 || list[0].Name != "docs" || list[1].Branch != "develop" {
		t.Errorf("list %+v", list)
	}
	if got, ok, _ := r.Get("oam-server"); !ok || got.User() != "lei" {
		t.Errorf("get %+v %v", got, ok)
	}
	if ok, _ := r.Remove("docs"); !ok {
		t.Error("remove")
	}
	for name, bad := range map[string]Repo{
		"password in URL": {Name: "x", URL: "https://lei:tok@gitee.com/o/x.git"},
		"http":            {Name: "x", URL: "http://gitee.com/o/x.git"},
		"file":            {Name: "x", URL: "file:///tmp/x"},
		"no path":         {Name: "x", URL: "https://gitee.com"},
		"bad name":        {Name: "../x", URL: "https://gitee.com/o/x.git"},
		"runner secret":   {Name: "x", URL: "https://gitee.com/o/x.git", Secret: "GIT_TOKEN"},
		"bad branch":      {Name: "x", URL: "https://gitee.com/o/x.git", Branch: "-x"},
	} {
		if err := r.Put(bad); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestNameAndUser(t *testing.T) {
	for url, want := range map[string]string{
		"https://gitee.com/org/oam-server.git": "oam-server",
		"https://github.com/org/docs/":         "docs",
		"git@github.com:org/tool.git":          "tool",
		"ssh://git@host/org/x.git":             "x",
	} {
		if got := NameFromURL(url); got != want {
			t.Errorf("NameFromURL(%s) = %s, want %s", url, got, want)
		}
	}
	if u := (Repo{URL: "https://gitee.com/o/x.git"}).User(); u != "oauth2" {
		t.Errorf("default user %s", u)
	}
}

// The credential helper agent-runner passes answers with the URL's user and
// the secret's token, overriding helpers in the repo's config.
func TestAuthHelper(t *testing.T) {
	secrets := agentsecrets.New(filepath.Join(t.TempDir(), "secrets.json"))
	secrets.Set("GITEE_TOKEN", "tok-123", "")
	rp := Repo{Name: "x", URL: "https://lei@gitee.com/o/x.git", Secret: "GITEE_TOKEN"}
	auth, err := rp.Auth(secrets)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run()
	exec.Command("git", "-C", repo, "config", "credential.helper", "!echo password=planted").Run()
	cmd := exec.Command("git", append(auth.Args, "credential", "fill")...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), auth.Env...)
	cmd.Stdin = strings.NewReader("protocol=https\nhost=gitee.com\n\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.Contains(string(out), "username=lei") || !strings.Contains(string(out), "password=tok-123") {
		t.Errorf("credential fill:\n%s", out)
	}
	if _, err := (Repo{Name: "x", URL: rp.URL, Secret: "MISSING_TOKEN"}).Auth(secrets); err == nil || !strings.Contains(err.Error(), "/env secret MISSING_TOKEN") {
		t.Errorf("missing secret: %v", err)
	}
	if a, err := (Repo{Name: "x", URL: rp.URL}).Auth(secrets); err != nil || len(a.Args) != 0 {
		t.Errorf("public repo: %+v %v", a, err)
	}
}

// Sync clones into a bare cache, then fetches; Checkout gives a working copy
// on the branch, origin pointing at the repo's URL, sharing nothing with the
// cache.
func TestSyncAndCheckout(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")
	g := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	g(base, "init", "-q", "--bare", "-b", "main", origin)
	g(base, "clone", "-q", origin, work)
	g(work, "config", "user.email", "t@t")
	g(work, "config", "user.name", "T")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("1"), 0o644)
	g(work, "add", "-A")
	g(work, "commit", "-qm", "one")
	g(work, "push", "-q", "origin", "HEAD")
	g(work, "checkout", "-qb", "develop")
	g(work, "push", "-q", "origin", "develop")

	cache := filepath.Join(base, "cache")
	rp := Repo{Name: "site", URL: origin, Push: true}
	dst := filepath.Join(base, "ws", "site")
	if err := Checkout(ctx, rp, cache, dst, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "1" {
		t.Errorf("checkout content %q", b)
	}
	out, _ := exec.Command("git", "-C", dst, "remote", "get-url", "origin").Output()
	if strings.TrimSpace(string(out)) != origin {
		t.Errorf("origin %s", out)
	}

	// A new commit upstream reaches the next checkout; develop is honoured.
	g(work, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(work, "a.txt"), []byte("2"), 0o644)
	g(work, "commit", "-qam", "two")
	g(work, "push", "-q", "origin", "main")
	dst2 := filepath.Join(base, "ws2", "site")
	if err := Checkout(ctx, rp, cache, dst2, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst2, "a.txt")); string(b) != "2" {
		t.Errorf("not refreshed: %q", b)
	}
	rp.Branch = "develop"
	dst3 := filepath.Join(base, "ws3", "site")
	if err := Checkout(ctx, rp, cache, dst3, nil); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", dst3, "branch", "--show-current").Output(); strings.TrimSpace(string(out)) != "develop" {
		t.Errorf("branch %s", out)
	}
	// Writing the workspace never touches the cache.
	if fi, err := os.Stat(filepath.Join(cache, "site.git", "HEAD")); err != nil || fi.IsDir() {
		t.Errorf("bare cache missing: %v", err)
	}
	if err := Checkout(ctx, Repo{Name: "nope", URL: filepath.Join(base, "missing.git")}, cache, filepath.Join(base, "ws4", "nope"), nil); err == nil {
		t.Error("a missing remote must fail")
	}
}

// An ssh repo logs in with the secret's key as a file of its own, through
// agent-runner's ssh command (never the host's keys, agent or config), over
// a core.sshCommand planted in the repo.
func TestSSHAuth(t *testing.T) {
	dir := t.TempDir()
	secrets := agentsecrets.New(filepath.Join(dir, "secrets.json"))
	key := "-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n-----END OPENSSH PRIVATE KEY-----"
	secrets.Set("GITEE_SSH_KEY", key, "")

	fake := filepath.Join(dir, "fake-ssh")
	argLog := filepath.Join(dir, "ssh-args")
	os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > "+argLog+"\nexit 1\n"), 0o755)
	saved := sshProgram
	sshProgram = fake
	defer func() { sshProgram = saved }()

	rp := Repo{Name: "x", URL: "git@gitee.com:org/x.git", Secret: "GITEE_SSH_KEY", Push: true}
	if err := rp.Check(); err != nil {
		t.Fatal(err)
	}
	auth, err := rp.Auth(secrets)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	exec.Command("git", "-C", repo, "init", "-q").Run()
	exec.Command("git", "-C", repo, "config", "core.sshCommand", "touch "+filepath.Join(dir, "planted")).Run()
	cmd := exec.Command("git", append(auth.Args, "ls-remote", rp.URL)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), auth.Env...)
	cmd.Run() // fails: the fake ssh exits 1

	if _, err := os.Stat(filepath.Join(dir, "planted")); err == nil {
		t.Fatal("the repo's core.sshCommand ran")
	}
	args, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("agent-runner's ssh did not run: %v", err)
	}
	keyFile := filepath.Join(agentsecrets.FilesDir(secrets.Path()), "GITEE_SSH_KEY")
	for _, want := range []string{"-i\n" + keyFile, "IdentityAgent=none", "-F\n/dev/null", "StrictHostKeyChecking=accept-new", "git@gitee.com"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("ssh args lack %q:\n%s", want, args)
		}
	}
	if b, _ := os.ReadFile(keyFile); string(b) != key+"\n" {
		t.Errorf("key file %q", b)
	}
	if fi, _ := os.Stat(keyFile); fi.Mode().Perm() != 0o600 {
		t.Errorf("key file mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(keyFile)); fi.Mode().Perm() != 0o700 {
		t.Errorf("key dir mode %v", fi.Mode().Perm())
	}
	if err := (Repo{Name: "y", URL: "git@gitee.com:org/y.git"}).Check(); err == nil || !strings.Contains(err.Error(), "needs a key") {
		t.Errorf("ssh without a secret: %v", err)
	}
}

// A remote that renames its default branch: the next checkout follows it
// instead of coming out empty.
func TestSyncFollowsRenamedDefaultBranch(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	src := filepath.Join(base, "src")
	g := func(dir string, args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	g(base, "init", "-q", "--bare", "-b", "master", origin)
	g(base, "clone", "-q", origin, src)
	os.WriteFile(filepath.Join(src, "a.txt"), []byte("a"), 0o644)
	g(src, "add", "-A")
	g(src, "-c", "user.email=t@t", "-c", "user.name=T", "commit", "-qm", "one")
	g(src, "push", "-q", "origin", "HEAD:master")
	cache := filepath.Join(base, "cache")
	rp := Repo{Name: "site", URL: origin}
	if err := Checkout(ctx, rp, cache, filepath.Join(base, "w1", "site"), nil); err != nil {
		t.Fatal(err)
	}
	// Rename master -> main on the remote.
	g(src, "push", "-q", "origin", "HEAD:main")
	g(origin, "symbolic-ref", "HEAD", "refs/heads/main")
	g(src, "push", "-q", "origin", ":master")

	dst := filepath.Join(base, "w2", "site")
	if err := Checkout(ctx, rp, cache, dst, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "a.txt")); string(b) != "a" {
		t.Errorf("checkout after the rename is empty")
	}
	if out, _ := exec.Command("git", "-C", dst, "branch", "--show-current").Output(); strings.TrimSpace(string(out)) != "main" {
		t.Errorf("branch %s, want main", out)
	}
}

// An empty registered repo (no branch yet) can be checked out again.
func TestCheckoutEmptyRepoTwice(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	origin := filepath.Join(base, "empty.git")
	exec.Command("git", "init", "-q", "--bare", origin).Run()
	rp := Repo{Name: "empty", URL: origin}
	cache := filepath.Join(base, "cache")
	for i := 0; i < 2; i++ {
		if err := Checkout(ctx, rp, cache, filepath.Join(base, "w"+string(rune('0'+i)), "empty"), nil); err != nil {
			t.Fatalf("checkout %d: %v", i+1, err)
		}
	}
}
