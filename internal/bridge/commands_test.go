package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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

			// Clean up for next test
			db.Close()
			db = openTestDB(t)
			group.ChatID = 100
			db.UpsertGroup(ctx, group)
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

			// Clean up for next test
			db.Close()
			db = openTestDB(t)
			group.ChatID = 100
			db.UpsertGroup(ctx, group)
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
