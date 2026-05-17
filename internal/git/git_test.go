package git

import (
	"testing"
)

func TestParseDiffStat_Typical(t *testing.T) {
	output := ` main.go    | 10 +++++-----
 handler.go |  5 +++--
 2 files changed, 8 insertions(+), 7 deletions(-)`

	s := parseDiffStat(output)
	if s.Insertions != 8 {
		t.Errorf("expected 8 insertions, got %d", s.Insertions)
	}
	if s.Deletions != 7 {
		t.Errorf("expected 7 deletions, got %d", s.Deletions)
	}
}

func TestParseDiffStat_InsertionsOnly(t *testing.T) {
	output := ` new_file.go | 50 +++++++++++++++++++++++++++++
 1 file changed, 50 insertions(+)`

	s := parseDiffStat(output)
	if s.Insertions != 50 {
		t.Errorf("expected 50 insertions, got %d", s.Insertions)
	}
	if s.Deletions != 0 {
		t.Errorf("expected 0 deletions, got %d", s.Deletions)
	}
}

func TestParseDiffStat_DeletionsOnly(t *testing.T) {
	output := ` old_file.go | 20 --------------------
 1 file changed, 20 deletions(-)`

	s := parseDiffStat(output)
	if s.Insertions != 0 {
		t.Errorf("expected 0 insertions, got %d", s.Insertions)
	}
	if s.Deletions != 20 {
		t.Errorf("expected 20 deletions, got %d", s.Deletions)
	}
}

func TestParseDiffStat_SingularForms(t *testing.T) {
	output := ` file.go | 1 +
 1 file changed, 1 insertion(+)`

	s := parseDiffStat(output)
	if s.Insertions != 1 {
		t.Errorf("expected 1 insertion, got %d", s.Insertions)
	}
}

func TestParseDiffStat_SingularDeletion(t *testing.T) {
	output := ` file.go | 1 -
 1 file changed, 1 deletion(-)`

	s := parseDiffStat(output)
	if s.Deletions != 1 {
		t.Errorf("expected 1 deletion, got %d", s.Deletions)
	}
}

func TestParseDiffStat_Empty(t *testing.T) {
	s := parseDiffStat("")
	if s.Insertions != 0 || s.Deletions != 0 {
		t.Errorf("expected 0/0, got %d/%d", s.Insertions, s.Deletions)
	}
}

func TestParseDiffStat_LargeNumbers(t *testing.T) {
	output := ` generated.go | 10000 ++++++++++------
 1 file changed, 8500 insertions(+), 1500 deletions(-)`

	s := parseDiffStat(output)
	if s.Insertions != 8500 {
		t.Errorf("expected 8500 insertions, got %d", s.Insertions)
	}
	if s.Deletions != 1500 {
		t.Errorf("expected 1500 deletions, got %d", s.Deletions)
	}
}

func TestParseDiffStat_MultipleFiles(t *testing.T) {
	output := ` a.go | 10 +++++++---
 b.go |  5 ++---
 c.go | 20 ++++++++++++++++++++
 3 files changed, 25 insertions(+), 10 deletions(-)`

	s := parseDiffStat(output)
	if s.Insertions != 25 {
		t.Errorf("expected 25 insertions, got %d", s.Insertions)
	}
	if s.Deletions != 10 {
		t.Errorf("expected 10 deletions, got %d", s.Deletions)
	}
}

func TestInjectToken(t *testing.T) {
	cases := []struct {
		name   string
		remote string
		token  string
		want   string
	}{
		{
			name:   "injects token into clean https URL",
			remote: "https://git.example.com/org/repo.git",
			token:  "mytoken",
			want:   "https://oauth2:mytoken@git.example.com/org/repo.git",
		},
		{
			name:   "injects token into clean http URL",
			remote: "http://git.example.com/org/repo.git",
			token:  "mytoken",
			want:   "http://oauth2:mytoken@git.example.com/org/repo.git",
		},
		{
			name:   "skips injection when URL already has credentials",
			remote: "https://user:existingtoken@git.example.com/org/repo.git",
			token:  "newtoken",
			want:   "https://user:existingtoken@git.example.com/org/repo.git",
		},
		{
			name:   "SSH URL returned unchanged",
			remote: "git@github.com:org/repo.git",
			token:  "mytoken",
			want:   "git@github.com:org/repo.git",
		},
		{
			name:   "empty token returns remote unchanged",
			remote: "https://git.example.com/org/repo.git",
			token:  "",
			want:   "https://git.example.com/org/repo.git",
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
