package bridge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// newTestSessionManagerForContext creates a minimal SessionManager for testing /context
func newTestSessionManagerForContext(t *testing.T, db *DB, sender *Sender) *SessionManager {
	t.Helper()
	// Create a mock HTTP server for the proxy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))

	sm := NewSessionManager(db, sender, srv.URL, nil, 5)
	t.Cleanup(srv.Close)
	return sm
}

func TestCommandHandler_cmdContext_ByThreadID(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	sender, _ := NewSender("http://fake.test", "")
	sm := newTestSessionManagerForContext(t, db, sender)

	handler := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")
	handler.SetSessionManager(sm)

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      5.0,
		TimeoutSec:     300,
		PermissionMode: "acceptEdits",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a source session with a known thread ID
	sourceSession := &Session{
		ChatID:    123,
		ThreadID:  456,
		SessionID: "test-session-1",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "fix-auth-bug",
	}
	if err := db.CreateSession(ctx, sourceSession); err != nil {
		t.Fatalf("create source session: %v", err)
	}

	// Create a target session (where we're calling /context from)
	targetThreadID := int64(789)
	targetSession := &Session{
		ChatID:    123,
		ThreadID:  targetThreadID,
		SessionID: "test-session-2",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "new-feature",
	}
	if err := db.CreateSession(ctx, targetSession); err != nil {
		t.Fatalf("create target session: %v", err)
	}

	// Test calling /context with a numeric thread ID
	text := "/context 456"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		ThreadID:   &targetThreadID,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, group, "456")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	expected := "Context from thread 456 will be included in your next prompt."
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdContext_ByTopicName(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	sender, _ := NewSender("http://fake.test", "")
	sm := newTestSessionManagerForContext(t, db, sender)

	handler := NewCommandHandler(db, sender, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")
	handler.SetSessionManager(sm)

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      5.0,
		TimeoutSec:     300,
		PermissionMode: "acceptEdits",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a source session with a topic name
	sourceSession := &Session{
		ChatID:    123,
		ThreadID:  456,
		SessionID: "test-session-1",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "fix-auth-bug",
	}
	if err := db.CreateSession(ctx, sourceSession); err != nil {
		t.Fatalf("create source session: %v", err)
	}

	// Create a target session
	targetThreadID := int64(789)
	targetSession := &Session{
		ChatID:    123,
		ThreadID:  targetThreadID,
		SessionID: "test-session-2",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "new-feature",
	}
	if err := db.CreateSession(ctx, targetSession); err != nil {
		t.Fatalf("create target session: %v", err)
	}

	// Test calling /context with a topic name instead of thread ID
	text := "/context fix-auth-bug"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		ThreadID:   &targetThreadID,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, group, "fix-auth-bug")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	expected := "Context from thread 456 will be included in your next prompt."
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdContext_TopicNotFound(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	handler := NewCommandHandler(db, nil, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      5.0,
		TimeoutSec:     300,
		PermissionMode: "acceptEdits",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a target session
	targetThreadID := int64(789)
	targetSession := &Session{
		ChatID:    123,
		ThreadID:  targetThreadID,
		SessionID: "test-session-2",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "new-feature",
	}
	if err := db.CreateSession(ctx, targetSession); err != nil {
		t.Fatalf("create target session: %v", err)
	}

	// Test calling /context with a non-existent topic name
	text := "/context nonexistent-topic"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		ThreadID:   &targetThreadID,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, group, "nonexistent-topic")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	if !containsSubstring(reply, "Topic not found") {
		t.Errorf("expected topic not found message, got %q", reply)
	}
	if !containsSubstring(reply, "nonexistent-topic") {
		t.Errorf("expected message to mention the topic name, got %q", reply)
	}
}

func TestCommandHandler_cmdContext_NoArgs(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	handler := NewCommandHandler(db, nil, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      5.0,
		TimeoutSec:     300,
		PermissionMode: "acceptEdits",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	targetThreadID := int64(789)
	text := "/context"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		ThreadID:   &targetThreadID,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	// Should return usage message
	if !containsSubstring(reply, "Usage:") {
		t.Errorf("expected usage message, got %q", reply)
	}
	if !containsSubstring(reply, "thread_id or topic_name") {
		t.Errorf("expected usage to mention both thread_id and topic_name, got %q", reply)
	}
}

func TestCommandHandler_cmdContext_NoGroup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	handler := NewCommandHandler(db, nil, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")

	targetThreadID := int64(789)
	text := "/context 456"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		ThreadID:   &targetThreadID,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, nil, "456")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	expected := "This group is not registered."
	if reply[:len(expected)] != expected {
		t.Errorf("expected message starting with %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdContext_NoTargetSession(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	handler := NewCommandHandler(db, nil, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      5.0,
		TimeoutSec:     300,
		PermissionMode: "acceptEdits",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// No target session (ThreadID is nil)
	text := "/context 456"
	update := contract.Update{
		ChatID:   123,
		MessageID: 1,
		FromUser: contract.FromUser{ID: 1},
		Content:  &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdContext(ctx, update, group, "456")
	if err != nil {
		t.Fatalf("cmdContext failed: %v", err)
	}

	expected := "Context commands only work within a topic session."
	if reply[:len(expected)] != expected {
		t.Errorf("expected message starting with %q, got %q", expected, reply)
	}
}

func TestDB_GetSessionByTopicName(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create a group for the first chat
	group1 := &Group{
		ChatID:    123,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group1); err != nil {
		t.Fatalf("upsert group1: %v", err)
	}

	// Create a session with a topic name
	session := &Session{
		ChatID:    123,
		ThreadID:  456,
		SessionID: "test-session",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "my-topic",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Test finding session by topic name
	found, err := db.GetSessionByTopicName(ctx, 123, "my-topic")
	if err != nil {
		t.Fatalf("GetSessionByTopicName failed: %v", err)
	}
	if found == nil {
		t.Fatal("session not found by topic name")
	}
	if found.ThreadID != 456 {
		t.Errorf("expected threadID 456, got %d", found.ThreadID)
	}
	if found.TopicName != "my-topic" {
		t.Errorf("expected topic name %q, got %q", "my-topic", found.TopicName)
	}

	// Test non-existent topic name
	notFound, err := db.GetSessionByTopicName(ctx, 123, "nonexistent")
	if err != nil {
		t.Fatalf("GetSessionByTopicName with non-existent failed: %v", err)
	}
	if notFound != nil {
		t.Error("expected nil for non-existent topic name")
	}

	// Test that topic names are scoped to chat IDs
	// Create group for the second chat
	group2 := &Group{
		ChatID:    789,
		CWD:       "/test2",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group2); err != nil {
		t.Fatalf("upsert group2: %v", err)
	}

	// Create same topic name in different chat
	session2 := &Session{
		ChatID:    789,
		ThreadID:  999,
		SessionID: "test-session-2",
		CWD:       "/test2",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "my-topic", // same name, different chat
	}
	if err := db.CreateSession(ctx, session2); err != nil {
		t.Fatalf("create session2: %v", err)
	}

	// Verify chat 123 finds its own session
	found1, err := db.GetSessionByTopicName(ctx, 123, "my-topic")
	if err != nil {
		t.Fatalf("GetSessionByTopicName for chat 123 failed: %v", err)
	}
	if found1.ThreadID != 456 {
		t.Errorf("expected chat 123 to find threadID 456, got %d", found1.ThreadID)
	}

	// Verify chat 789 finds its own session
	found2, err := db.GetSessionByTopicName(ctx, 789, "my-topic")
	if err != nil {
		t.Fatalf("GetSessionByTopicName for chat 789 failed: %v", err)
	}
	if found2.ThreadID != 999 {
		t.Errorf("expected chat 789 to find threadID 999, got %d", found2.ThreadID)
	}
}

func TestDB_Session_TopicNameStored(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create a group first (required by foreign key constraint)
	group := &Group{
		ChatID:    123,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a session with a topic name
	session := &Session{
		ChatID:    123,
		ThreadID:  456,
		SessionID: "test-session",
		CWD:       "/test",
		Model:     "claude-sonnet-4-6",
		Status:    "active",
		TopicName: "test-topic-name",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Retrieve the session and verify topic name is preserved
	retrieved, err := db.GetSession(ctx, 123, 456)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if retrieved == nil {
		t.Fatal("session not found")
	}
	if retrieved.TopicName != "test-topic-name" {
		t.Errorf("expected topic name %q, got %q", "test-topic-name", retrieved.TopicName)
	}

	// Update session and verify topic name is preserved
	retrieved.Model = "claude-opus-4-7"
	if err := db.UpdateSession(ctx, retrieved); err != nil {
		t.Fatalf("update session: %v", err)
	}

	updated, err := db.GetSession(ctx, 123, 456)
	if err != nil {
		t.Fatalf("get updated session: %v", err)
	}
	if updated.TopicName != "test-topic-name" {
		t.Errorf("topic name not preserved after update, got %q", updated.TopicName)
	}
}
