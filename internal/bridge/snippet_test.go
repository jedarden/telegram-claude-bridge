package bridge

import (
	"context"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func TestCommandHandler_cmdSnippet_Create(t *testing.T) {
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

	text := "/snippet api-key sk-12345"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "api-key sk-12345")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Created snippet: api-key"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was created
	snippet, err := db.GetSnippet(ctx, 123, "api-key")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if snippet == nil {
		t.Fatal("snippet not found")
	}
	if snippet.Content != "sk-12345" {
		t.Errorf("expected content %q, got %q", "sk-12345", snippet.Content)
	}
}

func TestCommandHandler_cmdSnippet_Update(t *testing.T) {
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

	// Create initial snippet
	snippet := &Snippet{
		ChatID:  123,
		Name:    "api-key",
		Content: "old-key",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	text := "/snippet api-key new-key"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "api-key new-key")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Updated snippet: api-key"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was updated
	updated, err := db.GetSnippet(ctx, 123, "api-key")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if updated.Content != "new-key" {
		t.Errorf("expected content %q, got %q", "new-key", updated.Content)
	}
}

func TestCommandHandler_cmdSnippet_Delete(t *testing.T) {
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

	// Create initial snippet
	snippet := &Snippet{
		ChatID:  123,
		Name:    "old-key",
		Content: "to-be-deleted",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	text := "/snippet delete old-key"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "delete old-key")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Deleted snippet: old-key"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was deleted
	deleted, err := db.GetSnippet(ctx, 123, "old-key")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if deleted != nil {
		t.Error("snippet should have been deleted")
	}
}

func TestCommandHandler_cmdSnippet_DeleteNonExistent(t *testing.T) {
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

	text := "/snippet delete nonexistent"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "delete nonexistent")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Snippet \"nonexistent\" not found."
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdSnippet_EmptyArgs(t *testing.T) {
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

	text := "/snippet"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	// Should return usage message
	if reply == "" {
		t.Error("expected usage message, got empty string")
	}
}

func TestCommandHandler_cmdSnippet_OnlyName(t *testing.T) {
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

	text := "/snippet only-name"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "only-name")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	// Check that it's a usage message
	expected := "Usage: /snippet <name> <content>"
	if reply[:len(expected)] != expected {
		t.Errorf("expected usage message starting with %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdSnippets_List(t *testing.T) {
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

	// Create multiple snippets
	snippets := []*Snippet{
		{ChatID: 123, Name: "api-key", Content: "sk-12345"},
		{ChatID: 123, Name: "project-root", Content: "/home/user/project"},
		{ChatID: 123, Name: "database-url", Content: "postgres://localhost/db"},
	}
	for _, s := range snippets {
		if err := db.CreateSnippet(ctx, s); err != nil {
			t.Fatalf("create snippet: %v", err)
		}
	}

	text := "/snippets"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippets(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdSnippets failed: %v", err)
	}

	// Verify all snippets are listed
	if !containsSubstring(reply, "api-key") {
		t.Errorf("expected reply to contain api-key, got: %s", reply)
	}
	if !containsSubstring(reply, "project-root") {
		t.Errorf("expected reply to contain project-root, got: %s", reply)
	}
	if !containsSubstring(reply, "database-url") {
		t.Errorf("expected reply to contain database-url, got: %s", reply)
	}
	if !containsSubstring(reply, "3") {
		t.Errorf("expected reply to show 3 snippets, got: %s", reply)
	}
}

func TestCommandHandler_cmdSnippets_EmptyList(t *testing.T) {
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

	text := "/snippets"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippets(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdSnippets failed: %v", err)
	}

	expected := "No snippets saved for this chat."
	if reply[:len(expected)] != expected {
		t.Errorf("expected message starting with %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdSnippets_NoGroup(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()
	handler := NewCommandHandler(db, nil, "http://fake.test", nil, nil, "v1.0", "abc123", "2024-01-01")

	text := "/snippets"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippets(ctx, update, nil)
	if err != nil {
		t.Fatalf("cmdSnippets failed: %v", err)
	}

	expected := "This group is not registered."
	if reply[:len(expected)] != expected {
		t.Errorf("expected message starting with %q, got %q", expected, reply)
	}
}

func TestCommandHandler_cmdSnippet_LongContent(t *testing.T) {
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

	longContent := "This is a very long content that should be stored properly in the database without any issues and should be retrievable later."
	text := "/snippet long " + longContent
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "long "+longContent)
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Created snippet: long"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was stored correctly
	snippet, err := db.GetSnippet(ctx, 123, "long")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if snippet.Content != longContent {
		t.Errorf("expected content %q, got %q", longContent, snippet.Content)
	}
}

func TestCommandHandler_cmdSnippet_ContentWithSpaces(t *testing.T) {
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

	contentWithSpaces := "content with multiple words and spaces"
	text := "/snippet name " + contentWithSpaces
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "name "+contentWithSpaces)
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Created snippet: name"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was stored correctly
	snippet, err := db.GetSnippet(ctx, 123, "name")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if snippet.Content != contentWithSpaces {
		t.Errorf("expected content %q, got %q", contentWithSpaces, snippet.Content)
	}
}

func TestCommandHandler_cmdSnippet_CaseInsensitiveDelete(t *testing.T) {
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

	// Create initial snippet
	snippet := &Snippet{
		ChatID:  123,
		Name:    "test-key",
		Content: "to-be-deleted",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	// Test with uppercase DELETE
	text := "/snippet DELETE test-key"
	update := contract.Update{
		ChatID:     123,
		MessageID:  1,
		FromUser:   contract.FromUser{ID: 1},
		Content:    &contract.Content{Type: contract.ContentTypeText, Text: &text},
	}

	reply, err := handler.cmdSnippet(ctx, update, group, "DELETE test-key")
	if err != nil {
		t.Fatalf("cmdSnippet failed: %v", err)
	}

	expected := "Deleted snippet: test-key"
	if reply != expected {
		t.Errorf("expected %q, got %q", expected, reply)
	}

	// Verify snippet was deleted
	deleted, err := db.GetSnippet(ctx, 123, "test-key")
	if err != nil {
		t.Fatalf("get snippet failed: %v", err)
	}
	if deleted != nil {
		t.Error("snippet should have been deleted")
	}
}

func TestDB_Snippet_Crud(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Test creating a snippet
	snippet := &Snippet{
		ChatID:  123,
		Name:    "test-snippet",
		Content: "test content",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	// Test getting a snippet
	retrieved, err := db.GetSnippet(ctx, 123, "test-snippet")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	if retrieved == nil {
		t.Fatal("snippet not found")
	}
	if retrieved.Content != "test content" {
		t.Errorf("expected content %q, got %q", "test content", retrieved.Content)
	}

	// Test updating a snippet
	retrieved.Content = "updated content"
	if err := db.UpdateSnippet(ctx, retrieved); err != nil {
		t.Fatalf("update snippet: %v", err)
	}

	// Verify update
	updated, err := db.GetSnippet(ctx, 123, "test-snippet")
	if err != nil {
		t.Fatalf("get updated snippet: %v", err)
	}
	if updated.Content != "updated content" {
		t.Errorf("expected content %q, got %q", "updated content", updated.Content)
	}

	// Test listing snippets
	snippets, err := db.ListSnippets(ctx, 123)
	if err != nil {
		t.Fatalf("list snippets: %v", err)
	}
	if len(snippets) != 1 {
		t.Errorf("expected 1 snippet, got %d", len(snippets))
	}

	// Test deleting a snippet
	if err := db.DeleteSnippet(ctx, 123, "test-snippet"); err != nil {
		t.Fatalf("delete snippet: %v", err)
	}

	// Verify deletion
	deleted, err := db.GetSnippet(ctx, 123, "test-snippet")
	if err != nil {
		t.Fatalf("get deleted snippet: %v", err)
	}
	if deleted != nil {
		t.Error("snippet should have been deleted")
	}
}

func TestDB_Snippet_PerChatIsolation(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create snippet in chat 1
	snippet1 := &Snippet{
		ChatID:  123,
		Name:    "shared-name",
		Content: "chat1-value",
	}
	if err := db.CreateSnippet(ctx, snippet1); err != nil {
		t.Fatalf("create snippet1: %v", err)
	}

	// Create snippet with same name in chat 2
	snippet2 := &Snippet{
		ChatID:  456,
		Name:    "shared-name",
		Content: "chat2-value",
	}
	if err := db.CreateSnippet(ctx, snippet2); err != nil {
		t.Fatalf("create snippet2: %v", err)
	}

	// Verify chat 1 sees its value
	s1, err := db.GetSnippet(ctx, 123, "shared-name")
	if err != nil {
		t.Fatalf("get snippet1: %v", err)
	}
	if s1.Content != "chat1-value" {
		t.Errorf("expected chat1-value, got %s", s1.Content)
	}

	// Verify chat 2 sees its value
	s2, err := db.GetSnippet(ctx, 456, "shared-name")
	if err != nil {
		t.Fatalf("get snippet2: %v", err)
	}
	if s2.Content != "chat2-value" {
		t.Errorf("expected chat2-value, got %s", s2.Content)
	}

	// List snippets for chat 1
	list1, err := db.ListSnippets(ctx, 123)
	if err != nil {
		t.Fatalf("list snippets1: %v", err)
	}
	if len(list1) != 1 {
		t.Errorf("expected 1 snippet in chat1, got %d", len(list1))
	}

	// List snippets for chat 2
	list2, err := db.ListSnippets(ctx, 456)
	if err != nil {
		t.Fatalf("list snippets2: %v", err)
	}
	if len(list2) != 1 {
		t.Errorf("expected 1 snippet in chat2, got %d", len(list2))
	}
}

func TestDB_Snippet_UniqueConstraint(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create first snippet
	snippet1 := &Snippet{
		ChatID:  123,
		Name:    "unique-name",
		Content: "first",
	}
	if err := db.CreateSnippet(ctx, snippet1); err != nil {
		t.Fatalf("create first snippet: %v", err)
	}

	// Try to create duplicate (same chat_id and name)
	snippet2 := &Snippet{
		ChatID:  123,
		Name:    "unique-name",
		Content: "second",
	}
	err := db.CreateSnippet(ctx, snippet2)
	if err == nil {
		t.Error("expected error for duplicate snippet, got nil")
	}
}

