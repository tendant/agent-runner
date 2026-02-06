package executor

import (
	"testing"
)

func TestValidateDiff_GitDirViolation(t *testing.T) {
	v := NewValidator(nil, false)

	tests := []struct {
		name    string
		file    string
		blocked bool
	}{
		{"git config", ".git/config", true},
		{"git exact", ".git", true},
		{"git hooks", ".git/hooks/pre-commit", true},
		{"gitignore not blocked", ".gitignore", false},
		{"gitkeep not blocked", "src/.gitkeep", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateDiff([]string{tt.file}, nil)
			if tt.blocked {
				if err == nil {
					t.Errorf("expected GIT_DIR_VIOLATION for %s", tt.file)
				} else if err.Code != "GIT_DIR_VIOLATION" {
					t.Errorf("expected GIT_DIR_VIOLATION, got %s", err.Code)
				}
			} else {
				if err != nil && err.Code == "GIT_DIR_VIOLATION" {
					t.Errorf("did not expect GIT_DIR_VIOLATION for %s", tt.file)
				}
			}
		})
	}
}

func TestValidateDiff_CIConfigViolation(t *testing.T) {
	v := NewValidator(nil, false)

	tests := []struct {
		name string
		file string
	}{
		{"github workflow", ".github/workflows/ci.yml"},
		{"github dir", ".github/CODEOWNERS"},
		{"gitlab ci", ".gitlab-ci.yml"},
		{"gitlab ci yaml", ".gitlab-ci.yaml"},
		{"travis", ".travis.yml"},
		{"circleci", ".circleci/config.yml"},
		{"jenkinsfile", "Jenkinsfile"},
		{"azure pipelines", "azure-pipelines.yml"},
		{"drone", ".drone.yml"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateDiff([]string{tt.file}, nil)
			if err == nil {
				t.Errorf("expected CI_CONFIG_VIOLATION for %s", tt.file)
			} else if err.Code != "CI_CONFIG_VIOLATION" {
				t.Errorf("expected CI_CONFIG_VIOLATION, got %s for %s", err.Code, tt.file)
			}
		})
	}
}

func TestValidateDiff_HooksViolation(t *testing.T) {
	v := NewValidator(nil, false)

	tests := []struct {
		name string
		file string
	}{
		{"husky pre-commit", ".husky/pre-commit"},
		{"husky commit-msg", ".husky/commit-msg"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateDiff([]string{tt.file}, nil)
			if err == nil {
				t.Errorf("expected HOOKS_VIOLATION for %s", tt.file)
			} else if err.Code != "HOOKS_VIOLATION" {
				t.Errorf("expected HOOKS_VIOLATION, got %s for %s", err.Code, tt.file)
			}
		})
	}
}

func TestValidateDiff_SecretsViolation(t *testing.T) {
	v := NewValidator(nil, false)

	tests := []struct {
		name string
		file string
	}{
		{"env", ".env"},
		{"env local", ".env.local"},
		{"env production", ".env.production"},
		{"env development", ".env.development"},
		{"secrets dir", "secrets/api-key.txt"},
		{"credentials json", "credentials.json"},
		{"credentials yaml", "credentials.yaml"},
		{"service account", "service-account.json"},
		{"aws credentials", ".aws/credentials"},
		{"ssh dir", ".ssh/id_rsa"},
		{"id_rsa", "id_rsa"},
		{"id_ed25519", "id_ed25519"},
		{"pem file", "server.pem"},
		{"key file", "private.key"},
		{"nested pem", "certs/server.pem"},
		{"nested key", "ssl/private.key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := v.ValidateDiff([]string{tt.file}, nil)
			if err == nil {
				t.Errorf("expected SECRETS_VIOLATION for %s", tt.file)
			} else if err.Code != "SECRETS_VIOLATION" {
				t.Errorf("expected SECRETS_VIOLATION, got %s for %s", err.Code, tt.file)
			}
		})
	}
}

func TestValidateDiff_BlockedPaths_DirPattern(t *testing.T) {
	v := NewValidator([]string{"vendor/", "node_modules/"}, false)

	err := v.ValidateDiff([]string{"vendor/lib/foo.go"}, nil)
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for vendor/ directory")
	}

	err = v.ValidateDiff([]string{"node_modules/pkg/index.js"}, nil)
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for node_modules/ directory")
	}
}

func TestValidateDiff_BlockedPaths_WildcardExt(t *testing.T) {
	v := NewValidator([]string{"*.exe", "*.dll"}, false)

	err := v.ValidateDiff([]string{"build/app.exe"}, nil)
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for *.exe")
	}
}

func TestValidateDiff_BlockedPaths_ExactMatch(t *testing.T) {
	v := NewValidator([]string{"Makefile", "go.sum"}, false)

	err := v.ValidateDiff([]string{"Makefile"}, nil)
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for exact match Makefile")
	}
}

func TestValidateDiff_AllowedPaths_InDir(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{"src/main.go"}, []string{"src/"})
	if err != nil {
		t.Errorf("expected no error for file in allowed dir, got %v", err)
	}
}

func TestValidateDiff_AllowedPaths_OutOfDir(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{"pkg/util.go"}, []string{"src/"})
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for file outside allowed dir")
	}
}

func TestValidateDiff_AllowedPaths_ExactFile(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{"README.md"}, []string{"README.md"})
	if err != nil {
		t.Errorf("expected no error for exact file match, got %v", err)
	}
}

func TestValidateDiff_AllowedPaths_WildcardExt(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{"src/main.go", "tests/test.go"}, []string{"*.go"})
	if err != nil {
		t.Errorf("expected no error for wildcard match, got %v", err)
	}

	err = v.ValidateDiff([]string{"src/main.py"}, []string{"*.go"})
	if err == nil || err.Code != "PATH_VIOLATION" {
		t.Error("expected PATH_VIOLATION for .py with *.go allowed")
	}
}

func TestValidateDiff_AllowedPaths_DirAutoDetection(t *testing.T) {
	v := NewValidator(nil, false)

	// "src" without trailing slash should be auto-detected as directory (no dot in basename)
	err := v.ValidateDiff([]string{"src/main.go"}, []string{"src"})
	if err != nil {
		t.Errorf("expected no error for dir auto-detection, got %v", err)
	}
}

func TestValidateDiff_PriorityOrdering(t *testing.T) {
	v := NewValidator(nil, false)

	// .git/ should be caught before CI
	err := v.ValidateDiff([]string{".git/config", ".github/ci.yml"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Code != "GIT_DIR_VIOLATION" {
		t.Errorf("expected GIT_DIR_VIOLATION (highest priority), got %s", err.Code)
	}

	// CI should be caught before secrets
	err = v.ValidateDiff([]string{".github/ci.yml", ".env"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Code != "CI_CONFIG_VIOLATION" {
		t.Errorf("expected CI_CONFIG_VIOLATION before SECRETS_VIOLATION, got %s", err.Code)
	}
}

func TestValidateDiff_EmptyFilesList(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{}, nil)
	if err != nil {
		t.Errorf("expected no error for empty files list, got %v", err)
	}
}

func TestValidateDiff_EmptyAllowedPaths(t *testing.T) {
	v := NewValidator(nil, false)

	// Empty allowed paths means no path restriction
	err := v.ValidateDiff([]string{"any/file.go"}, []string{})
	if err != nil {
		t.Errorf("expected no error for empty allowed paths, got %v", err)
	}
}

func TestValidateDiff_SafeFiles(t *testing.T) {
	v := NewValidator(nil, false)

	safeFiles := []string{
		"main.go",
		"src/handler.go",
		"tests/handler_test.go",
		"README.md",
		"docs/guide.md",
	}

	err := v.ValidateDiff(safeFiles, nil)
	if err != nil {
		t.Errorf("expected no error for safe files, got: %s: %s", err.Code, err.Message)
	}
}

func TestValidateDiff_MultipleViolatingFiles(t *testing.T) {
	v := NewValidator(nil, false)

	err := v.ValidateDiff([]string{".env", ".env.local", "secrets/key.txt"}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	if len(err.Files) != 3 {
		t.Errorf("expected 3 violating files, got %d", len(err.Files))
	}
}
