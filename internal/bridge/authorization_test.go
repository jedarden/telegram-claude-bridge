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
	edited := textUpdate(1, 999, int64Ptr(5), "edited", false)
	edited.Type = "edited_message"
	updates = append(updates, edited)
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

func TestAuthorizer_BlockedChatIsRejectedBeforeUserLookup(t *testing.T) {
	// A blocked chat must not reach the user database at all. Keeping the DB
	// nil makes a user lookup an immediate test failure while still allowing
	// the chat gate itself to be exercised.
	authorizer := NewAuthorizer(nil, 100, 9001)

	allowed, err := authorizer.CanReceiveUpdate(context.Background(), commandTextUpdate(300, 999, 1, "/config"))
	if err != nil {
		t.Fatalf("blocked chat authorization: %v", err)
	}
	if allowed {
		t.Fatal("update from a blocked chat was authorized")
	}
}

func TestRouter_AllowedChatIDRequiresAnExactMatch(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 200)
	r := NewRouter(db, nil, 100)

	var calls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { calls++ }

	for _, chatID := range []int64{99, 101} {
		r.Route(context.Background(), commandTextUpdate(200, chatID, 1, "/help"))
	}
	if calls != 0 {
		t.Fatalf("commands from adjacent chats reached a handler %d times", calls)
	}

	r.Route(context.Background(), commandTextUpdate(200, 100, 1, "/help"))
	if calls != 1 {
		t.Fatalf("command from the configured chat reached handler %d times, want 1", calls)
	}
}

func TestRouter_BootstrapAdministratorReachesEveryHandlerType(t *testing.T) {
	db := openTestDB(t)
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)

	r := NewRouter(db, nil, 100)
	r.SetAdminUserID(9001)

	var commandCalls, sessionCalls, serviceCalls, callbackCalls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { commandCalls++ }
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { sessionCalls++ }
	r.OnService = func(context.Context, contract.Update) { serviceCalls++ }
	r.OnCallback = func(context.Context, contract.Update) { callbackCalls++ }

	threadID := int64(5)
	updates := []contract.Update{
		commandTextUpdate(9001, 100, 1, "/help"),
		textUpdate(9001, 100, &threadID, "hello", false),
		serviceUpdate(9001, 100, &threadID, contract.ServiceTypeForumTopicCreated),
		callbackUpdate(9001, 100),
	}
	for _, update := range updates {
		r.Route(context.Background(), update)
	}

	if commandCalls != 1 || sessionCalls != 1 || serviceCalls != 1 || callbackCalls != 1 {
		t.Fatalf("bootstrap handler calls = command:%d session:%d service:%d callback:%d, want one each", commandCalls, sessionCalls, serviceCalls, callbackCalls)
	}
}

func TestRouter_RejectedCommandsDoNotDiscloseConfiguration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("seed administrator: %v", err)
	}
	if err := db.UpsertGroup(ctx, &Group{
		ChatID:         100,
		CWD:            "/sensitive/project",
		DefaultModel:   "claude-opus-4-6",
		PermissionMode: "dontAsk",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed group: %v", err)
	}

	sender, recorder := newRecordingProxy(t)
	h := NewCommandHandler(db, sender, "http://unused", nil, nil, "v1.0.0", "test", "test")
	authorizer := NewAuthorizer(db, 100, 0)
	r := NewRouter(db, nil, 100)
	r.SetAuthorizer(authorizer)
	r.OnCommand = func(ctx context.Context, update contract.Update, group *Group) {
		h.Handle(ctx, update, group)
	}

	// /config would expose the configured working directory and model if it
	// reached the command handler. Test both an unknown user in the configured
	// chat and an allow-listed user in a different chat.
	for _, update := range []contract.Update{
		commandTextUpdate(300, 100, 1, "/config"),
		commandTextUpdate(9001, 999, 2, "/config"),
	} {
		r.Route(ctx, update)
	}

	if sends := recorder.all(); len(sends) != 0 {
		t.Fatalf("rejected configuration commands produced proxy responses: %+v", sends)
	}
}

func TestAuthorizer_NoBootstrapRequiresDatabaseAdministrator(t *testing.T) {
	db := openTestDB(t)
	authorizer := NewAuthorizer(db, 100, 0)
	ctx := context.Background()

	allowed, err := authorizer.CanReceiveUpdate(ctx, commandTextUpdate(9001, 100, 1, "/help"))
	if err != nil {
		t.Fatalf("unknown user authorization: %v", err)
	}
	if allowed {
		t.Fatal("ADMIN_USER_ID=0 must not authorize an unknown user")
	}

	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 9001, Role: "user"}); err != nil {
		t.Fatalf("seed non-admin user: %v", err)
	}
	isAdmin, err := authorizer.IsAdmin(ctx, 9001)
	if err != nil {
		t.Fatalf("check non-admin role: %v", err)
	}
	if isAdmin {
		t.Fatal("an allowlisted user role must not become an administrator without a bootstrap identity")
	}

	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("promote database user: %v", err)
	}
	isAdmin, err = authorizer.IsAdmin(ctx, 9001)
	if err != nil {
		t.Fatalf("check database admin role: %v", err)
	}
	if !isAdmin {
		t.Fatal("a database role=admin row must authorize an administrator with no bootstrap ID")
	}
}

func TestSeededAdministratorCanRegisterGroupWhenBootstrapDisabled(t *testing.T) {
	const (
		adminUser    = int64(9001)
		nonAdminUser = int64(200)
		adminChat    = int64(100)
		nonAdminChat = int64(200)
	)

	db := openTestDB(t)
	ctx := context.Background()

	// This is the database state produced by manage-admins.sh bootstrap. With
	// ADMIN_USER_ID=0, the runtime must use this row rather than an environment
	// identity to authorize the first group registration.
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: adminUser, Role: "admin"}); err != nil {
		t.Fatalf("seed administrator: %v", err)
	}
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: nonAdminUser, Role: "user"}); err != nil {
		t.Fatalf("seed non-admin user: %v", err)
	}

	root := t.TempDir()
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	sender, _ := newRecordingProxy(t)
	h := NewCommandHandler(db, sender, "http://unused", nil, nil, "v1.0.0", "test", "test")
	h.SetWorkingDirectoryPolicy(policy)

	// A zero admin ID is the important part of this fixture: only the seeded
	// database role should authorize the command.
	authorizer := NewAuthorizer(db, 0, 0)
	r := NewRouter(db, nil)
	r.SetAuthorizer(authorizer)
	r.OnCommand = h.Handle

	r.Route(ctx, commandTextUpdate(adminUser, adminChat, 1, "/cwd "+root))
	group, err := db.GetGroup(ctx, adminChat)
	if err != nil {
		t.Fatalf("get administrator group: %v", err)
	}
	if group == nil {
		t.Fatal("seeded administrator was not allowed to register the first group")
	}
	if group.CWD != root {
		t.Fatalf("administrator group cwd = %q, want %q", group.CWD, root)
	}

	r.Route(ctx, commandTextUpdate(nonAdminUser, nonAdminChat, 2, "/cwd "+root))
	group, err = db.GetGroup(ctx, nonAdminChat)
	if err != nil {
		t.Fatalf("get non-admin group: %v", err)
	}
	if group != nil {
		t.Fatalf("non-admin user registered a group: %+v", group)
	}
}

func TestCommandHandler_NoBootstrapRejectsUnauthorizedAdminMutation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user"}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdAddUser(ctx, commandTextUpdate(200, 100, 1, "/adduser 300 admin"), "300 admin")
	if err != nil {
		t.Fatalf("cmdAddUser: %v", err)
	}
	if !strings.Contains(reply, "Permission denied") {
		t.Fatalf("unauthorized admin mutation reply = %q, want generic denial", reply)
	}
	created, err := db.GetAllowedUser(ctx, 300)
	if err != nil {
		t.Fatalf("look up unauthorized target: %v", err)
	}
	if created != nil {
		t.Fatalf("unauthorized user created allowlist entry: %+v", created)
	}
}

func TestCommandHandler_AdminRoleChangesCannotRemoveLastAdministrator(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 42, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	h := newTestCommandHandler(t, db)
	h.SetAdminUserID(9001)
	removeReply, err := h.cmdRemoveUser(ctx, makeUpdate(100, nil, 1, "/removeuser 42", 9001), "42")
	if err != nil {
		t.Fatalf("cmdRemoveUser: %v", err)
	}
	if !strings.Contains(removeReply, "last administrator") {
		t.Fatalf("last admin removal reply = %q, want protection", removeReply)
	}

	demoteReply, err := h.cmdAddUser(ctx, makeUpdate(100, nil, 1, "/adduser 42 user", 9001), "42 user")
	if err != nil {
		t.Fatalf("cmdAddUser demotion: %v", err)
	}
	if !strings.Contains(demoteReply, "last administrator") {
		t.Fatalf("last admin demotion reply = %q, want protection", demoteReply)
	}

	admin, err := db.GetAllowedUser(ctx, 42)
	if err != nil {
		t.Fatalf("look up protected admin: %v", err)
	}
	if admin == nil || admin.Role != "admin" {
		t.Fatalf("last admin row after rejected mutations = %+v, want role=admin", admin)
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

func TestCommandHandler_NonAdminCannotChangeWorkspace(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	const userID = int64(200)
	seedUser(t, db, userID)

	originalCWD := t.TempDir()
	newCWD := t.TempDir()
	group := &Group{ChatID: 100, CWD: originalCWD, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdCWD(ctx, makeUpdate(100, nil, 1, "/cwd "+newCWD, userID), group, newCWD)
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}
	if !strings.Contains(reply, "Permission denied") {
		t.Fatalf("non-admin workspace change reply = %q, want denial", reply)
	}
	if group.CWD != originalCWD {
		t.Fatalf("in-memory workspace = %q, want unchanged %q", group.CWD, originalCWD)
	}

	stored, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if stored.CWD != originalCWD {
		t.Fatalf("stored workspace = %q, want unchanged %q", stored.CWD, originalCWD)
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

func TestRouter_AuthorizationBoundary(t *testing.T) {
	const (
		configuredChat = int64(100)
		otherChat      = int64(999)
		adminUser      = int64(9001)
		allowedUser    = int64(200)
		unknownUser    = int64(300)
	)

	tests := []struct {
		name          string
		allowedChatID int64
		chatID        int64
		userID        int64
		allowlisted   bool
		admin         bool
		wantHandler   bool
	}{
		{
			name:          "configured chat mismatch is silently dropped even for admin",
			allowedChatID: configuredChat,
			chatID:        otherChat,
			userID:        adminUser,
			admin:         true,
		},
		{
			name:          "zero allowed chat accepts allowlisted user from any chat",
			allowedChatID: 0,
			chatID:        otherChat,
			userID:        allowedUser,
			allowlisted:   true,
			wantHandler:   true,
		},
		{
			name:          "non-allowlisted user is silently dropped",
			allowedChatID: configuredChat,
			chatID:        configuredChat,
			userID:        unknownUser,
		},
		{
			name:          "bootstrap admin is authorized in configured chat",
			allowedChatID: configuredChat,
			chatID:        configuredChat,
			userID:        adminUser,
			admin:         true,
			wantHandler:   true,
		},
		{
			name:          "allowlisted user reaches command handler",
			allowedChatID: configuredChat,
			chatID:        configuredChat,
			userID:        allowedUser,
			allowlisted:   true,
			wantHandler:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.allowlisted {
				seedUser(t, db, tc.userID)
			}

			sender, recorder := newRecordingProxy(t)
			h := NewCommandHandler(db, sender, "http://unused", nil, nil, "v1.0.0", "test", "test")
			authorizer := NewAuthorizer(db, tc.allowedChatID, 0)
			if tc.admin {
				authorizer.SetAdminUserID(tc.userID)
			}

			r := NewRouter(db, nil, tc.allowedChatID)
			r.SetAuthorizer(authorizer)
			var handlerCalls int
			r.OnCommand = func(ctx context.Context, update contract.Update, group *Group) {
				handlerCalls++
				h.Handle(ctx, update, group)
			}

			r.Route(context.Background(), commandTextUpdate(tc.userID, tc.chatID, 1, "/help"))

			if got := handlerCalls > 0; got != tc.wantHandler {
				t.Fatalf("handler called = %t, want %t", got, tc.wantHandler)
			}
			if sends := recorder.all(); tc.wantHandler && len(sends) != 1 {
				t.Fatalf("proxy sends = %d, want one handler reply: %+v", len(sends), sends)
			} else if !tc.wantHandler && len(sends) != 0 {
				t.Fatalf("unauthorized update produced proxy sends, want silent drop: %+v", sends)
			}
		})
	}
}

func TestRouter_AuthorizedUpdateTypesReachHandlers(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 200)
	if err := db.UpsertAllowedUser(context.Background(), &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)

	r := NewRouter(db, nil, 100)
	var messageCalls, callbackCalls, serviceCalls int
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { messageCalls++ }
	r.OnCallback = func(context.Context, contract.Update) { callbackCalls++ }
	r.OnService = func(context.Context, contract.Update) { serviceCalls++ }

	message := textUpdate(200, 100, int64Ptr(5), "hello", false)
	edited := message
	edited.Type = "edited_message"
	r.Route(context.Background(), message)
	r.Route(context.Background(), edited)
	r.Route(context.Background(), callbackUpdate(9001, 100))
	r.Route(context.Background(), serviceUpdate(200, 100, int64Ptr(5), contract.ServiceTypeForumTopicCreated))

	if messageCalls != 2 {
		t.Fatalf("message handlers called %d times, want 2 for message and edited_message", messageCalls)
	}
	if callbackCalls != 1 {
		t.Fatalf("callback handler called %d times, want 1", callbackCalls)
	}
	if serviceCalls != 1 {
		t.Fatalf("service handler called %d times, want 1", serviceCalls)
	}
}

func TestRouter_UnauthorizedUpdateTypesAreSilentlyRejected(t *testing.T) {
	db := openTestDB(t)
	r := NewRouter(db, nil, 100)

	var calls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { calls++ }
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { calls++ }
	r.OnCallback = func(context.Context, contract.Update) { calls++ }
	r.OnService = func(context.Context, contract.Update) { calls++ }

	message := textUpdate(300, 100, int64Ptr(5), "hello", false)
	edited := message
	edited.Type = "edited_message"
	updates := []contract.Update{
		message,
		edited,
		callbackUpdate(300, 100),
		serviceUpdate(300, 100, int64Ptr(5), contract.ServiceTypeForumTopicCreated),
	}
	for _, update := range updates {
		r.Route(context.Background(), update)
	}

	if calls != 0 {
		t.Fatalf("unauthorized update handlers called %d times, want silent rejection", calls)
	}
}

func TestAuthorizer_AdminUserIDZeroDoesNotAuthorizeUserZero(t *testing.T) {
	db := openTestDB(t)
	authorizer := NewAuthorizer(db, 100, 0)
	ctx := context.Background()

	admin, err := authorizer.IsAdmin(ctx, 0)
	if err != nil {
		t.Fatalf("IsAdmin: %v", err)
	}
	if admin {
		t.Fatal("ADMIN_USER_ID=0 must not bootstrap user 0 as an administrator")
	}

	for _, update := range []contract.Update{
		textUpdate(0, 100, nil, "/help", true),
		callbackUpdate(0, 100),
	} {
		allowed, err := authorizer.CanReceiveUpdate(ctx, update)
		if err != nil {
			t.Fatalf("CanReceiveUpdate(%q): %v", update.Type, err)
		}
		if allowed {
			t.Fatalf("ADMIN_USER_ID=0 unexpectedly authorized %q", update.Type)
		}
	}
}

func TestRouter_DatabaseAdminRemainsAuthorizedWithZeroBootstrapID(t *testing.T) {
	db := openTestDB(t)
	if err := db.UpsertAllowedUser(context.Background(), &AllowedUser{UserID: 9001, Role: "admin"}); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	r := NewRouter(db, nil, 100)
	r.SetAdminUserID(0)

	called := false
	r.OnCallback = func(context.Context, contract.Update) { called = true }
	r.Route(context.Background(), callbackUpdate(9001, 100))

	if !called {
		t.Fatal("database administrator should remain authorized when ADMIN_USER_ID=0")
	}
}

func TestCommandHandler_AdminCommandsRequireAdministrator(t *testing.T) {
	const (
		adminUser   = int64(9001)
		regularUser = int64(200)
	)

	tests := []struct {
		name       string
		invoke     func(context.Context, *CommandHandler, contract.Update, *Group) (string, error)
		adminReply string
	}{
		{
			name: "adduser",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, _ *Group) (string, error) {
				return h.cmdAddUser(ctx, update, "300")
			},
			adminReply: "Added user 300",
		},
		{
			name: "removeuser",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, _ *Group) (string, error) {
				return h.cmdRemoveUser(ctx, update, "300")
			},
			adminReply: "Removed user 300",
		},
		{
			name: "users",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, _ *Group) (string, error) {
				return h.cmdUsers(ctx, update)
			},
			adminReply: "Allowed users (1)",
		},
		{
			name: "config",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, group *Group) (string, error) {
				return h.cmdConfig(ctx, update, group, "permission_mode plan")
			},
			adminReply: "Permission mode set to: plan",
		},
		{
			name: "update",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, _ *Group) (string, error) {
				return h.cmdUpdate(ctx, update, "")
			},
			adminReply: "No updates available",
		},
		{
			name: "permission",
			invoke: func(ctx context.Context, h *CommandHandler, update contract.Update, group *Group) (string, error) {
				return h.cmdPermission(ctx, update, group, "plan")
			},
			adminReply: "Permission mode set to: plan",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seedUser(t, db, regularUser)
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
			h.SetAdminUserID(adminUser)
			if tc.name == "update" {
				h.updater = &mockUpdaterImpl{}
			}

			regularReply, err := tc.invoke(ctx, h, makeUpdate(100, nil, 1, "/"+tc.name, regularUser), group)
			if err != nil {
				t.Fatalf("regular user command: %v", err)
			}
			if !strings.Contains(regularReply, "Permission denied") {
				t.Fatalf("regular user reply = %q, want permission denial", regularReply)
			}

			adminReply, err := tc.invoke(ctx, h, makeUpdate(100, nil, 2, "/"+tc.name, adminUser), group)
			if err != nil {
				t.Fatalf("bootstrap admin command: %v", err)
			}
			if strings.Contains(adminReply, "Permission denied") {
				t.Fatalf("bootstrap admin was denied: %q", adminReply)
			}
			if !strings.Contains(adminReply, tc.adminReply) {
				t.Fatalf("bootstrap admin reply = %q, want %q", adminReply, tc.adminReply)
			}
		})
	}
}
