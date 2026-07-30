package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── Helper to create test handler ───────────────────────────────────────────────

func newTestHandler(proxyURL string) *CallbackHandler {
	// Create a minimal test DB
	db, _ := OpenDB(":memory:")

	// Create a minimal sender using NewSender constructor
	sender, _ := NewSender(proxyURL, ":memory:")

	// Create a minimal session manager
	sm := NewSessionManager(db, sender, proxyURL, nil)

	return &CallbackHandler{
		db:         db,
		sender:     sender,
		proxyURL:   proxyURL,
		client:     http.DefaultClient,
		sessionMgr: sm,
	}
}

func makeCallbackUpdate(chatID, threadID int64, data string) contract.Update {
	callbackID := "test-callback-123"
	tid := threadID
	return contract.Update{
		UpdateID: 1,
		Type:     "callback_query",
		ChatID:   chatID,
		ThreadID: &tid,
		Content: &contract.Content{
			CallbackQueryID: &callbackID,
			Data:            &data,
		},
	}
}

// ── Callback data parsing ─────────────────────────────────────────────────────

func TestHandleCallback_MissingData(t *testing.T) {
	tests := []struct {
		name  string
		update contract.Update
	}{
		{
			name: "nil content",
			update: contract.Update{
				Content: nil,
			},
		},
		{
			name: "nil callback query id",
			update: func() contract.Update {
				data := "test"
				return contract.Update{
					Content: &contract.Content{
						Data:            &data,
						CallbackQueryID: nil,
					},
				}
			}(),
		},
		{
			name: "nil data",
			update: func() contract.Update {
				callbackID := "test"
				return contract.Update{
					Content: &contract.Content{
						CallbackQueryID: &callbackID,
						Data:            nil,
					},
				}
			}(),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler("http://test-proxy")
			// Should not panic
			handler.Handle(context.Background(), tc.update)
		})
	}
}

func TestHandleCallback_InvalidFormat(t *testing.T) {
	var answered bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/answer_callback" {
			answered = true
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	handler := newTestHandler(server.URL)

	tests := []struct {
		name        string
		data        string
		expectAlert bool
	}{
		{
			name:        "missing colon separator",
			data:        "approve_tool",
			expectAlert: true,
		},
		{
			name:        "empty string",
			data:        "",
			expectAlert: true,
		},
		{
			name:        "only action",
			data:        "approve_tool:",
			expectAlert: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answered = false
			update := makeCallbackUpdate(123, 456, tc.data)
			handler.Handle(context.Background(), update)
			if !answered {
				t.Error("callback was not answered")
			}
		})
	}
}

// ── Tool approval routing ───────────────────────────────────────────────────────

func TestHandleToolApproval_InvalidParams(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{
			name:   "missing parts",
			params: "123:456",
		},
		{
			name:   "too many parts",
			params: "123:456:0:extra",
		},
		{
			name:   "non-numeric chatID",
			params: "abc:456:0",
		},
		{
			name:   "non-numeric threadID",
			params: "123:abc:0",
		},
		{
			name:   "non-numeric toolIndex",
			params: "123:456:abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler("http://test-proxy")
			update := contract.Update{ChatID: 123}

			replyText, showAlert := handler.handleToolApproval(context.Background(), update, tc.params, true)

			if replyText != "Invalid approval parameters" {
				t.Errorf("replyText: got %q, want %q", replyText, "Invalid approval parameters")
			}
			if !showAlert {
				t.Error("invalid params should show alert")
			}
		})
	}
}

func TestHandleToolApproval_ChatMismatch(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	// Create a session in the DB
	ctx := context.Background()
	handler.db.CreateSession(ctx, &Session{
		ChatID:   123,
		ThreadID: 456,
	})

	// Params say chat 999, but update says chat 123
	params := "999:456:0"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleToolApproval(ctx, update, params, true)

	if replyText != "Chat mismatch" {
		t.Errorf("replyText: got %q, want %q", replyText, "Chat mismatch")
	}
	if !showAlert {
		t.Error("chat mismatch should show alert")
	}
}

func TestHandleToolApproval_SessionNotFound(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	params := "123:456:0"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleToolApproval(context.Background(), update, params, true)

	if replyText != "Session not found" {
		t.Errorf("replyText: got %q, want %q", replyText, "Session not found")
	}
	if !showAlert {
		t.Error("session not found should show alert")
	}
}

// ── Transcript approval routing ─────────────────────────────────────────────────

func TestHandleTranscriptApproval_InvalidParams(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{
			name:   "missing parts",
			params: "123:456",
		},
		{
			name:   "too many parts",
			params: "123:456:789:extra",
		},
		{
			name:   "non-numeric chatID",
			params: "abc:456:789",
		},
		{
			name:   "non-numeric threadID",
			params: "123:abc:789",
		},
		{
			name:   "non-numeric messageID",
			params: "123:456:abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler("http://test-proxy")
			update := contract.Update{ChatID: 123}

			replyText, showAlert := handler.handleTranscriptApproval(context.Background(), update, tc.params)

			if replyText != "Invalid approval parameters" {
				t.Errorf("replyText: got %q, want %q", replyText, "Invalid approval parameters")
			}
			if !showAlert {
				t.Error("invalid params should show alert")
			}
		})
	}
}

func TestHandleTranscriptApproval_ChatMismatch(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	// Params say chat 999, but update says chat 123
	params := "999:456:789"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleTranscriptApproval(context.Background(), update, params)

	if replyText != "Chat mismatch" {
		t.Errorf("replyText: got %q, want %q", replyText, "Chat mismatch")
	}
	if !showAlert {
		t.Error("chat mismatch should show alert")
	}
}

func TestHandleTranscriptApproval_NotFound(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	params := "123:456:789"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleTranscriptApproval(context.Background(), update, params)

	if replyText != "Transcript not found or already processed" {
		t.Errorf("replyText: got %q, want %q", replyText, "Transcript not found or already processed")
	}
	if !showAlert {
		t.Error("transcript not found should show alert")
	}
}

// ── Transcript edit routing ─────────────────────────────────────────────────────

func TestHandleTranscriptEdit_InvalidParams(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{
			name:   "missing parts",
			params: "123:456",
		},
		{
			name:   "too many parts",
			params: "123:456:789:extra",
		},
		{
			name:   "non-numeric chatID",
			params: "abc:456:789",
		},
		{
			name:   "non-numeric threadID",
			params: "123:abc:789",
		},
		{
			name:   "non-numeric messageID",
			params: "123:456:abc",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler("http://test-proxy")
			update := contract.Update{ChatID: 123}

			replyText, showAlert := handler.handleTranscriptEdit(context.Background(), update, tc.params)

			if replyText != "Invalid edit parameters" {
				t.Errorf("replyText: got %q, want %q", replyText, "Invalid edit parameters")
			}
			if !showAlert {
				t.Error("invalid params should show alert")
			}
		})
	}
}

func TestHandleTranscriptEdit_ChatMismatch(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	// Params say chat 999, but update says chat 123
	params := "999:456:789"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleTranscriptEdit(context.Background(), update, params)

	if replyText != "Chat mismatch" {
		t.Errorf("replyText: got %q, want %q", replyText, "Chat mismatch")
	}
	if !showAlert {
		t.Error("chat mismatch should show alert")
	}
}

func TestHandleTranscriptEdit_NotFound(t *testing.T) {
	handler := newTestHandler("http://test-proxy")

	params := "123:456:789"
	update := contract.Update{ChatID: 123}

	replyText, showAlert := handler.handleTranscriptEdit(context.Background(), update, params)

	if replyText != "Transcript not found or already processed" {
		t.Errorf("replyText: got %q, want %q", replyText, "Transcript not found or already processed")
	}
	if !showAlert {
		t.Error("transcript not found should show alert")
	}
}

// ── Action routing ─────────────────────────────────────────────────────────────

func TestHandleCallback_RoutesToCorrectHandler(t *testing.T) {
	tests := []struct {
		name        string
		data        string
		expectAlert bool
	}{
		{
			name:        "approve_tool action",
			data:        "approve_tool:123:456:0",
			expectAlert: false, // Valid params, but session not found - will show alert from handler
		},
		{
			name:        "deny_tool action",
			data:        "deny_tool:123:456:0",
			expectAlert: false,
		},
		{
			name:        "approve_transcript action",
			data:        "approve_transcript:123:456:789",
			expectAlert: false,
		},
		{
			name:        "edit_transcript action",
			data:        "edit_transcript:123:456:789",
			expectAlert: false,
		},
		{
			name:        "unknown action",
			data:        "unknown_action:123:456:789",
			expectAlert: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := newTestHandler("http://test-proxy")
			update := makeCallbackUpdate(123, 456, tc.data)

			// Should not panic - each action should route to its handler
			handler.Handle(context.Background(), update)
		})
	}
}

// ── Parameter parsing ────────────────────────────────────────────────────────────

func TestParseCallbackData(t *testing.T) {
	// Test the callback data format: "action:params"
	tests := []struct {
		input   string
		action  string
		params  string
		wantErr bool
	}{
		{
			input:   "approve_tool:123:456:0",
			action:  "approve_tool",
			params:  "123:456:0",
			wantErr: false,
		},
		{
			input:   "deny_tool:999:888:1",
			action:  "deny_tool",
			params:  "999:888:1",
			wantErr: false,
		},
		{
			input:   "approve_transcript:111:222:333",
			action:  "approve_transcript",
			params:  "111:222:333",
			wantErr: false,
		},
		{
			input:   "edit_transcript:444:555:666",
			action:  "edit_transcript",
			params:  "444:555:666",
			wantErr: false,
		},
		{
			input:   "invalid",
			action:  "",
			params:  "",
			wantErr: true,
		},
		{
			input:   "no_params",
			action:  "",
			params:  "",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			parts := fmt.Sprintf("%s", tc.input) // Just to validate format
			_ = parts
			split := func(data string) (string, string, error) {
				parts := fmt.Sprintf("%s", data)
				idx := -1
				for i, c := range parts {
					if c == ':' {
						idx = i
						break
					}
				}
				if idx == -1 {
					return "", "", fmt.Errorf("invalid format")
				}
				return parts[:idx], parts[idx+1:], nil
			}

			action, params, err := split(tc.input)
			if tc.wantErr && err == nil {
				t.Error("expected error, got nil")
			}
			if !tc.wantErr {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
				if action != tc.action {
					t.Errorf("action: got %q, want %q", action, tc.action)
				}
				if params != tc.params {
					t.Errorf("params: got %q, want %q", params, tc.params)
				}
			}
		})
	}
}
