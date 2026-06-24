package bridge

import (
	"context"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// TestNotifyCommandStreamingAlias verifies that the /notify command
// accepts "streaming" as an alias for "live" mode.
func TestNotifyCommandStreamingAlias(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      10.0,
		TimeoutSec:     300,
		PermissionMode: "dontAsk",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}

	// Create a test session
	session := &Session{
		ChatID:          123,
		ThreadID:        456,
		SessionID:       "test-session",
		CWD:             "/test",
		Model:           "claude-sonnet-4-6",
		Status:          "active",
		NotificationMode: "",
	}
	if err := db.CreateSession(ctx, session); err != nil {
		t.Fatalf("create session: %v", err)
	}

	// Create a command handler
	handler := NewCommandHandler(db, nil, "http://test-proxy", nil, nil, "v1.0.0", "abc123", "2024-01-01")

	tests := []struct {
		name        string
		args        string
		wantReply   string
		wantMode    string
	}{
		{
			name:      "set streaming mode",
			args:      "streaming",
			wantReply: "Notification mode set to: streaming",
			wantMode:  "live", // Stored as "live" internally
		},
		{
			name:      "set live mode (alias)",
			args:      "live",
			wantReply: "Notification mode set to: streaming", // Displayed as "streaming"
			wantMode:  "live", // Stored as "live" internally
		},
		{
			name:      "set summary mode",
			args:      "summary",
			wantReply: "Notification mode set to: summary",
			wantMode:  "summary",
		},
		{
			name:      "set quiet mode",
			args:      "quiet",
			wantReply: "Notification mode set to: quiet",
			wantMode:  "quiet",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			threadID := int64(456)
			text := "/notify " + tt.args
			update := contract.Update{
				ChatID:    123,
				MessageID: 1,
				ThreadID:  &threadID,
				FromUser:  contract.FromUser{ID: 1},
				Content:   &contract.Content{Type: contract.ContentTypeText, Text: &text},
			}

			reply, err := handler.cmdNotify(ctx, update, group, tt.args)
			if err != nil {
				t.Fatalf("cmdNotify error: %v", err)
			}
			if reply != tt.wantReply {
				t.Errorf("reply = %q, want %q", reply, tt.wantReply)
			}

			// Verify the mode was stored correctly
			sess, err := db.GetSession(ctx, 123, 456)
			if err != nil {
				t.Fatalf("get session: %v", err)
			}
			if sess.NotificationMode != tt.wantMode {
				t.Errorf("stored mode = %q, want %q", sess.NotificationMode, tt.wantMode)
			}
		})
	}
}

// TestNotifyCommandShowMode verifies that /notify without args
// displays the current mode correctly (showing "streaming" for "live").
func TestNotifyCommandShowMode(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	ctx := context.Background()

	// Create a test group
	group := &Group{
		ChatID:         123,
		CWD:            "/test",
		DefaultModel:   "claude-sonnet-4-6",
		MaxBudget:      10.0,
		TimeoutSec:     300,
		PermissionMode: "dontAsk",
	}
	if err := db.UpsertGroup(ctx, group); err != nil {
		t.Fatalf("create group: %v", err)
	}

	handler := NewCommandHandler(db, nil, "http://test-proxy", nil, nil, "v1.0.0", "abc123", "2024-01-01")

	tests := []struct {
		name              string
		initialMode       string
		wantDisplayedMode string
	}{
		{
			name:              "empty mode defaults to streaming",
			initialMode:       "",
			wantDisplayedMode: "streaming",
		},
		{
			name:              "live mode displays as streaming",
			initialMode:       "live",
			wantDisplayedMode: "streaming",
		},
		{
			name:              "summary mode displays as summary",
			initialMode:       "summary",
			wantDisplayedMode: "summary",
		},
		{
			name:              "quiet mode displays as quiet",
			initialMode:       "quiet",
			wantDisplayedMode: "quiet",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Use unique thread IDs for each test case
			threadID := int64(500 + i)

			// Create a test session with the initial mode
			session := &Session{
				ChatID:          123,
				ThreadID:        threadID,
				SessionID:       "test-session",
				CWD:             "/test",
				Model:           "claude-sonnet-4-6",
				Status:          "active",
				NotificationMode: tt.initialMode,
			}
			if err := db.CreateSession(ctx, session); err != nil {
				t.Fatalf("create session: %v", err)
			}

			text := "/notify"
			update := contract.Update{
				ChatID:    123,
				MessageID: 1,
				ThreadID:  &threadID,
				FromUser:  contract.FromUser{ID: 1},
				Content:   &contract.Content{Type: contract.ContentTypeText, Text: &text},
			}

			reply, err := handler.cmdNotify(ctx, update, group, "")
			if err != nil {
				t.Fatalf("cmdNotify error: %v", err)
			}

			// Check that the reply contains the expected mode
			expectedPrefix := "Notification mode: " + tt.wantDisplayedMode
			if reply[:len(expectedPrefix)] != expectedPrefix {
				t.Errorf("reply prefix = %q, want %q", reply[:len(expectedPrefix)], expectedPrefix)
			}
		})
	}
}
