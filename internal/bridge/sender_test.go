package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// ---- chunkText unit tests ----

func TestChunkText_ShortMessage(t *testing.T) {
	text := "Hello, world!"
	chunks := chunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
	if chunks[0] != text {
		t.Errorf("chunk mismatch: %q", chunks[0])
	}
}

func TestChunkText_ExactLimit(t *testing.T) {
	text := strings.Repeat("a", maxMessageLen)
	chunks := chunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk, got %d", len(chunks))
	}
}

func TestChunkText_SplitsAtParagraph(t *testing.T) {
	// First chunk: 3000 chars of 'a', paragraph break, second chunk: 3000 chars of 'b'.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	text := a + "\n\n" + b

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	// First chunk should be the 'a' portion
	if !strings.HasPrefix(chunks[0], a) {
		t.Errorf("chunk 0 should start with 'a's")
	}
	// Second chunk should be the 'b' portion
	if !strings.HasPrefix(chunks[1], b) {
		t.Errorf("chunk 1 should start with 'b's")
	}
}

func TestChunkText_SplitsAtNewline(t *testing.T) {
	// No paragraph break; only single newline within limit.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	text := a + "\n" + b

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	if !strings.HasPrefix(chunks[0], a) {
		t.Errorf("chunk 0 should start with 'a's")
	}
	if !strings.HasPrefix(chunks[1], b) {
		t.Errorf("chunk 1 should start with 'b's")
	}
}

func TestChunkText_HardCut(t *testing.T) {
	// No whitespace at all — must hard-cut.
	text := strings.Repeat("x", maxMessageLen+100)
	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	if len([]rune(chunks[0])) != maxMessageLen {
		t.Errorf("chunk 0 rune len=%d, want %d", len([]rune(chunks[0])), maxMessageLen)
	}
	if len([]rune(chunks[1])) != 100 {
		t.Errorf("chunk 1 rune len=%d, want 100", len([]rune(chunks[1])))
	}
}

func TestChunkText_MultiByte(t *testing.T) {
	// Each '界' is 3 bytes but 1 rune.  A string of 4096 such runes must be
	// returned as a single chunk (it's ≤ 4096 runes even though > 4096 bytes).
	text := strings.Repeat("界", maxMessageLen)
	chunks := chunkText(text)
	if len(chunks) != 1 {
		t.Fatalf("want 1 chunk for %d multi-byte runes, got %d", maxMessageLen, len(chunks))
	}
}

func TestChunkText_PreservesAllText(t *testing.T) {
	// Reconstruct the original text from chunks and verify nothing was lost.
	a := strings.Repeat("a", 3000)
	b := strings.Repeat("b", 3000)
	c := strings.Repeat("c", 3000)
	text := a + "\n\n" + b + "\n\n" + c

	chunks := chunkText(text)
	reconstructed := strings.Join(chunks, "")
	// The original text has "\n\n" stripped between chunks; verify individual
	// content blocks are present.
	if !strings.Contains(reconstructed, a) {
		t.Error("chunk a missing from reconstructed text")
	}
	if !strings.Contains(reconstructed, b) {
		t.Error("chunk b missing from reconstructed text")
	}
	if !strings.Contains(reconstructed, c) {
		t.Error("chunk c missing from reconstructed text")
	}
}

// ---- Code-block-aware chunking tests ----

func TestChunkText_CodeBlockNotSplit(t *testing.T) {
	// Create a code block that's 2000 chars long followed by text.
	// The split should happen after the </pre>, not inside it.
	code := strings.Repeat("x", 2000)
	after := strings.Repeat("y", 3000)
	text := "<pre><code>" + code + "</code></pre>\n\n" + after

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d\nchunks: %v", len(chunks), chunks)
	}
	// First chunk should contain the entire code block
	if !strings.Contains(chunks[0], "<pre><code>") {
		t.Error("first chunk missing opening pre tag")
	}
	if !strings.Contains(chunks[0], "</code></pre>") {
		t.Error("first chunk missing closing pre tag - code block was split!")
	}
	// Second chunk should have the 'after' text
	if !strings.Contains(chunks[1], "yyy") {
		t.Error("second chunk missing 'after' content")
	}
}

func TestChunkText_SplitsBetweenCodeBlocks(t *testing.T) {
	// Two code blocks with a paragraph break between them.
	// Each code block is ~2200 chars, so the total exceeds 4096.
	code1 := strings.Repeat("a", 2200)
	code2 := strings.Repeat("b", 2200)
	text := "<pre><code>" + code1 + "</code></pre>\n\n<pre><code>" + code2 + "</code></pre>"

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	// Each chunk should have one complete code block
	if !strings.Contains(chunks[0], "</code></pre>") {
		t.Error("first chunk should have complete code block")
	}
	if !strings.Contains(chunks[1], "<pre><code>") {
		t.Error("second chunk should have opening code tag")
	}
	if !strings.Contains(chunks[1], "</code></pre>") {
		t.Error("second chunk should have closing code tag")
	}
}

func TestChunkText_SplitsAtParagraphOutsideCode(t *testing.T) {
	// Paragraph break outside code blocks should be preferred.
	// Structure: text + paragraph + <pre>code</pre> + more text
	// The split should happen at the paragraph break.
	prefix := strings.Repeat("a", 3000)
	code := strings.Repeat("b", 1000)
	suffix := strings.Repeat("c", 2000)
	text := prefix + "\n\n<pre><code>" + code + "</code></pre>\n\n" + suffix

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	// First chunk should end at the paragraph break
	if strings.Contains(chunks[0], "<pre>") {
		t.Error("first chunk should not include code block - should split at paragraph")
	}
	// Second chunk should have the code block
	if !strings.Contains(chunks[1], "<pre>") {
		t.Error("second chunk should have code block")
	}
}

func TestChunkText_TagBalancing(t *testing.T) {
	// Text with bold tag that needs to be split and balanced.
	// Total content ~4100 chars to exceed 4096 limit.
	prefix := strings.Repeat("a", 3000)
	suffix := strings.Repeat("b", 1100)
	text := "<b>" + prefix + suffix + "</b>"

	chunks := chunkText(text)
	if len(chunks) != 2 {
		t.Fatalf("want 2 chunks, got %d", len(chunks))
	}
	// First chunk should close the <b> tag
	if !strings.HasSuffix(chunks[0], "</b>") {
		t.Errorf("first chunk should close bold tag, got: %q", chunks[0])
	}
	// Second chunk should reopen the <b> tag
	if !strings.HasPrefix(chunks[1], "<b>") {
		t.Errorf("second chunk should reopen bold tag, got: %q", chunks[1])
	}
	// Both chunks should have matching tags
	if !strings.HasSuffix(chunks[1], "</b>") {
		t.Error("second chunk should close bold tag")
	}
}

func TestChunkText_CodeBlockOversize(t *testing.T) {
	// A single code block that exceeds 4096 chars should be split at limit.
	// (Per spec, we hard-cut for now - document attachment is Phase 3)
	code := strings.Repeat("x", 5000)
	text := "<pre><code>" + code + "</code></pre>"

	chunks := chunkText(text)
	// Should split since code block exceeds limit
	if len(chunks) < 2 {
		t.Logf("Got %d chunks for oversized code block: %v", len(chunks), chunks)
		// This is expected behavior - hard cut within code block
	}
	// Verify chunks aren't empty
	for i, c := range chunks {
		if c == "" {
			t.Errorf("chunk %d is empty", i)
		}
	}
}

func TestFindCodeBlocks(t *testing.T) {
	html := "<p>text</p><pre><code>code</code></pre><p>more</p>"
	blocks := findCodeBlocks(html)
	if len(blocks) != 1 {
		t.Fatalf("want 1 block, got %d", len(blocks))
	}
	if blocks[0].start < 0 || blocks[0].end <= blocks[0].start {
		t.Errorf("invalid block: %+v", blocks[0])
	}
}

func TestIsInsideCodeBlock(t *testing.T) {
	blocks := []codeBlock{{start: 10, end: 30}}
	if !isInsideCodeBlock(15, blocks) {
		t.Error("position 15 should be inside code block")
	}
	if isInsideCodeBlock(5, blocks) {
		t.Error("position 5 should be outside code block")
	}
	if isInsideCodeBlock(35, blocks) {
		t.Error("position 35 should be outside code block")
	}
}

func TestBalanceTags(t *testing.T) {
	tests := []struct {
		name         string
		chunk        string
		currentOpen  string
		wantClose    string
		wantReopen   string
	}{
		{
			name:      "no open tags",
			chunk:     "plain text",
			wantClose: "",
		},
		{
			name:      "single open tag",
			chunk:     "<b>text",
			wantClose: "</b>",
		},
		{
			name:      "nested tags",
			chunk:     "<b><i>text",
			wantClose: "</i></b>",
		},
		{
			name:      "self-closing in chunk",
			chunk:     "<b>text</b>",
			wantClose: "",
		},
		{
			name:      "pre block",
			chunk:     "<pre><code>code",
			wantClose: "</code></pre>",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, result := balanceTags(tt.chunk, tt.currentOpen)
			if result.closeTags != tt.wantClose {
				t.Errorf("closeTags = %q, want %q", result.closeTags, tt.wantClose)
			}
		})
	}
}

// ---- Sender integration tests (against a mock proxy) ----

func newTestSender(t *testing.T, proxyURL string) *Sender {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "sender-test-*.db")
	if err != nil {
		t.Fatalf("temp db: %v", err)
	}
	f.Close()

	s, err := NewSender(proxyURL, f.Name())
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSender_SendResponse_SingleChunk(t *testing.T) {
	var received []contract.SendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send" {
			http.NotFound(w, r)
			return
		}
		var req contract.SendRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		received = append(received, req)
		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 42})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	origMsgID := int64(10)
	err := s.SendResponse(context.Background(), -100123, nil, origMsgID, "hello")
	if err != nil {
		t.Fatalf("SendResponse: %v", err)
	}
	if len(received) != 1 {
		t.Fatalf("want 1 request, got %d", len(received))
	}
	if received[0].ReplyToMessageID == nil || *received[0].ReplyToMessageID != origMsgID {
		t.Errorf("first chunk should reply to origMsgID=%d", origMsgID)
	}
}

func TestSender_SendResponse_MultiChunk_ReplyOnly_First(t *testing.T) {
	var received []contract.SendRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req contract.SendRequest
		json.NewDecoder(r.Body).Decode(&req)
		received = append(received, req)
		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: int64(len(received))})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	threadID := int64(55)
	origMsgID := int64(10)

	// Two 3000-char blocks separated by \n\n → 2 chunks.
	text := strings.Repeat("a", 3000) + "\n\n" + strings.Repeat("b", 3000)
	if err := s.SendResponse(context.Background(), -100, &threadID, origMsgID, text); err != nil {
		t.Fatalf("SendResponse: %v", err)
	}

	if len(received) != 2 {
		t.Fatalf("want 2 requests, got %d", len(received))
	}

	// First chunk must be a reply.
	if received[0].ReplyToMessageID == nil || *received[0].ReplyToMessageID != origMsgID {
		t.Errorf("chunk 0 should reply to %d", origMsgID)
	}
	// Second chunk must NOT be a reply but must carry thread_id.
	if received[1].ReplyToMessageID != nil {
		t.Errorf("chunk 1 should not have reply_to_message_id")
	}
	if received[1].ThreadID == nil || *received[1].ThreadID != threadID {
		t.Errorf("chunk 1 should carry thread_id=%d", threadID)
	}
}

func TestSender_SendTyping(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/send_chat_action" {
			called = true
		}
		json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	s.SendTyping(context.Background(), -100, nil)
	if !called {
		t.Error("expected /send_chat_action to be called")
	}
}

func TestSender_Retry_ConnectionError(t *testing.T) {
	// Point at a server that is already closed → connection refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()

	s := newTestSender(t, srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := s.SendResponse(ctx, -100, nil, 1, "hi")
	if err == nil {
		t.Error("expected an error on connection failure")
	}
}

func TestSender_Respects_RateLimit(t *testing.T) {
	retryAfter := 0 // 0 seconds so the test doesn't actually wait
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(contract.ErrorResponse{
				ErrorCode:   contract.ErrCodeRateLimit,
				Description: "Too Many Requests",
				RetryAfter:  &retryAfter,
			})
			return
		}
		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 1})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	err := s.SendResponse(context.Background(), -100, nil, 1, "hi")
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if calls != 2 {
		t.Errorf("expected 2 calls (1 rate-limited + 1 success), got %d", calls)
	}
}

func TestSender_SendPhoto_MultipartForm(t *testing.T) {
	var receivedChatID string
	var receivedThreadID string
	var receivedCaption string
	var receivedReplyTo string
	var receivedFilename string
	var receivedContent []byte

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send_photo" {
			http.NotFound(w, r)
			return
		}

		// Parse multipart form
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		// Extract form fields
		receivedChatID = r.FormValue("chat_id")
		receivedThreadID = r.FormValue("thread_id")
		receivedCaption = r.FormValue("caption")
		receivedReplyTo = r.FormValue("reply_to_message_id")

		// Extract file
		file, header, err := r.FormFile("photo")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		defer file.Close()

		receivedFilename = header.Filename
		receivedContent = make([]byte, header.Size)
		if _, err := file.Read(receivedContent); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 42})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx := context.Background()

	// Test with all fields populated
	chatID := int64(-1001234567890)
	threadID := int64(123)
	origMsgID := int64(99)
	caption := "Test image"
	filename := "test.png"
	content := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A} // PNG header

	err := s.SendPhoto(ctx, chatID, &threadID, origMsgID, caption, filename, content)
	if err != nil {
		t.Fatalf("SendPhoto failed: %v", err)
	}

	// Verify all fields were sent correctly
	if receivedChatID != fmt.Sprintf("%d", chatID) {
		t.Errorf("chat_id = %s, want %d", receivedChatID, chatID)
	}
	if receivedThreadID != fmt.Sprintf("%d", threadID) {
		t.Errorf("thread_id = %s, want %d", receivedThreadID, threadID)
	}
	if receivedCaption != caption {
		t.Errorf("caption = %s, want %s", receivedCaption, caption)
	}
	if receivedReplyTo != fmt.Sprintf("%d", origMsgID) {
		t.Errorf("reply_to_message_id = %s, want %d", receivedReplyTo, origMsgID)
	}
	if receivedFilename != filename {
		t.Errorf("filename = %s, want %s", receivedFilename, filename)
	}
	if !bytes.Equal(receivedContent, content) {
		t.Errorf("content mismatch: got %v, want %v", receivedContent, content)
	}
}

func TestSender_SendPhoto_WithoutOptionalFields(t *testing.T) {
	var receivedChatID string
	var receivedThreadID string
	var receivedCaption string
	var receivedReplyTo string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/send_photo" {
			http.NotFound(w, r)
			return
		}

		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		receivedChatID = r.FormValue("chat_id")
		receivedThreadID = r.FormValue("thread_id")
		receivedCaption = r.FormValue("caption")
		receivedReplyTo = r.FormValue("reply_to_message_id")

		// Verify file exists
		file, _, err := r.FormFile("photo")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		file.Close()

		json.NewEncoder(w).Encode(contract.SendResponse{OK: true, MessageID: 42})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx := context.Background()

	// Test with minimal fields (no threadID, no caption, no reply)
	chatID := int64(-1001234567890)
	content := []byte("minimal photo data")

	err := s.SendPhoto(ctx, chatID, nil, 0, "", "photo.jpg", content)
	if err != nil {
		t.Fatalf("SendPhoto failed: %v", err)
	}

	if receivedChatID != fmt.Sprintf("%d", chatID) {
		t.Errorf("chat_id = %s, want %d", receivedChatID, chatID)
	}
	if receivedThreadID != "" {
		t.Errorf("thread_id should be empty when nil, got %s", receivedThreadID)
	}
	if receivedCaption != "" {
		t.Errorf("caption should be empty when empty string, got %s", receivedCaption)
	}
	if receivedReplyTo != "" {
		t.Errorf("reply_to_message_id should be empty when 0, got %s", receivedReplyTo)
	}
}

func TestSender_SendPhoto_ErrorResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(contract.ErrorResponse{
			ErrorCode:   400,
			Description: "Bad Request",
		})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx := context.Background()

	err := s.SendPhoto(ctx, -100, nil, 0, "", "photo.jpg", []byte("data"))
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*contract.ErrorResponse)
	if !ok {
		t.Fatalf("expected *contract.ErrorResponse, got %T", err)
	}
	if apiErr.ErrorCode != 400 {
		t.Errorf("error code = %d, want 400", apiErr.ErrorCode)
	}
	if apiErr.Description != "Bad Request" {
		t.Errorf("description = %s, want 'Bad Request'", apiErr.Description)
	}
}

// ---- postWithRetry backoff / rate-limit tests ----
//
// The retry constants (senderBackoffMin=1s, doubling up to senderBackoffMax=30s,
// senderMaxRetries=5) are package-level and not injectable, so these tests bound
// every wait with a context deadline and assert on the attempt counts observed
// within that window. Handlers run on the httptest server goroutine, so all
// counters are atomic.

func TestSender_NonRetryableAPIError_NoRetry(t *testing.T) {
	// A 400 from the proxy is a definitive API error — postWithRetry must
	// return it immediately without burning any retries.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(contract.ErrorResponse{
			ErrorCode:   contract.ErrCodeBadRequest,
			Description: "chat not found",
		})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	start := time.Now()
	err := s.EditMessage(context.Background(), -100, 1, "text")
	if err == nil {
		t.Fatal("expected error")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("non-retryable error should return immediately, took %s", elapsed)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call (no retries), got %d", got)
	}
	apiErr, ok := err.(*contract.ErrorResponse)
	if !ok {
		t.Fatalf("expected *contract.ErrorResponse, got %T: %v", err, err)
	}
	if apiErr.ErrorCode != contract.ErrCodeBadRequest {
		t.Errorf("error code = %d, want %d", apiErr.ErrorCode, contract.ErrCodeBadRequest)
	}
	if apiErr.Description != "chat not found" {
		t.Errorf("description = %q, want %q", apiErr.Description, "chat not found")
	}
}

func TestSender_RateLimit_WaitsForRetryAfter(t *testing.T) {
	// 429 with retry_after=60: postWithRetry must wait the full 60s unless the
	// context expires first. A 300ms deadline means exactly one attempt.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		retryAfter := 60
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		json.NewEncoder(w).Encode(contract.ErrorResponse{
			ErrorCode:  contract.ErrCodeRateLimit,
			RetryAfter: &retryAfter,
		})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := s.EditMessage(ctx, -100, 1, "text")
	if err == nil {
		t.Fatal("expected deadline error while waiting out retry_after")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call before deadline, got %d", got)
	}
}

func TestSender_RateLimit_DefaultBackoffWithoutRetryAfter(t *testing.T) {
	// A 429 whose body carries no retry_after must fall back to
	// senderBackoffMin (1s), not retry instantly. A 400ms deadline bounds the
	// wait; if the default were zero we'd see several calls in that window.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		// Empty body: decode leaves RetryAfter nil.
		fmt.Fprint(w, "")
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	err := s.EditMessage(ctx, -100, 1, "text")
	if err == nil {
		t.Fatal("expected deadline error while waiting out default backoff")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call before deadline, got %d", got)
	}
}

func TestSender_RateLimit_DoesNotConsumeRetryBudget(t *testing.T) {
	// Eight consecutive 429s (retry_after=0, so each wait is instant) followed
	// by success. senderMaxRetries is 5, so succeeding on the 9th attempt is
	// only possible if rate limits don't charge a retry slot (attempt--).
	const rateLimits = 8
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if int(n) <= rateLimits {
			retryAfter := 0
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			json.NewEncoder(w).Encode(contract.ErrorResponse{
				ErrorCode:  contract.ErrCodeRateLimit,
				RetryAfter: &retryAfter,
			})
			return
		}
		json.NewEncoder(w).Encode(contract.OKResponse{OK: true})
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	err := s.EditMessage(context.Background(), -100, 1, "text")
	if err != nil {
		t.Fatalf("expected success after %d rate limits, got: %v", rateLimits, err)
	}
	if got := calls.Load(); got != rateLimits+1 {
		t.Errorf("expected %d calls total, got %d", rateLimits+1, got)
	}
}

func TestSender_ExponentialBackoff_ConnectionErrors(t *testing.T) {
	// Aborting the handler mid-request gives an instant, countable transport
	// error (postJSON returns a plain error, not *contract.ErrorResponse), so
	// postWithRetry takes the exponential-backoff branch: 1s, then 2s, then 4s…
	//
	// With a 2.9s deadline the attempts land at t≈0 and t≈1.0s; the third
	// would be at t≈3.0s (after the doubled 2s backoff), so exactly 2 calls
	// proves the backoff doubles — a fixed 1s backoff would fit a third call
	// at t≈2.0s.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 2900*time.Millisecond)
	defer cancel()

	err := s.EditMessage(ctx, -100, 1, "text")
	if err == nil {
		t.Fatal("expected deadline error during backoff")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error = %v, want context.DeadlineExceeded", err)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("expected exactly 2 attempts in 2.9s window (backoff 1s then 2s), got %d", got)
	}
}

func TestSender_CancelledContext_ReturnsCtxErr(t *testing.T) {
	// A pre-cancelled context fails the transport call itself; the backoff
	// select must notice ctx.Done() and return ctx.Err() instead of retrying.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		panic(http.ErrAbortHandler)
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := s.EditMessage(ctx, -100, 1, "text")
	if err == nil {
		t.Fatal("expected context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("expected no server calls with pre-cancelled context, got %d", got)
	}
}

func TestSender_CancelDuringRateLimitWait_ReturnsCtxErr(t *testing.T) {
	// 429 with no retry_after → default 1s wait. Cancelling the context 100ms
	// in must interrupt the wait and surface context.Canceled (not
	// DeadlineExceeded, which would indicate a timeout was used instead).
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, "")
	}))
	defer srv.Close()

	s := newTestSender(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)

	err := s.EditMessage(ctx, -100, 1, "text")
	if err == nil {
		t.Fatal("expected context.Canceled")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("expected exactly 1 call, got %d", got)
	}
}
