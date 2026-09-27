package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkingDirectoryPolicyEnforcesMultipleWorkspaceRoots(t *testing.T) {
	parent := t.TempDir()
	rootA := filepath.Join(parent, "workspace-a")
	rootB := filepath.Join(parent, "workspace-b")
	project := filepath.Join(rootB, "project")
	outside := t.TempDir()
	if err := os.MkdirAll(rootA, 0o755); err != nil {
		t.Fatalf("mkdir first workspace root: %v", err)
	}
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir second workspace project: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(rootA, rootB)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}

	if got := policy.AllowedRoots(); len(got) != 2 || got[0] != rootA || got[1] != rootB {
		t.Fatalf("AllowedRoots = %#v, want [%q %q]", got, rootA, rootB)
	}
	got, err := policy.Resolve(filepath.Join(project, "."))
	if err != nil {
		t.Fatalf("Resolve second workspace root: %v", err)
	}
	if got != project {
		t.Fatalf("Resolve second workspace root = %q, want canonical %q", got, project)
	}
	if _, err := policy.Resolve(outside); !errors.Is(err, ErrWorkingDirectoryNotAllowed) {
		t.Fatalf("Resolve outside configured roots error = %v, want %v", err, ErrWorkingDirectoryNotAllowed)
	}
}

func TestCmdCWDRejectsUnsafePathsWithoutRegisteringGroup(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	project := filepath.Join(root, "project")
	sensitive := filepath.Join(project, ".config")
	outside := t.TempDir()
	escape := filepath.Join(root, "escape")
	if err := os.MkdirAll(sensitive, 0o700); err != nil {
		t.Fatalf("mkdir sensitive directory: %v", err)
	}
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatalf("symlink escape: %v", err)
	}

	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	h := newTestCommandHandler(t, db)
	h.SetWorkingDirectoryPolicy(policy)

	tests := []struct {
		name string
		path string
		want error
	}{
		{name: "traversal", path: root + string(filepath.Separator) + "project" + string(filepath.Separator) + ".." + string(filepath.Separator) + "project", want: ErrWorkingDirectoryTraversal},
		{name: "outside root", path: outside, want: ErrWorkingDirectoryNotAllowed},
		{name: "symlink escape", path: escape, want: ErrWorkingDirectoryNotAllowed},
		{name: "sensitive configuration directory", path: sensitive, want: ErrWorkingDirectorySensitive},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			update := makeUpdate(100, nil, 1, "/cwd "+test.path, 9001)
			if _, err := h.cmdCWD(ctx, update, nil, test.path); !errors.Is(err, test.want) {
				t.Fatalf("cmdCWD(%q) error = %v, want %v", test.path, err, test.want)
			}
			group, err := db.GetGroup(ctx, 100)
			if err != nil {
				t.Fatalf("get rejected group: %v", err)
			}
			if group != nil {
				t.Fatalf("rejected /cwd registered group: %+v", group)
			}
		})
	}
}

func TestRouterCWDRegistrationRequiresAdminAndStoresCanonicalPath(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "workspace")
	project := filepath.Join(root, "project")
	alias := filepath.Join(root, "project-alias")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatalf("mkdir project: %v", err)
	}
	if err := os.Symlink(project, alias); err != nil {
		t.Fatalf("symlink project: %v", err)
	}

	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user"}); err != nil {
		t.Fatalf("seed non-admin: %v", err)
	}
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	sender, _ := newRecordingProxy(t)
	h := NewCommandHandler(db, sender, "http://unused", nil, nil, "v1.0.0", "test", "test")
	h.SetWorkingDirectoryPolicy(policy)
	r := NewRouter(db, nil)
	r.SetAuthorizer(NewAuthorizer(db, 0, 0))
	r.OnCommand = h.Handle

	// A regular allow-listed user may reach the router, but cannot create the
	// first group through the privileged /cwd mutation.
	r.Route(ctx, commandTextUpdate(200, 200, 1, "/cwd "+project))
	group, err := db.GetGroup(ctx, 200)
	if err != nil {
		t.Fatalf("get non-admin group: %v", err)
	}
	if group != nil {
		t.Fatalf("non-admin user registered a group: %+v", group)
	}

	r.Route(ctx, commandTextUpdate(9001, 100, 2, "/cwd "+alias))
	group, err = db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get admin group: %v", err)
	}
	if group == nil {
		t.Fatal("administrator was not allowed to register a group")
	}
	if group.CWD != project {
		t.Fatalf("registered CWD = %q, want canonical %q", group.CWD, project)
	}
}
