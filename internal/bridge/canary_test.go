package bridge

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRunCanaryTest_NoGroups verifies that the canary test handles the case
// where no groups are configured gracefully.
func TestRunCanaryTest_NoGroups(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	// Create a mock sender (we don't need a real proxy for this test)
	sender, err := NewSender("http://fake-proxy", "")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	sessionMgr := NewSessionManager(db, sender, "http://fake-proxy", nil, 0)
	ctx := context.Background()

	// Run canary with no groups configured
	result := sessionMgr.RunCanaryTest(ctx, 0)

	// Should fail gracefully with a clear error message
	if result.Success {
		t.Error("expected canary to fail with no groups configured")
	}
	if result.Error == nil {
		t.Error("expected error when no groups configured")
	}
	if result.DurationSec <= 0 {
		t.Error("expected duration to be recorded")
	}
}

// TestRunCanaryTest_SendsAlertOnFailure verifies that admin alerts are sent
// when the canary test fails and ADMIN_CHAT_ID is configured.
func TestRunCanaryTest_SendsAlertOnFailure(t *testing.T) {
	var requestPath string
	var requestBody struct {
		ChatID   int64  `json:"chat_id"`
		ThreadID *int64 `json:"thread_id"`
		Text     string `json:"text"`
	}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read alert request: %v", err)
			return
		}
		if err := json.Unmarshal(body, &requestBody); err != nil {
			t.Errorf("decode alert request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message_id":1}`))
	}))
	defer proxy.Close()

	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sender, err := NewSender(proxy.URL, filepath.Join(t.TempDir(), "sender.db"))
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	sessionMgr := NewSessionManager(db, sender, "http://fake-proxy", nil, 0)
	ctx := context.Background()

	// Run canary with admin chat configured
	adminChatID := int64(12345)
	result := sessionMgr.RunCanaryTest(ctx, adminChatID)

	// Should fail (no groups) and trigger alert
	if result.Success {
		t.Error("expected canary to fail")
	}

	if requestPath != "/send" {
		t.Errorf("alert request path = %q, want /send", requestPath)
	}
	if requestBody.ChatID != adminChatID {
		t.Errorf("alert chat_id = %d, want %d", requestBody.ChatID, adminChatID)
	}
	if requestBody.ThreadID == nil || *requestBody.ThreadID != 1 {
		t.Errorf("alert thread_id = %v, want General topic (1)", requestBody.ThreadID)
	}
	if !strings.Contains(requestBody.Text, "PTY Canary Test Failed") {
		t.Errorf("alert text = %q, want canary failure text", requestBody.Text)
	}
}

// TestCanaryResult_Fields verifies that all result fields are populated
// correctly on a failed canary test.
func TestCanaryResult_Fields(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sender, err := NewSender("http://fake-proxy", "")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	sessionMgr := NewSessionManager(db, sender, "http://fake-proxy", nil, 0)
	ctx := context.Background()

	result := sessionMgr.RunCanaryTest(ctx, 0)

	// Verify all fields are populated
	if result.DurationSec <= 0 {
		t.Error("expected duration to be positive")
	}

	if result.Success {
		t.Error("expected failure with no groups")
	}

	// On failure, StopHookOK and PTYOK should both be false
	// (we couldn't even spawn a pane)
	if result.StopHookOK {
		t.Error("expected stop-hook to be marked as failed")
	}
	if result.PTYOK {
		t.Error("expected PTY extraction to be marked as failed")
	}

	if result.Error == nil {
		t.Error("expected error to be set")
	}
}

// TestCanaryResult_Timeout verifies that the canary test respects the
// context timeout and doesn't hang indefinitely.
func TestCanCanaryResult_Timeout(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sender, err := NewSender("http://fake-proxy", "")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	sessionMgr := NewSessionManager(db, sender, "http://fake-proxy", nil, 0)

	// Use a very short timeout
	shortCtx, cancel := context.WithTimeout(context.Background(), 1*time.Nanosecond)
	defer cancel()

	start := time.Now()
	result := sessionMgr.RunCanaryTest(shortCtx, 0)
	duration := time.Since(start)

	// Should fail quickly due to timeout
	if result.Success {
		t.Error("expected canary to fail with short timeout")
	}

	// Should complete within a few seconds (not hang)
	if duration > 5*time.Second {
		t.Errorf("canary took too long: %v", duration)
	}
}

// TestCanaryTest_NoAdminChat verifies that no alert is attempted when
// ADMIN_CHAT_ID is zero or unset.
func TestCanaryTest_NoAdminChat(t *testing.T) {
	db, err := OpenDB(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	sender, err := NewSender("http://fake-proxy", "")
	if err != nil {
		t.Fatalf("create sender: %v", err)
	}
	defer sender.Close()

	sessionMgr := NewSessionManager(db, sender, "http://fake-proxy", nil, 0)
	ctx := context.Background()

	// Run with adminChatID = 0 (no alert should be sent)
	result := sessionMgr.RunCanaryTest(ctx, 0)

	if result.Success {
		t.Error("expected canary to fail")
	}

	// No panic should occur (alert sending is skipped when adminChatID is 0)
}
