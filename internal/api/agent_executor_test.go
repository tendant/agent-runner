package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildErrorContext_Basic(t *testing.T) {
	result := buildErrorContext(3, "timeout exceeded", "some output here")

	if !strings.Contains(result, "iteration 3") {
		t.Error("expected iteration number in output")
	}
	if !strings.Contains(result, "timeout exceeded") {
		t.Error("expected error message in output")
	}
	if !strings.Contains(result, "some output here") {
		t.Error("expected partial output in output")
	}
}

func TestBuildErrorContext_EmptyPartialOutput(t *testing.T) {
	result := buildErrorContext(1, "failed", "")

	if !strings.Contains(result, "failed") {
		t.Error("expected error message")
	}
	if strings.Contains(result, "Partial output") {
		t.Error("should not include partial output section when empty")
	}
}

func TestBuildErrorContext_TruncatesLongOutput(t *testing.T) {
	longOutput := strings.Repeat("x", 3000)
	result := buildErrorContext(1, "err", longOutput)

	if !strings.Contains(result, "... (truncated)") {
		t.Error("expected truncation marker")
	}
	// The partial output in the result should be capped at maxPartialOutputChars
	if strings.Contains(result, strings.Repeat("x", 2001)) {
		t.Error("output should be truncated to 2000 chars")
	}
}

func TestBuildErrorContext_ExactlyAtLimit(t *testing.T) {
	exactOutput := strings.Repeat("y", maxPartialOutputChars)
	result := buildErrorContext(1, "err", exactOutput)

	if strings.Contains(result, "... (truncated)") {
		t.Error("output at exactly the limit should not be truncated")
	}
	if !strings.Contains(result, exactOutput) {
		t.Error("expected full output when at limit")
	}
}

func TestCollectOutputFiles_NonexistentDir(t *testing.T) {
	files, err := collectOutputFiles("/nonexistent/path")
	if err != nil {
		t.Fatalf("expected nil error for nonexistent dir, got: %v", err)
	}
	if files != nil {
		t.Errorf("expected nil files, got %d", len(files))
	}
}

func TestCollectOutputFiles_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 0 {
		t.Errorf("expected 0 files, got %d", len(files))
	}
}

func TestCollectOutputFiles_CollectsFiles(t *testing.T) {
	dir := t.TempDir()

	// Create test files
	os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0644)
	os.WriteFile(filepath.Join(dir, "file2.txt"), []byte("world"), 0644)

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 files, got %d", len(files))
	}

	// Check that file data is correct
	names := map[string]bool{}
	for _, f := range files {
		names[f.Name] = true
		if f.ContentType == "" {
			t.Errorf("file %s has empty content type", f.Name)
		}
	}
	if !names["file1.txt"] || !names["file2.txt"] {
		t.Error("expected both file1.txt and file2.txt")
	}
}

func TestCollectOutputFiles_SkipsDirs(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("data"), 0644)
	os.MkdirAll(filepath.Join(dir, "subdir"), 0755)

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("expected 1 file (dirs skipped), got %d", len(files))
	}
	if files[0].Name != "file.txt" {
		t.Errorf("expected file.txt, got %s", files[0].Name)
	}
}

func TestCollectOutputFiles_FileLimit(t *testing.T) {
	dir := t.TempDir()

	// Create 25 files (exceeds maxOutputFiles = 20)
	for i := 0; i < 25; i++ {
		name := filepath.Join(dir, strings.Replace("file_XX.txt", "XX", strings.Repeat("a", i+1), 1))
		os.WriteFile(name, []byte("data"), 0644)
	}

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(files) > maxOutputFiles {
		t.Errorf("expected at most %d files, got %d", maxOutputFiles, len(files))
	}
}

func TestCollectOutputFiles_SizeLimit(t *testing.T) {
	dir := t.TempDir()

	// Create a file that's just under the size limit
	bigData := make([]byte, 9<<20) // 9MB
	os.WriteFile(filepath.Join(dir, "big.bin"), bigData, 0644)

	// Create another file that would exceed the limit
	os.WriteFile(filepath.Join(dir, "extra.bin"), make([]byte, 2<<20), 0644) // 2MB

	files, err := collectOutputFiles(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should collect the big file but skip the extra one (9MB + 2MB > 10MB)
	if len(files) != 1 {
		t.Errorf("expected 1 file (size limit), got %d", len(files))
	}
}
