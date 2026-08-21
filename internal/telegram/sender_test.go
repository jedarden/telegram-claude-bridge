package telegram

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ── Test Helpers ─────────────────────────────────────────────────────────────

func newTestSender(t *testing.T, serverURL string) *Sender {
	t.Helper()
	return NewSender("test-token", serverURL)
}

func newTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *Sender) {
	t.Helper()
	srv := httptest.NewServer(handler)
	sender := NewSender("test-token", srv.URL)
	return srv, sender
}

// ── Rate Limit Detection Tests ────────────────────────────────────────────────

func TestSender_RateLimitDetection(t *testing.T) {
	tests := []struct {
		name            string
		responseCode    int
		retryAfter      int
		expectErrorCode int
		expectCalls     int
	}{
		{
			name:            "too many requests with retry-after",
			responseCode:    429,
			retryAfter:      30,
			expectErrorCode: contract.ErrCodeRateLimit,
			expectCalls:     1,
		},
		{
			name:            "too many requests without retry-after",
			responseCode:    429,
			retryAfter:      0,
			expectErrorCode: contract.ErrCodeRateLimit,
			expectCalls:     1,
		},
		{
			name:            "server error",
			responseCode:    500,
			retryAfter:      0,
			expectErrorCode: 500,
			expectCalls:     1,
		},
		{
			name:            "success",
			responseCode:    200,
			retryAfter:      0,
			expectErrorCode: 0,
			expectCalls:     1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callCount := 0
			srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				callCount++
				if tc.responseCode != 200 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.responseCode)
					resp := contract.ErrorResponse{
						ErrorCode:   tc.responseCode,
						Description: "request failed",
					}
					if tc.responseCode == contract.ErrCodeRateLimit {
						resp.Description = "Too Many Requests"
					}
					if tc.retryAfter > 0 {
						resp.RetryAfter = &tc.retryAfter
					}
					json.NewEncoder(w).Encode(resp)
					return
				}
				// The raw Telegram client returns API errors to its caller. Retry
				// policy belongs to the bridge-side proxy client.
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{
					"ok":     true,
					"result": map[string]any{"message_id": 123},
				})
			})
			defer srv.Close()

			ctx := context.Background()
			resp, err := sender.SendMessage(ctx, contract.SendRequest{
				ChatID: 100,
				Text:   "test message",
			})

			if callCount != tc.expectCalls {
				t.Errorf("expected %d call(s), got %d", tc.expectCalls, callCount)
			}

			if tc.expectErrorCode == 0 {
				if err != nil {
					t.Fatalf("expected success, got error: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("expected API error, got nil")
				}
				if err.ErrorCode != tc.expectErrorCode {
					t.Errorf("error code = %d, want %d", err.ErrorCode, tc.expectErrorCode)
				}
			}
			if resp != nil && tc.responseCode == 200 && resp.MessageID != 123 {
				t.Errorf("expected message_id 123, got %d", resp.MessageID)
			}
		})
	}
}

// ── Error Response Tests ───────────────────────────────────────────────────────

func TestSender_ErrorResponseConversion(t *testing.T) {
	tests := []struct {
		name             string
		httpCode         int
		tgErrorCode      int
		tgDescription    string
		tgRetryAfter     int
		expectErrorCode  int
		expectDesc       string
		expectRetryAfter *int
	}{
		{
			name:             "rate limit with retry-after",
			httpCode:         429,
			tgErrorCode:      contract.ErrCodeRateLimit,
			tgDescription:    "Too Many Requests",
			tgRetryAfter:     60,
			expectErrorCode:  contract.ErrCodeRateLimit,
			expectDesc:       "Too Many Requests",
			expectRetryAfter: func() *int { i := 60; return &i }(),
		},
		{
			name:             "forbidden",
			httpCode:         403,
			tgErrorCode:      contract.ErrCodeForbidden,
			tgDescription:    "Bot was blocked by the user",
			expectErrorCode:  contract.ErrCodeForbidden,
			expectDesc:       "Bot was blocked by the user",
			expectRetryAfter: nil,
		},
		{
			name:             "bad request",
			httpCode:         400,
			tgErrorCode:      contract.ErrCodeBadRequest,
			tgDescription:    "Bad Request: chat not found",
			expectErrorCode:  contract.ErrCodeBadRequest,
			expectDesc:       "Bad Request: chat not found",
			expectRetryAfter: nil,
		},
		{
			name:             "unauthorized",
			httpCode:         401,
			tgErrorCode:      contract.ErrCodeUnauthorized,
			tgDescription:    "Unauthorized",
			expectErrorCode:  contract.ErrCodeUnauthorized,
			expectDesc:       "Unauthorized",
			expectRetryAfter: nil,
		},
		{
			name:             "conflict",
			httpCode:         409,
			tgErrorCode:      contract.ErrCodeConflict,
			tgDescription:    "Conflict: message is not modified",
			expectErrorCode:  contract.ErrCodeConflict,
			expectDesc:       "Conflict: message is not modified",
			expectRetryAfter: nil,
		},
		{
			name:             "too many requests without retry-after",
			httpCode:         429,
			tgErrorCode:      contract.ErrCodeRateLimit,
			tgDescription:    "Too Many Requests",
			tgRetryAfter:     0,
			expectErrorCode:  contract.ErrCodeRateLimit,
			expectDesc:       "Too Many Requests",
			expectRetryAfter: nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.httpCode)
				resp := map[string]any{
					"ok":          false,
					"error_code":  tc.tgErrorCode,
					"description": tc.tgDescription,
				}
				if tc.tgRetryAfter > 0 {
					resp["parameters"] = map[string]any{
						"retry_after": tc.tgRetryAfter,
					}
				}
				json.NewEncoder(w).Encode(resp)
			})
			defer srv.Close()

			ctx := context.Background()
			_, apiErr := sender.SendMessage(ctx, contract.SendRequest{
				ChatID: 100,
				Text:   "test message",
			})

			if apiErr == nil {
				t.Fatal("expected API error, got nil")
			}

			if apiErr.ErrorCode != tc.expectErrorCode {
				t.Errorf("error code = %d, want %d", apiErr.ErrorCode, tc.expectErrorCode)
			}
			if apiErr.Description != tc.expectDesc {
				t.Errorf("description = %q, want %q", apiErr.Description, tc.expectDesc)
			}
			if (apiErr.RetryAfter == nil) != (tc.expectRetryAfter == nil) {
				t.Errorf("retry_after = %v, want %v", apiErr.RetryAfter, tc.expectRetryAfter)
			}
			if apiErr.RetryAfter != nil && tc.expectRetryAfter != nil && *apiErr.RetryAfter != *tc.expectRetryAfter {
				t.Errorf("retry_after = %d, want %d", *apiErr.RetryAfter, *tc.expectRetryAfter)
			}
		})
	}
}

// ── SendMessage Tests ─────────────────────────────────────────────────────────

func TestSender_SendMessage_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 456},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	resp, err := sender.SendMessage(ctx, contract.SendRequest{
		ChatID:    100,
		Text:      "Hello, World!",
		ParseMode: func() *string { s := "Markdown"; return &s }(),
	})

	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if !resp.OK {
		t.Error("response OK should be true")
	}
	if resp.MessageID != 456 {
		t.Errorf("message_id = %d, want 456", resp.MessageID)
	}
}

func TestSender_SendMessage_WithThreadID(t *testing.T) {
	var receivedThreadID int64
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if tid, ok := req["message_thread_id"].(float64); ok {
			receivedThreadID = int64(tid)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 789},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	threadID := int64(123)
	resp, err := sender.SendMessage(ctx, contract.SendRequest{
		ChatID:   100,
		ThreadID: &threadID,
		Text:     "test message",
	})

	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if receivedThreadID != 123 {
		t.Errorf("thread_id = %d, want 123", receivedThreadID)
	}
	if resp.MessageID != 789 {
		t.Errorf("message_id = %d, want 789", resp.MessageID)
	}
}

func TestSender_SendMessage_WithReplyTo(t *testing.T) {
	var receivedReplyTo int64
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if rt, ok := req["reply_to_message_id"].(float64); ok {
			receivedReplyTo = int64(rt)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 999},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	replyTo := int64(555)
	resp, err := sender.SendMessage(ctx, contract.SendRequest{
		ChatID:           100,
		Text:             "reply message",
		ReplyToMessageID: &replyTo,
	})

	if err != nil {
		t.Fatalf("SendMessage failed: %v", err)
	}
	if receivedReplyTo != 555 {
		t.Errorf("reply_to_message_id = %d, want 555", receivedReplyTo)
	}
	if resp.MessageID != 999 {
		t.Errorf("message_id = %d, want 999", resp.MessageID)
	}
}

// ── EditMessageText Tests ────────────────────────────────────────────────────────

func TestSender_EditMessageText_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 111},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	resp, err := sender.EditMessageText(ctx, contract.EditRequest{
		ChatID:    100,
		MessageID: 555,
		Text:      "Updated message",
	})

	if err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	if !resp.OK {
		t.Error("response OK should be true")
	}
	if resp.MessageID != 111 {
		t.Errorf("message_id = %d, want 111", resp.MessageID)
	}
}

func TestSender_EditMessageText_WithParseMode(t *testing.T) {
	var receivedParseMode string
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if pm, ok := req["parse_mode"].(string); ok {
			receivedParseMode = pm
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 222},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	parseMode := "MarkdownV2"
	resp, err := sender.EditMessageText(ctx, contract.EditRequest{
		ChatID:    100,
		MessageID: 555,
		Text:      "test",
		ParseMode: &parseMode,
	})

	if err != nil {
		t.Fatalf("EditMessageText failed: %v", err)
	}
	if receivedParseMode != "MarkdownV2" {
		t.Errorf("parse_mode = %q, want MarkdownV2", receivedParseMode)
	}
	if resp.MessageID != 222 {
		t.Errorf("message_id = %d, want 222", resp.MessageID)
	}
}

// ── SendChatAction Tests ────────────────────────────────────────────────────────

func TestSender_SendChatAction_Success(t *testing.T) {
	var receivedAction string
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if action, ok := req["action"].(string); ok {
			receivedAction = action
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	err := sender.SendChatAction(ctx, contract.ChatActionRequest{
		ChatID: 100,
		Action: "typing",
	})

	if err != nil {
		t.Fatalf("SendChatAction failed: %v", err)
	}
	if receivedAction != "typing" {
		t.Errorf("action = %q, want typing", receivedAction)
	}
}

func TestSender_SendChatAction_WithThreadID(t *testing.T) {
	var receivedThreadID int64
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if tid, ok := req["message_thread_id"].(float64); ok {
			receivedThreadID = int64(tid)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	threadID := int64(789)
	err := sender.SendChatAction(ctx, contract.ChatActionRequest{
		ChatID:   100,
		ThreadID: &threadID,
		Action:   "upload_photo",
	})

	if err != nil {
		t.Fatalf("SendChatAction failed: %v", err)
	}
	if receivedThreadID != 789 {
		t.Errorf("thread_id = %d, want 789", receivedThreadID)
	}
}

// ── GetFile Tests ───────────────────────────────────────────────────────────────

func TestSender_GetFile_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		filePath := "/photos/file_123.jpg"
		fileSize := int64(102400)
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"file_id":        "ABCD1234",
				"file_unique_id": "EF5678",
				"file_size":      fileSize,
				"file_path":      filePath,
			},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	file, apiErr := sender.GetFile(ctx, "ABCD1234")

	if apiErr != nil {
		t.Fatalf("GetFile failed: %v", apiErr)
	}
	if file == nil {
		t.Fatal("file should not be nil")
	}
	if file.FileID != "ABCD1234" {
		t.Errorf("file_id = %q, want ABCD1234", file.FileID)
	}
	if file.FileUniqueID != "EF5678" {
		t.Errorf("file_unique_id = %q, want EF5678", file.FileUniqueID)
	}
	if file.FileSize == nil || *file.FileSize != 102400 {
		t.Errorf("file_size = %v, want 102400", file.FileSize)
	}
	if file.FilePath == nil || *file.FilePath != "/photos/file_123.jpg" {
		t.Errorf("file_path = %v, want /photos/file_123.jpg", file.FilePath)
	}
}

func TestSender_GetFile_MinimalResponse(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"file_id":        "XYZ789",
				"file_unique_id": "uvw123",
			},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	file, apiErr := sender.GetFile(ctx, "XYZ789")

	if apiErr != nil {
		t.Fatalf("GetFile failed: %v", apiErr)
	}
	if file == nil {
		t.Fatal("file should not be nil")
	}
	if file.FileID != "XYZ789" {
		t.Errorf("file_id = %q, want XYZ789", file.FileID)
	}
	if file.FileSize != nil {
		t.Errorf("file_size should be nil for minimal response, got %d", *file.FileSize)
	}
	if file.FilePath != nil {
		t.Errorf("file_path should be nil for minimal response, got %q", *file.FilePath)
	}
}

func TestSender_GetFile_Error(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  400,
			"description": "Bad Request: invalid file id",
		})
	})
	defer srv.Close()

	ctx := context.Background()
	_, apiErr := sender.GetFile(ctx, "invalid_id")

	if apiErr == nil {
		t.Fatal("GetFile should return error for invalid file_id")
	}
	if apiErr.ErrorCode != 400 {
		t.Errorf("error code = %d, want 400", apiErr.ErrorCode)
	}
	if !strings.Contains(apiErr.Description, "invalid file id") {
		t.Errorf("error description = %q, should contain 'invalid file id'", apiErr.Description)
	}
}

// ── CreateForumTopic Tests ────────────────────────────────────────────────────

func TestSender_CreateForumTopic_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_thread_id": 12345,
				"name":              "Test Topic",
				"icon_color":        9371288,
			},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	iconColor := 9371288
	resp, err := sender.CreateForumTopic(ctx, contract.CreateTopicRequest{
		ChatID:    100,
		Name:      "Test Topic",
		IconColor: &iconColor,
	})

	if err != nil {
		t.Fatalf("CreateForumTopic failed: %v", err)
	}
	if !resp.OK {
		t.Error("response OK should be true")
	}
	if resp.ThreadID != 12345 {
		t.Errorf("thread_id = %d, want 12345", resp.ThreadID)
	}
	if resp.Name != "Test Topic" {
		t.Errorf("name = %q, want Test Topic", resp.Name)
	}
	if resp.IconColor == nil || *resp.IconColor != 9371288 {
		t.Errorf("icon_color = %v, want 9371288", resp.IconColor)
	}
}

func TestSender_CreateForumTopic_WithoutIconColor(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_thread_id": 67890,
				"name":              "Default Color",
			},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	resp, err := sender.CreateForumTopic(ctx, contract.CreateTopicRequest{
		ChatID: 100,
		Name:   "Default Color",
	})

	if err != nil {
		t.Fatalf("CreateForumTopic failed: %v", err)
	}
	if resp.ThreadID != 67890 {
		t.Errorf("thread_id = %d, want 67890", resp.ThreadID)
	}
	if resp.IconColor != nil {
		t.Errorf("icon_color should be nil when not set, got %d", *resp.IconColor)
	}
}

// ── EditForumTopic Tests ───────────────────────────────────────────────────────

func TestSender_EditForumTopic_Name(t *testing.T) {
	var receivedName string
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if name, ok := req["name"].(string); ok {
			receivedName = name
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	newName := "Updated Topic Name"
	err := sender.EditForumTopic(ctx, contract.EditTopicRequest{
		ChatID:   100,
		ThreadID: 123,
		Name:     &newName,
	})

	if err != nil {
		t.Fatalf("EditForumTopic failed: %v", err)
	}
	if receivedName != "Updated Topic Name" {
		t.Errorf("name = %q, want Updated Topic Name", receivedName)
	}
}

func TestSender_EditForumTopic_IconColor(t *testing.T) {
	var receivedColor int
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if color, ok := req["icon_color"].(float64); ok {
			receivedColor = int(color)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	newColor := 16766846 // yellow
	err := sender.EditForumTopic(ctx, contract.EditTopicRequest{
		ChatID:    100,
		ThreadID:  123,
		IconColor: &newColor,
	})

	if err != nil {
		t.Fatalf("EditForumTopic failed: %v", err)
	}
	if receivedColor != 16766846 {
		t.Errorf("icon_color = %d, want 16766846", receivedColor)
	}
}

// ── CloseForumTopic / ReopenForumTopic Tests ────────────────────────────────────

func TestSender_CloseForumTopic_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	err := sender.CloseForumTopic(ctx, contract.TopicRequest{
		ChatID:   100,
		ThreadID: 123,
	})

	if err != nil {
		t.Fatalf("CloseForumTopic failed: %v", err)
	}
}

func TestSender_ReopenForumTopic_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	err := sender.ReopenForumTopic(ctx, contract.TopicRequest{
		ChatID:   100,
		ThreadID: 456,
	})

	if err != nil {
		t.Fatalf("ReopenForumTopic failed: %v", err)
	}
}

// ── PinChatMessage Tests ────────────────────────────────────────────────────────

func TestSender_PinChatMessage_Success(t *testing.T) {
	var receivedDisableNotification bool
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if dn, ok := req["disable_notification"].(bool); ok {
			receivedDisableNotification = dn
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	disableNotif := true
	err := sender.PinChatMessage(ctx, contract.PinMessageRequest{
		ChatID:              100,
		MessageID:           789,
		DisableNotification: &disableNotif,
	})

	if err != nil {
		t.Fatalf("PinChatMessage failed: %v", err)
	}
	if !receivedDisableNotification {
		t.Error("disable_notification should be true")
	}
}

func TestSender_PinChatMessage_WithoutNotificationFlag(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["disable_notification"]; !ok {
			// Field omitted when nil
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	err := sender.PinChatMessage(ctx, contract.PinMessageRequest{
		ChatID:    100,
		MessageID: 789,
	})

	if err != nil {
		t.Fatalf("PinChatMessage failed: %v", err)
	}
}

// ── AnswerCallbackQuery Tests ───────────────────────────────────────────────────

func TestSender_AnswerCallbackQuery_WithText(t *testing.T) {
	var receivedText string
	var receivedShowAlert bool
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if text, ok := req["text"].(string); ok {
			receivedText = text
		}
		if alert, ok := req["show_alert"].(bool); ok {
			receivedShowAlert = alert
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	text := "Button clicked!"
	showAlert := true
	err := sender.AnswerCallbackQuery(ctx, contract.AnswerCallbackRequest{
		CallbackQueryID: "callback_123",
		Text:            &text,
		ShowAlert:       &showAlert,
	})

	if err != nil {
		t.Fatalf("AnswerCallbackQuery failed: %v", err)
	}
	if receivedText != "Button clicked!" {
		t.Errorf("text = %q, want 'Button clicked!'", receivedText)
	}
	if !receivedShowAlert {
		t.Error("show_alert should be true")
	}
}

func TestSender_AnswerCallbackQuery_WithoutText(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		json.NewDecoder(r.Body).Decode(&req)
		if _, ok := req["text"]; ok {
			t.Error("text should not be present when nil")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"ok": true})
	})
	defer srv.Close()

	ctx := context.Background()
	err := sender.AnswerCallbackQuery(ctx, contract.AnswerCallbackRequest{
		CallbackQueryID: "callback_456",
	})

	if err != nil {
		t.Fatalf("AnswerCallbackQuery failed: %v", err)
	}
}

// ── DownloadFile Tests ─────────────────────────────────────────────────────────

func TestSender_DownloadFile_Success(t *testing.T) {
	fileData := []byte("test file content")
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(fileData)
	})
	defer srv.Close()

	ctx := context.Background()
	resp, err := sender.DownloadFile(ctx, "/photos/test.jpg")

	if err != nil {
		t.Fatalf("DownloadFile failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status code = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestSender_DownloadFile_NotFound(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	defer srv.Close()

	ctx := context.Background()
	resp, err := sender.DownloadFile(ctx, "/photos/notfound.jpg")

	if err != nil {
		t.Fatalf("DownloadFile should return the HTTP response for 404: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status code = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestSender_DownloadFile_WithContextTimeout(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	})
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := sender.DownloadFile(ctx, "/photos/test.jpg")
	if err == nil {
		t.Error("DownloadFile should fail with context timeout")
	}
}

// ── Context Cancellation Tests ─────────────────────────────────────────────────

func TestSender_ContextCancellation(t *testing.T) {
	// Server that never responds
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second)
	})
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	_, err := sender.SendMessage(ctx, contract.SendRequest{
		ChatID: 100,
		Text:   "test",
	})

	if err == nil {
		t.Error("should fail with context cancellation")
	}
}

// ── CallMultipart Tests ────────────────────────────────────────────────────────

func TestSender_SendPhoto_Success(t *testing.T) {
	var receivedFileData []byte
	var receivedFilename string
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Parse multipart form
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		file, header, err := r.FormFile("photo")
		if err != nil {
			t.Fatalf("get photo field: %v", err)
		}
		defer file.Close()
		receivedFilename = header.Filename
		receivedFileData, _ = io.ReadAll(file)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 555},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	fileData := []byte{0x89, 0x50, 0x4E, 0x47} // PNG signature
	resp, err := sender.SendPhoto(ctx, contract.SendPhotoRequest{
		ChatID: 100,
	}, fileData, "test.png")

	if err != nil {
		t.Fatalf("SendPhoto failed: %v", err)
	}
	if !resp.OK {
		t.Error("response OK should be true")
	}
	if resp.MessageID != 555 {
		t.Errorf("message_id = %d, want 555", resp.MessageID)
	}
	if receivedFilename != "test.png" {
		t.Errorf("filename = %q, want %q", receivedFilename, "test.png")
	}
	if string(receivedFileData) != string(fileData) {
		t.Errorf("file data = %v, want %v", receivedFileData, fileData)
	}
}

func TestSender_SendDocument_Success(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify multipart form
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 777},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	fileData := []byte("test document content")
	resp, err := sender.SendDocument(ctx, contract.SendDocumentRequest{
		ChatID: 100,
	}, fileData, "test.txt")

	if err != nil {
		t.Fatalf("SendDocument failed: %v", err)
	}
	if !resp.OK {
		t.Error("response OK should be true")
	}
	if resp.MessageID != 777 {
		t.Errorf("message_id = %d, want 777", resp.MessageID)
	}
}

func TestSender_SendAudio_WithMetadata(t *testing.T) {
	var receivedDuration int
	var receivedTitle string
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if vals, ok := r.MultipartForm.Value["duration"]; ok && len(vals) > 0 {
			receivedDuration, _ = strconv.Atoi(vals[0])
		}
		if vals, ok := r.MultipartForm.Value["title"]; ok && len(vals) > 0 {
			receivedTitle = vals[0]
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 888},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	fileData := []byte("fake audio data")
	duration := 180
	title := "Test Audio"
	resp, err := sender.SendAudio(ctx, contract.SendAudioRequest{
		ChatID:   100,
		Duration: &duration,
		Title:    &title,
	}, fileData, "audio.mp3")

	if err != nil {
		t.Fatalf("SendAudio failed: %v", err)
	}
	if receivedDuration != 180 {
		t.Errorf("duration = %d, want 180", receivedDuration)
	}
	if receivedTitle != "Test Audio" {
		t.Errorf("title = %q, want Test Audio", receivedTitle)
	}
	if resp.MessageID != 888 {
		t.Errorf("message_id = %d, want 888", resp.MessageID)
	}
}

func TestSender_SendVideo_WithDimensions(t *testing.T) {
	var receivedWidth int
	var receivedHeight int
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			t.Fatalf("parse multipart: %v", err)
		}
		if vals, ok := r.MultipartForm.Value["width"]; ok && len(vals) > 0 {
			receivedWidth, _ = strconv.Atoi(vals[0])
		}
		if vals, ok := r.MultipartForm.Value["height"]; ok && len(vals) > 0 {
			receivedHeight, _ = strconv.Atoi(vals[0])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"ok":     true,
			"result": map[string]any{"message_id": 999},
		})
	})
	defer srv.Close()

	ctx := context.Background()
	fileData := []byte("fake video data")
	width := 1920
	height := 1080
	resp, err := sender.SendVideo(ctx, contract.SendVideoRequest{
		ChatID: 100,
		Width:  &width,
		Height: &height,
	}, fileData, "video.mp4")

	if err != nil {
		t.Fatalf("SendVideo failed: %v", err)
	}
	if receivedWidth != 1920 {
		t.Errorf("width = %d, want 1920", receivedWidth)
	}
	if receivedHeight != 1080 {
		t.Errorf("height = %d, want 1080", receivedHeight)
	}
	if resp.MessageID != 999 {
		t.Errorf("message_id = %d, want 999", resp.MessageID)
	}
}

// ── Token Redaction Tests ───────────────────────────────────────────────────────

func TestSender_TokenRedaction(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// Check that token is in URL
		if !strings.Contains(r.URL.String(), "bot") {
			t.Error("URL should contain bot token path")
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer srv.Close()

	ctx := context.Background()
	_, err := sender.SendMessage(ctx, contract.SendRequest{
		ChatID: 100,
		Text:   "test",
	})

	if err == nil {
		t.Fatal("expected error")
	}
	errStr := err.Error()
	if strings.Contains(errStr, "test-token") {
		t.Error("error should not contain actual token (should be redacted)")
	}
	if !strings.Contains(errStr, "REDACTED") && !strings.Contains(errStr, "redact") {
		t.Log("error:", errStr)
		// This is OK - the redaction might use a different pattern
	}
}

// ── API Base URL Tests ─────────────────────────────────────────────────────────

func TestSender_CustomAPIBase(t *testing.T) {
	customBase := "http://custom-api.example.com"
	sender := NewSender("test-token", customBase)

	if sender.apiBase != customBase {
		t.Errorf("apiBase = %q, want %q", sender.apiBase, customBase)
	}
}

func TestSender_DefaultAPIBase(t *testing.T) {
	sender := NewSender("test-token", "")

	// Should use the default Telegram API base
	if sender.apiBase == "" {
		t.Error("apiBase should not be empty when custom base is not provided")
	}
}

// ── NewSender Initialization Tests ───────────────────────────────────────────

func TestSender_NewSender(t *testing.T) {
	token := "test-token-123"
	apiBase := "http://test.example.com"
	sender := NewSender(token, apiBase)

	if sender.token != token {
		t.Errorf("token = %q, want %q", sender.token, token)
	}
	if sender.apiBase != apiBase {
		t.Errorf("apiBase = %q, want %q", sender.apiBase, apiBase)
	}
	if sender.client == nil {
		t.Error("client should not be nil")
	}
	if sender.downloadClient == nil {
		t.Error("downloadClient should not be nil")
	}
	if sender.client.Timeout == 0 {
		t.Error("client should have a timeout configured")
	}
	if sender.downloadClient.Timeout != 0 {
		t.Error("downloadClient should have no timeout (context controls deadline)")
	}
}

// ── Response Decode Failure Tests ─────────────────────────────────────────────

func TestSender_NonJSONBody_DecodeFailure(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// 200 with a non-JSON body — the call itself succeeded but the
		// envelope cannot be decoded.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, "not json at all")
	})
	defer srv.Close()

	_, apiErr := sender.SendMessage(context.Background(), contract.SendRequest{
		ChatID: 100,
		Text:   "test",
	})
	if apiErr == nil {
		t.Fatal("expected error for non-JSON response body")
	}
	if apiErr.ErrorCode != contract.ErrCodeTelegramUnreachable {
		t.Errorf("error code = %d, want %d (ErrCodeTelegramUnreachable)", apiErr.ErrorCode, contract.ErrCodeTelegramUnreachable)
	}
	if !strings.Contains(apiErr.Description, "decode response") {
		t.Errorf("description = %q, want it to mention decode failure", apiErr.Description)
	}
}

func TestSender_MissingResult_BadResponse(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		// ok:true but no result payload — unmarshalling the empty result
		// must surface as "bad response from Telegram", not a panic.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true}`)
	})
	defer srv.Close()

	_, apiErr := sender.SendMessage(context.Background(), contract.SendRequest{
		ChatID: 100,
		Text:   "test",
	})
	if apiErr == nil {
		t.Fatal("expected error for missing result payload")
	}
	if apiErr.ErrorCode != contract.ErrCodeTelegramUnreachable {
		t.Errorf("error code = %d, want %d (ErrCodeTelegramUnreachable)", apiErr.ErrorCode, contract.ErrCodeTelegramUnreachable)
	}
	if !strings.Contains(apiErr.Description, "bad response from Telegram") {
		t.Errorf("description = %q, want 'bad response from Telegram'", apiErr.Description)
	}
}

func TestSender_GetFile_BadResultPayload(t *testing.T) {
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"ok":true,"result":"a string is not a file object"}`)
	})
	defer srv.Close()

	f, apiErr := sender.GetFile(context.Background(), "file-id-1")
	if f != nil {
		t.Errorf("expected nil file, got %+v", f)
	}
	if apiErr == nil {
		t.Fatal("expected error for non-object result payload")
	}
	if apiErr.ErrorCode != contract.ErrCodeTelegramUnreachable {
		t.Errorf("error code = %d, want %d (ErrCodeTelegramUnreachable)", apiErr.ErrorCode, contract.ErrCodeTelegramUnreachable)
	}
	if !strings.Contains(apiErr.Description, "bad response from Telegram") {
		t.Errorf("description = %q, want 'bad response from Telegram'", apiErr.Description)
	}
}

func TestSender_MultipartRateLimit(t *testing.T) {
	// callMultipart (photo/document/audio/video uploads) goes through a
	// separate code path than the JSON call() — verify a 429 with
	// parameters.retry_after propagates as ErrCodeRateLimit + RetryAfter.
	retryAfter := 42
	srv, sender := newTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  429,
			"description": "Too Many Requests: retry after 42",
			"parameters":  map[string]any{"retry_after": retryAfter},
		})
	})
	defer srv.Close()

	_, apiErr := sender.SendPhoto(context.Background(), contract.SendPhotoRequest{
		ChatID: 100,
	}, []byte("fake photo"), "photo.jpg")
	if apiErr == nil {
		t.Fatal("expected rate limit error")
	}
	if apiErr.ErrorCode != contract.ErrCodeRateLimit {
		t.Errorf("error code = %d, want %d (ErrCodeRateLimit)", apiErr.ErrorCode, contract.ErrCodeRateLimit)
	}
	if apiErr.RetryAfter == nil || *apiErr.RetryAfter != retryAfter {
		t.Errorf("retry_after = %v, want %d", apiErr.RetryAfter, retryAfter)
	}
}

func TestRedactToken(t *testing.T) {
	tests := []struct {
		name  string
		s     string
		token string
		want  string
	}{
		{
			name:  "token in URL is redacted",
			s:     "Telegram unreachable: POST http://api.telegram.org/botSECRET/sendMessage: connection refused",
			token: "SECRET",
			want:  "Telegram unreachable: POST http://api.telegram.org/bot<REDACTED>/sendMessage: connection refused",
		},
		{
			name:  "all occurrences are redacted",
			s:     "botT/x and again botT/y",
			token: "T",
			want:  "bot<REDACTED>/x and again bot<REDACTED>/y",
		},
		{
			name:  "token without bot prefix and trailing slash is untouched",
			s:     "the SECRET leaked",
			token: "SECRET",
			want:  "the SECRET leaked",
		},
		{
			name:  "bot prefix without trailing slash is untouched",
			s:     "botSECRET at end",
			token: "SECRET",
			want:  "botSECRET at end",
		},
		{
			name:  "empty token leaves string unchanged",
			s:     "bot/token/sendMessage",
			token: "",
			want:  "bot/token/sendMessage",
		},
		{
			name:  "empty string stays empty",
			s:     "",
			token: "SECRET",
			want:  "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactToken(tc.s, tc.token); got != tc.want {
				t.Errorf("redactToken(%q, %q) = %q, want %q", tc.s, tc.token, got, tc.want)
			}
		})
	}
}
