package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func int64Ptr(v int64) *int64 { return &v }

func seedUser(t *testing.T, db *DB, userID int64) {
	t.Helper()
	if err := db.UpsertAllowedUser(context.Background(), &AllowedUser{
		UserID:  userID,
		Role:    "user",
		AddedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertAllowedUser: %v", err)
	}
}

func seedGroup(t *testing.T, db *DB, chatID int64) {
	t.Helper()
	if err := db.UpsertGroup(context.Background(), &Group{
		ChatID:       chatID,
		Name:         "test group",
		CWD:          "/home/coding/project",
		DefaultModel: "claude-sonnet-4-6",
		MaxBudget:    5.0,
		TimeoutSec:   300,
		CreatedAt:    time.Now(),
	}); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
}

func seedSession(t *testing.T, db *DB, chatID, threadID int64) *Session {
	t.Helper()
	s := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: "sess-abc",
		CWD:       "/home/coding/project",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(context.Background(), s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func textUpdate(userID, chatID int64, threadID *int64, text string, isCmd bool) contract.Update {
	entities := []contract.Entity{}
	if isCmd {
		entities = []contract.Entity{{Type: "bot_command", Offset: 0, Length: len(text)}}
	}
	return contract.Update{
		UpdateID: 1,
		Type:     "message",
		ChatID:   chatID,
		ThreadID: threadID,
		FromUser: contract.FromUser{ID: userID},
		Content: &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     strPtr(text),
			Entities: entities,
		},
	}
}

func strPtr(s string) *string { return &s }

func callbackUpdate(userID, chatID int64) contract.Update {
	return contract.Update{
		UpdateID: 2,
		Type:     "callback_query",
		ChatID:   chatID,
		FromUser: contract.FromUser{ID: userID},
		Content: &contract.Content{
			Type:            contract.ContentTypeCallback,
			CallbackQueryID: strPtr("cq-1"),
			Data:            strPtr("action:yes"),
		},
	}
}

func serviceUpdate(userID, chatID int64, threadID *int64, svcType string) contract.Update {
	return contract.Update{
		UpdateID: 3,
		Type:     "service",
		ChatID:   chatID,
		ThreadID: threadID,
		FromUser: contract.FromUser{ID: userID},
		Service:  &contract.Service{Type: svcType},
	}
}

// ── tests ─────────────────────────────────────────────────────────────────────

func TestRouter_UnauthorizedUserDropped(t *testing.T) {
	db := openTestDB(t)
	r := NewRouter(db)

	called := false
	r.OnCommand = func(_ context.Context, _ contract.Update, _ *Group) { called = true }
	r.OnSession = func(_ context.Context, _ contract.Update, _ *Session, _ *Group) { called = true }
	r.OnService = func(_ context.Context, _ contract.Update) { called = true }
	r.OnCallback = func(_ context.Context, _ contract.Update) { called = true }

	// user 99 is not in allowed_users
	update := textUpdate(99, 100, nil, "/start", true)
	r.Route(context.Background(), update)

	if called {
		t.Fatal("expected silent drop for unauthorized user; got handler call")
	}
}

func TestRouter_CallbackQuery(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	r := NewRouter(db)

	var got *contract.Update
	r.OnCallback = func(_ context.Context, u contract.Update) { got = &u }

	r.Route(context.Background(), callbackUpdate(1, 100))

	if got == nil {
		t.Fatal("OnCallback not called")
	}
	if got.Type != "callback_query" {
		t.Fatalf("expected callback_query, got %q", got.Type)
	}
}

func TestRouter_ServiceMessage(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	r := NewRouter(db)

	var got *contract.Update
	r.OnService = func(_ context.Context, u contract.Update) { got = &u }

	r.Route(context.Background(), serviceUpdate(1, 100, int64Ptr(5), contract.ServiceTypeForumTopicCreated))

	if got == nil {
		t.Fatal("OnService not called")
	}
}

func TestRouter_GeneralTopic_Command(t *testing.T) {
	tests := []struct {
		name     string
		threadID *int64
	}{
		{"nil thread_id", nil},
		{"thread_id=1", int64Ptr(1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			seedUser(t, db, 1)
			r := NewRouter(db)

			var got *contract.Update
			r.OnCommand = func(_ context.Context, u contract.Update, _ *Group) { got = &u }

			r.Route(context.Background(), textUpdate(1, 100, tc.threadID, "/status", true))

			if got == nil {
				t.Fatal("OnCommand not called")
			}
		})
	}
}

func TestRouter_GeneralTopic_NonCommandIgnored(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	r := NewRouter(db)

	called := false
	r.OnCommand = func(_ context.Context, _ contract.Update, _ *Group) { called = true }
	r.OnSession = func(_ context.Context, _ contract.Update, _ *Session, _ *Group) { called = true }

	// Plain text in General topic (no thread_id)
	r.Route(context.Background(), textUpdate(1, 100, nil, "hello world", false))

	if called {
		t.Fatal("expected non-command in General topic to be ignored")
	}
}

func TestRouter_GeneralTopic_PassesGroupToCommandHandler(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	seedGroup(t, db, 100)
	r := NewRouter(db)

	var gotGroup *Group
	r.OnCommand = func(_ context.Context, _ contract.Update, g *Group) { gotGroup = g }

	r.Route(context.Background(), textUpdate(1, 100, nil, "/status", true))

	if gotGroup == nil {
		t.Fatal("expected non-nil group passed to OnCommand")
	}
	if gotGroup.ChatID != 100 {
		t.Fatalf("expected chat_id 100, got %d", gotGroup.ChatID)
	}
}

func TestRouter_GeneralTopic_NilGroupWhenUnregistered(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	// no group seeded for chat 100
	r := NewRouter(db)

	var gotGroup *Group
	called := false
	r.OnCommand = func(_ context.Context, _ contract.Update, g *Group) {
		called = true
		gotGroup = g
	}

	r.Route(context.Background(), textUpdate(1, 100, nil, "/cwd /some/path", true))

	if !called {
		t.Fatal("OnCommand should be called even for unregistered group (/cwd registers it)")
	}
	if gotGroup != nil {
		t.Fatalf("expected nil group for unregistered chat, got %+v", gotGroup)
	}
}

func TestRouter_NamedTopic_ExistingSession(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	seedGroup(t, db, 100)
	sess := seedSession(t, db, 100, 5)
	r := NewRouter(db)

	var gotSess *Session
	var gotGroup *Group
	r.OnSession = func(_ context.Context, _ contract.Update, s *Session, g *Group) {
		gotSess = s
		gotGroup = g
	}

	r.Route(context.Background(), textUpdate(1, 100, int64Ptr(5), "hello claude", false))

	if gotSess == nil {
		t.Fatal("OnSession not called with existing session")
	}
	if gotSess.SessionID != sess.SessionID {
		t.Fatalf("expected session %q, got %q", sess.SessionID, gotSess.SessionID)
	}
	if gotGroup != nil {
		t.Fatal("expected nil group when session exists")
	}
}

func TestRouter_NamedTopic_NewSession_RegisteredGroup(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	seedGroup(t, db, 100)
	// no session for thread 7
	r := NewRouter(db)

	var gotSess *Session
	var gotGroup *Group
	r.OnSession = func(_ context.Context, _ contract.Update, s *Session, g *Group) {
		gotSess = s
		gotGroup = g
	}

	r.Route(context.Background(), textUpdate(1, 100, int64Ptr(7), "start a new task", false))

	if gotSess != nil {
		t.Fatalf("expected nil session for new topic, got %+v", gotSess)
	}
	if gotGroup == nil {
		t.Fatal("expected non-nil group for new session in registered group")
	}
	if gotGroup.ChatID != 100 {
		t.Fatalf("expected chat_id 100, got %d", gotGroup.ChatID)
	}
}

func TestRouter_NamedTopic_UnregisteredGroup_Ignored(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	// no group registered for chat 999
	r := NewRouter(db)

	called := false
	r.OnSession = func(_ context.Context, _ contract.Update, _ *Session, _ *Group) { called = true }

	r.Route(context.Background(), textUpdate(1, 999, int64Ptr(3), "hello", false))

	if called {
		t.Fatal("expected silent drop for unregistered group")
	}
}

func TestRouter_NilHandlers_NoPanic(t *testing.T) {
	db := openTestDB(t)
	seedUser(t, db, 1)
	seedGroup(t, db, 100)
	seedSession(t, db, 100, 5)

	r := NewRouter(db) // all handlers nil

	updates := []contract.Update{
		callbackUpdate(1, 100),
		serviceUpdate(1, 100, int64Ptr(5), contract.ServiceTypeForumTopicClosed),
		textUpdate(1, 100, nil, "/status", true),
		textUpdate(1, 100, int64Ptr(5), "hello", false),
		textUpdate(1, 100, int64Ptr(9), "new topic", false),
	}
	for _, u := range updates {
		r.Route(context.Background(), u) // must not panic
	}
}
