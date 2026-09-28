package bridge

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Errors returned by WorkingDirectoryPolicy.  The exported values let command
// handlers distinguish an invalid path from a missing path without exposing a
// user-supplied path in an error message.
var (
	ErrWorkingDirectoryEmpty        = errors.New("working directory is empty")
	ErrWorkingDirectoryNotAbsolute  = errors.New("working directory must be absolute")
	ErrWorkingDirectoryTraversal    = errors.New("working directory contains traversal")
	ErrWorkingDirectoryDoesNotExist = errors.New("working directory does not exist")
	ErrWorkingDirectoryNotDirectory = errors.New("working directory is not a directory")
	ErrWorkingDirectorySensitive    = errors.New("working directory is sensitive")
	ErrWorkingDirectoryNotAllowed   = errors.New("working directory is outside the allowed roots")
)

// WorkingDirectoryPolicy constrains directories that may be used as Claude's
// working directory. An empty root list is useful for callers that only need
// path validation (for example, unit tests); production must configure one or
// more explicit roots.
type WorkingDirectoryPolicy struct {
	allowedRoots []string
}

// NewWorkingDirectoryPolicy canonicalizes the configured roots and returns a
// policy that only permits directories below those roots. Roots must already
// exist and must themselves be safe workspace directories.
func NewWorkingDirectoryPolicy(allowedRoots ...string) (*WorkingDirectoryPolicy, error) {
	policy := &WorkingDirectoryPolicy{}
	for _, rawRoot := range allowedRoots {
		root := strings.TrimSpace(rawRoot)
		if root == "" {
			return nil, fmt.Errorf("workspace root is empty")
		}
		if !filepath.IsAbs(root) {
			return nil, fmt.Errorf("workspace root must be absolute")
		}
		if hasTraversalComponent(root) {
			return nil, fmt.Errorf("workspace root contains traversal")
		}
		canonical, err := canonicalWorkingDirectory(root)
		if err != nil {
			return nil, fmt.Errorf("workspace root: %w", err)
		}
		if err := validateWorkingDirectoryShape(canonical); err != nil {
			return nil, fmt.Errorf("workspace root: %w", err)
		}
		policy.allowedRoots = appendUniquePath(policy.allowedRoots, canonical)
	}
	return policy, nil
}

// AllowedRoots returns a copy of the policy's canonical roots.
func (p *WorkingDirectoryPolicy) AllowedRoots() []string {
	if p == nil {
		return nil
	}
	return append([]string(nil), p.allowedRoots...)
}

// Resolve validates path, resolves all symlinks, and returns the canonical
// directory that may be handed to a child process. A path containing an
// explicit .. component is rejected even when it would normalize back inside
// an allowed root; this keeps the /cwd contract unambiguous and prevents
// traversal checks from depending on string-cleaning order.
func (p *WorkingDirectoryPolicy) Resolve(path string) (string, error) {
	if p == nil {
		return path, nil
	}
	if path == "" {
		return "", ErrWorkingDirectoryEmpty
	}
	if strings.ContainsRune(path, 0) {
		return "", ErrWorkingDirectoryTraversal
	}
	if !filepath.IsAbs(path) {
		return "", ErrWorkingDirectoryNotAbsolute
	}
	if hasTraversalComponent(path) {
		return "", ErrWorkingDirectoryTraversal
	}

	canonical, err := canonicalWorkingDirectory(path)
	if err != nil {
		return "", err
	}
	if err := validateWorkingDirectoryShape(canonical); err != nil {
		return "", err
	}

	if len(p.allowedRoots) == 0 {
		return canonical, nil
	}
	for _, root := range p.allowedRoots {
		if pathWithin(root, canonical) {
			return canonical, nil
		}
	}
	return "", ErrWorkingDirectoryNotAllowed
}

// canonicalWorkingDirectory verifies that path names an existing directory
// and resolves every symlink in the path before policy checks are applied.
func canonicalWorkingDirectory(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrWorkingDirectoryDoesNotExist
		}
		return "", fmt.Errorf("working directory unavailable: %w", err)
	}
	if !info.IsDir() {
		return "", ErrWorkingDirectoryNotDirectory
	}

	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return "", ErrWorkingDirectoryDoesNotExist
		}
		return "", fmt.Errorf("working directory symlink resolution failed: %w", err)
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", fmt.Errorf("working directory canonicalization failed: %w", err)
	}
	return filepath.Clean(canonical), nil
}

func validateWorkingDirectoryShape(path string) error {
	clean := filepath.Clean(path)
	switch clean {
	case string(filepath.Separator), "/home", "/root", "/etc", "/proc", "/sys", "/dev", "/var", "/boot", "/run":
		return ErrWorkingDirectorySensitive
	}

	for _, component := range pathComponents(clean) {
		lower := strings.ToLower(component)
		switch lower {
		case ".ssh", ".gnupg", ".gpg", ".aws", ".azure", ".config", ".kube", ".docker", ".claude", ".git", ".git-credentials", ".vault-token", ".vault-token-openbao-v2", "credentials", "secrets", "secret", "tokens", "token", "vault", "id_rsa", "id_ed25519":
			return ErrWorkingDirectorySensitive
		}
		if lower == ".env" || strings.HasPrefix(lower, ".env.") {
			return ErrWorkingDirectorySensitive
		}
	}
	return nil
}

func hasTraversalComponent(path string) bool {
	for _, component := range pathComponents(path) {
		if component == ".." {
			return true
		}
	}
	return false
}

func pathComponents(path string) []string {
	return strings.FieldsFunc(path, func(r rune) bool {
		return r == '/' || r == '\\'
	})
}

func pathWithin(root, candidate string) bool {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func appendUniquePath(paths []string, path string) []string {
	for _, existing := range paths {
		if existing == path {
			return paths
		}
	}
	return append(paths, path)
}
