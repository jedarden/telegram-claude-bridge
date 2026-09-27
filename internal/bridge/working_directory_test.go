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

	traversal := filepath.Join(root, "child") + string(filepath.Separator) + ".." + string(filepath.Separator) + "child"
	_, err = policy.Resolve(traversal)
	if !errors.Is(err, ErrWorkingDirectoryTraversal) {
		t.Fatalf("Resolve traversal error = %v, want %v", err, ErrWorkingDirectoryTraversal)
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
	sensitive := filepath.Join(root, ".ssh")
	if err := os.Mkdir(sensitive, 0o700); err != nil {
		t.Fatalf("mkdir sensitive directory: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	_, err = policy.Resolve(sensitive)
	if !errors.Is(err, ErrWorkingDirectorySensitive) {
		t.Fatalf("Resolve sensitive directory error = %v, want %v", err, ErrWorkingDirectorySensitive)
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
