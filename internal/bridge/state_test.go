package bridge

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := OpenDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("OpenDB: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestOpenDB_CreatesSchema(t *testing.T) {
	openTestDB(t)
}

func TestOpenDB_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bridge.db")

	db1, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	db1.Close()

	db2, err := OpenDB(path)
	if err != nil {
		t.Fatalf("second open failed: %v", err)
	}
	db2.Close()
}

// ── groups ────────────────────────────────────────────────────────────────────

func TestGroup_UpsertAndGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	g := &Group{
		ChatID:       100,
		Name:         "test group",
		CWD:          "/home/coding/myproject",
		DefaultModel: "claude-sonnet-4-6",
		MaxBudget:    10.0,
		TimeoutSec:   600,
		CreatedAt:    time.Now().UTC().Truncate(time.Second),
	}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}

	got, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("GetGroup: %v", err)
	}
	if got == nil {
		t.Fatal("expected group, got nil")
	}
	if got.ChatID != g.ChatID {
		t.Errorf("ChatID: got %d, want %d", got.ChatID, g.ChatID)
	}
	if got.Name != g.Name {
		t.Errorf("Name: got %q, want %q", got.Name, g.Name)
	}
	if got.CWD != g.CWD {
		t.Errorf("CWD: got %q, want %q", got.CWD, g.CWD)
	}
	if got.DefaultModel != g.DefaultModel {
		t.Errorf("DefaultModel: got %q, want %q", got.DefaultModel, g.DefaultModel)
	}
	if got.MaxBudget != g.MaxBudget {
		t.Errorf("MaxBudget: got %f, want %f", got.MaxBudget, g.MaxBudget)
	}
}

func TestGroup_GetMissing(t *testing.T) {
	db := openTestDB(t)
	g, err := db.GetGroup(context.Background(), 999)
	if err != nil {
		t.Fatal(err)
	}
	if g != nil {
		t.Errorf("expected nil for missing group, got %+v", g)
	}
}

func TestGroup_Upsert_UpdatesExisting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	g := &Group{ChatID: 1, CWD: "/old", DefaultModel: "claude-haiku-4-5-20251001", MaxBudget: 1.0, TimeoutSec: 60, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	g.CWD = "/new"
	g.DefaultModel = "claude-sonnet-4-6"
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetGroup(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.CWD != "/new" {
		t.Errorf("CWD: got %q, want /new", got.CWD)
	}
}

func TestGroup_ListAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for i := int64(1); i <= 3; i++ {
		g := &Group{ChatID: i, CWD: "/tmp", DefaultModel: "claude-sonnet-4-6", MaxBudget: 5.0, TimeoutSec: 300, CreatedAt: time.Now().UTC()}
		if err := db.UpsertGroup(ctx, g); err != nil {
			t.Fatal(err)
		}
	}

	list, err := db.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("ListGroups: got %d, want 3", len(list))
	}

	if err := db.DeleteGroup(ctx, 2); err != nil {
		t.Fatal(err)
	}
	list, err = db.ListGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("ListGroups after delete: got %d, want 2", len(list))
	}
}

// ── sessions ──────────────────────────────────────────────────────────────────

func setupGroupAndSession(t *testing.T, db *DB, chatID, threadID int64) *Session {
	t.Helper()
	ctx := context.Background()
	g := &Group{ChatID: chatID, CWD: "/tmp", DefaultModel: "claude-sonnet-4-6", MaxBudget: 5.0, TimeoutSec: 300, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatalf("UpsertGroup: %v", err)
	}
	s := &Session{
		ChatID:    chatID,
		ThreadID:  threadID,
		SessionID: "sess-abc123",
		CWD:       "/tmp",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
	}
	if err := db.CreateSession(ctx, s); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func TestSession_CreateAndGet(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	setupGroupAndSession(t, db, 100, 10)

	got, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected session, got nil")
	}
	if got.SessionID != "sess-abc123" {
		t.Errorf("SessionID: got %q, want sess-abc123", got.SessionID)
	}
	if got.Status != "active" {
		t.Errorf("Status: got %q, want active", got.Status)
	}
}

func TestSession_GetMissing(t *testing.T) {
	db := openTestDB(t)
	got, err := db.GetSession(context.Background(), 999, 999)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestSession_Update(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	s := setupGroupAndSession(t, db, 1, 1)

	s.Status = "closed"
	s.MessageCount = 42
	s.LastActive = time.Now().UTC()
	if err := db.UpdateSession(ctx, s); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetSession(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Errorf("Status: got %q, want closed", got.Status)
	}
	if got.MessageCount != 42 {
		t.Errorf("MessageCount: got %d, want 42", got.MessageCount)
	}
}

func TestSession_Touch(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	setupGroupAndSession(t, db, 1, 1)

	for i := 0; i < 3; i++ {
		if err := db.TouchSession(ctx, 1, 1); err != nil {
			t.Fatal(err)
		}
	}

	got, err := db.GetSession(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.MessageCount != 3 {
		t.Errorf("MessageCount: got %d, want 3", got.MessageCount)
	}
}

func TestSession_ListAndDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	g := &Group{ChatID: 1, CWD: "/tmp", DefaultModel: "claude-sonnet-4-6", MaxBudget: 5.0, TimeoutSec: 300, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, g); err != nil {
		t.Fatal(err)
	}

	for i := int64(1); i <= 4; i++ {
		s := &Session{ChatID: 1, ThreadID: i, SessionID: "s" + string(rune('0'+i)), CWD: "/tmp"}
		if err := db.CreateSession(ctx, s); err != nil {
			t.Fatal(err)
		}
	}

	list, err := db.ListSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Errorf("ListSessions: got %d, want 4", len(list))
	}

	if err := db.DeleteSession(ctx, 1, 2); err != nil {
		t.Fatal(err)
	}
	list, err = db.ListSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("after delete: got %d, want 3", len(list))
	}
}

// ── allowed_users ─────────────────────────────────────────────────────────────

func TestAllowedUser_UpsertGetDelete(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := &AllowedUser{UserID: 42, Role: "admin"}
	if err := db.UpsertAllowedUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAllowedUser(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected user, got nil")
	}
	if got.Role != "admin" {
		t.Errorf("Role: got %q, want admin", got.Role)
	}

	ok, err := db.IsUserAllowed(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("expected user to be allowed")
	}

	ok, err = db.IsUserAllowed(ctx, 9999)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("unknown user should not be allowed")
	}

	if err := db.DeleteAllowedUser(ctx, 42); err != nil {
		t.Fatal(err)
	}
	ok, err = db.IsUserAllowed(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("deleted user should not be allowed")
	}
}

func TestAllowedUser_RoleUpdate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	u := &AllowedUser{UserID: 1, Role: "user"}
	if err := db.UpsertAllowedUser(ctx, u); err != nil {
		t.Fatal(err)
	}
	u.Role = "admin"
	if err := db.UpsertAllowedUser(ctx, u); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAllowedUser(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got.Role != "admin" {
		t.Errorf("role after update: got %q, want admin", got.Role)
	}
}

func TestAllowedUser_List(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	for _, uid := range []int64{1, 2, 3} {
		if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: uid, Role: "user"}); err != nil {
			t.Fatal(err)
		}
	}

	list, err := db.ListAllowedUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Errorf("ListAllowedUsers: got %d, want 3", len(list))
	}
}

// ── sent_messages ─────────────────────────────────────────────────────────────

func TestSentMessages(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	m := &SentMessage{ChatID: 1, ThreadID: 2, MessageID: 3, Purpose: "typing_indicator"}
	if err := db.RecordSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetSentMessage(ctx, 1, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil {
		t.Fatal("expected message, got nil")
	}
	if got.Purpose != "typing_indicator" {
		t.Errorf("Purpose: got %q, want typing_indicator", got.Purpose)
	}

	// FindByPurpose
	m2 := &SentMessage{ChatID: 1, ThreadID: 2, MessageID: 4, Purpose: "typing_indicator"}
	if err := db.RecordSentMessage(ctx, m2); err != nil {
		t.Fatal(err)
	}
	found, err := db.FindSentMessageByPurpose(ctx, 1, 2, "typing_indicator")
	if err != nil {
		t.Fatal(err)
	}
	if found == nil {
		t.Fatal("expected to find message by purpose")
	}

	// Delete
	if err := db.DeleteSentMessage(ctx, 1, 2, 3); err != nil {
		t.Fatal(err)
	}
	gone, err := db.GetSentMessage(ctx, 1, 2, 3)
	if err != nil {
		t.Fatal(err)
	}
	if gone != nil {
		t.Error("expected nil after delete")
	}
}

func TestSentMessages_IgnoreDuplicate(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	m := &SentMessage{ChatID: 1, ThreadID: 1, MessageID: 1, Purpose: "reply"}
	if err := db.RecordSentMessage(ctx, m); err != nil {
		t.Fatal(err)
	}
	// Second insert should be a no-op (INSERT OR IGNORE).
	if err := db.RecordSentMessage(ctx, m); err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
}

// ── DeleteGroup cascades ──────────────────────────────────────────────────────

func TestDeleteGroup_CascadesSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	setupGroupAndSession(t, db, 1, 1)
	setupGroupAndSession(t, db, 1, 2)

	if err := db.DeleteGroup(ctx, 1); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetGroup(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Error("group should be gone")
	}

	list, err := db.ListSessions(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 {
		t.Errorf("sessions after group delete: got %d, want 0", len(list))
	}
}

// ── file exists after close ───────────────────────────────────────────────────

func TestDB_FilePersistedOnDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persist.db")

	db, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertAllowedUser(context.Background(), &AllowedUser{UserID: 1, Role: "admin"}); err != nil {
		t.Fatal(err)
	}
	db.Close()

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("db file not found: %v", err)
	}

	db2, err := OpenDB(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()

	ok, err := db2.IsUserAllowed(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Error("data not persisted across close/reopen")
	}
}
