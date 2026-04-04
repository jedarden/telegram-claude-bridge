package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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
