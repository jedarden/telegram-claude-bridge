package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

func TestProxyHTTP_TruncatedMultipartIsBadRequest(t *testing.T) {
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
				t.Errorf("Telegram API called for truncated multipart request: %s", r.URL.Path)
				writeTelegramFailure(w, http.StatusBadGateway, "unexpected request", nil)
			}))
			defer api.Close()

			const boundary = "proxy-truncated-boundary"
			body := fmt.Sprintf(
				"--%s\r\nContent-Disposition: form-data; name=\"chat_id\"\r\n\r\n-1001234567890\r\n"+
					"--%s\r\nContent-Disposition: form-data; name=\"%s\"; filename=\"partial.bin\"\r\n"+
					"Content-Type: application/octet-stream\r\n\r\npartial file data",
				boundary, boundary, tc.fileField,
			)
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			got := decodeProxyError(t, rec)
			if got.ErrorCode != contract.ErrCodeBadRequest {
				t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeBadRequest)
			}
			if !strings.HasPrefix(got.Description, "invalid multipart form:") {
				t.Errorf("description = %q, want invalid multipart form prefix", got.Description)
			}
		})
	}
}

func TestProxyHTTP_FileDownloadDefaultsMissingContentType(t *testing.T) {
	const fileBody = "raw file bytes"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottest-token/getFile":
			writeTelegramJSON(w, map[string]any{
				"file_id":   "file-id",
				"file_size": len(fileBody),
				"file_path": "documents/report.bin",
			})
		case "/file/bottest-token/documents/report.bin":
			// Hijacking lets the fixture send a successful raw response with no
			// Content-Type header, which is the CDN fallback case.
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support HTTP hijacking")
			}
			conn, rw, err := hijacker.Hijack()
			if err != nil {
				t.Fatalf("hijack CDN response: %v", err)
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(fileBody), fileBody)
			_ = rw.Flush()
		default:
			t.Errorf("unexpected Telegram path %q", r.URL.Path)
			writeTelegramFailure(w, http.StatusNotFound, "unexpected path", nil)
		}
	}))
	defer api.Close()

	poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", api.URL)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/file/file-id", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if got := rec.Body.String(); got != fileBody {
		t.Errorf("body = %q, want %q", got, fileBody)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream fallback", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="report.bin"` {
		t.Errorf("Content-Disposition = %q, want report.bin attachment", got)
	}
}

func TestProxyHTTP_DocumentedErrorStatusMappings(t *testing.T) {
	t.Run("malformed JSON maps to 400", func(t *testing.T) {
		poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "test-version", "test-sha", "")
		sender := telegram.NewSender("test-token", "http://127.0.0.1:1")
		rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, "/send", "{malformed")

		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
		}
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeBadRequest {
			t.Errorf("error_code = %d, want %d", got.ErrorCode, contract.ErrCodeBadRequest)
		}
	})

	cases := []struct {
		name       string
		errorCode  int
		wantStatus int
		retryAfter *int
	}{
		{name: "rate limit", errorCode: contract.ErrCodeRateLimit, wantStatus: http.StatusTooManyRequests, retryAfter: intPointer(11)},
		{name: "unreachable", errorCode: contract.ErrCodeTelegramUnreachable, wantStatus: http.StatusBadGateway},
		{name: "not polling", errorCode: contract.ErrCodeNotPolling, wantStatus: http.StatusServiceUnavailable},
		{name: "timeout", errorCode: contract.ErrCodeTelegramTimeout, wantStatus: http.StatusGatewayTimeout},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeTelegramFailure(w, tc.errorCode, "documented upstream failure", tc.retryAfter)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "test-version", "test-sha", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"hello"}`)

			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			got := decodeProxyError(t, rec)
			if got.ErrorCode != tc.errorCode {
				t.Errorf("error_code = %d, want preserved Telegram code %d", got.ErrorCode, tc.errorCode)
			}
			if got.Description != "documented upstream failure" {
				t.Errorf("description = %q, want preserved upstream description", got.Description)
			}
			if tc.retryAfter == nil {
				if got.RetryAfter != nil {
					t.Errorf("retry_after = %v, want omitted", got.RetryAfter)
				}
			} else if got.RetryAfter == nil || *got.RetryAfter != *tc.retryAfter {
				t.Errorf("retry_after = %v, want %d", got.RetryAfter, *tc.retryAfter)
			}
		})
	}
}
