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
