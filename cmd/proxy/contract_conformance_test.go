package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

func TestProxyHTTP_GeneralTopicOmitsThreadID(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		telegramMethod string
		fileField      string
		request        func(t *testing.T) *http.Request
	}{
		{
			name:           "send",
			path:           "/send",
			telegramMethod: "sendMessage",
			request: func(t *testing.T) *http.Request {
				return proxyJSONRequestForConformance(t, http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"General 🌍"}`)
			},
		},
		{
			name:           "chat action",
			path:           "/send_chat_action",
			telegramMethod: "sendChatAction",
			request: func(t *testing.T) *http.Request {
				return proxyJSONRequestForConformance(t, http.MethodPost, "/send_chat_action", `{"chat_id":-1001234567890,"action":"typing"}`)
			},
		},
		{
			name: "photo",
			path: "/send_photo", telegramMethod: "sendPhoto", fileField: "photo",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_photo", map[string]string{"chat_id": "-1001234567890"}, "photo", "general.jpg", []byte("photo"), true)
			},
		},
		{
			name: "document",
			path: "/send_document", telegramMethod: "sendDocument", fileField: "document",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_document", map[string]string{"chat_id": "-1001234567890"}, "document", "general.txt", []byte("document"), true)
			},
		},
		{
			name: "audio",
			path: "/send_audio", telegramMethod: "sendAudio", fileField: "audio",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_audio", map[string]string{"chat_id": "-1001234567890"}, "audio", "general.ogg", []byte("audio"), true)
			},
		},
		{
			name: "video",
			path: "/send_video", telegramMethod: "sendVideo", fileField: "video",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_video", map[string]string{"chat_id": "-1001234567890"}, "video", "general.mp4", []byte("video"), true)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if got, want := r.URL.Path, "/bottest-token/"+tc.telegramMethod; got != want {
					t.Errorf("Telegram path = %q, want %q", got, want)
				}
				if tc.fileField == "" {
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatalf("decode Telegram request: %v", err)
					}
					if _, present := body["message_thread_id"]; present {
						t.Errorf("General-topic request included message_thread_id: %#v", body)
					}
				} else {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatalf("parse Telegram multipart request: %v", err)
					}
					if _, present := r.MultipartForm.Value["message_thread_id"]; present {
						t.Errorf("General-topic media request included message_thread_id: %#v", r.MultipartForm.Value)
					}
					if _, _, err := r.FormFile(tc.fileField); err != nil {
						t.Errorf("Telegram file part %q: %v", tc.fileField, err)
					}
				}
				if tc.fileField == "" && tc.telegramMethod == "sendChatAction" {
					writeTelegramJSON(w, true)
				} else {
					writeTelegramJSON(w, map[string]any{"message_id": 9001})
				}
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, tc.request(t))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)
		})
	}
}

func proxyJSONRequestForConformance(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// repeatedByteReader lets limit tests stream more than the configured limit
// without allocating a second 50 MiB buffer in the test process.
type repeatedByteReader struct {
	remaining int64
}

func (r *repeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= n
	return int(n), nil
}

func oversizedMultipartRequest(t *testing.T, path, fileField string, fileSize int64) *http.Request {
	t.Helper()
	const boundary = "proxy-conformance-boundary"
	prefix := fmt.Sprintf(
		"--%s\r\nContent-Disposition: form-data; name=\"chat_id\"\r\n\r\n-1001234567890\r\n--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"oversized.bin\"\r\nContent-Type: application/octet-stream\r\n\r\n",
		boundary, boundary, fileField,
	)
	suffix := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	body := io.MultiReader(
		strings.NewReader(prefix),
		&repeatedByteReader{remaining: fileSize},
		strings.NewReader(suffix),
	)
	req := httptest.NewRequest(http.MethodPost, path, body)
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	return req
}

func TestProxyHTTP_MediaMultipartValidationAcrossEndpoints(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		fileField string
	}{
		{name: "photo", path: "/send_photo", fileField: "photo"},
		{name: "document", path: "/send_document", fileField: "document"},
		{name: "audio", path: "/send_audio", fileField: "audio"},
		{name: "video", path: "/send_video", fileField: "video"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				t.Errorf("Telegram API called for malformed multipart request: %s", r.URL.Path)
				writeTelegramFailure(w, http.StatusBadGateway, "unexpected request", nil)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("not multipart"))
			// Deliberately omit Content-Type to verify that a boundary is required.
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			got := decodeProxyError(t, rec)
			if got.ErrorCode != contract.ErrCodeBadRequest || !strings.Contains(got.Description, "invalid multipart form") {
				t.Errorf("error = %+v, want bad multipart error", got)
			}
		})
	}
}

func TestProxyHTTP_MediaSizeLimitsAcrossEndpoints(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		fileField string
		limit     int64
	}{
		{name: "photo", path: "/send_photo", fileField: "photo", limit: maxPhotoSize},
		{name: "document", path: "/send_document", fileField: "document", limit: maxMediaSize},
		{name: "audio", path: "/send_audio", fileField: "audio", limit: maxMediaSize},
		{name: "video", path: "/send_video", fileField: "video", limit: maxMediaSize},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var telegramCalls atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				telegramCalls.Add(1)
				writeTelegramFailure(w, http.StatusBadGateway, "unexpected request", nil)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			// The documented request cap includes a 4096-byte allowance for
			// multipart headers and metadata, so exceed that allowance too.
			req := oversizedMultipartRequest(t, tc.path, tc.fileField, tc.limit+8192)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)
			if rec.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d, want 413; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			got := decodeProxyError(t, rec)
			if got.ErrorCode != http.StatusRequestEntityTooLarge {
				t.Errorf("error_code = %d, want 413", got.ErrorCode)
			}
			wantDescription := fmt.Sprintf("file exceeds %dMB limit", tc.limit/(1024*1024))
			if got.Description != wantDescription {
				t.Errorf("description = %q, want %q", got.Description, wantDescription)
			}
			if got := telegramCalls.Load(); got != 0 {
				t.Errorf("Telegram calls = %d, want 0 for rejected upload", got)
			}
		})
	}
}

func TestProxyHTTP_MediaTelegramErrorMappings(t *testing.T) {
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
		{name: "unauthorized", errorCode: 401, wantStatus: http.StatusBadGateway},
		{name: "forbidden", errorCode: 403, wantStatus: http.StatusBadGateway},
		{name: "conflict", errorCode: 409, wantStatus: http.StatusBadGateway},
		{name: "rate limit", errorCode: 429, wantStatus: http.StatusTooManyRequests, retryAfter: intPointer(7)},
	}

	for _, endpoint := range endpoints {
		for _, failure := range failures {
			t.Run(endpoint.name+"/"+failure.name, func(t *testing.T) {
				api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					writeTelegramFailure(w, failure.errorCode, "Telegram media failure", failure.retryAfter)
				}))
				defer api.Close()

				poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
				sender := telegram.NewSender("test-token", api.URL)
				req := multipartRequest(t, endpoint.path, map[string]string{"chat_id": "-1001234567890"}, endpoint.fileField, "file.bin", []byte("media"), true)
				rec := httptest.NewRecorder()
				proxyContractMux(poller, sender).ServeHTTP(rec, req)
				if rec.Code != failure.wantStatus {
					t.Fatalf("status = %d, want %d; body=%q", rec.Code, failure.wantStatus, rec.Body.String())
				}
				assertJSONContentType(t, rec)
				got := decodeProxyError(t, rec)
				if got.ErrorCode != failure.errorCode || got.Description != "Telegram media failure" {
					t.Errorf("error = %+v, want code=%d and preserved description", got, failure.errorCode)
				}
				if got.RetryAfter == nil && failure.retryAfter != nil {
					t.Fatalf("retry_after = nil, want %d", *failure.retryAfter)
				}
				if (got.RetryAfter == nil) != (failure.retryAfter == nil) || (got.RetryAfter != nil && *got.RetryAfter != *failure.retryAfter) {
					t.Errorf("retry_after = %v, want %v", got.RetryAfter, failure.retryAfter)
				}
			})
		}
	}
}
