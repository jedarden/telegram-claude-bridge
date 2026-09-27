package bridge

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func TestRouter_AllowedChatIDDropsEveryUpdateTypeFromOtherChats(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	r := NewRouter(db, nil, 100)

	var calls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { calls++ }
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { calls++ }
	r.OnService = func(context.Context, contract.Update) { calls++ }
	r.OnCallback = func(context.Context, contract.Update) { calls++ }

	updates := []contract.Update{
		textUpdate(1, 999, nil, "/help", true),
		textUpdate(1, 999, int64Ptr(5), "hello", false),
		serviceUpdate(1, 999, int64Ptr(5), contract.ServiceTypeForumTopicCreated),
		callbackUpdate(1, 999),
	}
	for _, update := range updates {
		r.Route(context.Background(), update)
	}

	if calls != 0 {
		t.Fatalf("handlers called %d times for updates from a blocked chat", calls)
	}
}

func TestRouter_AllowedChatIDStillRoutesConfiguredChat(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	r := NewRouter(db, nil, 100)

	called := false
	r.OnCommand = func(context.Context, contract.Update, *Group) { called = true }
	r.Route(context.Background(), textUpdate(1, 100, nil, "/help", true))

	if !called {
		t.Fatal("expected an update from the configured chat to route")
	}
}

func TestRouter_AdminUserIDCanRouteWithoutAllowListRow(t *testing.T) {
	db := openTestDB(t)
	r := NewRouter(db, nil, 100)
	r.SetAdminUserID(9001)

	called := false
	r.OnCommand = func(context.Context, contract.Update, *Group) { called = true }
	r.Route(context.Background(), textUpdate(9001, 100, nil, "/help", true))

	if !called {
		t.Fatal("configured ADMIN_USER_ID should be authorized even without a database row")
	}
}

func TestRouter_AdminUserIDStillRespectsAllowedChatID(t *testing.T) {
	db := openTestDB(t)
	r := NewRouter(db, nil, 100)
	r.SetAdminUserID(9001)

	called := false
	r.OnCommand = func(context.Context, contract.Update, *Group) { called = true }
	r.Route(context.Background(), textUpdate(9001, 999, nil, "/help", true))

	if called {
		t.Fatal("configured ADMIN_USER_ID must not bypass ALLOWED_CHAT_ID")
	}
}

func TestCommandHandler_AdminUserIDCanChangeConfiguration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	group := &Group{
		ChatID:         100,
		CWD:            t.TempDir(),
		PermissionMode: defaultPermissionMode,
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	h.SetAdminUserID(9001)
	reply, err := h.cmdConfig(ctx, makeUpdate(100, nil, 1, "/config permission_mode plan", 9001), group, "permission_mode plan")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}
	if !strings.Contains(reply, "Permission mode set to: plan") {
		t.Fatalf("configured admin was denied: %q", reply)
	}
	if group.PermissionMode != "plan" {
		t.Fatalf("permission mode = %q, want plan", group.PermissionMode)
	}
}

func TestUntrustedUserCannotChangeBypassPermissionsDefault(t *testing.T) {
	for _, command := range []string{"/permission plan", "/config permission_mode plan"} {
		t.Run(command, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user"}); err != nil {
				t.Fatalf("upsert user: %v", err)
			}
			group := &Group{
				ChatID:         100,
				CWD:            t.TempDir(),
				PermissionMode: defaultPermissionMode,
				CreatedAt:      time.Now().UTC(),
			}
			if err := db.UpsertGroup(ctx, group); err != nil {
				t.Fatalf("upsert group: %v", err)
			}

			h := newTestCommandHandler(t, db)
			update := makeUpdate(100, nil, 1, command, 200)
			var reply string
			var err error
			if strings.HasPrefix(command, "/permission") {
				reply, err = h.cmdPermission(ctx, update, group, "plan")
			} else {
				reply, err = h.cmdConfig(ctx, update, group, "permission_mode plan")
			}
			if err != nil {
				t.Fatalf("authorization check: %v", err)
			}
			if !strings.Contains(reply, "Permission denied") {
				t.Fatalf("untrusted user reply = %q, want denial", reply)
			}

			stored, err := db.GetGroup(ctx, 100)
			if err != nil {
				t.Fatalf("get group: %v", err)
			}
			if stored.PermissionMode != defaultPermissionMode {
				t.Fatalf("permission mode = %q, want protected default %q", stored.PermissionMode, defaultPermissionMode)
			}
			if got, want := resolvePermissionArgs(stored), []string{"--dangerously-skip-permissions"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("permission args = %v, want %v", got, want)
			}
		})
	}
}

func TestCommandHandler_NonAdminCannotChangeModel(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	threadID := int64(10)
	if err := db.CreateSession(ctx, &Session{
		ChatID: 100, ThreadID: threadID, SessionID: "session-10", Model: "claude-sonnet-4-6",
		CWD: group.CWD, Status: "active", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdModel(ctx, makeUpdate(100, &threadID, 1, "/model opus", 200), group, "opus")
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}
	if !strings.Contains(reply, "Permission denied") {
		t.Fatalf("non-admin model change reply = %q, want denial", reply)
	}

	stored, err := db.GetSession(ctx, 100, threadID)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if stored.Model != "claude-sonnet-4-6" {
		t.Fatalf("model = %q, want unchanged sonnet", stored.Model)
	}
}

func TestCommandHandler_NonAdminCannotCreateSession(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user"}); err != nil {
		t.Fatalf("upsert user: %v", err)
	}
	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdNew(ctx, makeUpdate(100, nil, 1, "/new restricted", 200), group, "restricted")
	if err != nil {
		t.Fatalf("cmdNew: %v", err)
	}
	if !strings.Contains(reply, "Permission denied") {
		t.Fatalf("non-admin session creation reply = %q, want denial", reply)
	}
}
