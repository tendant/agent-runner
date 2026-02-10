package executor

import (
	"path/filepath"
	"strings"
)

// ValidationError represents a diff validation error
type ValidationError struct {
	Code    string
	Message string
	Files   []string
}

// Validator handles diff validation against allowlists
type Validator struct {
	BlockedPaths     []string
	BlockBinaryFiles bool
}

// NewValidator creates a new diff validator
func NewValidator(blockedPaths []string, blockBinaryFiles bool) *Validator {
	return &Validator{
		BlockedPaths:     blockedPaths,
		BlockBinaryFiles: blockBinaryFiles,
	}
}

// ValidateDiff checks changed files against the paths allowlist and blocked paths
func (v *Validator) ValidateDiff(changedFiles []string, allowedPaths []string) *ValidationError {
	// Check for .git/ directory modifications
	var gitDirViolations []string
	for _, file := range changedFiles {
		if strings.HasPrefix(file, ".git/") || file == ".git" {
			gitDirViolations = append(gitDirViolations, file)
		}
	}
	if len(gitDirViolations) > 0 {
		return &ValidationError{
			Code:    "GIT_DIR_VIOLATION",
			Message: "Attempted to modify .git/ directory",
			Files:   gitDirViolations,
		}
	}

	// Check for CI config violations
	var ciViolations []string
	for _, file := range changedFiles {
		if v.isCIConfig(file) {
			ciViolations = append(ciViolations, file)
		}
	}
	if len(ciViolations) > 0 {
		return &ValidationError{
			Code:    "CI_CONFIG_VIOLATION",
			Message: "Attempted to modify CI configuration",
			Files:   ciViolations,
		}
	}

	// Check for Git hooks modifications
	var hooksViolations []string
	for _, file := range changedFiles {
		if strings.Contains(file, ".git/hooks/") || strings.HasPrefix(file, ".husky/") {
			hooksViolations = append(hooksViolations, file)
		}
	}
	if len(hooksViolations) > 0 {
		return &ValidationError{
			Code:    "HOOKS_VIOLATION",
			Message: "Attempted to modify Git hooks",
			Files:   hooksViolations,
		}
	}

	// Check for secrets patterns
	var secretsViolations []string
	for _, file := range changedFiles {
		if v.isSecretFile(file) {
			secretsViolations = append(secretsViolations, file)
		}
	}
	if len(secretsViolations) > 0 {
		return &ValidationError{
			Code:    "SECRETS_VIOLATION",
			Message: "Secrets patterns detected in changes",
			Files:   secretsViolations,
		}
	}

	// Check for blocked paths
	var blockedViolations []string
	for _, file := range changedFiles {
		if v.isBlockedPath(file) {
			blockedViolations = append(blockedViolations, file)
		}
	}
	if len(blockedViolations) > 0 {
		return &ValidationError{
			Code:    "PATH_VIOLATION",
			Message: "Changes to blocked paths detected",
			Files:   blockedViolations,
		}
	}

	// Check against allowed paths
	// Empty allowedPaths or a single "*" entry means all paths are allowed
	if len(allowedPaths) > 0 && !(len(allowedPaths) == 1 && allowedPaths[0] == "*") {
		var pathViolations []string
		for _, file := range changedFiles {
			if !v.isPathAllowed(file, allowedPaths) {
				pathViolations = append(pathViolations, file)
			}
		}
		if len(pathViolations) > 0 {
			return &ValidationError{
				Code:    "PATH_VIOLATION",
				Message: "Files outside allowed paths",
				Files:   pathViolations,
			}
		}
	}

	return nil
}

// isCIConfig checks if a file is a CI configuration file
func (v *Validator) isCIConfig(file string) bool {
	ciPatterns := []string{
		".github/",
		".gitlab-ci.yml",
		".gitlab-ci.yaml",
		".travis.yml",
		".circleci/",
		"Jenkinsfile",
		"azure-pipelines.yml",
		".drone.yml",
	}

	for _, pattern := range ciPatterns {
		if strings.HasPrefix(file, pattern) || file == pattern {
			return true
		}
	}
	return false
}

// isSecretFile checks if a file matches common secrets patterns
func (v *Validator) isSecretFile(file string) bool {
	secretPatterns := []string{
		".env",
		".env.local",
		".env.production",
		".env.development",
		"secrets/",
		"credentials.json",
		"credentials.yaml",
		"service-account.json",
		".aws/credentials",
		".ssh/",
		"id_rsa",
		"id_ed25519",
		"*.pem",
		"*.key",
	}

	baseName := filepath.Base(file)
	for _, pattern := range secretPatterns {
		if strings.HasPrefix(pattern, "*.") {
			// Wildcard pattern
			ext := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(file, ext) {
				return true
			}
		} else if strings.HasSuffix(pattern, "/") {
			// Directory pattern
			if strings.HasPrefix(file, pattern) {
				return true
			}
		} else {
			// Exact match or basename match
			if file == pattern || baseName == pattern {
				return true
			}
		}
	}
	return false
}

// isBlockedPath checks if a file matches any blocked path patterns
func (v *Validator) isBlockedPath(file string) bool {
	for _, pattern := range v.BlockedPaths {
		if strings.HasSuffix(pattern, "/") {
			// Directory pattern
			if strings.HasPrefix(file, pattern) {
				return true
			}
		} else if strings.HasPrefix(pattern, "*.") {
			// Wildcard extension pattern
			ext := strings.TrimPrefix(pattern, "*")
			if strings.HasSuffix(file, ext) {
				return true
			}
		} else {
			// Exact match
			if file == pattern {
				return true
			}
		}
	}
	return false
}

// isPathAllowed checks if a file is within the allowed paths
func (v *Validator) isPathAllowed(file string, allowedPaths []string) bool {
	for _, allowed := range allowedPaths {
		// Normalize: ensure directories end with /
		allowedPath := allowed
		if !strings.HasSuffix(allowedPath, "/") && !strings.Contains(filepath.Base(allowedPath), ".") {
			// Looks like a directory, add trailing slash
			allowedPath += "/"
		}

		if strings.HasSuffix(allowedPath, "/") {
			// Directory allowlist: file must be under this directory
			if strings.HasPrefix(file, allowedPath) || strings.HasPrefix(file+"/", allowedPath) {
				return true
			}
			// Also allow exact directory match without trailing slash
			if strings.TrimSuffix(allowedPath, "/") == file {
				return true
			}
		} else {
			// File pattern or exact match
			if strings.HasPrefix(allowedPath, "*.") {
				// Wildcard pattern
				ext := strings.TrimPrefix(allowedPath, "*")
				if strings.HasSuffix(file, ext) {
					return true
				}
			} else {
				// Exact file match
				if file == allowedPath {
					return true
				}
			}
		}
	}
	return false
}
