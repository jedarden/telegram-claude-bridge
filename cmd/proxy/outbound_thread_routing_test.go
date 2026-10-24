package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

// TestProxyHTTP_OutboundLegacyGeneralThreadIDForwardedUnchanged pins the
// thread-routing half of the data contract: the proxy does not validate or
// rewrite outbound thread_id values, so a legacy client that addresses the
// General topic with Telegram's raw ID (1) must see it reach Telegram
// unchanged on /send, /send_chat_action, and all four media endpoints.
// Normalizing 1 to an omitted thread_id is an inbound producer obligation
// (covered by the v1 update-envelope suite); applying it outbound too would
// silently change which topic Telegram targets.
func TestProxyHTTP_OutboundLegacyGeneralThreadIDForwardedUnchanged(t *testing.T) {
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
				return proxyJSONRequestForConformance(t, http.MethodPost, "/send", `{"chat_id":-1001234567890,"thread_id":1,"text":"General 🌍"}`)
			},
		},
		{
			name:           "chat action",
			path:           "/send_chat_action",
			telegramMethod: "sendChatAction",
			request: func(t *testing.T) *http.Request {
				return proxyJSONRequestForConformance(t, http.MethodPost, "/send_chat_action", `{"chat_id":-1001234567890,"thread_id":1,"action":"typing"}`)
			},
		},
		{
			name: "photo",
			path: "/send_photo", telegramMethod: "sendPhoto", fileField: "photo",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_photo", map[string]string{"chat_id": "-1001234567890", "thread_id": "1"}, "photo", "general.jpg", []byte("photo"), true)
			},
		},
		{
			name: "document",
			path: "/send_document", telegramMethod: "sendDocument", fileField: "document",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_document", map[string]string{"chat_id": "-1001234567890", "thread_id": "1"}, "document", "general.txt", []byte("document"), true)
			},
		},
		{
			name: "audio",
			path: "/send_audio", telegramMethod: "sendAudio", fileField: "audio",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_audio", map[string]string{"chat_id": "-1001234567890", "thread_id": "1"}, "audio", "general.ogg", []byte("audio"), true)
			},
		},
		{
			name: "video",
			path: "/send_video", telegramMethod: "sendVideo", fileField: "video",
			request: func(t *testing.T) *http.Request {
				return multipartRequest(t, "/send_video", map[string]string{"chat_id": "-1001234567890", "thread_id": "1"}, "video", "general.mp4", []byte("video"), true)
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
					if got, present := body["message_thread_id"]; !present {
						t.Errorf("legacy General request lost message_thread_id: %#v", body)
					} else if got != float64(1) {
						t.Errorf("Telegram message_thread_id = %#v, want 1 forwarded unchanged", got)
					}
				} else {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Fatalf("parse Telegram multipart request: %v", err)
					}
					if got := r.MultipartForm.Value["message_thread_id"]; len(got) != 1 || got[0] != "1" {
						t.Errorf("Telegram message_thread_id = %#v, want [1] forwarded unchanged", got)
					}
					if _, _, err := r.FormFile(tc.fileField); err != nil {
						t.Errorf("Telegram file part %q: %v", tc.fileField, err)
					}
				}
				if tc.fileField == "" && tc.telegramMethod == "sendChatAction" {
					writeTelegramJSON(w, true)
				} else {
					writeTelegramJSON(w, map[string]any{"message_id": 9002})
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
