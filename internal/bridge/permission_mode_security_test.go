package bridge

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func TestPermissionMode_NewGroupStoresDocumentedDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const adminID int64 = 9001

	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: adminID, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := newTestCommandHandler(t, db)
	cwd := t.TempDir()
	reply, err := h.cmdCWD(ctx, makeUpdate(100, nil, 1, "/cwd "+cwd, adminID), nil, cwd)
	if err != nil {
		t.Fatalf("register group: %v", err)
	}
	if !strings.Contains(reply, "Working directory set to") {
		t.Fatalf("register group reply = %q, want success", reply)
	}

	group, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("load new group: %v", err)
	}
	if group == nil {
		t.Fatal("new group was not persisted")
	}
	if group.PermissionMode != defaultPermissionMode {
		t.Fatalf("new group permission mode = %q, want documented default %q", group.PermissionMode, defaultPermissionMode)
	}
	if got, want := resolvePermissionArgs(group), []string{"--dangerously-skip-permissions"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("new group Claude permission args = %v, want %v", got, want)
	}
}

func TestPermissionMode_AdminChangesPersistAcrossRestartAndNonAdminCannotChange(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/bridge.db"
	const (
		chatID     int64 = 100
		adminID    int64 = 9001
		nonAdminID int64 = 200
	)

	db, err := OpenDB(dbPath)
	if err != nil {
		t.Fatalf("open initial database: %v", err)
	}
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: adminID, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: nonAdminID, Role: "user"}); err != nil {
		t.Fatalf("seed non-admin: %v", err)
	}
	group := &Group{
		ChatID:         chatID,
		CWD:            t.TempDir(),
		PermissionMode: defaultPermissionMode,
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	adminUpdate := makeUpdate(chatID, nil, 1, "/permission plan", adminID)
	reply, err := h.cmdPermission(ctx, adminUpdate, group, "plan")
	if err != nil {
		t.Fatalf("admin /permission: %v", err)
	}
	if !strings.Contains(reply, "Permission mode set to: plan") {
		t.Fatalf("admin /permission reply = %q, want success", reply)
	}

	adminUpdate = makeUpdate(chatID, nil, 2, "/config permission_mode dontAsk", adminID)
	reply, err = h.cmdConfig(ctx, adminUpdate, group, "permission_mode dontAsk")
	if err != nil {
		t.Fatalf("admin /config: %v", err)
	}
	if !strings.Contains(reply, "Permission mode set to: dontAsk") {
		t.Fatalf("admin /config reply = %q, want success", reply)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close initial database: %v", err)
	}

	// Re-open the same file to model a bridge restart. The mode must come from
	// durable state rather than an in-memory Group or process default.
	db, err = OpenDB(dbPath)
	if err != nil {
		t.Fatalf("re-open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h = newTestCommandHandler(t, db)
	stored, err := db.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("load group after restart: %v", err)
	}
	if stored.PermissionMode != "dontAsk" {
		t.Fatalf("permission mode after restart = %q, want dontAsk", stored.PermissionMode)
	}

	for _, test := range []struct {
		name   string
		text   string
		args   string
		invoke func(contract.Update, *Group) (string, error)
	}{
		{
			name: "permission command",
			text: "/permission bypassPermissions",
			args: "bypassPermissions",
			invoke: func(update contract.Update, current *Group) (string, error) {
				return h.cmdPermission(ctx, update, current, "bypassPermissions")
			},
		},
		{
			name: "config command",
			text: "/config permission_mode bypassPermissions",
			args: "permission_mode bypassPermissions",
			invoke: func(update contract.Update, current *Group) (string, error) {
				return h.cmdConfig(ctx, update, current, "permission_mode bypassPermissions")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			current, err := db.GetGroup(ctx, chatID)
			if err != nil {
				t.Fatalf("load group before rejected %s: %v", test.args, err)
			}
			reply, err := test.invoke(makeUpdate(chatID, nil, 3, test.text, nonAdminID), current)
			if err != nil {
				t.Fatalf("non-admin %s: %v", test.args, err)
			}
			if !strings.Contains(reply, "Permission denied") {
				t.Fatalf("non-admin %s reply = %q, want denial", test.args, reply)
			}
			if current.PermissionMode != "dontAsk" {
				t.Fatalf("rejected %s changed in-memory mode to %q", test.args, current.PermissionMode)
			}
			persisted, err := db.GetGroup(ctx, chatID)
			if err != nil {
				t.Fatalf("load group after rejected %s: %v", test.args, err)
			}
			if persisted.PermissionMode != "dontAsk" {
				t.Fatalf("rejected %s changed persisted mode to %q", test.args, persisted.PermissionMode)
			}
		})
	}
}

func TestPermissionMode_WorkerClaudeInvocationArgsForEveryMode(t *testing.T) {
	for _, test := range []struct {
		mode string
	}{
		{mode: "bypassPermissions"},
		{mode: "acceptEdits"},
		{mode: "plan"},
		{mode: "dontAsk"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			env := newWorkerPoolErrorEnv(t, 5, 1)
			env.group.PermissionMode = test.mode
			mockTmuxCommandFailure(t, "new-window", "window creation refused", 1)

			workerID, _, err := env.wp.SpawnWorker(context.Background(), 100, 10, 1000, env.group,
				json.RawMessage(`{"prompt":"permission mode regression"}`))
			if err != nil {
				t.Fatalf("SpawnWorker: %v", err)
			}

			waitForTmuxCall(t, env.tmux, "new-window", 3*time.Second)
			call := findWorkerNewWindowCall(t, env.tmux)
			if test.mode == "bypassPermissions" {
				if !strings.Contains(call, "'--dangerously-skip-permissions'") {
					t.Fatalf("%s invocation = %s, want dangerous skip-permissions flag", test.mode, call)
				}
				if strings.Contains(call, "'--permission-mode'") {
					t.Fatalf("%s invocation = %s, unexpectedly has --permission-mode", test.mode, call)
				}
			} else {
				want := "'--permission-mode' '" + test.mode + "'"
				if !strings.Contains(call, want) {
					t.Fatalf("%s invocation = %s, want %s", test.mode, call, want)
				}
				if strings.Contains(call, "'--dangerously-skip-permissions'") {
					t.Fatalf("%s invocation = %s, safer mode received dangerous skip-permissions flag", test.mode, call)
				}
			}

			worker := waitForWorkerTerminalStatus(t, env.db, workerID, 3*time.Second)
			if worker.Status != "failed" || !strings.Contains(worker.Error, "spawn pane") {
				t.Fatalf("worker = status %q error %q, want spawn failure", worker.Status, worker.Error)
			}
			waitForPendingWorkerResults(t, env.sm, 100, 10, 1, 2*time.Second)
			waitForGlobalSlot(t, env.wp, 2*time.Second)
		})
	}
}
