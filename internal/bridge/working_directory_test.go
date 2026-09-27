package bridge

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkingDirectoryPolicyRejectsTraversal(t *testing.T) {
	root := t.TempDir()
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}

	traversals := []struct {
		name string
		path string
	}{
		{name: "unix separators", path: root + string(filepath.Separator) + "child" + string(filepath.Separator) + ".." + string(filepath.Separator) + "child"},
		{name: "backslash separators", path: root + `\..\child`},
		{name: "nul byte", path: root + "\x00child"},
	}
	for _, traversal := range traversals {
		t.Run(traversal.name, func(t *testing.T) {
			_, err := policy.Resolve(traversal.path)
			if !errors.Is(err, ErrWorkingDirectoryTraversal) {
				t.Fatalf("Resolve(%q) error = %v, want %v", traversal.path, err, ErrWorkingDirectoryTraversal)
			}
		})
	}
}

func TestWorkingDirectoryPolicyEnforcesAllowedRoots(t *testing.T) {
	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := t.TempDir()
	if err := os.Mkdir(allowed, 0o755); err != nil {
		t.Fatalf("mkdir allowed directory: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	if got, err := policy.Resolve(allowed); err != nil || got != allowed {
		t.Fatalf("Resolve allowed directory = (%q, %v), want (%q, nil)", got, err, allowed)
	}
	if _, err := policy.Resolve(outside); !errors.Is(err, ErrWorkingDirectoryNotAllowed) {
		t.Fatalf("Resolve outside directory error = %v, want %v", err, ErrWorkingDirectoryNotAllowed)
	}
}

func TestWorkingDirectoryPolicyCanonicalizesConfiguredRoot(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	project := filepath.Join(root, "project")
	rootAlias := filepath.Join(parent, "workspace-alias")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Fatalf("symlink root: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(rootAlias)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	if got := policy.AllowedRoots(); len(got) != 1 || got[0] != root {
		t.Fatalf("AllowedRoots = %#v, want [%q]", got, root)
	}
	got, err := policy.Resolve(filepath.Join(rootAlias, "project"))
	if err != nil {
		t.Fatalf("Resolve through root symlink: %v", err)
	}
	if got != project {
		t.Fatalf("Resolve through root symlink = %q, want %q", got, project)
	}
}

func TestWorkingDirectoryPolicyCanonicalizesSafeSymlink(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "project")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	got, err := policy.Resolve(alias)
	if err != nil {
		t.Fatalf("Resolve safe symlink: %v", err)
	}
	if got != target {
		t.Fatalf("Resolve safe symlink = %q, want canonical target %q", got, target)
	}
}

func TestWorkingDirectoryPolicyRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(root, "outside")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	_, err = policy.Resolve(alias)
	if !errors.Is(err, ErrWorkingDirectoryNotAllowed) {
		t.Fatalf("Resolve symlink escape error = %v, want %v", err, ErrWorkingDirectoryNotAllowed)
	}
}

func TestWorkingDirectoryPolicyRejectsSensitiveDirectory(t *testing.T) {
	root := t.TempDir()
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}

	sensitiveNames := []string{
		".ssh", ".gnupg", ".gpg", ".aws", ".azure", ".config", ".kube", ".docker", ".claude", ".git",
		".git-credentials", "credentials", "secrets", "secret", "tokens", "token", "vault",
		"id_rsa", "id_ed25519", ".env", ".env.local",
	}
	for _, name := range sensitiveNames {
		t.Run(name, func(t *testing.T) {
			sensitive := filepath.Join(root, name)
			if err := os.Mkdir(sensitive, 0o700); err != nil {
				t.Fatalf("mkdir sensitive directory: %v", err)
			}
			_, err := policy.Resolve(sensitive)
			if !errors.Is(err, ErrWorkingDirectorySensitive) {
				t.Fatalf("Resolve sensitive directory error = %v, want %v", err, ErrWorkingDirectorySensitive)
			}
		})
	}
}

func TestWorkingDirectoryPolicyRejectsInvalidPaths(t *testing.T) {
	root := t.TempDir()
	regularFile := filepath.Join(root, "file")
	if err := os.WriteFile(regularFile, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write regular file: %v", err)
	}
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}

	tests := []struct {
		name string
		path string
		want error
	}{
		{name: "empty", path: "", want: ErrWorkingDirectoryEmpty},
		{name: "relative", path: "relative/path", want: ErrWorkingDirectoryNotAbsolute},
		{name: "missing", path: filepath.Join(root, "missing"), want: ErrWorkingDirectoryDoesNotExist},
		{name: "regular file", path: regularFile, want: ErrWorkingDirectoryNotDirectory},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := policy.Resolve(test.path)
			if !errors.Is(err, test.want) {
				t.Fatalf("Resolve(%q) error = %v, want %v", test.path, err, test.want)
			}
		})
	}
}

func TestNewWorkingDirectoryPolicyRejectsInvalidRoots(t *testing.T) {
	parent := t.TempDir()
	sensitive := filepath.Join(parent, ".ssh")
	if err := os.Mkdir(sensitive, 0o700); err != nil {
		t.Fatalf("mkdir sensitive root: %v", err)
	}

	tests := []struct {
		name string
		root string
		want error
	}{
		{name: "empty", root: "", want: nil},
		{name: "relative", root: "relative/path", want: nil},
		{name: "traversal", root: parent + string(filepath.Separator) + ".." + string(filepath.Separator) + filepath.Base(parent), want: nil},
		{name: "missing", root: filepath.Join(parent, "missing"), want: ErrWorkingDirectoryDoesNotExist},
		{name: "sensitive", root: sensitive, want: ErrWorkingDirectorySensitive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := NewWorkingDirectoryPolicy(test.root)
			if err == nil {
				t.Fatal("NewWorkingDirectoryPolicy succeeded, want an error")
			}
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("NewWorkingDirectoryPolicy error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestPTYManagerRejectsWorkingDirectoryBeforeSpawn(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(root, "escape")
	if err := os.Symlink(outside, alias); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}

	_, err = NewPTYManager(policy).SpawnPane("unsafe", alias, nil)
	if !errors.Is(err, ErrWorkingDirectoryNotAllowed) {
		t.Fatalf("SpawnPane error = %v, want %v", err, ErrWorkingDirectoryNotAllowed)
	}
}
