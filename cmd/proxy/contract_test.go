package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

// proxyContractMux is the production route table kept in a test helper so the
// contract tests exercise both handler behavior and net/http path matching.
func proxyContractMux(p *telegram.Poller, s *telegram.Sender) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", handleHealth(p))
	mux.HandleFunc("/updates", handleUpdates(p))
	mux.HandleFunc("/send", handleSend(s))
	mux.HandleFunc("/edit", handleEdit(s))
	mux.HandleFunc("/send_chat_action", handleSendChatAction(s))
	mux.HandleFunc("/create_topic", handleCreateTopic(s))
	mux.HandleFunc("/edit_topic", handleEditTopic(s))
	mux.HandleFunc("/close_topic", handleCloseTopic(s))
	mux.HandleFunc("/reopen_topic", handleReopenTopic(s))
	mux.HandleFunc("/pin_message", handlePinMessage(s))
	mux.HandleFunc("/get_message", handleGetMessage(p))
	mux.HandleFunc("/answer_callback", handleAnswerCallback(s))
	mux.HandleFunc("GET /file/{file_id}", handleFile(s))
	mux.HandleFunc("/send_photo", handleSendPhoto(s))
	mux.HandleFunc("/send_document", handleSendDocument(s))
	mux.HandleFunc("/send_audio", handleSendAudio(s))
	mux.HandleFunc("/send_video", handleSendVideo(s))
	return mux
}

func proxyJSONRequest(t *testing.T, handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func proxyJSONRequestWithContext(t *testing.T, handler http.Handler, ctx context.Context, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func jsonObject(t *testing.T, value string) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal([]byte(value), &object); err != nil {
		t.Fatalf("decode JSON object %q: %v", value, err)
	}
	return object
}

func writeTelegramJSON(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func writeTelegramFailure(w http.ResponseWriter, errorCode int, description string, retryAfter *int) {
	w.Header().Set("Content-Type", "application/json")
	response := map[string]any{
		"ok":          false,
		"error_code":  errorCode,
		"description": description,
	}
	if retryAfter != nil {
		response["parameters"] = map[string]any{"retry_after": *retryAfter}
	}
	_ = json.NewEncoder(w).Encode(response)
}

func decodeProxyError(t *testing.T, rec *httptest.ResponseRecorder) contract.ErrorResponse {
	t.Helper()
	var got contract.ErrorResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode proxy error: %v; body=%q", err, rec.Body.String())
	}
	if got.OK {
		t.Errorf("error response has ok=true: %+v", got)
	}
	return got
}

func assertJSONContentType(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestProxyHTTP_MethodValidation(t *testing.T) {
	poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "test-version", "test-sha", "")
	sender := telegram.NewSender("test-token", "http://127.0.0.1:1")
	mux := proxyContractMux(poller, sender)

	cases := []struct {
		name string
		path string
		want string
	}{
		{name: "health", path: "/health", want: http.MethodPost},
		{name: "updates", path: "/updates", want: http.MethodPost},
		{name: "send", path: "/send", want: http.MethodGet},
		{name: "edit", path: "/edit", want: http.MethodGet},
		{name: "chat action", path: "/send_chat_action", want: http.MethodGet},
		{name: "create topic", path: "/create_topic", want: http.MethodGet},
		{name: "edit topic", path: "/edit_topic", want: http.MethodGet},
		{name: "close topic", path: "/close_topic", want: http.MethodGet},
		{name: "reopen topic", path: "/reopen_topic", want: http.MethodGet},
		{name: "pin message", path: "/pin_message", want: http.MethodGet},
		{name: "get message", path: "/get_message", want: http.MethodPut},
		{name: "answer callback", path: "/answer_callback", want: http.MethodGet},
		{name: "file", path: "/file/file-id", want: http.MethodPost},
		{name: "photo", path: "/send_photo", want: http.MethodGet},
		{name: "document", path: "/send_document", want: http.MethodGet},
		{name: "audio", path: "/send_audio", want: http.MethodGet},
		{name: "video", path: "/send_video", want: http.MethodGet},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.want, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s status = %d, want %d; body=%q", tc.want, tc.path, rec.Code, http.StatusMethodNotAllowed, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
				t.Errorf("%s %s Content-Type = %q, want plain text", tc.want, tc.path, got)
			}
			if got := strings.TrimSpace(rec.Body.String()); !strings.EqualFold(got, "method not allowed") {
				t.Errorf("%s %s body = %q, want plain-text 405 response", tc.want, tc.path, got)
			}
		})
	}
}

func TestProxyHTTP_JSONValidation(t *testing.T) {
	cases := []struct {
		name string
		path string
	}{
		{name: "send", path: "/send"},
		{name: "edit", path: "/edit"},
		{name: "chat action", path: "/send_chat_action"},
		{name: "create topic", path: "/create_topic"},
		{name: "edit topic", path: "/edit_topic"},
		{name: "close topic", path: "/close_topic"},
		{name: "reopen topic", path: "/reopen_topic"},
		{name: "pin message", path: "/pin_message"},
		{name: "get message", path: "/get_message"},
		{name: "answer callback", path: "/answer_callback"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("Telegram API called for invalid JSON: %s", r.URL.Path)
				writeTelegramFailure(w, http.StatusBadGateway, "unexpected request", nil)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, tc.path, "{not-json")
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			got := decodeProxyError(t, rec)
			if got.ErrorCode != contract.ErrCodeBadRequest {
				t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeBadRequest)
			}
			if !strings.HasPrefix(got.Description, "invalid JSON:") {
				t.Errorf("description = %q, want invalid JSON prefix", got.Description)
			}
		})
	}
}

func TestProxyHTTP_JSONEndpoints(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		telegramMethod string
		body           string
		telegramBody   string
		wantResult     any
	}{
		{
			name:           "send",
			path:           "/send",
			telegramMethod: "sendMessage",
			body:           `{"chat_id":-100123,"thread_id":42,"text":"hello","parse_mode":"HTML","reply_to_message_id":7,"reply_markup":{"inline_keyboard":[[{"text":"Approve","callback_data":"approve"}]]}}`,
			telegramBody:   `{"chat_id":-100123,"message_thread_id":42,"text":"hello","parse_mode":"HTML","reply_to_message_id":7,"reply_markup":{"inline_keyboard":[[{"text":"Approve","callback_data":"approve"}]]}}`,
			wantResult:     map[string]any{"message_id": float64(2001)},
		},
		{
			name:           "edit",
			path:           "/edit",
			telegramMethod: "editMessageText",
			body:           `{"chat_id":-100123,"message_id":2001,"text":"updated","parse_mode":"MarkdownV2","reply_markup":{"inline_keyboard":[]}}`,
			wantResult:     map[string]any{"message_id": float64(2001)},
		},
		{
			name:           "chat action",
			path:           "/send_chat_action",
			telegramMethod: "sendChatAction",
			body:           `{"chat_id":-100123,"thread_id":42,"action":"typing"}`,
			telegramBody:   `{"chat_id":-100123,"message_thread_id":42,"action":"typing"}`,
			wantResult:     true,
		},
		{
			name:           "create topic",
			path:           "/create_topic",
			telegramMethod: "createForumTopic",
			body:           `{"chat_id":-100123,"name":"new topic","icon_color":7322096}`,
			wantResult:     map[string]any{"message_thread_id": float64(43), "name": "new topic", "icon_color": float64(7322096)},
		},
		{
			name:           "edit topic",
			path:           "/edit_topic",
			telegramMethod: "editForumTopic",
			body:           `{"chat_id":-100123,"thread_id":43,"name":"renamed","icon_color":9371288}`,
			telegramBody:   `{"chat_id":-100123,"message_thread_id":43,"name":"renamed","icon_color":9371288}`,
			wantResult:     true,
		},
		{
			name:           "close topic",
			path:           "/close_topic",
			telegramMethod: "closeForumTopic",
			body:           `{"chat_id":-100123,"thread_id":43}`,
			telegramBody:   `{"chat_id":-100123,"message_thread_id":43}`,
			wantResult:     true,
		},
		{
			name:           "reopen topic",
			path:           "/reopen_topic",
			telegramMethod: "reopenForumTopic",
			body:           `{"chat_id":-100123,"thread_id":43}`,
			telegramBody:   `{"chat_id":-100123,"message_thread_id":43}`,
			wantResult:     true,
		},
		{
			name:           "pin message",
			path:           "/pin_message",
			telegramMethod: "pinChatMessage",
			body:           `{"chat_id":-100123,"message_id":2001,"disable_notification":true}`,
			wantResult:     true,
		},
		{
			name:           "answer callback",
			path:           "/answer_callback",
			telegramMethod: "answerCallbackQuery",
			body:           `{"callback_query_id":"query-id","text":"Approved","show_alert":false}`,
			wantResult:     true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				wantPath := "/bottest-token/" + tc.telegramMethod
				if r.URL.Path != wantPath {
					t.Errorf("Telegram path = %q, want %q", r.URL.Path, wantPath)
				}
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode Telegram request: %v", err)
				}
				want := jsonObject(t, tc.body)
				if tc.telegramBody != "" {
					want = jsonObject(t, tc.telegramBody)
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("Telegram body = %#v, want %#v", got, want)
				}
				writeTelegramJSON(w, tc.wantResult)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, tc.path, tc.body)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)

			switch tc.path {
			case "/send", "/edit":
				var got contract.SendResponse
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if got != (contract.SendResponse{OK: true, MessageID: 2001}) {
					t.Errorf("response = %+v, want ok=true message_id=2001", got)
				}
			case "/create_topic":
				var got contract.CreateTopicResponse
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if got.OK != true || got.ThreadID != 43 || got.Name != "new topic" || got.IconColor == nil || *got.IconColor != contract.IconColorLightBlue {
					t.Errorf("response = %+v, want created topic 43", got)
				}
			default:
				var got contract.OKResponse
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if !got.OK {
					t.Errorf("response = %+v, want ok=true", got)
				}
			}
		})
	}
}

func TestProxyHTTP_TelegramErrorStatusMapping(t *testing.T) {
	endpoints := []struct {
		name string
		path string
		body string
	}{
		{name: "send", path: "/send", body: `{"chat_id":-100123,"text":"hello"}`},
		{name: "edit", path: "/edit", body: `{"chat_id":-100123,"message_id":2001,"text":"updated"}`},
		{name: "chat action", path: "/send_chat_action", body: `{"chat_id":-100123,"action":"typing"}`},
		{name: "create topic", path: "/create_topic", body: `{"chat_id":-100123,"name":"topic"}`},
		{name: "edit topic", path: "/edit_topic", body: `{"chat_id":-100123,"thread_id":43,"name":"renamed"}`},
		{name: "close topic", path: "/close_topic", body: `{"chat_id":-100123,"thread_id":43}`},
		{name: "reopen topic", path: "/reopen_topic", body: `{"chat_id":-100123,"thread_id":43}`},
		{name: "pin message", path: "/pin_message", body: `{"chat_id":-100123,"message_id":2001}`},
		{name: "answer callback", path: "/answer_callback", body: `{"callback_query_id":"query-id"}`},
	}

	errorCases := []struct {
		name       string
		errorCode  int
		wantStatus int
		retryAfter *int
	}{
		{name: "bad request", errorCode: 400, wantStatus: http.StatusBadGateway},
		{name: "unauthorized", errorCode: 401, wantStatus: http.StatusBadGateway},
		{name: "forbidden", errorCode: 403, wantStatus: http.StatusBadGateway},
		{name: "conflict", errorCode: 409, wantStatus: http.StatusBadGateway},
		{name: "rate limit", errorCode: 429, wantStatus: http.StatusTooManyRequests, retryAfter: intPointer(3)},
	}

	for _, endpoint := range endpoints {
		for _, failure := range errorCases {
			t.Run(endpoint.name+"/"+failure.name, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeTelegramFailure(w, failure.errorCode, "Telegram says no", failure.retryAfter)
				}))
				defer api.Close()

				poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
				sender := telegram.NewSender("test-token", api.URL)
				rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, endpoint.path, endpoint.body)
				if rec.Code != failure.wantStatus {
					t.Fatalf("status = %d, want %d; body=%q", rec.Code, failure.wantStatus, rec.Body.String())
				}
				assertJSONContentType(t, rec)
				got := decodeProxyError(t, rec)
				if got.ErrorCode != failure.errorCode {
					t.Errorf("error_code = %d, want %d", got.ErrorCode, failure.errorCode)
				}
				if got.Description != "Telegram says no" {
					t.Errorf("description = %q, want Telegram says no", got.Description)
				}
				if !reflect.DeepEqual(got.RetryAfter, failure.retryAfter) {
					t.Errorf("retry_after = %v, want %v", got.RetryAfter, failure.retryAfter)
				}
			})
		}
	}
}

func intPointer(value int) *int { return &value }

func TestProxyHTTP_SendTimeoutAndCancellation(t *testing.T) {
	t.Run("deadline maps to timeout error", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer api.Close()

		poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
		sender := telegram.NewSender("test-token", api.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		started := time.Now()
		rec := proxyJSONRequestWithContext(t, proxyContractMux(poller, sender), ctx, http.MethodPost, "/send", `{"chat_id":-100123,"text":"hello"}`)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("deadline response took %s", elapsed)
		}
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
		}
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeTelegramTimeout {
			t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeTelegramTimeout)
		}
	})

	t.Run("cancellation is immediate and distinct from timeout", func(t *testing.T) {
		var calls atomic.Int32
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			<-r.Context().Done()
		}))
		defer api.Close()

		poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
		sender := telegram.NewSender("test-token", api.URL)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		started := time.Now()
		rec := proxyJSONRequestWithContext(t, proxyContractMux(poller, sender), ctx, http.MethodPost, "/send", `{"chat_id":-100123,"text":"hello"}`)
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("cancellation response took %s", elapsed)
		}
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusBadGateway, rec.Body.String())
		}
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeTelegramUnreachable {
			t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeTelegramUnreachable)
		}
		if calls.Load() != 0 {
			t.Errorf("Telegram calls = %d, want 0 for pre-cancelled request", calls.Load())
		}
	})
}

func TestProxyHTTP_UpdatesTimeoutAndCancellation(t *testing.T) {
	cases := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
		query   string
		maxTime time.Duration
	}{
		{
			name: "request deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 30*time.Millisecond)
			},
			query:   "timeout=1",
			maxTime: time.Second,
		},
		{
			name: "already cancelled",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, func() {}
			},
			query:   "timeout=1",
			maxTime: time.Second,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "test-version", "test-sha", "")
			ctx, cancel := tc.context()
			defer cancel()
			started := time.Now()
			req := httptest.NewRequest(http.MethodGet, "/updates?"+tc.query, nil).WithContext(ctx)
			rec := httptest.NewRecorder()
			handleUpdates(poller).ServeHTTP(rec, req)
			if elapsed := time.Since(started); elapsed > tc.maxTime {
				t.Fatalf("request took %s, want less than %s", elapsed, tc.maxTime)
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			assertJSONContentType(t, rec)
			var got contract.UpdatesResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if !got.OK || got.Updates == nil || len(got.Updates) != 0 {
				t.Errorf("response = %+v, want ok=true and empty non-nil updates", got)
			}
		})
	}
}

func TestProxyHTTP_UpdatesActiveLongPollCancellation(t *testing.T) {
	poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "test-version", "test-sha", "")
	handler := handleUpdates(poller)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/updates?timeout=30", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	done := make(chan struct{})

	go func() {
		handler.ServeHTTP(rec, req)
		close(done)
	}()

	// Give PeekUpdates time to enter its wait before canceling the request.
	time.Sleep(20 * time.Millisecond)
	started := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("active long poll did not stop after cancellation")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("canceled long poll took %s", elapsed)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	var got contract.UpdatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !got.OK || got.Updates == nil || len(got.Updates) != 0 {
		t.Errorf("response = %+v, want ok=true and empty non-nil updates", got)
	}
}

func TestProxyHTTP_GetMessage(t *testing.T) {
	var calls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/bottest-token/getUpdates" {
			http.NotFound(w, r)
			return
		}
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(telegram.GetUpdatesResponse{
				OK:     true,
				Result: []telegram.Update{tgTextUpdate(700, 70)},
			})
			return
		}
		<-r.Context().Done()
	}))

	poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
	ctx, cancel := context.WithCancel(context.Background())
	go poller.Start(ctx)
	deadline := time.Now().Add(time.Second)
	for poller.GetMessage(-100123456789, 70) == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if poller.GetMessage(-100123456789, 70) == nil {
		cancel()
		api.Close()
		t.Fatal("poller did not cache message")
	}

	mux := proxyContractMux(poller, telegram.NewSender("test-token", api.URL))
	cases := []struct {
		name       string
		method     string
		path       string
		body       string
		wantStatus int
	}{
		{name: "GET query", method: http.MethodGet, path: "/get_message?chat_id=-100123456789&message_id=70", wantStatus: http.StatusOK},
		{name: "POST JSON", method: http.MethodPost, path: "/get_message", body: `{"chat_id":-100123456789,"message_id":70}`, wantStatus: http.StatusOK},
		{name: "missing query parameter", method: http.MethodGet, path: "/get_message?chat_id=-100123456789", wantStatus: http.StatusBadRequest},
		{name: "invalid chat id", method: http.MethodGet, path: "/get_message?chat_id=bad&message_id=70", wantStatus: http.StatusBadRequest},
		{name: "invalid message id", method: http.MethodGet, path: "/get_message?chat_id=-100123456789&message_id=bad", wantStatus: http.StatusBadRequest},
		{name: "not cached", method: http.MethodGet, path: "/get_message?chat_id=-100123456789&message_id=71", wantStatus: http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			if tc.method == http.MethodPost {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if tc.wantStatus == http.StatusOK {
				assertJSONContentType(t, rec)
				var got contract.GetMessageResponse
				if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
					t.Fatalf("decode response: %v", err)
				}
				if !got.OK || got.Message == nil || got.Message.Type != contract.ContentTypeText || got.Message.Text == nil || *got.Message.Text != "hello" {
					t.Errorf("response = %+v, want cached hello message", got)
				}
			} else {
				got := decodeProxyError(t, rec)
				wantCode := contract.ErrCodeBadRequest
				if tc.wantStatus == http.StatusNotFound {
					wantCode = http.StatusNotFound
				}
				if got.ErrorCode != wantCode {
					t.Errorf("error_code = %d, want %d", got.ErrorCode, wantCode)
				}
			}
		})
	}
	cancel()
	api.Close()
}

func multipartRequest(t *testing.T, path string, fields map[string]string, fileField, filename string, data []byte, includeFile bool) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("write form field %s: %v", key, err)
		}
	}
	if includeFile {
		part, err := writer.CreateFormFile(fileField, filename)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(data); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart form: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}

func TestProxyHTTP_MediaEndpoints(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		telegramMethod string
		fileField      string
		filename       string
		data           []byte
		fields         map[string]string
		wantName       string
	}{
		{
			name: "photo", path: "/send_photo", telegramMethod: "sendPhoto", fileField: "photo", filename: "image.jpg", data: []byte("photo-bytes"),
			fields:   map[string]string{"chat_id": "-100123", "thread_id": "42", "caption": "caption", "parse_mode": "HTML", "reply_to_message_id": "7"},
			wantName: "image.jpg",
		},
		{
			name: "document", path: "/send_document", telegramMethod: "sendDocument", fileField: "document", filename: "original.txt", data: []byte("document-bytes"),
			fields:   map[string]string{"chat_id": "-100123", "thread_id": "42", "caption": "caption", "parse_mode": "MarkdownV2", "reply_to_message_id": "7", "file_name": "override.md"},
			wantName: "override.md",
		},
		{
			name: "audio", path: "/send_audio", telegramMethod: "sendAudio", fileField: "audio", filename: "recording.ogg", data: []byte("audio-bytes"),
			fields:   map[string]string{"chat_id": "-100123", "thread_id": "42", "caption": "caption", "parse_mode": "HTML", "duration": "12", "title": "meeting", "reply_to_message_id": "7"},
			wantName: "recording.ogg",
		},
		{
			name: "video", path: "/send_video", telegramMethod: "sendVideo", fileField: "video", filename: "clip.mp4", data: []byte("video-bytes"),
			fields:   map[string]string{"chat_id": "-100123", "thread_id": "42", "caption": "caption", "parse_mode": "HTML", "duration": "30", "width": "1920", "height": "1080", "reply_to_message_id": "7"},
			wantName: "clip.mp4",
		},
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/bottest-token/"+tc.telegramMethod {
					t.Errorf("Telegram path = %q, want /bottest-token/%s", r.URL.Path, tc.telegramMethod)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Errorf("parse Telegram multipart form: %v", err)
					return
				}
				for key, want := range tc.fields {
					if key == "file_name" {
						continue
					}
					telegramKey := key
					if key == "thread_id" {
						telegramKey = "message_thread_id"
					}
					if got := r.FormValue(telegramKey); got != want {
						t.Errorf("Telegram field %s = %q, want %q", telegramKey, got, want)
					}
				}
				file, header, err := r.FormFile(tc.fileField)
				if err != nil {
					t.Errorf("Telegram file %s: %v", tc.fileField, err)
					return
				}
				defer file.Close()
				gotData, err := io.ReadAll(file)
				if err != nil {
					t.Errorf("read Telegram file: %v", err)
				}
				if !bytes.Equal(gotData, tc.data) {
					t.Errorf("Telegram file data = %q, want %q", gotData, tc.data)
				}
				if header.Filename != tc.wantName {
					t.Errorf("Telegram filename = %q, want %q", header.Filename, tc.wantName)
				}
				writeTelegramJSON(w, map[string]any{"message_id": 3000 + index})
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			req := multipartRequest(t, tc.path, tc.fields, tc.fileField, tc.filename, tc.data, true)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			var got contract.SendResponse
			if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if got.MessageID != int64(3000+index) || !got.OK {
				t.Errorf("response = %+v, want message_id=%d", got, 3000+index)
			}
		})
	}
}

func TestProxyHTTP_MediaTelegramErrorStatusMapping(t *testing.T) {
	endpoints := []struct {
		name      string
		path      string
		fileField string
	}{
		{name: "photo", path: "/send_photo", fileField: "photo"},
		{name: "document", path: "/send_document", fileField: "document"},
		{name: "audio", path: "/send_audio", fileField: "audio"},
		{name: "video", path: "/send_video", fileField: "video"},
	}
	failures := []struct {
		name       string
		errorCode  int
		wantStatus int
		retryAfter *int
	}{
		{name: "bad request", errorCode: 400, wantStatus: http.StatusBadGateway},
		{name: "rate limit", errorCode: 429, wantStatus: http.StatusTooManyRequests, retryAfter: intPointer(4)},
	}

	for _, endpoint := range endpoints {
		for _, failure := range failures {
			t.Run(endpoint.name+"/"+failure.name, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeTelegramFailure(w, failure.errorCode, "Telegram media error", failure.retryAfter)
				}))
				defer api.Close()

				poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
				sender := telegram.NewSender("test-token", api.URL)
				req := multipartRequest(t, endpoint.path, map[string]string{"chat_id": "-100123"}, endpoint.fileField, "file.bin", []byte("media"), true)
				rec := httptest.NewRecorder()
				proxyContractMux(poller, sender).ServeHTTP(rec, req)
				if rec.Code != failure.wantStatus {
					t.Fatalf("status = %d, want %d; body=%q", rec.Code, failure.wantStatus, rec.Body.String())
				}
				got := decodeProxyError(t, rec)
				if got.ErrorCode != failure.errorCode {
					t.Errorf("error_code = %d, want %d", got.ErrorCode, failure.errorCode)
				}
				if !reflect.DeepEqual(got.RetryAfter, failure.retryAfter) {
					t.Errorf("retry_after = %v, want %v", got.RetryAfter, failure.retryAfter)
				}
			})
		}
	}
}

func TestProxyHTTP_MediaValidationAndLimits(t *testing.T) {
	cases := []struct {
		name        string
		path        string
		fileField   string
		fields      map[string]string
		data        []byte
		includeFile bool
		wantStatus  int
		wantCode    int
	}{
		{name: "missing chat id", path: "/send_photo", fileField: "photo", fields: nil, data: []byte("x"), includeFile: true, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "invalid chat id", path: "/send_document", fileField: "document", fields: map[string]string{"chat_id": "not-an-int"}, data: []byte("x"), includeFile: true, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "missing photo", path: "/send_photo", fileField: "photo", fields: map[string]string{"chat_id": "-100123"}, includeFile: false, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "missing document", path: "/send_document", fileField: "document", fields: map[string]string{"chat_id": "-100123"}, includeFile: false, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "missing audio", path: "/send_audio", fileField: "audio", fields: map[string]string{"chat_id": "-100123"}, includeFile: false, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "missing video", path: "/send_video", fileField: "video", fields: map[string]string{"chat_id": "-100123"}, includeFile: false, wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "invalid multipart", path: "/send_photo", fileField: "photo", wantStatus: http.StatusBadRequest, wantCode: 400},
		{name: "photo too large", path: "/send_photo", fileField: "photo", fields: map[string]string{"chat_id": "-100123"}, data: bytes.Repeat([]byte("x"), maxPhotoSize+8192), includeFile: true, wantStatus: http.StatusRequestEntityTooLarge, wantCode: 413},
		{name: "document too large", path: "/send_document", fileField: "document", fields: map[string]string{"chat_id": "-100123"}, data: bytes.Repeat([]byte("x"), maxMediaSize+8192), includeFile: true, wantStatus: http.StatusRequestEntityTooLarge, wantCode: 413},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("Telegram API called for invalid media request: %s", r.URL.Path)
				writeTelegramFailure(w, http.StatusBadGateway, "unexpected request", nil)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			var rec *httptest.ResponseRecorder
			if tc.name == "invalid multipart" {
				req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("not multipart"))
				req.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
				rec = httptest.NewRecorder()
				proxyContractMux(poller, sender).ServeHTTP(rec, req)
			} else {
				req := multipartRequest(t, tc.path, tc.fields, tc.fileField, "file.bin", tc.data, tc.includeFile)
				rec = httptest.NewRecorder()
				proxyContractMux(poller, sender).ServeHTTP(rec, req)
			}
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			got := decodeProxyError(t, rec)
			if got.ErrorCode != tc.wantCode {
				t.Errorf("error_code = %d, want %d", got.ErrorCode, tc.wantCode)
			}
		})
	}
}

func TestProxyHTTP_FileEndpoint(t *testing.T) {
	cases := []struct {
		name         string
		fileResult   any
		getFileError bool
		getFileCode  int
		retryAfter   *int
		cdnStatus    int
		cdnContent   []byte
		wantStatus   int
		wantCode     int
		wantBody     []byte
		wantCDNCalls int32
	}{
		{
			name:         "success",
			fileResult:   map[string]any{"file_id": "file-id", "file_size": 11, "file_path": "photos/report.pdf"},
			cdnStatus:    http.StatusOK,
			cdnContent:   []byte("file contents"),
			wantStatus:   http.StatusOK,
			wantBody:     []byte("file contents"),
			wantCDNCalls: 1,
		},
		{
			name:         "Telegram rejects file id",
			getFileError: true,
			wantStatus:   http.StatusNotFound,
			wantCode:     http.StatusNotFound,
		},
		{
			name:         "Telegram rate limit",
			getFileError: true,
			getFileCode:  http.StatusTooManyRequests,
			retryAfter:   intPointer(3),
			wantStatus:   http.StatusTooManyRequests,
			wantCode:     contract.ErrCodeRateLimit,
		},
		{
			name:       "missing file path",
			fileResult: map[string]any{"file_id": "file-id"},
			wantStatus: http.StatusNotFound,
			wantCode:   http.StatusNotFound,
		},
		{
			name:       "download too large",
			fileResult: map[string]any{"file_id": "file-id", "file_size": telegram.MaxFileSize + 1, "file_path": "photos/large.bin"},
			wantStatus: http.StatusRequestEntityTooLarge,
			wantCode:   http.StatusRequestEntityTooLarge,
		},
		{
			name:         "CDN not found",
			fileResult:   map[string]any{"file_id": "file-id", "file_size": 11, "file_path": "photos/report.pdf"},
			cdnStatus:    http.StatusNotFound,
			wantStatus:   http.StatusNotFound,
			wantCode:     http.StatusNotFound,
			wantCDNCalls: 1,
		},
		{
			name:         "CDN failure",
			fileResult:   map[string]any{"file_id": "file-id", "file_size": 11, "file_path": "photos/report.pdf"},
			cdnStatus:    http.StatusInternalServerError,
			wantStatus:   http.StatusBadGateway,
			wantCode:     http.StatusBadGateway,
			wantCDNCalls: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cdnCalls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/bottest-token/getFile":
					if tc.getFileError {
						code := tc.getFileCode
						if code == 0 {
							code = http.StatusBadRequest
						}
						writeTelegramFailure(w, code, "Telegram file error", tc.retryAfter)
						return
					}
					writeTelegramJSON(w, tc.fileResult)
				default:
					cdnCalls.Add(1)
					if tc.cdnStatus != http.StatusOK {
						w.WriteHeader(tc.cdnStatus)
						return
					}
					w.Header().Set("Content-Type", "application/pdf")
					_, _ = w.Write(tc.cdnContent)
				}
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			req := httptest.NewRequest(http.MethodGet, "/file/file-id", nil)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if got := cdnCalls.Load(); got != tc.wantCDNCalls {
				t.Errorf("CDN calls = %d, want %d", got, tc.wantCDNCalls)
			}
			if tc.wantStatus == http.StatusOK {
				if !bytes.Equal(rec.Body.Bytes(), tc.wantBody) {
					t.Errorf("body = %q, want %q", rec.Body.Bytes(), tc.wantBody)
				}
				if got := rec.Header().Get("Content-Type"); got != "application/pdf" {
					t.Errorf("Content-Type = %q, want application/pdf", got)
				}
				if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="report.pdf"` {
					t.Errorf("Content-Disposition = %q, want report.pdf attachment", got)
				}
			} else {
				got := decodeProxyError(t, rec)
				if got.ErrorCode != tc.wantCode {
					t.Errorf("error_code = %d, want %d", got.ErrorCode, tc.wantCode)
				}
				if !reflect.DeepEqual(got.RetryAfter, tc.retryAfter) {
					t.Errorf("retry_after = %v, want %v", got.RetryAfter, tc.retryAfter)
				}
			}
		})
	}
}

func TestProxyHTTP_FileDownloadTimeout(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bottest-token/getFile" {
			writeTelegramJSON(w, map[string]any{"file_id": "file-id", "file_path": "photos/report.pdf"})
			return
		}
		time.Sleep(100 * time.Millisecond)
	}))
	defer api.Close()

	poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
	sender := telegram.NewSender("test-token", api.URL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/file/file-id", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	proxyContractMux(poller, sender).ServeHTTP(rec, req)
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body=%q", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	got := decodeProxyError(t, rec)
	if got.ErrorCode != contract.ErrCodeTelegramTimeout {
		t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeTelegramTimeout)
	}
}

func TestProxyHTTP_HealthReadiness(t *testing.T) {
	poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "test-version", "test-sha", "")
	health := handleHealth(poller)

	before := httptest.NewRecorder()
	health.ServeHTTP(before, httptest.NewRequest(http.MethodGet, "/health", nil))
	if before.Code != http.StatusOK {
		t.Fatalf("health before Start status = %d, want 200", before.Code)
	}
	assertJSONContentType(t, before)
	var beforeResponse contract.HealthResponse
	if err := json.NewDecoder(before.Body).Decode(&beforeResponse); err != nil {
		t.Fatalf("decode health before Start: %v", err)
	}
	if beforeResponse.OK || beforeResponse.Polling || beforeResponse.LastUpdateID != nil {
		t.Errorf("health before Start = %+v, want not ready and no last update", beforeResponse)
	}
	if beforeResponse.UptimeSeconds < 0 {
		t.Errorf("health before Start uptime = %d, want non-negative", beforeResponse.UptimeSeconds)
	}
	if beforeResponse.ContractVersion != contract.ContractVersion || beforeResponse.Version != "test-version" || beforeResponse.CommitSHA != "test-sha" {
		t.Errorf("health metadata = %+v, want contract/version/commit metadata", beforeResponse)
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer api.Close()
	poller = telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
	ctx, cancel := context.WithCancel(context.Background())
	go poller.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for !poller.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !poller.Health().Polling {
		t.Fatal("poller did not become polling-ready")
	}

	after := httptest.NewRecorder()
	health = handleHealth(poller)
	health.ServeHTTP(after, httptest.NewRequest(http.MethodGet, "/health", nil))
	if after.Code != http.StatusOK {
		t.Fatalf("health while polling status = %d, want 200", after.Code)
	}
	var afterResponse contract.HealthResponse
	if err := json.NewDecoder(after.Body).Decode(&afterResponse); err != nil {
		t.Fatalf("decode health while polling: %v", err)
	}
	if !afterResponse.OK || !afterResponse.Polling {
		t.Errorf("health while polling = %+v, want ready", afterResponse)
	}

	cancel()
	deadline = time.Now().Add(time.Second)
	for poller.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if poller.Health().Polling {
		t.Fatal("poller did not become not-ready after cancellation")
	}
}

func TestProxyHTTP_HealthReportsLatestUpdateAndUptime(t *testing.T) {
	telegramAPI := mockTelegram(t, [][]telegram.Update{{tgTextUpdate(7_321, 1)}})
	defer telegramAPI.Close()

	poller := telegram.NewPoller("test-token", telegramAPI.URL, "test-version", "test-sha", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for poller.Health().LastUpdateID == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := poller.Health().LastUpdateID; got == nil || *got != 7_321 {
		if got == nil {
			t.Fatal("poller did not record the received update ID")
		}
		t.Fatalf("poller last update ID = %d, want 7321", *got)
	}

	rec := httptest.NewRecorder()
	handleHealth(poller).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200", rec.Code)
	}
	assertJSONContentType(t, rec)

	var body map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode health JSON: %v; body=%q", err, rec.Body.String())
	}
	var lastID int64
	if err := json.Unmarshal(body["last_update_id"], &lastID); err != nil {
		t.Fatalf("decode last_update_id: %v; body=%q", err, rec.Body.String())
	}
	if lastID != 7_321 {
		t.Errorf("last_update_id = %d, want 7321", lastID)
	}
	var uptime int64
	if err := json.Unmarshal(body["uptime_seconds"], &uptime); err != nil {
		t.Fatalf("decode uptime_seconds: %v; body=%q", err, rec.Body.String())
	}
	if uptime < 0 {
		t.Errorf("uptime_seconds = %d, want non-negative", uptime)
	}
}
