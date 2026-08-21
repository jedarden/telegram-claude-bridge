package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── Helper Function Tests ─────────────────────────────────────────────────────────

func TestColorToName(t *testing.T) {
	tests := []struct {
		input    int
		expected string
	}{
		{ColorActive, "active"},
		{ColorComplete, "complete"},
		{ColorBlocked, "blocked"},
		{ColorError, "error"},
		{ColorReview, "review"},
		{ColorResearch, "research"},
		{999, "unknown (999)"},
		{0, "unknown (0)"},
		{-1, "unknown (-1)"},
	}

	for _, tc := range tests {
		t.Run(tc.expected, func(t *testing.T) {
			got := colorToName(tc.input)
			if got != tc.expected {
				t.Errorf("colorToName(%d) = %q, want %q", tc.input, got, tc.expected)
			}
		})
	}
}

func TestSplitParallelPrompts(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "single prompt",
			input:    "What is 2+2?",
			expected: []string{"What is 2+2?"},
		},
		{
			name:     "two prompts",
			input:    "What is 2+2?\n---\nWhat is 3+3?",
			expected: []string{"What is 2+2?", "What is 3+3?"},
		},
		{
			name:     "multiple prompts",
			input:    "First\n---\nSecond\n---\nThird",
			expected: []string{"First", "Second", "Third"},
		},
		{
			name:     "empty prompts filtered",
			input:    "First\n---\n\n---\nSecond",
			expected: []string{"First", "Second"},
		},
		{
			name:     "whitespace trimmed",
			input:    "  First  \n---\n  Second  ",
			expected: []string{"First", "Second"},
		},
		{
			name:     "only delimiters",
			input:    "\n---\n",
			expected: []string{},
		},
		{
			name:     "multiline prompts",
			input:    "Line 1\nLine 2\n---\nLine 3\nLine 4",
			expected: []string{"Line 1\nLine 2", "Line 3\nLine 4"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitParallelPrompts(tc.input)
			if len(got) != len(tc.expected) {
				t.Fatalf("splitParallelPrompts() returned %d prompts, want %d", len(got), len(tc.expected))
			}
			for i := range got {
				if got[i] != tc.expected[i] {
					t.Errorf("prompt %d = %q, want %q", i, got[i], tc.expected[i])
				}
			}
		})
	}
}

func TestSplitParallelPrompts_ConsumesLineDelimiters(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "delimiter at beginning",
			input: "---\ntext",
			want:  []string{"text"},
		},
		{
			name:  "delimiter at end",
			input: "text\n---",
			want:  []string{"text"},
		},
		{
			name:  "spaces around delimiter",
			input: "first\n --- \nsecond",
			want:  []string{"first", "second"},
		},
		{
			name:  "empty input",
			input: "",
			want:  nil,
		},
		{
			name:  "bare delimiter is content",
			input: "---",
			want:  []string{"---"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := splitParallelPrompts(tc.input)
			if len(got) != len(tc.want) {
				t.Fatalf("splitParallelPrompts() returned %d prompts, want %d: %q", len(got), len(tc.want), got)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("prompt[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// ── Command Handler Setup ─────────────────────────────────────────────────────────────

func newTestCommandHandler(t *testing.T, db *DB) *CommandHandler {
	t.Helper()

	// Create a mock HTTP server for the proxy
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			json.NewEncoder(w).Encode(contract.HealthResponse{
				Version:       "v1.0.0",
				CommitSHA:     "abc123",
				UptimeSeconds: 3600,
			})
		case "/send", "/edit", "/create_topic", "/pin_message", "/close_topic", "/edit_topic":
			json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	h := NewCommandHandler(db, nil, srv.URL, nil, nil, "v1.0.0", "abc123", "2024-01-01")
	t.Cleanup(srv.Close)

	return h
}

func makeUpdate(chatID int64, threadID *int64, messageID int64, text string, userID int64) contract.Update {
	username := fmt.Sprintf("user%d", userID)
	return contract.Update{
		ChatID:    chatID,
		ThreadID:  threadID,
		MessageID: messageID,
		FromUser: contract.FromUser{
			ID:       userID,
			Username: &username,
		},
		Content: &contract.Content{
			Text: &text,
		},
	}
}

func makeCommandUpdate(chatID int64, messageID int64, text string, userID int64) contract.Update {
	command := strings.Fields(text)[0]
	return contract.Update{
		Type:      "message",
		ChatID:    chatID,
		MessageID: messageID,
		FromUser:  contract.FromUser{ID: userID},
		Content: &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     &text,
			Entities: []contract.Entity{{Type: "bot_command", Offset: 0, Length: len([]rune(command))}},
		},
	}
}

func TestCommandHandler_Handle_RoutesCommandsAndParsesArgs(t *testing.T) {
	tests := []struct {
		name           string
		text           string
		setup          func(t *testing.T, db *DB)
		wantReply      string
		wantMaxWorkers int
	}{
		{
			name:      "help command",
			text:      "/help",
			wantReply: "Available commands:",
		},
		{
			name:      "unknown command",
			text:      "/does-not-exist extra words",
			wantReply: "Unknown command: /does-not-exist",
		},
		{
			name: "config command receives parsed setting and value",
			text: "/config max_workers 3",
			setup: func(t *testing.T, db *DB) {
				t.Helper()
				ctx := context.Background()
				if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()}); err != nil {
					t.Fatalf("upsert admin: %v", err)
				}
				if err := db.UpsertGroup(ctx, &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}); err != nil {
					t.Fatalf("upsert group: %v", err)
				}
			},
			wantReply:      "Max workers set to: 3",
			wantMaxWorkers: 3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.setup != nil {
				tc.setup(t, db)
			}

			var sent []contract.SendRequest
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/send" {
					http.NotFound(w, r)
					return
				}
				var req contract.SendRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode send request: %v", err)
					return
				}
				sent = append(sent, req)
				_ = json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 900})
			}))
			defer srv.Close()

			sender, err := NewSender(srv.URL, t.TempDir()+"/sender.db")
			if err != nil {
				t.Fatalf("NewSender: %v", err)
			}
			defer sender.Close()

			h := NewCommandHandler(db, sender, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
			h.Handle(context.Background(), makeCommandUpdate(100, 12, tc.text, 42), func() *Group {
				group, _ := db.GetGroup(context.Background(), 100)
				return group
			}())

			if len(sent) != 1 {
				t.Fatalf("send request count = %d, want 1", len(sent))
			}
			if !strings.Contains(sent[0].Text, tc.wantReply) {
				t.Errorf("reply = %q, want substring %q", sent[0].Text, tc.wantReply)
			}
			if tc.wantMaxWorkers != 0 {
				group, err := db.GetGroup(context.Background(), 100)
				if err != nil {
					t.Fatalf("get group: %v", err)
				}
				if group.MaxWorkers != tc.wantMaxWorkers {
					t.Errorf("MaxWorkers = %d, want %d", group.MaxWorkers, tc.wantMaxWorkers)
				}
			}
		})
	}
}

// ── cmdCWD Tests ─────────────────────────────────────────────────────────────────────

func TestCmdCWD_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a group
	group := &Group{
		ChatID:    100,
		CWD:       "/existing/path",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)

	threadID := int64(1)
	update := makeUpdate(100, &threadID, 100, "/cwd", 12345)

	reply, err := h.cmdCWD(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	expected := "Working directory: /existing/path"
	if reply != expected {
		t.Errorf("reply = %q, want %q", reply, expected)
	}
}

func TestCmdCWD_UnregisteredGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd", 12345)

	reply, err := h.cmdCWD(ctx, update, nil, "")
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "not registered") {
		t.Errorf("reply should mention unregistered group, got: %s", reply)
	}
}

func TestCmdCWD_SetPath_AdminCheck(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create a non-admin user
	user := &AllowedUser{
		UserID:  12345,
		Role:    "user",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/existing/path",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd /new/path", 12345)

	reply, err := h.cmdCWD(ctx, update, group, "/new/path")
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "Permission denied") {
		t.Errorf("non-admin should get permission denied, got: %s", reply)
	}
}

func TestCmdCWD_SetPath_InvalidPath(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create an admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/existing/path",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd /nonexistent/path", 12345)

	reply, err := h.cmdCWD(ctx, update, group, "/nonexistent/path")
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "does not exist") {
		t.Errorf("should report nonexistent path, got: %s", reply)
	}
}

func TestCmdCWD_SetPath_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Create temp directory for testing
	tempDir := t.TempDir()

	// Create an admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/existing/path",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd "+tempDir, 12345)

	reply, err := h.cmdCWD(ctx, update, group, tempDir)
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "Working directory set to") {
		t.Errorf("should confirm path set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.CWD != tempDir {
		t.Errorf("CWD in DB = %q, want %q", updated.CWD, tempDir)
	}
}

func TestCmdCWD_RegisterGroup_SetsSchemaDefaults(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	tempDir := t.TempDir()

	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd "+tempDir, 12345)

	reply, err := h.cmdCWD(ctx, update, nil, tempDir)
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "Working directory set to") {
		t.Errorf("should confirm path set, got: %s", reply)
	}

	// UpsertGroup writes every column explicitly, so the DB DEFAULTs never
	// apply — registration must set the same values itself. A zero
	// progress_interval_sec here silently disables the Phase 8.4 ticker.
	created, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if created.ProgressIntervalSec != 120 {
		t.Errorf("ProgressIntervalSec = %d, want 120 (schema default; 0 disables the ticker)", created.ProgressIntervalSec)
	}
	if created.MaxSubtasks != 5 {
		t.Errorf("MaxSubtasks = %d, want 5 (schema default)", created.MaxSubtasks)
	}
	if created.MaxWorkers != 5 {
		t.Errorf("MaxWorkers = %d, want 5 (schema default)", created.MaxWorkers)
	}
	if created.DispatcherMode != 1 {
		t.Errorf("DispatcherMode = %d, want 1 (schema default)", created.DispatcherMode)
	}
}

func TestCmdCWD_SetPath_PreservesConfiguredSettings(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	tempDir := t.TempDir()

	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:              100,
		CWD:                 "/existing/path",
		DefaultModel:        "claude-opus-4-6",
		PermissionMode:      "dontAsk",
		AllowedTools:        `["Read","Grep"]`,
		DisallowedTools:     `["Bash"]`,
		MaxSubtasks:         7,
		MaxWorkers:          3,
		ProgressIntervalSec: 60,
		DispatcherMode:      0,
		TranscriptVerify:    true,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cwd "+tempDir, 12345)

	reply, err := h.cmdCWD(ctx, update, group, tempDir)
	if err != nil {
		t.Fatalf("cmdCWD: %v", err)
	}

	if !strings.Contains(reply, "Working directory set to") {
		t.Errorf("should confirm path set, got: %s", reply)
	}

	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.CWD != tempDir {
		t.Errorf("CWD in DB = %q, want %q", updated.CWD, tempDir)
	}
	if updated.ProgressIntervalSec != 60 {
		t.Errorf("ProgressIntervalSec = %d, want 60 (path update must not reset it)", updated.ProgressIntervalSec)
	}
	if updated.PermissionMode != "dontAsk" {
		t.Errorf("PermissionMode = %q, want dontAsk", updated.PermissionMode)
	}
	if updated.AllowedTools != `["Read","Grep"]` {
		t.Errorf("AllowedTools = %q, want [\"Read\",\"Grep\"]", updated.AllowedTools)
	}
	if updated.DisallowedTools != `["Bash"]` {
		t.Errorf("DisallowedTools = %q, want [\"Bash\"]", updated.DisallowedTools)
	}
	if updated.MaxSubtasks != 7 {
		t.Errorf("MaxSubtasks = %d, want 7", updated.MaxSubtasks)
	}
	if updated.MaxWorkers != 3 {
		t.Errorf("MaxWorkers = %d, want 3", updated.MaxWorkers)
	}
	if updated.DispatcherMode != 0 {
		t.Errorf("DispatcherMode = %d, want 0", updated.DispatcherMode)
	}
	if !updated.TranscriptVerify {
		t.Error("TranscriptVerify = false, want true")
	}
}

// ── cmdConfig Tests ───────────────────────────────────────────────────────────────────

func TestCmdConfig_ShowAll(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:              100,
		CWD:                 "/test/path",
		DefaultModel:        "claude-sonnet-4-6",
		MaxBudget:           10.0,
		TimeoutSec:          300,
		PermissionMode:      "acceptEdits",
		AllowedTools:        `["Read","Grep"]`,
		DisallowedTools:     `["Bash"]`,
		MaxSubtasks:         5,
		MaxWorkers:          3,
		ProgressIntervalSec: 60,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	// Verify key fields are shown
	if !strings.Contains(reply, "Working Directory:") {
		t.Errorf("reply should show working directory")
	}
	if !strings.Contains(reply, "Default Model:") {
		t.Errorf("reply should show default model")
	}
	if !strings.Contains(reply, "Max Budget:") {
		t.Errorf("reply should show max budget")
	}
	if !strings.Contains(reply, "Allowed Tools:") {
		t.Errorf("reply should show allowed tools")
	}
}

func TestCmdConfig_UnregisteredGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config", 12345)

	reply, err := h.cmdConfig(ctx, update, nil, "")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "not registered") {
		t.Errorf("should mention unregistered group, got: %s", reply)
	}
}

func TestCmdConfig_SetPermissionMode_AdminCheck(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Non-admin user
	user := &AllowedUser{
		UserID:  12345,
		Role:    "user",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config permission_mode dontAsk", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "permission_mode dontAsk")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Permission denied") {
		t.Errorf("non-admin should get permission denied, got: %s", reply)
	}
}

func TestCmdConfig_SetPermissionMode_InvalidMode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config permission_mode invalid_mode", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "permission_mode invalid_mode")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Invalid permission mode") {
		t.Errorf("should reject invalid mode, got: %s", reply)
	}
}

func TestCmdConfig_SetPermissionMode_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		PermissionMode: "acceptEdits",
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config permission_mode bypassPermissions", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "permission_mode bypassPermissions")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Permission mode set to") {
		t.Errorf("should confirm mode set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.PermissionMode != "bypassPermissions" {
		t.Errorf("PermissionMode in DB = %q, want bypassPermissions", updated.PermissionMode)
	}
}

func TestCmdConfig_SetAllowedTools_InvalidJSON(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config allowed_tools invalid_json", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "allowed_tools invalid_json")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "JSON array") {
		t.Errorf("should require JSON array, got: %s", reply)
	}
}

func TestCmdConfig_SetAllowedTools_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config allowed_tools [\"Read\",\"Grep\"]", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "allowed_tools [\"Read\",\"Grep\"]")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Allowed tools set to") {
		t.Errorf("should confirm tools set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.AllowedTools != `["Read","Grep"]` {
		t.Errorf("AllowedTools in DB = %q, want [\"Read\",\"Grep\"]", updated.AllowedTools)
	}
}

func TestCmdConfig_SetAllowedTools_Clear(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:       100,
		CWD:          "/test",
		AllowedTools: `["Read"]`,
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config allowed_tools", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "allowed_tools")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "cleared") {
		t.Errorf("should confirm tools cleared, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.AllowedTools != "" {
		t.Errorf("AllowedTools in DB = %q, want empty (cleared)", updated.AllowedTools)
	}
}

func TestCmdConfig_SetDisallowedTools_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config disallowed_tools [\"Bash\",\"Edit\"]", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "disallowed_tools [\"Bash\",\"Edit\"]")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Disallowed tools set to") {
		t.Errorf("should confirm tools set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.DisallowedTools != `["Bash","Edit"]` {
		t.Errorf("DisallowedTools in DB = %q, want [\"Bash\",\"Edit\"]", updated.DisallowedTools)
	}
}

func TestCmdConfig_SetMaxSubtasks_Invalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)

	tests := []struct {
		name  string
		value string
	}{
		{"negative", "-1"},
		{"invalid", "abc"},
		{"zero", "0"}, // 0 is actually valid
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.value == "0" {
				t.Skip("0 is valid for max_subtasks")
			}
			update := makeUpdate(100, nil, 100, "/config max_subtasks "+tc.value, 12345)

			reply, err := h.cmdConfig(ctx, update, group, "max_subtasks "+tc.value)
			if err != nil {
				t.Fatalf("cmdConfig: %v", err)
			}

			if !strings.Contains(reply, "valid positive integer") {
				t.Errorf("should reject invalid value for %s, got: %s", tc.name, reply)
			}
		})
	}
}

func TestCmdConfig_SetMaxSubtasks_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:      100,
		CWD:         "/test",
		MaxSubtasks: 5,
		CreatedAt:   time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config max_subtasks 10", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "max_subtasks 10")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Max subtasks set to: 10") {
		t.Errorf("should confirm value set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.MaxSubtasks != 10 {
		t.Errorf("MaxSubtasks in DB = %d, want 10", updated.MaxSubtasks)
	}
}

func TestCmdConfig_SetMaxWorkers_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:     100,
		CWD:        "/test",
		MaxWorkers: 5,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config max_workers 3", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "max_workers 3")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Max workers set to: 3") {
		t.Errorf("should confirm value set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.MaxWorkers != 3 {
		t.Errorf("MaxWorkers in DB = %d, want 3", updated.MaxWorkers)
	}
}

func TestCmdConfig_SetProgressInterval_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:              100,
		CWD:                 "/test",
		ProgressIntervalSec: 30,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config progress_interval_sec 60", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "progress_interval_sec 60")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Progress interval set to: 60 seconds") {
		t.Errorf("should confirm value set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.ProgressIntervalSec != 60 {
		t.Errorf("ProgressIntervalSec in DB = %d, want 60", updated.ProgressIntervalSec)
	}
}

func TestCmdConfig_SetProgressInterval_Zero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:              100,
		CWD:                 "/test",
		ProgressIntervalSec: 30,
		CreatedAt:           time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config progress_interval_sec 0", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "progress_interval_sec 0")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "disabled") {
		t.Errorf("should confirm ticker disabled, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.ProgressIntervalSec != 0 {
		t.Errorf("ProgressIntervalSec in DB = %d, want 0 (disabled)", updated.ProgressIntervalSec)
	}
}

func TestCmdConfig_UnknownSetting(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config unknown_setting value", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "unknown_setting value")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Unknown setting") {
		t.Errorf("should reject unknown setting, got: %s", reply)
	}
}

func TestCmdConfig_UsageHint(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/config", 12345)

	reply, err := h.cmdConfig(ctx, update, group, "only_one_arg")
	if err != nil {
		t.Fatalf("cmdConfig: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage hint, got: %s", reply)
	}
}

// ── cmdPermission Tests ────────────────────────────────────────────────────────────────

func TestCmdPermission_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		PermissionMode: "acceptEdits",
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/permission", 12345)

	reply, err := h.cmdPermission(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdPermission: %v", err)
	}

	if !strings.Contains(reply, "Permission mode: acceptEdits") {
		t.Errorf("should show current mode, got: %s", reply)
	}
}

func TestCmdPermission_SetMode_AdminCheck(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Non-admin user
	user := &AllowedUser{
		UserID:  12345,
		Role:    "user",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		PermissionMode: "acceptEdits",
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/permission bypassPermissions", 12345)

	reply, err := h.cmdPermission(ctx, update, group, "bypassPermissions")
	if err != nil {
		t.Fatalf("cmdPermission: %v", err)
	}

	if !strings.Contains(reply, "Permission denied") {
		t.Errorf("non-admin should get permission denied, got: %s", reply)
	}
}

func TestCmdPermission_SetMode_InvalidMode(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		PermissionMode: "acceptEdits",
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/permission invalid_mode", 12345)

	reply, err := h.cmdPermission(ctx, update, group, "invalid_mode")
	if err != nil {
		t.Fatalf("cmdPermission: %v", err)
	}

	if !strings.Contains(reply, "Invalid permission mode") {
		t.Errorf("should reject invalid mode, got: %s", reply)
	}
}

func TestCmdPermission_SetMode_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		PermissionMode: "acceptEdits",
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/permission plan", 12345)

	reply, err := h.cmdPermission(ctx, update, group, "plan")
	if err != nil {
		t.Fatalf("cmdPermission: %v", err)
	}

	if !strings.Contains(reply, "Permission mode set to: plan") {
		t.Errorf("should confirm mode set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.PermissionMode != "plan" {
		t.Errorf("PermissionMode in DB = %q, want plan", updated.PermissionMode)
	}
}

// ── cmdPing Tests ─────────────────────────────────────────────────────────────────────

func TestCmdPing_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Server that responds quickly
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	h := NewCommandHandler(db, nil, srv.URL, nil, nil, "v1.0.0", "abc123", "2024-01-01")

	reply, err := h.cmdPing(ctx)
	if err != nil {
		t.Fatalf("cmdPing: %v", err)
	}

	if !strings.Contains(reply, "pong") {
		t.Errorf("ping reply should contain pong, got: %s", reply)
	}
}

func TestCmdPing_ProxyError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Server that will refuse connections
	h := NewCommandHandler(db, nil, "http://invalid.test:9999", nil, nil, "v1.0.0", "abc123", "2024-01-01")

	_, err := h.cmdPing(ctx)
	if err == nil {
		t.Error("cmdPing with invalid proxy should return error")
	}
}

// ── cmdVersion Tests ───────────────────────────────────────────────────────────────────

func TestCmdVersion_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			json.NewEncoder(w).Encode(contract.HealthResponse{
				Version:       "v2.0.0",
				CommitSHA:     "def456",
				UptimeSeconds: 7200,
			})
		}
	}))
	defer srv.Close()

	h := NewCommandHandler(db, nil, srv.URL, nil, nil, "v1.0.0", "abc123", "2024-01-01")

	reply, err := h.cmdVersion(ctx)
	if err != nil {
		t.Fatalf("cmdVersion: %v", err)
	}

	// Should show bridge and proxy versions
	if !strings.Contains(reply, "Bridge:") {
		t.Errorf("should show bridge version, got: %s", reply)
	}
	if !strings.Contains(reply, "Proxy:") {
		t.Errorf("should show proxy version, got: %s", reply)
	}
	if !strings.Contains(reply, "Contract:") {
		t.Errorf("should show contract version, got: %s", reply)
	}
}

// ── cmdStatus Tests ───────────────────────────────────────────────────────────────────

func TestCmdStatus_NoActiveSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/status", 12345)

	reply, err := h.cmdStatus(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}

	if !strings.Contains(reply, "No active sessions") {
		t.Errorf("should report no active sessions, got: %s", reply)
	}
}

func TestCmdStatus_WithActiveSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create an active session
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Model:          "claude-sonnet-4-6",
		Status:         "active",
		MessageCount:   5,
		LastActive:     time.Now().UTC(),
		LastFromUserID: 12345,
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/status", 12345)

	reply, err := h.cmdStatus(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}

	if !strings.Contains(reply, "Active sessions") {
		t.Errorf("should show active sessions, got: %s", reply)
	}
	if !strings.Contains(reply, "thread 10") {
		t.Errorf("should show thread 10, got: %s", reply)
	}
	if !strings.Contains(reply, "5 messages") {
		t.Errorf("should show message count, got: %s", reply)
	}
}

func TestCmdStatus_UnregisteredGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/status", 12345)

	reply, err := h.cmdStatus(ctx, update, nil)
	if err != nil {
		t.Fatalf("cmdStatus: %v", err)
	}

	if !strings.Contains(reply, "not registered") {
		t.Errorf("should mention unregistered group, got: %s", reply)
	}
}

// ── cmdSessions Tests ─────────────────────────────────────────────────────────────────

func TestCmdSessions_NoSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)

	reply, err := h.cmdSessions(ctx)
	if err != nil {
		t.Fatalf("cmdSessions: %v", err)
	}

	if !strings.Contains(reply, "No sessions found") {
		t.Errorf("should report no sessions, got: %s", reply)
	}
}

func TestCmdSessions_WithSessions(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Sessions have a foreign key on groups(chat_id)
	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create sessions in different states
	for i, status := range []string{"active", "closed", "active"} {
		session := &Session{
			ChatID:         100,
			ThreadID:       int64(i + 10),
			SessionID:      fmt.Sprintf("session-%d", i),
			CWD:            "/test",
			Status:         status,
			MessageCount:   i + 1,
			LastActive:     time.Now().UTC(),
			LastFromUserID: 12345,
			CreatedAt:      time.Now().UTC(),
		}
		if err := db.CreateSession(ctx, session); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	h := newTestCommandHandler(t, db)

	reply, err := h.cmdSessions(ctx)
	if err != nil {
		t.Fatalf("cmdSessions: %v", err)
	}

	if !strings.Contains(reply, "All sessions (3):") {
		t.Errorf("should show 3 sessions, got: %s", reply)
	}
}

// ── cmdColor Tests ───────────────────────────────────────────────────────────────────

func TestCmdColor_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		IconColor: ColorComplete,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/color", 12345)

	reply, err := h.cmdColor(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdColor: %v", err)
	}

	if !strings.Contains(reply, "Current color: complete") {
		t.Errorf("should show current color, got: %s", reply)
	}
}

func TestCmdColor_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/color green", 12345)

	reply, err := h.cmdColor(ctx, update, group, "green")
	if err != nil {
		t.Fatalf("cmdColor: %v", err)
	}

	if !strings.Contains(reply, "only work within a topic") {
		t.Errorf("should require topic, got: %s", reply)
	}
}

func TestCmdColor_SetColor_Invalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/color invalid_color", 12345)

	reply, err := h.cmdColor(ctx, update, group, "invalid_color")
	if err != nil {
		t.Fatalf("cmdColor: %v", err)
	}

	if !strings.Contains(reply, "Invalid color") {
		t.Errorf("should reject invalid color, got: %s", reply)
	}
}

func TestCmdColor_ColorAliases(t *testing.T) {
	tests := []struct {
		input       string
		expectedCol int
	}{
		{"active", ColorActive},
		{"blue", ColorActive},
		{"lightblue", ColorActive},
		{"complete", ColorComplete},
		{"green", ColorComplete},
		{"closed", ColorComplete},
		{"blocked", ColorBlocked},
		{"yellow", ColorBlocked},
		{"error", ColorError},
		{"red", ColorError},
		{"review", ColorReview},
		{"pink", ColorReview},
		{"research", ColorResearch},
		{"purple", ColorResearch},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()

			group := &Group{
				ChatID:    100,
				CWD:       "/test",
				CreatedAt: time.Now().UTC(),
			}
			if err := db.UpsertGroup(ctx, group); err != nil {
				t.Fatalf("upsert group: %v", err)
			}

			threadID := int64(10)
			session := &Session{
				ChatID:    100,
				ThreadID:  10,
				SessionID: fmt.Sprintf("test-session-%s", tc.input),
				CWD:       "/test",
				Status:    "active",
				CreatedAt: time.Now().UTC(),
			}
			if err := db.CreateSession(ctx, session); err != nil {
				t.Fatalf("create session: %v", err)
			}

			h := newTestCommandHandler(t, db)
			update := makeUpdate(100, &threadID, 100, "/color "+tc.input, 12345)

			reply, err := h.cmdColor(ctx, update, group, tc.input)
			if err != nil {
				t.Fatalf("cmdColor: %v", err)
			}

			if !strings.Contains(reply, "set to") {
				t.Errorf("should confirm color set, got: %s", reply)
			}

			// Verify DB was updated
			updated, err := db.GetSession(ctx, 100, 10)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if updated.IconColor != tc.expectedCol {
				t.Errorf("IconColor for %s = %d, want %d", tc.input, updated.IconColor, tc.expectedCol)
			}
		})
	}
}

// ── cmdNotify Tests ──────────────────────────────────────────────────────────────────

func TestCmdNotify_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:           100,
		ThreadID:         10,
		SessionID:        "test-session",
		CWD:              "/test",
		Status:           "active",
		NotificationMode: "live",
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/notify", 12345)

	reply, err := h.cmdNotify(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdNotify: %v", err)
	}

	if !strings.Contains(reply, "Notification mode: streaming") {
		t.Errorf("should show current mode as 'streaming', got: %s", reply)
	}
}

func TestCmdNotify_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/notify quiet", 12345)

	reply, err := h.cmdNotify(ctx, update, group, "quiet")
	if err != nil {
		t.Fatalf("cmdNotify: %v", err)
	}

	if !strings.Contains(reply, "only work within a topic") {
		t.Errorf("should require topic, got: %s", reply)
	}
}

func TestCmdNotify_SetMode_Invalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/notify invalid_mode", 12345)

	reply, err := h.cmdNotify(ctx, update, group, "invalid_mode")
	if err != nil {
		t.Fatalf("cmdNotify: %v", err)
	}

	if !strings.Contains(reply, "Invalid notification mode") {
		t.Errorf("should reject invalid mode, got: %s", reply)
	}
}

func TestCmdNotify_SetMode_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/notify quiet", 12345)

	reply, err := h.cmdNotify(ctx, update, group, "quiet")
	if err != nil {
		t.Fatalf("cmdNotify: %v", err)
	}

	if !strings.Contains(reply, "set to: quiet") {
		t.Errorf("should confirm mode set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.NotificationMode != "quiet" {
		t.Errorf("NotificationMode in DB = %q, want quiet", updated.NotificationMode)
	}
}

func TestCmdNotify_LiveAlias(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/notify live", 12345)

	reply, err := h.cmdNotify(ctx, update, group, "live")
	if err != nil {
		t.Fatalf("cmdNotify: %v", err)
	}

	if !strings.Contains(reply, "set to: streaming") {
		t.Errorf("should confirm mode set to streaming (alias for live), got: %s", reply)
	}

	// Verify DB was updated (stored as "live")
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.NotificationMode != "live" {
		t.Errorf("NotificationMode in DB = %q, want live", updated.NotificationMode)
	}
}

// ── cmdInfo Tests ─────────────────────────────────────────────────────────────────────

func TestCmdInfo_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/info", 12345)

	reply, err := h.cmdInfo(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdInfo: %v", err)
	}

	if !strings.Contains(reply, "only available within a topic") {
		t.Errorf("should require topic, got: %s", reply)
	}
}

func TestCmdInfo_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:           100,
		ThreadID:         10,
		SessionID:        "test-session-123",
		CWD:              "/test",
		Model:            "claude-opus-4-7",
		Status:           "active",
		MessageCount:     15,
		TotalCostUSD:     0.50,
		NotificationMode: "live",
		CreatedAt:        time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/info", 12345)

	reply, err := h.cmdInfo(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdInfo: %v", err)
	}

	// Check key fields are shown
	if !strings.Contains(reply, "Session ID: test-session-123") {
		t.Errorf("should show session ID, got: %s", reply)
	}
	if !strings.Contains(reply, "Model: claude-opus-4-7") {
		t.Errorf("should show model, got: %s", reply)
	}
	if !strings.Contains(reply, "Messages: 15") {
		t.Errorf("should show message count, got: %s", reply)
	}
	if !strings.Contains(reply, "Cost: $0.50") {
		t.Errorf("should show cost, got: %s", reply)
	}
	if !strings.Contains(reply, "Notification mode: streaming") {
		t.Errorf("should show notification mode, got: %s", reply)
	}
}

// ── cmdModel Tests ────────────────────────────────────────────────────────────────────

func TestCmdModel_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:       100,
		CWD:          "/test",
		DefaultModel: "claude-sonnet-4-6",
		CreatedAt:    time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Model:     "claude-opus-4-7",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/model", 12345)

	reply, err := h.cmdModel(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}

	if !strings.Contains(reply, "Current model: claude-opus-4-7") {
		t.Errorf("should show current model, got: %s", reply)
	}
}

func TestCmdModel_SetModel_Shortcuts(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"haiku", "claude-haiku-4-5"},
		{"sonnet", "claude-sonnet-4-6"},
		{"opus", "claude-opus-4-7"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()

			group := &Group{
				ChatID:    100,
				CWD:       "/test",
				CreatedAt: time.Now().UTC(),
			}
			if err := db.UpsertGroup(ctx, group); err != nil {
				t.Fatalf("upsert group: %v", err)
			}

			threadID := int64(10)
			session := &Session{
				ChatID:    100,
				ThreadID:  10,
				SessionID: fmt.Sprintf("test-session-%s", tc.input),
				CWD:       "/test",
				Status:    "active",
				CreatedAt: time.Now().UTC(),
			}
			if err := db.CreateSession(ctx, session); err != nil {
				t.Fatalf("create session: %v", err)
			}

			h := newTestCommandHandler(t, db)
			update := makeUpdate(100, &threadID, 100, "/model "+tc.input, 12345)

			reply, err := h.cmdModel(ctx, update, group, tc.input)
			if err != nil {
				t.Fatalf("cmdModel: %v", err)
			}

			if !strings.Contains(reply, "set to: "+tc.expected) {
				t.Errorf("should confirm model set to %s, got: %s", tc.expected, reply)
			}

			// Verify DB was updated
			updated, err := db.GetSession(ctx, 100, 10)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if updated.Model != tc.expected {
				t.Errorf("Model in DB = %q, want %s", updated.Model, tc.expected)
			}
		})
	}
}

func TestCmdModel_SetModel_Custom(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/model claude-custom-4-0", 12345)

	reply, err := h.cmdModel(ctx, update, group, "claude-custom-4-0")
	if err != nil {
		t.Fatalf("cmdModel: %v", err)
	}

	if !strings.Contains(reply, "set to: claude-custom-4-0") {
		t.Errorf("should accept any model name, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.Model != "claude-custom-4-0" {
		t.Errorf("Model in DB = %q, want claude-custom-4-0", updated.Model)
	}
}

// ── cmdUpdate Tests ───────────────────────────────────────────────────────────────────

func TestCmdUpdate_NoUpdater(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := NewCommandHandler(db, nil, "http://test", nil, nil, "v1.0.0", "abc123", "2024-01-01")
	update := makeUpdate(100, nil, 100, "/update", 12345)

	reply, err := h.cmdUpdate(ctx, update, "")
	if err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}

	if !strings.Contains(reply, "not enabled") {
		t.Errorf("should report updater not enabled, got: %s", reply)
	}
}

func TestCmdUpdate_NotAdmin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Non-admin user
	user := &AllowedUser{
		UserID:  12345,
		Role:    "user",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	// Mock updater
	mockUpdater := &mockUpdaterImpl{}
	h := NewCommandHandler(db, nil, "http://test", mockUpdater, nil, "v1.0.0", "abc123", "2024-01-01")
	update := makeUpdate(100, nil, 100, "/update", 12345)

	reply, err := h.cmdUpdate(ctx, update, "")
	if err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}

	if !strings.Contains(reply, "Permission denied") {
		t.Errorf("non-admin should get permission denied, got: %s", reply)
	}
}

func TestCmdUpdate_CheckOnly(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	// Mock updater that reports no update
	mockUpdater := &mockUpdaterImpl{
		hasUpdate: false,
	}
	h := NewCommandHandler(db, nil, "http://test", mockUpdater, nil, "v1.0.0", "abc123", "2024-01-01")
	update := makeUpdate(100, nil, 100, "/update", 12345)

	reply, err := h.cmdUpdate(ctx, update, "")
	if err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}

	if !strings.Contains(reply, "No updates available") {
		t.Errorf("should report no updates, got: %s", reply)
	}
}

func TestCmdUpdate_UpdateAvailable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	// Mock updater that reports update available
	mockUpdater := &mockUpdaterImpl{
		hasUpdate: true,
		newCommit: "abc123def456",
	}
	h := NewCommandHandler(db, nil, "http://test", mockUpdater, nil, "v1.0.0", "abc123", "2024-01-01")
	update := makeUpdate(100, nil, 100, "/update", 12345)

	reply, err := h.cmdUpdate(ctx, update, "")
	if err != nil {
		t.Fatalf("cmdUpdate: %v", err)
	}

	if !strings.Contains(reply, "Update available") {
		t.Errorf("should report update available, got: %s", reply)
	}
	if !strings.Contains(reply, "abc123de") {
		t.Errorf("should show commit SHA, got: %s", reply)
	}
}

// ── cmdClose Tests ─────────────────────────────────────────────────────────────────────

func TestCmdClose_EmptyArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/close", 12345)

	reply, err := h.cmdClose(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage, got: %s", reply)
	}
}

func TestCmdClose_InvalidThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/close invalid", 12345)

	reply, err := h.cmdClose(ctx, update, group, "invalid")
	if err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	if !strings.Contains(reply, "Invalid thread_id") {
		t.Errorf("should reject invalid thread ID, got: %s", reply)
	}
}

func TestCmdClose_SessionNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/close 999", 12345)

	reply, err := h.cmdClose(ctx, update, group, "999")
	if err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	if !strings.Contains(reply, "No session found") {
		t.Errorf("should report session not found, got: %s", reply)
	}
}

func TestCmdClose_AlreadyClosed(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "closed",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/close 10", 12345)

	reply, err := h.cmdClose(ctx, update, group, "10")
	if err != nil {
		t.Fatalf("cmdClose: %v", err)
	}

	if !strings.Contains(reply, "already closed") {
		t.Errorf("should report already closed, got: %s", reply)
	}
}

// ── cmdTimeout Tests ───────────────────────────────────────────────────────────────────

func TestCmdTimeout_ShowCurrent_NoOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:     100,
		CWD:        "/test",
		TimeoutSec: 300,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:     100,
		ThreadID:   10,
		SessionID:  "test-session",
		CWD:        "/test",
		Status:     "active",
		TimeoutSec: 0, // Using group default
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "using group default of 300 seconds") {
		t.Errorf("should show group default, got: %s", reply)
	}
}

func TestCmdTimeout_ShowCurrent_WithOverride(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:     100,
		CWD:        "/test",
		TimeoutSec: 300,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:     100,
		ThreadID:   10,
		SessionID:  "test-session",
		CWD:        "/test",
		Status:     "active",
		TimeoutSec: 600,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "600 seconds") {
		t.Errorf("should show topic override, got: %s", reply)
	}
}

func TestCmdTimeout_Set_Invalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout invalid", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "invalid")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "Invalid timeout") {
		t.Errorf("should reject invalid value, got: %s", reply)
	}
}

func TestCmdTimeout_Set_Negative(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout -10", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "-10")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "cannot be negative") {
		t.Errorf("should reject negative value, got: %s", reply)
	}
}

func TestCmdTimeout_Set_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout 120", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "120")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "set to: 120 seconds") {
		t.Errorf("should confirm timeout set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.TimeoutSec != 120 {
		t.Errorf("TimeoutSec in DB = %d, want 120", updated.TimeoutSec)
	}
}

func TestCmdTimeout_Set_Zero(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:     100,
		ThreadID:   10,
		SessionID:  "test-session",
		CWD:        "/test",
		Status:     "active",
		TimeoutSec: 300,
		CreatedAt:  time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/timeout 0", 12345)

	reply, err := h.cmdTimeout(ctx, update, group, "0")
	if err != nil {
		t.Fatalf("cmdTimeout: %v", err)
	}

	if !strings.Contains(reply, "disabled") {
		t.Errorf("should confirm timeout disabled, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.TimeoutSec != 0 {
		t.Errorf("TimeoutSec in DB = %d, want 0 (disabled)", updated.TimeoutSec)
	}
}

// ── cmdCancel Tests ───────────────────────────────────────────────────────────────────

func TestCmdCancel_InTopic_NoArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create a session manager mock
	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	threadID := int64(10)
	update := makeUpdate(100, &threadID, 100, "/cancel", 12345)

	reply, err := h.cmdCancel(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdCancel: %v", err)
	}

	// Should report no active request (since we haven't actually started anything)
	if !strings.Contains(reply, "No active request") {
		t.Errorf("should report no active request, got: %s", reply)
	}
}

func TestCmdCancel_InGeneral_NoArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	update := makeUpdate(100, nil, 100, "/cancel", 12345)

	reply, err := h.cmdCancel(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdCancel: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage in general topic, got: %s", reply)
	}
}

func TestCmdCancel_InGeneral_InvalidThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	update := makeUpdate(100, nil, 100, "/cancel invalid", 12345)

	reply, err := h.cmdCancel(ctx, update, group, "invalid")
	if err != nil {
		t.Fatalf("cmdCancel: %v", err)
	}

	if !strings.Contains(reply, "Invalid thread_id") {
		t.Errorf("should reject invalid thread ID, got: %s", reply)
	}
}

// ── cmdContext Tests ──────────────────────────────────────────────────────────────────

func TestCmdContext_EmptyArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	threadID := int64(10)
	update := makeUpdate(100, &threadID, 100, "/context", 12345)

	reply, err := h.cmdContext(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdContext: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage, got: %s", reply)
	}
}

func TestCmdContext_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	update := makeUpdate(100, nil, 100, "/context 12345", 12345)

	reply, err := h.cmdContext(ctx, update, group, "12345")
	if err != nil {
		t.Fatalf("cmdContext: %v", err)
	}

	if !strings.Contains(reply, "only work within a topic") {
		t.Errorf("should require topic, got: %s", reply)
	}
}

func TestCmdContext_TopicNotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	sm := newTestSessionManager(t, db, nil)
	h.SetSessionManager(sm)

	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "test-session",
		CWD:       "/test",
		Status:    "active",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	update := makeUpdate(100, &threadID, 100, "/context nonexistent_topic", 12345)

	reply, err := h.cmdContext(ctx, update, group, "nonexistent_topic")
	if err != nil {
		t.Fatalf("cmdContext: %v", err)
	}

	if !strings.Contains(reply, "Topic not found") {
		t.Errorf("should report topic not found, got: %s", reply)
	}
}

// ── cmdSnippet Tests ──────────────────────────────────────────────────────────────────

func TestCmdSnippet_EmptyArgs(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage, got: %s", reply)
	}
}

func TestCmdSnippet_Delete_EmptyName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet delete", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "delete")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage for delete, got: %s", reply)
	}
}

func TestCmdSnippet_Delete_NotFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet delete nonexistent", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "delete nonexistent")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "not found") {
		t.Errorf("should report snippet not found, got: %s", reply)
	}
}

func TestCmdSnippet_Create_NoContent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet name_only", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "name_only")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Usage:") {
		t.Errorf("should show usage when content missing, got: %s", reply)
	}
}

func TestCmdSnippet_Create_EmptyName(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet  content", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, " content")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "cannot be empty") {
		t.Errorf("should reject empty name, got: %s", reply)
	}
}

func TestCmdSnippet_Create_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet api-key sk-12345", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "api-key sk-12345")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Created snippet") {
		t.Errorf("should confirm creation, got: %s", reply)
	}

	// Verify snippet was created
	snippet, err := db.GetSnippet(ctx, 100, "api-key")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	if snippet == nil {
		t.Fatal("snippet should exist")
	}
	if snippet.Content != "sk-12345" {
		t.Errorf("snippet content = %q, want sk-12345", snippet.Content)
	}
}

func TestCmdSnippet_Update_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create initial snippet
	snippet := &Snippet{
		ChatID:  100,
		Name:    "api-key",
		Content: "old-value",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet api-key new-value", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "api-key new-value")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Updated snippet") {
		t.Errorf("should confirm update, got: %s", reply)
	}

	// Verify snippet was updated
	updated, err := db.GetSnippet(ctx, 100, "api-key")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	if updated.Content != "new-value" {
		t.Errorf("snippet content = %q, want new-value", updated.Content)
	}
}

func TestCmdSnippet_Delete_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create snippet
	snippet := &Snippet{
		ChatID:  100,
		Name:    "to-delete",
		Content: "value",
	}
	if err := db.CreateSnippet(ctx, snippet); err != nil {
		t.Fatalf("create snippet: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippet delete to-delete", 12345)

	reply, err := h.cmdSnippet(ctx, update, group, "delete to-delete")
	if err != nil {
		t.Fatalf("cmdSnippet: %v", err)
	}

	if !strings.Contains(reply, "Deleted snippet") {
		t.Errorf("should confirm deletion, got: %s", reply)
	}

	// Verify snippet was deleted
	deleted, err := db.GetSnippet(ctx, 100, "to-delete")
	if err != nil {
		t.Fatalf("get snippet: %v", err)
	}
	if deleted != nil {
		t.Error("snippet should be deleted")
	}
}

// ── cmdSnippets Tests ─────────────────────────────────────────────────────────────────

func TestCmdSnippets_NoneFound(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippets", 12345)

	reply, err := h.cmdSnippets(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdSnippets: %v", err)
	}

	if !strings.Contains(reply, "No snippets") {
		t.Errorf("should report no snippets, got: %s", reply)
	}
}

func TestCmdSnippets_List(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Create snippets
	for i := 0; i < 3; i++ {
		snippet := &Snippet{
			ChatID:  100,
			Name:    fmt.Sprintf("snippet%d", i),
			Content: fmt.Sprintf("value%d", i),
		}
		if err := db.CreateSnippet(ctx, snippet); err != nil {
			t.Fatalf("create snippet: %v", err)
		}
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/snippets", 12345)

	reply, err := h.cmdSnippets(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdSnippets: %v", err)
	}

	if !strings.Contains(reply, "Context snippets (3):") {
		t.Errorf("should show 3 snippets, got: %s", reply)
	}
	if !strings.Contains(reply, "snippet0:") {
		t.Errorf("should list snippet names, got: %s", reply)
	}
}

// ── cmdCost Tests ─────────────────────────────────────────────────────────────────────

func TestCmdCost_UnregisteredGroup(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/cost", 12345)

	reply, err := h.cmdCost(ctx, update, nil)
	if err != nil {
		t.Fatalf("cmdCost: %v", err)
	}

	if !strings.Contains(reply, "not registered") {
		t.Errorf("should mention unregistered group, got: %s", reply)
	}
}

// ── cmdBudget Tests ───────────────────────────────────────────────────────────────────

func TestCmdBudget_ShowCurrent(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	// Add some cost events
	for i := 0; i < 5; i++ {
		event := &CostEvent{
			ChatID:    100,
			ThreadID:  int64(i + 10),
			CostUSD:   0.5,
			Model:     "claude-sonnet-4-6",
			CreatedAt: time.Now().UTC(),
		}
		if err := db.RecordCostEvent(ctx, event); err != nil {
			t.Fatalf("create cost event: %v", err)
		}
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "Max Budget: $10.00") {
		t.Errorf("should show max budget, got: %s", reply)
	}
	if !strings.Contains(reply, "Current Cost:") {
		t.Errorf("should show current cost, got: %s", reply)
	}
}

func TestCmdBudget_Set_NotAdmin(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Non-admin user
	user := &AllowedUser{
		UserID:  12345,
		Role:    "user",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, user); err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget 20", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "20")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "Permission denied") {
		t.Errorf("non-admin should get permission denied, got: %s", reply)
	}
}

func TestCmdBudget_Set_Invalid(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget invalid", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "invalid")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "Invalid amount") {
		t.Errorf("should reject invalid amount, got: %s", reply)
	}
}

func TestCmdBudget_Set_Negative(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget -10", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "-10")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "cannot be negative") {
		t.Errorf("should reject negative budget, got: %s", reply)
	}
}

func TestCmdBudget_Set_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		MaxBudget: 10.0,
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget 25.50", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "25.50")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "updated to: $25.50") {
		t.Errorf("should confirm budget set, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.MaxBudget != 25.50 {
		t.Errorf("MaxBudget in DB = %f, want 25.50", updated.MaxBudget)
	}
}

func TestCmdBudget_Set_WithDollarPrefix(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Admin user
	admin := &AllowedUser{
		UserID:  12345,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	if err := db.UpsertAllowedUser(ctx, admin); err != nil {
		t.Fatalf("upsert admin: %v", err)
	}

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/budget $50", 12345)

	reply, err := h.cmdBudget(ctx, update, group, "$50")
	if err != nil {
		t.Fatalf("cmdBudget: %v", err)
	}

	if !strings.Contains(reply, "updated to: $50.00") {
		t.Errorf("should handle $ prefix, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetGroup(ctx, 100)
	if err != nil {
		t.Fatalf("get group: %v", err)
	}
	if updated.MaxBudget != 50.0 {
		t.Errorf("MaxBudget in DB = %f, want 50.0", updated.MaxBudget)
	}
}

// ── cmdDispatch Tests ─────────────────────────────────────────────────────────────────

func TestCmdDispatch_NoThreadID(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 100, "/dispatch on", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "on")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "only work within a topic") {
		t.Errorf("should require topic, got: %s", reply)
	}
}

func TestCmdDispatch_ShowCurrent_UsingGroupDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		DispatcherMode: 1, // Enabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Status:         "active",
		DispatcherMode: -1, // Using group default
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/dispatch", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "using group default (enabled)") {
		t.Errorf("should show using group default, got: %s", reply)
	}
}

func TestCmdDispatch_ShowCurrent_OverrideEnabled(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Status:         "active",
		DispatcherMode: 1, // Enabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/dispatch", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "enabled (per-topic override)") {
		t.Errorf("should show override enabled, got: %s", reply)
	}
}

func TestCmdDispatch_Enable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Status:         "active",
		DispatcherMode: 0, // Disabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/dispatch on", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "on")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "Dispatcher mode enabled") {
		t.Errorf("should confirm enabled, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.DispatcherMode != 1 {
		t.Errorf("DispatcherMode in DB = %d, want 1 (enabled)", updated.DispatcherMode)
	}
}

func TestCmdDispatch_Disable(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:    100,
		CWD:       "/test",
		CreatedAt: time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Status:         "active",
		DispatcherMode: 1, // Enabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/dispatch off", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "off")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "Dispatcher mode disabled") {
		t.Errorf("should confirm disabled, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.DispatcherMode != 0 {
		t.Errorf("DispatcherMode in DB = %d, want 0 (disabled)", updated.DispatcherMode)
	}
}

func TestCmdDispatch_ResetToDefault(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{
		ChatID:         100,
		CWD:            "/test",
		DispatcherMode: 1, // Enabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	threadID := int64(10)
	session := &Session{
		ChatID:         100,
		ThreadID:       10,
		SessionID:      "test-session",
		CWD:            "/test",
		Status:         "active",
		DispatcherMode: 0, // Override disabled
		CreatedAt:      time.Now().UTC(),
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, &threadID, 100, "/dispatch default", 12345)

	reply, err := h.cmdDispatch(ctx, update, group, "default")
	if err != nil {
		t.Fatalf("cmdDispatch: %v", err)
	}

	if !strings.Contains(reply, "reset to group default (enabled)") {
		t.Errorf("should confirm reset to group default, got: %s", reply)
	}

	// Verify DB was updated
	updated, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("get session: %v", err)
	}
	if updated.DispatcherMode != -1 {
		t.Errorf("DispatcherMode in DB = %d, want -1 (using default)", updated.DispatcherMode)
	}
}

// ── Mock Implementations ─────────────────────────────────────────────────────────────

type mockUpdaterImpl struct {
	hasUpdate bool
	newCommit string
	updateErr error
}

func (m *mockUpdaterImpl) CheckForUpdates(ctx context.Context) *UpdateResult {
	return &UpdateResult{
		HasUpdate: m.hasUpdate,
		NewCommit: m.newCommit,
		Error:     m.updateErr,
	}
}

func (m *mockUpdaterImpl) ManualUpdate(ctx context.Context, args string) string {
	if args == "do" {
		if m.hasUpdate {
			return "Update applied successfully"
		}
		return "No updates to apply"
	}
	return "Invalid arguments"
}

// ── /new Topic Creation Tests ─────────────────────────────────────────────────────────

func TestCmdNew_Validation(t *testing.T) {
	tests := []struct {
		name      string
		setupDB   func(t *testing.T, db *DB)
		group     func(t *testing.T, db *DB) *Group
		args      string
		wantReply string
	}{
		{
			name:      "usage without args",
			args:      "",
			wantReply: "Usage: /new <topic name>",
		},
		{
			name:      "unregistered group",
			args:      "fix auth",
			wantReply: "This group is not registered. Use /cwd <path> to register it.",
		},
		{
			name: "topic name too long",
			group: func(t *testing.T, db *DB) *Group {
				t.Helper()
				group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
				if err := db.UpsertGroup(context.Background(), group); err != nil {
					t.Fatalf("upsert group: %v", err)
				}
				return group
			},
			args:      strings.Repeat("x", 129),
			wantReply: "Topic name too long (max 128 characters).",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			if tc.setupDB != nil {
				tc.setupDB(t, db)
			}
			group := (*Group)(nil)
			if tc.group != nil {
				group = tc.group(t, db)
			}

			h := newTestCommandHandler(t, db)
			reply, err := h.cmdNew(ctx, makeUpdate(100, nil, 5, "/new "+tc.args, 42), group, tc.args)
			if err != nil {
				t.Fatalf("cmdNew: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
		})
	}
}

func TestCmdNew_Success(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cwd := t.TempDir()
	group := &Group{ChatID: 100, CWD: cwd, DefaultModel: "claude-sonnet-4-6", CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	var mu sync.Mutex
	var gotCreateReq contract.CreateTopicRequest
	var gotPinReq contract.PinMessageRequest
	var sendReqs []contract.SendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/create_topic":
			mu.Lock()
			defer mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&gotCreateReq); err != nil {
				t.Errorf("decode create_topic: %v", err)
			}
			json.NewEncoder(w).Encode(contract.CreateTopicResponse{OK: true, ThreadID: 777, Name: gotCreateReq.Name})
		case "/send":
			mu.Lock()
			defer mu.Unlock()
			var req contract.SendRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode send: %v", err)
			}
			sendReqs = append(sendReqs, req)
			json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 555})
		case "/pin_message":
			mu.Lock()
			defer mu.Unlock()
			if err := json.NewDecoder(r.Body).Decode(&gotPinReq); err != nil {
				t.Errorf("decode pin_message: %v", err)
			}
			json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sender, err := NewSender(srv.URL, t.TempDir()+"/sender.db")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer sender.Close()

	h := NewCommandHandler(db, sender, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
	reply, err := h.cmdNew(ctx, makeUpdate(100, nil, 5, "/new fix auth middleware", 42), group, "fix auth middleware")
	if err != nil {
		t.Fatalf("cmdNew: %v", err)
	}
	if want := "Created topic: fix auth middleware (thread_id: 777)"; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}

	// Session record: created against the thread the proxy returned.
	session, err := db.GetSession(ctx, 100, 777)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if session == nil {
		t.Fatal("expected session for thread 777")
	}
	if session.TopicName != "fix auth middleware" {
		t.Errorf("TopicName = %q, want %q", session.TopicName, "fix auth middleware")
	}
	if session.Status != "active" {
		t.Errorf("Status = %q, want active", session.Status)
	}
	if session.CWD != cwd {
		t.Errorf("CWD = %q, want %q", session.CWD, cwd)
	}
	if session.Model != "claude-sonnet-4-6" {
		t.Errorf("Model = %q, want claude-sonnet-4-6 (group default)", session.Model)
	}
	if session.PinnedMessageID != 555 {
		t.Errorf("PinnedMessageID = %d, want 555", session.PinnedMessageID)
	}

	mu.Lock()
	defer mu.Unlock()
	if gotCreateReq.ChatID != 100 {
		t.Errorf("create_topic chat_id = %d, want 100", gotCreateReq.ChatID)
	}
	if gotCreateReq.IconColor == nil || *gotCreateReq.IconColor != contract.IconColorLightBlue {
		t.Errorf("create_topic icon_color = %v, want IconColorLightBlue", gotCreateReq.IconColor)
	}
	if gotPinReq.MessageID != 555 {
		t.Errorf("pin_message message_id = %d, want 555", gotPinReq.MessageID)
	}
	if len(sendReqs) != 1 {
		t.Fatalf("expected 1 metadata send, got %d", len(sendReqs))
	}
	if sendReqs[0].ThreadID == nil || *sendReqs[0].ThreadID != 777 {
		t.Errorf("metadata send thread_id = %v, want 777", sendReqs[0].ThreadID)
	}
	if !strings.Contains(sendReqs[0].Text, "Project: "+cwd) {
		t.Errorf("metadata text should mention project cwd, got: %q", sendReqs[0].Text)
	}
}

func TestCmdNew_CreateTopicProxyError(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(contract.ErrorResponse{ErrorCode: 400, Description: "Bad Request: chat is not a forum"})
	}))
	defer srv.Close()

	h := NewCommandHandler(db, nil, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
	reply, err := h.cmdNew(ctx, makeUpdate(100, nil, 5, "/new topic", 42), group, "topic")
	if err == nil {
		t.Fatalf("expected error, got reply %q", reply)
	}
	if !strings.Contains(err.Error(), "create topic") {
		t.Errorf("error = %v, want it to mention create topic", err)
	}
	if reply != "" {
		t.Errorf("reply = %q, want empty on error", reply)
	}
}

// ── /close Success Path ───────────────────────────────────────────────────────────────

func TestCmdClose_SuccessWithoutSessionManager(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	threadID := int64(10)
	session := &Session{
		ChatID:    100,
		ThreadID:  10,
		SessionID: "sess-1",
		CWD:       group.CWD,
		Status:    "active",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// No session manager wired: generateSessionSummary fails, which cmdClose
	// must tolerate and close the session anyway.
	h := newTestCommandHandler(t, db)
	reply, err := h.cmdClose(ctx, makeUpdate(100, &threadID, 50, "/close 10", 42), group, "10")
	if err != nil {
		t.Fatalf("cmdClose: %v", err)
	}
	if want := "Session closed and marked complete (thread 10)."; reply != want {
		t.Errorf("reply = %q, want %q", reply, want)
	}

	closed, err := db.GetSession(ctx, 100, 10)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if closed.Status != "closed" {
		t.Errorf("Status = %q, want closed", closed.Status)
	}
	if closed.IconColor != ColorComplete {
		t.Errorf("IconColor = %d, want %d (ColorComplete)", closed.IconColor, ColorComplete)
	}
	if closed.Summary != "" {
		t.Errorf("Summary = %q, want empty when summary generation is unavailable", closed.Summary)
	}
}

// ── User Management Tests (/adduser /removeuser /users) ──────────────────────────────

func TestCmdAddUser(t *testing.T) {
	tests := []struct {
		name      string
		threadID  *int64 // nil = General topic
		senderID  int64
		args      string
		wantReply string
		checkDB   func(t *testing.T, db *DB)
	}{
		{
			name:      "rejected outside general topic",
			threadID:  int64Ptr(5),
			senderID:  42,
			args:      "200",
			wantReply: "User management commands only work in the General topic.",
		},
		{
			name:      "non-admin denied",
			senderID:  43,
			args:      "200",
			wantReply: "Permission denied. Only admins can add users.",
		},
		{
			name:      "usage without args",
			senderID:  42,
			wantReply: "Usage: /adduser <telegram_user_id> [role]",
		},
		{
			name:      "invalid user id",
			senderID:  42,
			args:      "abc",
			wantReply: `Invalid user ID "abc". User ID must be a number.`,
		},
		{
			name:      "invalid role",
			senderID:  42,
			args:      "200 boss",
			wantReply: `Invalid role "boss". Role must be 'admin' or 'user'.`,
		},
		{
			name:      "success with default role",
			senderID:  42,
			args:      "200",
			wantReply: "Added user 200 with role: user",
			checkDB: func(t *testing.T, db *DB) {
				user, err := db.GetAllowedUser(context.Background(), 200)
				if err != nil {
					t.Fatalf("GetAllowedUser: %v", err)
				}
				if user == nil {
					t.Fatal("expected user 200 in allowed_users")
				}
				if user.Role != "user" {
					t.Errorf("role = %q, want user", user.Role)
				}
			},
		},
		{
			name:      "success with admin role",
			senderID:  42,
			args:      "201 admin",
			wantReply: "Added user 201 with role: admin",
			checkDB: func(t *testing.T, db *DB) {
				user, err := db.GetAllowedUser(context.Background(), 201)
				if err != nil {
					t.Fatalf("GetAllowedUser: %v", err)
				}
				if user == nil {
					t.Fatal("expected user 201 in allowed_users")
				}
				if user.Role != "admin" {
					t.Errorf("role = %q, want admin", user.Role)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("seed admin: %v", err)
			}

			h := newTestCommandHandler(t, db)
			reply, err := h.cmdAddUser(ctx, makeUpdate(100, tc.threadID, 1, "/adduser "+tc.args, tc.senderID), tc.args)
			if err != nil {
				t.Fatalf("cmdAddUser: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
			if tc.checkDB != nil {
				tc.checkDB(t, db)
			}
		})
	}
}

func TestCmdRemoveUser(t *testing.T) {
	seedUsers := func(t *testing.T, db *DB) {
		t.Helper()
		ctx := context.Background()
		for _, u := range []AllowedUser{
			{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()},
			{UserID: 200, Role: "user", AddedAt: time.Now().UTC()},
		} {
			if err := db.UpsertAllowedUser(ctx, &u); err != nil {
				t.Fatalf("seed user %d: %v", u.UserID, err)
			}
		}
	}

	tests := []struct {
		name      string
		threadID  *int64
		senderID  int64
		args      string
		wantReply string
		wantGone  int64 // user ID that must no longer exist afterwards (0 = none)
	}{
		{
			name:      "rejected outside general topic",
			threadID:  int64Ptr(5),
			senderID:  42,
			args:      "200",
			wantReply: "User management commands only work in the General topic.",
		},
		{
			name:      "non-admin denied",
			senderID:  200,
			args:      "201",
			wantReply: "Permission denied. Only admins can remove users.",
		},
		{
			name:      "usage without args",
			senderID:  42,
			wantReply: "Usage: /removeuser <telegram_user_id>",
		},
		{
			name:      "invalid user id",
			senderID:  42,
			args:      "not-a-number",
			wantReply: `Invalid user ID "not-a-number". User ID must be a number.`,
		},
		{
			name:      "cannot remove self",
			senderID:  42,
			args:      "42",
			wantReply: "You cannot remove yourself from the allowed users list.",
		},
		{
			name:      "success",
			senderID:  42,
			args:      "200",
			wantReply: "Removed user 200 from the allowed users list.",
			wantGone:  200,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seedUsers(t, db)

			h := newTestCommandHandler(t, db)
			reply, err := h.cmdRemoveUser(ctx, makeUpdate(100, tc.threadID, 1, "/removeuser "+tc.args, tc.senderID), tc.args)
			if err != nil {
				t.Fatalf("cmdRemoveUser: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
			if tc.wantGone != 0 {
				user, err := db.GetAllowedUser(ctx, tc.wantGone)
				if err != nil {
					t.Fatalf("GetAllowedUser: %v", err)
				}
				if user != nil {
					t.Errorf("user %d should have been removed", tc.wantGone)
				}
			}
		})
	}
}

func TestCmdUsers(t *testing.T) {
	tests := []struct {
		name      string
		threadID  *int64
		senderID  int64
		wantReply string
		notWant   string
	}{
		{
			name:      "rejected outside general topic",
			threadID:  int64Ptr(5),
			senderID:  42,
			wantReply: "User management commands only work in the General topic.",
		},
		{
			name:      "non-admin denied",
			senderID:  43,
			wantReply: "Permission denied. Only admins can list users.",
		},
		{
			name:      "lists all users with roles",
			senderID:  42,
			wantReply: "Allowed users (2):",
			notWant:   "", // placeholder, checked below with multiple contains
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			for _, u := range []AllowedUser{
				{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()},
				{UserID: 43, Role: "user", AddedAt: time.Now().UTC()},
			} {
				if err := db.UpsertAllowedUser(ctx, &u); err != nil {
					t.Fatalf("seed user %d: %v", u.UserID, err)
				}
			}

			h := newTestCommandHandler(t, db)
			reply, err := h.cmdUsers(ctx, makeUpdate(100, tc.threadID, 1, "/users", tc.senderID))
			if err != nil {
				t.Fatalf("cmdUsers: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
			if tc.name == "lists all users with roles" {
				for _, want := range []string{"User ID: 42", "User ID: 43", "Role: admin", "Role: user"} {
					if !strings.Contains(reply, want) {
						t.Errorf("reply = %q, want it to contain %q", reply, want)
					}
				}
			}
		})
	}
}

// ── /usage Admin Cost Report Tests ────────────────────────────────────────────────────

func TestCmdUsage(t *testing.T) {
	// seedCosts records events: user 42 → $0.02 across thread 10,
	// user 43 → $0.01 on thread 11.
	seedCosts := func(t *testing.T, db *DB) {
		t.Helper()
		ctx := context.Background()
		events := []CostEvent{
			{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-haiku-4-5", FromUserID: 42, CreatedAt: time.Now().UTC()},
			{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-sonnet-4-6", FromUserID: 42, CreatedAt: time.Now().UTC()},
			{ChatID: 100, ThreadID: 11, CostUSD: 0.01, Model: "claude-haiku-4-5", FromUserID: 43, CreatedAt: time.Now().UTC()},
		}
		for i := range events {
			if err := db.RecordCostEvent(ctx, &events[i]); err != nil {
				t.Fatalf("record cost event: %v", err)
			}
		}
	}

	seedUsers := func(t *testing.T, db *DB) {
		t.Helper()
		ctx := context.Background()
		for _, u := range []AllowedUser{
			{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()},
			{UserID: 43, Role: "user", AddedAt: time.Now().UTC()},
		} {
			if err := db.UpsertAllowedUser(ctx, &u); err != nil {
				t.Fatalf("seed user %d: %v", u.UserID, err)
			}
		}
	}

	tests := []struct {
		name      string
		senderID  int64
		args      string
		withCosts bool
		want      []string
	}{
		{
			name:     "non-admin denied",
			senderID: 43,
			args:     "",
			want:     []string{"Permission denied. Only admins can view usage statistics."},
		},
		{
			name:     "invalid user id argument",
			senderID: 42,
			args:     "abc",
			want:     []string{`Invalid user ID "abc"`, "numeric user ID"},
		},
		{
			name:     "no usage data",
			senderID: 42,
			args:     "",
			want:     []string{"No usage data found for any users."},
		},
		{
			name:     "all users summary sorted by cost",
			senderID: 42,
			args:     "",
			withCosts: true,
			want: []string{
				"Usage Report — All Users",
				"Total Cost: $0.0300",
				"User 42 [admin]: $0.0200 (66.7%)",
				"User 43 [user]: $0.0100 (33.3%)",
			},
		},
		{
			name:     "per-user detail",
			senderID: 42,
			args:     "42",
			withCosts: true,
			want: []string{
				"Usage Report — User 42",
				"Role: admin",
				"Total Cost: $0.0200",
				"chat 100/thread 10",
			},
		},
		{
			name:     "unknown user detail",
			senderID: 42,
			args:     "999",
			want:     []string{"User 999 not found in allowed users list."},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			seedUsers(t, db)
			if tc.withCosts {
				seedCosts(t, db)
			}

			h := newTestCommandHandler(t, db)
			reply, err := h.cmdUsage(ctx, makeUpdate(100, nil, 1, "/usage "+tc.args, tc.senderID), nil, tc.args)
			if err != nil {
				t.Fatalf("cmdUsage: %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(reply, want) {
					t.Errorf("reply missing %q\nreply: %s", want, reply)
				}
			}
		})
	}
}

// ── /cost Report Tests ────────────────────────────────────────────────────────────────

func TestCmdCost_GroupLevelReport(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	events := []CostEvent{
		{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-haiku-4-5", FromUserID: 42, CreatedAt: time.Now().UTC()},
		{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-sonnet-4-6", FromUserID: 43, CreatedAt: time.Now().UTC()},
		{ChatID: 100, ThreadID: 11, CostUSD: 0.01, Model: "claude-haiku-4-5", FromUserID: 42, CreatedAt: time.Now().UTC()},
	}
	for i := range events {
		if err := db.RecordCostEvent(ctx, &events[i]); err != nil {
			t.Fatalf("record cost event: %v", err)
		}
	}

	h := newTestCommandHandler(t, db)
	// General topic (no thread) → group-level breakdown.
	reply, err := h.cmdCost(ctx, makeUpdate(100, nil, 1, "/cost", 42), group)
	if err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	for _, want := range []string{
		"Group Cost Report",
		"Total Cost: $0.0300",
		"Thread 10: $0.0200 (2 events)",
		"Thread 11: $0.0100 (1 events)",
		"User 42: $0.0200 (2 events)",
		"User 43: $0.0100 (1 events)",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q\nreply: %s", want, reply)
		}
	}
	if !strings.Contains(reply, "Daily trend") {
		t.Errorf("reply should include daily trend section, got: %s", reply)
	}
}

func TestCmdCost_TopicLevelReport(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	threadID := int64(10)
	session := &Session{ChatID: 100, ThreadID: 10, SessionID: "sess-10", CWD: group.CWD, Status: "active"}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}
	events := []CostEvent{
		{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-haiku-4-5", FromUserID: 42, CreatedAt: time.Now().UTC()},
		{ChatID: 100, ThreadID: 10, CostUSD: 0.01, Model: "claude-sonnet-4-6", FromUserID: 43, CreatedAt: time.Now().UTC()},
		// Noise: another thread's cost must not appear.
		{ChatID: 100, ThreadID: 11, CostUSD: 0.05, Model: "claude-opus-4-6", FromUserID: 42, CreatedAt: time.Now().UTC()},
	}
	for i := range events {
		if err := db.RecordCostEvent(ctx, &events[i]); err != nil {
			t.Fatalf("record cost event: %v", err)
		}
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdCost(ctx, makeUpdate(100, &threadID, 1, "/cost", 42), group)
	if err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	for _, want := range []string{
		"Topic Cost: $0.0200",
		"Session: sess-10",
		"User 42: $0.0100 (1 events)",
		"User 43: $0.0100 (1 events)",
	} {
		if !strings.Contains(reply, want) {
			t.Errorf("reply missing %q\nreply: %s", want, reply)
		}
	}
	if strings.Contains(reply, "Thread 11") {
		t.Errorf("topic-level report must not include other threads' costs, got: %s", reply)
	}
}

func TestCmdCost_BudgetWarning(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), MaxBudget: 1.0, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	event := CostEvent{ChatID: 100, ThreadID: 10, CostUSD: 0.9, Model: "claude-haiku-4-5", FromUserID: 42, CreatedAt: time.Now().UTC()}
	if err := db.RecordCostEvent(ctx, &event); err != nil {
		t.Fatalf("record cost event: %v", err)
	}

	h := newTestCommandHandler(t, db)
	reply, err := h.cmdCost(ctx, makeUpdate(100, nil, 1, "/cost", 42), group)
	if err != nil {
		t.Fatalf("cmdCost: %v", err)
	}
	if !strings.Contains(reply, "$1.00 budget (90.0% used)") {
		t.Errorf("reply should show budget usage percentage, got: %s", reply)
	}
	if !strings.Contains(reply, "Approaching budget limit (80%)") {
		t.Errorf("reply should warn at 90%% of budget, got: %s", reply)
	}
}

// ── /parallel Subtask Orchestration Tests ─────────────────────────────────────────────

func TestCmdParallel_Validation(t *testing.T) {
	tests := []struct {
		name      string
		setup     func(t *testing.T, h *CommandHandler, db *DB) (*Group, contract.Update)
		args      string
		wantReply string
	}{
		{
			name:      "usage without args",
			args:      "",
			wantReply: "Usage: /parallel <prompt1>",
		},
		{
			name: "requires topic session",
			setup: func(t *testing.T, h *CommandHandler, db *DB) (*Group, contract.Update) {
				return nil, makeUpdate(100, nil, 1, "/parallel p1", 42)
			},
			args:      "p1",
			wantReply: "Parallel commands only work within a topic session",
		},
		{
			name: "unregistered group",
			setup: func(t *testing.T, h *CommandHandler, db *DB) (*Group, contract.Update) {
				return nil, makeUpdate(100, int64Ptr(10), 1, "/parallel p1", 42)
			},
			args:      "p1",
			wantReply: "This group is not registered. Use /cwd <path> to register it.",
		},
		{
			name: "no orchestrator wired",
			setup: func(t *testing.T, h *CommandHandler, db *DB) (*Group, contract.Update) {
				group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
				if err := db.UpsertGroup(context.Background(), group); err != nil {
					t.Fatalf("upsert group: %v", err)
				}
				return group, makeUpdate(100, int64Ptr(10), 1, "/parallel p1", 42)
			},
			args:      "p1",
			wantReply: "Subtask orchestrator not available.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			h := newTestCommandHandler(t, db)

			var group *Group
			update := makeUpdate(100, int64Ptr(10), 1, "/parallel "+tc.args, 42)
			if tc.setup != nil {
				group, update = tc.setup(t, h, db)
			}

			reply, err := h.cmdParallel(ctx, update, group, tc.args)
			if err != nil {
				t.Fatalf("cmdParallel: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
		})
	}
}

func TestCmdParallel_TooManyPrompts(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	h.SetSubtaskOrchestrator(NewSubtaskOrchestrator(db, nil, nil))

	args := "1\n---\n2\n---\n3\n---\n4\n---\n5\n---\n6"
	reply, err := h.cmdParallel(ctx, makeUpdate(100, int64Ptr(10), 1, "/parallel ...", 42), group, args)
	if err != nil {
		t.Fatalf("cmdParallel: %v", err)
	}
	if !strings.Contains(reply, "Maximum 5 prompts allowed.") {
		t.Errorf("reply = %q, want maximum-prompts rejection", reply)
	}

	// No subtask rows should have been created for the rejected batch.
	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected no subtasks created, got %d", len(subtasks))
	}
}

func TestCmdParallel_OrchestratorRejectsOverGroupLimit(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	// Group-level cap of 1 subtask; the request has 2 prompts. The
	// orchestrator rejects it synchronously, before any worker spawn.
	group := &Group{ChatID: 100, CWD: t.TempDir(), MaxSubtasks: 1, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}

	h := newTestCommandHandler(t, db)
	h.SetSubtaskOrchestrator(NewSubtaskOrchestrator(db, nil, nil))

	args := "first prompt\n---\nsecond prompt"
	reply, err := h.cmdParallel(ctx, makeUpdate(100, int64Ptr(10), 1, "/parallel ...", 42), group, args)
	if err == nil {
		t.Fatalf("expected orchestrator rejection, got reply %q", reply)
	}
	if !strings.Contains(err.Error(), "start parallel tasks") ||
		!strings.Contains(err.Error(), "group max_subtasks is 1") {
		t.Errorf("error = %v, want start parallel tasks / max_subtasks rejection", err)
	}

	subtasks, err := db.ListSubtasks(ctx, 100, 10)
	if err != nil {
		t.Fatalf("ListSubtasks: %v", err)
	}
	if len(subtasks) != 0 {
		t.Errorf("expected no subtasks created, got %d", len(subtasks))
	}
}

// ── /bg /jobs /kill Background Job Tests ──────────────────────────────────────────────

func TestCmdBG_Validation(t *testing.T) {
	tests := []struct {
		name      string
		withMgr   bool
		group     *Group
		args      string
		wantReply string
	}{
		{
			name:      "usage without args",
			args:      "",
			wantReply: "Usage: /bg <command>",
		},
		{
			name:      "unregistered group",
			args:      "echo hi",
			wantReply: "This group is not registered. Use /cwd <path> to register it.",
		},
		{
			name:      "no background job manager wired",
			withMgr:   false,
			group:     &Group{ChatID: 100, CWD: ".", CreatedAt: time.Now().UTC()},
			args:      "echo hi",
			wantReply: "Background job manager not available.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			ctx := context.Background()
			h := newTestCommandHandler(t, db)
			if tc.withMgr {
				h.SetBackgroundJobManager(NewBackgroundJobManager(db, nil))
			}

			reply, err := h.cmdBG(ctx, makeUpdate(100, nil, 1, "/bg "+tc.args, 42), tc.group, tc.args)
			if err != nil {
				t.Fatalf("cmdBG: %v", err)
			}
			if !strings.Contains(reply, tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", reply, tc.wantReply)
			}
		})
	}
}

func TestCmdBG_JobLifecycle(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	cwd := t.TempDir()
	group := &Group{ChatID: 100, CWD: cwd, CreatedAt: time.Now().UTC()}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("upsert group: %v", err)
	}
	threadID := int64(10)

	// The job's streaming messages are sent asynchronously by the job manager
	// goroutine, so the mock proxy's state is mutex-guarded.
	var mu sync.Mutex
	var sends int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sends++
		mu.Unlock()
		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 900})
	}))
	defer srv.Close()

	sender, err := NewSender(srv.URL, t.TempDir()+"/sender.db")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer sender.Close()

	h := NewCommandHandler(db, sender, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
	mgr := NewBackgroundJobManager(db, sender)
	h.SetBackgroundJobManager(mgr)

	// The real `sleep` binary is available: TestMain only poisons tmux/claude.
	update := makeUpdate(100, &threadID, 1, "/bg sleep 5", 42)
	reply, err := h.cmdBG(ctx, update, group, "sleep 5")
	if err != nil {
		t.Fatalf("cmdBG: %v", err)
	}
	// Reply format: "Background job started: `sleep 5`\nJob ID: `<id>`"
	parts := strings.Split(reply, "`")
	if len(parts) != 5 { // trailing backtick leaves a final empty segment
		t.Fatalf("reply = %q, want job-started format with 2 backtick-quoted fields", reply)
	}
	jobID := parts[3]
	if !strings.Contains(reply, "Background job started: `sleep 5`") {
		t.Errorf("reply = %q, want it to confirm the command", reply)
	}

	// /jobs lists the running job.
	jobsReply, err := h.cmdJobs(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdJobs: %v", err)
	}
	if !strings.Contains(jobsReply, "Background jobs (1):") {
		t.Errorf("jobs reply should list 1 job, got: %s", jobsReply)
	}
	if !strings.Contains(jobsReply, "`"+jobID+"`] sleep 5") {
		t.Errorf("jobs reply should include job %s running sleep 5, got: %s", jobID, jobsReply)
	}
	if !strings.Contains(jobsReply, "▶") {
		t.Errorf("running job should use the ▶ icon, got: %s", jobsReply)
	}

	// Kill a bogus job ID.
	bogusReply, err := h.cmdKill(ctx, update, "deadbeef")
	if err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if !strings.Contains(bogusReply, "Failed to kill job") {
		t.Errorf("bogus kill reply = %q, want failure", bogusReply)
	}

	// Kill the live job. The manager registers the process shortly after
	// cmdBG returns, so retry briefly until the kill lands.
	var killReply string
	deadline := time.Now().Add(5 * time.Second)
	for {
		killReply, err = h.cmdKill(ctx, update, jobID)
		if err != nil {
			t.Fatalf("cmdKill: %v", err)
		}
		if strings.Contains(killReply, "killed.") || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(killReply, "Job `"+jobID+"` killed.") {
		t.Fatalf("kill reply = %q, want confirmation for job %s", killReply, jobID)
	}

	// A second kill of the same job must fail: it is no longer tracked.
	secondReply, err := h.cmdKill(ctx, update, jobID)
	if err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if !strings.Contains(secondReply, "Failed to kill job") {
		t.Errorf("second kill reply = %q, want failure (job no longer running)", secondReply)
	}

	// The finished job remains listed (history) with a non-running icon.
	finalReply, err := h.cmdJobs(ctx, update, group)
	if err != nil {
		t.Fatalf("cmdJobs: %v", err)
	}
	if !strings.Contains(finalReply, "Background jobs (1):") {
		t.Errorf("final jobs reply should still list the job, got: %s", finalReply)
	}
	if !strings.Contains(finalReply, "■") {
		t.Errorf("killed job should use the ■ icon, got: %s", finalReply)
	}
}

func TestCmdJobs_Validation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)

	// Unregistered group.
	reply, err := h.cmdJobs(ctx, makeUpdate(100, nil, 1, "/jobs", 42), nil)
	if err != nil {
		t.Fatalf("cmdJobs: %v", err)
	}
	if !strings.Contains(reply, "This group is not registered.") {
		t.Errorf("reply = %q, want unregistered-group message", reply)
	}

	// No manager wired.
	group := &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}
	reply, err = h.cmdJobs(ctx, makeUpdate(100, nil, 1, "/jobs", 42), group)
	if err != nil {
		t.Fatalf("cmdJobs: %v", err)
	}
	if !strings.Contains(reply, "Background job manager not available.") {
		t.Errorf("reply = %q, want no-manager message", reply)
	}

	// Registered group + manager + no jobs.
	h.SetBackgroundJobManager(NewBackgroundJobManager(db, nil))
	reply, err = h.cmdJobs(ctx, makeUpdate(100, nil, 1, "/jobs", 42), group)
	if err != nil {
		t.Fatalf("cmdJobs: %v", err)
	}
	if !strings.Contains(reply, "No background jobs for this topic.") {
		t.Errorf("reply = %q, want empty-list message", reply)
	}
}

func TestCmdKill_Validation(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()

	h := newTestCommandHandler(t, db)
	update := makeUpdate(100, nil, 1, "/kill", 42)

	// Usage without args.
	reply, err := h.cmdKill(ctx, update, "")
	if err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if !strings.Contains(reply, "Usage: /kill <job_id>") {
		t.Errorf("reply = %q, want usage message", reply)
	}

	// No manager wired.
	reply, err = h.cmdKill(ctx, update, "abc123")
	if err != nil {
		t.Fatalf("cmdKill: %v", err)
	}
	if !strings.Contains(reply, "Background job manager not available.") {
		t.Errorf("reply = %q, want no-manager message", reply)
	}
}

// ── Handle() Routing for Model Shortcuts and Admin Commands ──────────────────────────

func makeThreadCommandUpdate(chatID, threadID, messageID int64, text string, userID int64) contract.Update {
	command := strings.Fields(text)[0]
	return contract.Update{
		Type:      "message",
		ChatID:    chatID,
		ThreadID:  &threadID,
		MessageID: messageID,
		FromUser:  contract.FromUser{ID: userID},
		Content: &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     &text,
			Entities: []contract.Entity{{Type: "bot_command", Offset: 0, Length: len([]rune(command))}},
		},
	}
}

func TestCommandHandler_Handle_ModelShortcutsAndAdminCommands(t *testing.T) {
	seedAdminAndGroup := func(t *testing.T, db *DB) {
		t.Helper()
		ctx := context.Background()
		if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 42, Role: "admin", AddedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("upsert admin: %v", err)
		}
		if err := db.UpsertAllowedUser(ctx, &AllowedUser{UserID: 200, Role: "user", AddedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("upsert user: %v", err)
		}
		if err := db.UpsertGroup(ctx, &Group{ChatID: 100, CWD: t.TempDir(), CreatedAt: time.Now().UTC()}); err != nil {
			t.Fatalf("upsert group: %v", err)
		}
	}
	seedSession := func(t *testing.T, db *DB) {
		t.Helper()
		if err := db.CreateSession(context.Background(), &Session{
			ChatID: 100, ThreadID: 10, SessionID: "sess-10", CWD: "/test", Status: "active",
		}); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	tests := []struct {
		name      string
		setup     func(t *testing.T, db *DB)
		text      string
		wantReply string
	}{
		{
			name:  "haiku shortcut sets model",
			setup: func(t *testing.T, db *DB) { seedAdminAndGroup(t, db); seedSession(t, db) },
			text:  "/haiku",
			wantReply: "Model set to: claude-haiku-4-5",
		},
		{
			name:  "sonnet shortcut sets model",
			setup: func(t *testing.T, db *DB) { seedAdminAndGroup(t, db); seedSession(t, db) },
			text:  "/sonnet",
			wantReply: "Model set to: claude-sonnet-4-6",
		},
		{
			name:  "opus shortcut sets model",
			setup: func(t *testing.T, db *DB) { seedAdminAndGroup(t, db); seedSession(t, db) },
			// Note: Handle routes /opus to "claude-opus-4-6" directly, so the
			// cmdModel alias table (which expands "opus" to claude-opus-4-7)
			// does not apply on this path.
			text:      "/opus",
			wantReply: "Model set to: claude-opus-4-6",
		},
		{
			name:      "adduser routes and parses args",
			setup:     seedAdminAndGroup,
			text:      "/adduser 300 admin",
			wantReply: "Added user 300 with role: admin",
		},
		{
			name:      "removeuser routes and parses args",
			setup:     seedAdminAndGroup,
			text:      "/removeuser 200",
			wantReply: "Removed user 200 from the allowed users list.",
		},
		{
			name:      "users routes",
			setup:     seedAdminAndGroup,
			text:      "/users",
			wantReply: "Allowed users (2):",
		},
		{
			name:      "usage routes with no data",
			setup:     seedAdminAndGroup,
			text:      "/usage",
			wantReply: "No usage data found for any users.",
		},
		{
			name:      "usage routes with user id arg",
			setup:     seedAdminAndGroup,
			text:      "/usage 42",
			wantReply: "Usage Report — User 42",
		},
		{
			name:  "new routes without registration",
			text:  "/new some topic",
			wantReply: "This group is not registered. Use /cwd <path> to register it.",
		},
		{
			name:  "close routes with args",
			setup: seedAdminAndGroup,
			text:  "/close notanumber",
			wantReply: `Invalid thread_id "notanumber"`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := openTestDB(t)
			if tc.setup != nil {
				tc.setup(t, db)
			}

			var mu sync.Mutex
			var replies []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/send" {
					http.NotFound(w, r)
					return
				}
				var req contract.SendRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Errorf("decode send request: %v", err)
					return
				}
				mu.Lock()
				replies = append(replies, req.Text)
				mu.Unlock()
				_ = json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 900})
			}))
			defer srv.Close()

			sender, err := NewSender(srv.URL, t.TempDir()+"/sender.db")
			if err != nil {
				t.Fatalf("NewSender: %v", err)
			}
			defer sender.Close()

			h := NewCommandHandler(db, sender, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
			// Model shortcuts and /close need a thread; user-management
			// commands need the General topic. Both use thread 10 only when a
			// session was seeded; otherwise the update has no thread.
			var update contract.Update
			if strings.HasPrefix(tc.text, "/haiku") || strings.HasPrefix(tc.text, "/sonnet") ||
				strings.HasPrefix(tc.text, "/opus") || strings.HasPrefix(tc.text, "/close") {
				update = makeThreadCommandUpdate(100, 10, 12, tc.text, 42)
			} else {
				update = makeCommandUpdate(100, 12, tc.text, 42)
			}
			group, _ := db.GetGroup(context.Background(), 100)
			h.Handle(context.Background(), update, group)

			mu.Lock()
			defer mu.Unlock()
			if len(replies) != 1 {
				t.Fatalf("expected 1 reply sent, got %d", len(replies))
			}
			if !strings.Contains(replies[0], tc.wantReply) {
				t.Errorf("reply = %q, want it to contain %q", replies[0], tc.wantReply)
			}
		})
	}
}

func TestCommandHandler_Handle_WrapsHandlerErrors(t *testing.T) {
	db := openTestDB(t)

	var mu sync.Mutex
	var replies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			// Abort the connection so cmdPing's health check fails at the
			// transport layer while /send keeps working.
			panic(http.ErrAbortHandler)
		case "/send":
			var req contract.SendRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode send request: %v", err)
				return
			}
			mu.Lock()
			replies = append(replies, req.Text)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 900})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	sender, err := NewSender(srv.URL, t.TempDir()+"/sender.db")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	defer sender.Close()

	h := NewCommandHandler(db, sender, srv.URL, nil, nil, "1.0.0", "abc123", "2024-01-01")
	h.Handle(context.Background(), makeCommandUpdate(100, 12, "/ping", 42), nil)

	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 1 {
		t.Fatalf("expected 1 reply sent, got %d", len(replies))
	}
	if !strings.Contains(replies[0], "Error: health check failed") {
		t.Errorf("reply = %q, want wrapped error mentioning health check", replies[0])
	}
}
