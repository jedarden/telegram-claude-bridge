package bridge

import (
	"context"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func authorizationMediaUpdate(userID, chatID, threadID int64, contentType string) contract.Update {
	return contract.Update{
		UpdateID: 100,
		Type:     "message",
		ChatID:   chatID,
		ThreadID: int64Ptr(threadID),
		FromUser: contract.FromUser{ID: userID},
		Content:  &contract.Content{Type: contentType},
	}
}

func TestRouter_AllowlistedNonAdminCannotUseCallbackActions(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 200)
	r := NewRouter(db, nil, 100)

	called := false
	r.OnCallback = func(context.Context, contract.Update) { called = true }
	r.Route(context.Background(), callbackUpdate(200, 100))

	if called {
		t.Fatal("allowlisted non-admin callback reached the handler")
	}
}

func TestRouter_AllowlistedMediaFromBlockedChatIsSilentlyRejected(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 200)
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)
	r := NewRouter(db, nil, 100)

	var calls int
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { calls++ }
	for _, contentType := range []string{
		contract.ContentTypePhoto,
		contract.ContentTypeVoice,
		contract.ContentTypeAudio,
		contract.ContentTypeVideo,
		contract.ContentTypeVideoNote,
		contract.ContentTypeDocument,
	} {
		r.Route(context.Background(), authorizationMediaUpdate(200, 999, 5, contentType))
	}

	if calls != 0 {
		t.Fatalf("media from blocked chats reached the session handler %d times", calls)
	}
}

func TestRouter_AuthorizedMediaReachesSessionHandler(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 200)
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)
	r := NewRouter(db, nil, 100)

	var calls []string
	r.OnSession = func(_ context.Context, update contract.Update, _ *Session, _ *Group) {
		calls = append(calls, update.Content.Type)
	}
	mediaTypes := []string{
		contract.ContentTypePhoto,
		contract.ContentTypeVoice,
		contract.ContentTypeAudio,
		contract.ContentTypeVideo,
		contract.ContentTypeVideoNote,
		contract.ContentTypeDocument,
	}
	for _, contentType := range mediaTypes {
		r.Route(context.Background(), authorizationMediaUpdate(200, 100, 5, contentType))
	}

	if len(calls) != len(mediaTypes) {
		t.Fatalf("authorized media handler calls = %d, want %d (%v)", len(calls), len(mediaTypes), calls)
	}
	for i, want := range mediaTypes {
		if calls[i] != want {
			t.Errorf("authorized media call %d content type = %q, want %q", i, calls[i], want)
		}
	}
}

func TestRouter_BootstrapAdminCanRouteMediaWithoutAllowlistRow(t *testing.T) {
	db := openTestDB(t)
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)
	r := NewRouter(db, nil, 100)
	r.SetAdminUserID(9001)

	var calls int
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { calls++ }
	for _, contentType := range []string{
		contract.ContentTypePhoto,
		contract.ContentTypeVoice,
		contract.ContentTypeDocument,
	} {
		r.Route(context.Background(), authorizationMediaUpdate(9001, 100, 5, contentType))
	}

	if calls != 3 {
		t.Fatalf("bootstrap admin media handler calls = %d, want 3", calls)
	}
}

func TestFirstDatabaseAdminIsRequiredBeforeCWDCanRegisterGroup(t *testing.T) {
	const (
		adminUser = int64(9001)
		chatID    = int64(100)
	)

	db := openTestDB(t)
	ctx := context.Background()
	root := t.TempDir()
	policy, err := NewWorkingDirectoryPolicy(root)
	if err != nil {
		t.Fatalf("NewWorkingDirectoryPolicy: %v", err)
	}
	sender, _ := newRecordingProxy(t)
	h := NewCommandHandler(db, sender, "http://unused", nil, nil, "v1.0.0", "test", "test")
	h.SetWorkingDirectoryPolicy(policy)
	authorizer := NewAuthorizer(db, 0, 0)
	h.SetAuthorizer(authorizer)
	r := NewRouter(db, nil, chatID)
	r.SetAuthorizer(authorizer)
	r.OnCommand = h.Handle

	// With ADMIN_USER_ID=0 and no database administrator, even the first
	// /cwd must not create a group.
	r.Route(ctx, commandTextUpdate(adminUser, chatID, 1, "/cwd "+root))
	group, err := db.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group before bootstrap: %v", err)
	}
	if group != nil {
		t.Fatalf("/cwd registered a group before an administrator was seeded: %+v", group)
	}

	// Being allow-listed as a regular user is still insufficient for the
	// administrator-only registration command.
	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: adminUser, Role: "user"}); err != nil {
		t.Fatalf("seed regular user: %v", err)
	}
	r.Route(ctx, commandTextUpdate(adminUser, chatID, 2, "/cwd "+root))
	group, err = db.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group after regular allowlist row: %v", err)
	}
	if group != nil {
		t.Fatalf("regular allowlist user registered a group: %+v", group)
	}

	if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: adminUser, Role: "admin"}); err != nil {
		t.Fatalf("seed first administrator: %v", err)
	}
	r.Route(ctx, commandTextUpdate(adminUser, chatID, 3, "/cwd "+root))
	group, err = db.GetGroup(ctx, chatID)
	if err != nil {
		t.Fatalf("get group after administrator bootstrap: %v", err)
	}
	if group == nil {
		t.Fatal("database administrator could not register the first group")
	}
	if group.CWD != root {
		t.Fatalf("first group cwd = %q, want %q", group.CWD, root)
	}
}
