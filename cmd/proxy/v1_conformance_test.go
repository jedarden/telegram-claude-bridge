package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/bridge"
	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/telegram"
)

func assertV1JSONHasNoNull(t *testing.T, body []byte) {
	t.Helper()
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode JSON for null check: %v; body=%q", err, body)
	}

	var walk func(any, string)
	walk = func(value any, path string) {
		switch value := value.(type) {
		case nil:
			t.Errorf("optional field at %s was encoded as null", path)
		case map[string]any:
			for key, child := range value {
				walk(child, path+"."+key)
			}
		case []any:
			for index, child := range value {
				walk(child, path+"["+string(rune('0'+index))+"]")
			}
		}
	}
	walk(value, "$")
}

func assertV1JSONKeyAbsent(t *testing.T, body []byte, key string) {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("decode JSON object: %v; body=%q", err, body)
	}
	if _, ok := value[key]; ok {
		t.Errorf("JSON object contains optional key %q: %s", key, body)
	}
}

func int64PointerV1(value int64) *int64 { return &value }

func stringPointerV1(value string) *string { return &value }

func TestProxyBridgeV1_HealthContract(t *testing.T) {
	poller := telegram.NewPoller("test-token", "http://127.0.0.1:1", "v1-test", "abc123", "")
	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", "http://127.0.0.1:1")).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /health status = %d, want 200", rec.Code)
	}
	assertJSONContentType(t, rec)
	assertV1JSONHasNoNull(t, rec.Body.Bytes())

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if got["ok"] != false || got["polling"] != false {
		t.Errorf("health readiness = %#v, want ok=false and polling=false", got)
	}
	if got["contract_version"] != contract.ContractVersion || got["version"] != "v1-test" || got["commit"] != "abc123" {
		t.Errorf("health metadata = %#v, want contract/version/commit metadata", got)
	}
	if _, ok := got["last_update_id"]; ok {
		t.Errorf("health response included last_update_id before any update: %#v", got)
	}
}

func TestProxyBridgeV1_UpdatesNormalizeGeneralAndOmitOptionals(t *testing.T) {
	generalText := "General 🌍 — no optional fields"
	general := tgTextUpdate(7_100, 101)
	general.Message.Date = 1_700_000_001
	general.Message.MessageThreadID = int64PointerV1(1)
	general.Message.Text = &generalText

	namedText := "topic message"
	namedUsername := "topic-user"
	named := tgTextUpdate(7_101, 102)
	named.Message.Date = 1_700_000_042
	named.Message.MessageThreadID = int64PointerV1(42)
	named.Message.From.Username = &namedUsername
	named.Message.Text = &namedText
	named.Message.Entities = []telegram.MessageEntity{{Type: "bold", Offset: 0, Length: len(namedText)}}
	named.Message.ReplyToMessage = &telegram.Message{MessageID: 99}

	telegramAPI := mockTelegram(t, [][]telegram.Update{{general, named}})
	defer telegramAPI.Close()
	poller := telegram.NewPoller("test-token", telegramAPI.URL, "v1-test", "abc123", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for !poller.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !poller.Health().Polling {
		t.Fatal("proxy poller did not start")
	}

	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", telegramAPI.URL)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates?timeout=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	assertV1JSONHasNoNull(t, rec.Body.Bytes())

	var response struct {
		OK      bool              `json:"ok"`
		Updates []json.RawMessage `json:"updates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode updates response: %v", err)
	}
	if !response.OK || len(response.Updates) != 2 {
		t.Fatalf("updates response = %+v, want two successful updates", response)
	}

	var generalEnvelope contract.Update
	if err := json.Unmarshal(response.Updates[0], &generalEnvelope); err != nil {
		t.Fatalf("decode General envelope: %v", err)
	}
	if generalEnvelope.ChatID != -100123456789 || generalEnvelope.ThreadID != nil || !generalEnvelope.IsGeneralTopic() {
		t.Errorf("General envelope = %+v, want signed chat ID and omitted thread_id", generalEnvelope)
	}
	if generalEnvelope.Timestamp != 1_700_000_001 {
		t.Errorf("General timestamp = %d, want 1700000001", generalEnvelope.Timestamp)
	}
	if generalEnvelope.Content == nil || generalEnvelope.Content.Text == nil || *generalEnvelope.Content.Text != generalText {
		t.Errorf("General content = %+v, want UTF-8 text", generalEnvelope.Content)
	}
	assertV1JSONKeyAbsent(t, response.Updates[0], "thread_id")
	assertV1JSONKeyAbsent(t, response.Updates[0], "reply_to_message_id")
	var generalFrom map[string]json.RawMessage
	if err := json.Unmarshal(response.Updates[0], &map[string]json.RawMessage{}); err != nil {
		t.Fatalf("decode General envelope object: %v", err)
	}
	var generalObject map[string]json.RawMessage
	_ = json.Unmarshal(response.Updates[0], &generalObject)
	if err := json.Unmarshal(generalObject["from_user"], &generalFrom); err != nil {
		t.Fatalf("decode General sender: %v", err)
	}
	if _, ok := generalFrom["username"]; ok {
		t.Errorf("General sender included absent username: %s", generalObject["from_user"])
	}

	var namedEnvelope contract.Update
	if err := json.Unmarshal(response.Updates[1], &namedEnvelope); err != nil {
		t.Fatalf("decode named-topic envelope: %v", err)
	}
	if namedEnvelope.ChatID != -100123456789 || namedEnvelope.ThreadID == nil || *namedEnvelope.ThreadID != 42 {
		t.Errorf("named-topic envelope = %+v, want signed chat ID and thread_id=42", namedEnvelope)
	}
	if namedEnvelope.Timestamp != 1_700_000_042 {
		t.Errorf("named-topic timestamp = %d, want 1700000042", namedEnvelope.Timestamp)
	}
	if namedEnvelope.ReplyToMessageID == nil || *namedEnvelope.ReplyToMessageID != 99 {
		t.Errorf("named-topic reply_to_message_id = %v, want 99", namedEnvelope.ReplyToMessageID)
	}
	if namedEnvelope.FromUser.Username == nil || *namedEnvelope.FromUser.Username != namedUsername {
		t.Errorf("named-topic username = %v, want %q", namedEnvelope.FromUser.Username, namedUsername)
	}
}

func TestProxyBridgeV1_UpdatesNormalizeCallbackAndServiceEnvelopes(t *testing.T) {
	callbackData := "approve 🌍"
	callbackThreadID := int64(42)
	callbackMessage := tgTextUpdate(7_200, 201).Message
	callbackMessage.Date = 1_700_001_201
	callbackMessage.MessageThreadID = &callbackThreadID
	callback := telegram.Update{
		UpdateID: 7_200,
		CallbackQuery: &telegram.CallbackQuery{
			ID:      "callback-🌍",
			From:    telegram.User{ID: 9, FirstName: "Callback user"},
			Message: callbackMessage,
			Data:    &callbackData,
		},
	}

	service := tgTextUpdate(7_201, 202)
	service.Message.Date = 1_700_001_202
	service.Message.MessageThreadID = &callbackThreadID
	service.Message.Text = nil
	service.Message.ForumTopicClosed = &telegram.ForumTopicClosed{}

	telegramAPI := mockTelegram(t, [][]telegram.Update{{callback, service}})
	defer telegramAPI.Close()
	poller := telegram.NewPoller("test-token", telegramAPI.URL, "v1-test", "abc123", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for !poller.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !poller.Health().Polling {
		t.Fatal("proxy poller did not start")
	}

	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", telegramAPI.URL)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates?timeout=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	assertV1JSONHasNoNull(t, rec.Body.Bytes())

	var response struct {
		OK      bool              `json:"ok"`
		Updates []json.RawMessage `json:"updates"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode updates response: %v", err)
	}
	if !response.OK || len(response.Updates) != 2 {
		t.Fatalf("updates response = %+v, want callback and service updates", response)
	}

	var callbackEnvelope contract.Update
	if err := json.Unmarshal(response.Updates[0], &callbackEnvelope); err != nil {
		t.Fatalf("decode callback envelope: %v", err)
	}
	if callbackEnvelope.Type != "callback_query" || callbackEnvelope.ChatID != -100123456789 || callbackEnvelope.ThreadID == nil || *callbackEnvelope.ThreadID != callbackThreadID {
		t.Errorf("callback envelope = %+v, want named-topic callback", callbackEnvelope)
	}
	if callbackEnvelope.Timestamp != 1_700_001_201 || callbackEnvelope.Content == nil || callbackEnvelope.Content.CallbackQueryID == nil || *callbackEnvelope.Content.CallbackQueryID != "callback-🌍" || callbackEnvelope.Content.Data == nil || *callbackEnvelope.Content.Data != callbackData {
		t.Errorf("callback envelope content = %+v, want UTF-8 callback and timestamp", callbackEnvelope)
	}
	assertV1JSONKeyAbsent(t, response.Updates[0], "service")
	assertV1JSONKeyAbsent(t, response.Updates[0], "message_thread_id")

	var serviceEnvelope contract.Update
	if err := json.Unmarshal(response.Updates[1], &serviceEnvelope); err != nil {
		t.Fatalf("decode service envelope: %v", err)
	}
	if serviceEnvelope.Type != "service" || serviceEnvelope.ChatID != -100123456789 || serviceEnvelope.ThreadID == nil || *serviceEnvelope.ThreadID != callbackThreadID || serviceEnvelope.Timestamp != 1_700_001_202 {
		t.Errorf("service envelope = %+v, want signed named-topic service with timestamp", serviceEnvelope)
	}
	if serviceEnvelope.Content != nil || serviceEnvelope.Service == nil || serviceEnvelope.Service.Type != contract.ServiceTypeForumTopicClosed {
		t.Errorf("service envelope payload = %+v, want forum_topic_closed without content", serviceEnvelope)
	}
	assertV1JSONKeyAbsent(t, response.Updates[1], "content")
}

func TestProxyBridgeV1_UpdatesNormalizeForumTopicServices(t *testing.T) {
	const chatID int64 = -100123456789
	threadID := int64(43)
	createdName := "created topic"
	editedName := "renamed topic"
	iconEmojiID := "emoji-🌍"

	created := tgTextUpdate(7_300, 301)
	created.Message.Date = 1_700_002_301
	created.Message.MessageThreadID = &threadID
	created.Message.Text = nil
	created.Message.ForumTopicCreated = &telegram.ForumTopicCreated{
		Name:              createdName,
		IconColor:         contract.IconColorGreen,
		IconCustomEmojiID: &iconEmojiID,
	}

	edited := tgTextUpdate(7_301, 302)
	edited.Message.Date = 1_700_002_302
	edited.Message.MessageThreadID = &threadID
	edited.Message.Text = nil
	edited.Message.ForumTopicEdited = &telegram.ForumTopicEdited{
		Name:              &editedName,
		IconCustomEmojiID: &iconEmojiID,
	}

	reopened := tgTextUpdate(7_302, 303)
	reopened.Message.Date = 1_700_002_303
	reopened.Message.MessageThreadID = &threadID
	reopened.Message.Text = nil
	reopened.Message.ForumTopicReopened = &telegram.ForumTopicReopened{}

	telegramAPI := mockTelegram(t, [][]telegram.Update{{created, edited, reopened}})
	defer telegramAPI.Close()
	poller := telegram.NewPoller("test-token", telegramAPI.URL, "v1-test", "abc123", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go poller.Start(ctx)

	deadline := time.Now().Add(time.Second)
	for !poller.Health().Polling && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !poller.Health().Polling {
		t.Fatal("proxy poller did not start")
	}

	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", telegramAPI.URL)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/updates?timeout=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /updates status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	assertJSONContentType(t, rec)
	assertV1JSONHasNoNull(t, rec.Body.Bytes())

	var response contract.UpdatesResponse
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode updates response: %v", err)
	}
	if !response.OK || len(response.Updates) != 3 {
		t.Fatalf("updates response = %+v, want three forum-topic services", response)
	}

	cases := []struct {
		name        string
		update      contract.Update
		serviceType string
		wantName    *string
		wantColor   *int
		wantEmoji   *string
	}{
		{
			name:        "created",
			update:      response.Updates[0],
			serviceType: contract.ServiceTypeForumTopicCreated,
			wantName:    &createdName,
			wantColor:   intPointer(contract.IconColorGreen),
			wantEmoji:   &iconEmojiID,
		},
		{
			name:        "edited",
			update:      response.Updates[1],
			serviceType: contract.ServiceTypeForumTopicEdited,
			wantName:    &editedName,
			wantEmoji:   &iconEmojiID,
		},
		{
			name:        "reopened",
			update:      response.Updates[2],
			serviceType: contract.ServiceTypeForumTopicReopened,
		},
	}

	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.update
			if got.UpdateID != int64(7_300+index) || got.Type != "service" || got.ChatID != chatID || got.ThreadID == nil || *got.ThreadID != threadID {
				t.Errorf("envelope = %+v, want service for signed chat %d/thread %d", got, chatID, threadID)
			}
			if got.MessageID != int64(301+index) || got.Timestamp != int64(1_700_002_301+index) {
				t.Errorf("message metadata = message_id %d timestamp %d, want %d/%d", got.MessageID, got.Timestamp, 301+index, 1_700_002_301+index)
			}
			if got.Content != nil {
				t.Errorf("service content = %+v, want omitted", got.Content)
			}
			if got.Service == nil {
				t.Fatal("service payload is nil")
			}
			if got.Service.Type != tc.serviceType {
				t.Errorf("service payload = %+v/content = %+v, want %q without content", got.Service, got.Content, tc.serviceType)
			}
			if got.Service.Name != nil && tc.wantName == nil || got.Service.Name == nil && tc.wantName != nil || tc.wantName != nil && *got.Service.Name != *tc.wantName {
				t.Errorf("service name = %v, want %v", got.Service.Name, tc.wantName)
			}
			if got.Service.IconColor != nil && tc.wantColor == nil || got.Service.IconColor == nil && tc.wantColor != nil || tc.wantColor != nil && *got.Service.IconColor != *tc.wantColor {
				t.Errorf("service icon_color = %v, want %v", got.Service.IconColor, tc.wantColor)
			}
			if got.Service.IconCustomEmojiID != nil && tc.wantEmoji == nil || got.Service.IconCustomEmojiID == nil && tc.wantEmoji != nil || tc.wantEmoji != nil && *got.Service.IconCustomEmojiID != *tc.wantEmoji {
				t.Errorf("service icon_custom_emoji_id = %v, want %v", got.Service.IconCustomEmojiID, tc.wantEmoji)
			}
			body, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("encode %s service envelope: %v", tc.name, err)
			}
			assertV1JSONKeyAbsent(t, body, "content")
		})
	}
}

func TestProxyBridgeV1_MinimalJSONRequestsOmitOptionalFields(t *testing.T) {
	cases := []struct {
		name           string
		path           string
		telegramMethod string
		requestBody    string
		wantTelegram   map[string]any
		result         any
	}{
		{
			name:           "send",
			path:           "/send",
			telegramMethod: "sendMessage",
			requestBody:    `{"chat_id":-1001234567890,"text":"héllo 🌍"}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "text": "héllo 🌍"},
			result:         map[string]any{"message_id": float64(901)},
		},
		{
			name:           "edit",
			path:           "/edit",
			telegramMethod: "editMessageText",
			requestBody:    `{"chat_id":-1001234567890,"message_id":901,"text":"updated"}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "message_id": float64(901), "text": "updated"},
			result:         map[string]any{"message_id": float64(901)},
		},
		{
			name:           "chat action",
			path:           "/send_chat_action",
			telegramMethod: "sendChatAction",
			requestBody:    `{"chat_id":-1001234567890,"action":"typing"}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "action": "typing"},
			result:         true,
		},
		{
			name:           "create topic",
			path:           "/create_topic",
			telegramMethod: "createForumTopic",
			requestBody:    `{"chat_id":-1001234567890,"name":"topic"}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "name": "topic"},
			result:         map[string]any{"message_thread_id": float64(42), "name": "topic"},
		},
		{
			name:           "edit topic",
			path:           "/edit_topic",
			telegramMethod: "editForumTopic",
			requestBody:    `{"chat_id":-1001234567890,"thread_id":42}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "message_thread_id": float64(42)},
			result:         true,
		},
		{
			name:           "close topic",
			path:           "/close_topic",
			telegramMethod: "closeForumTopic",
			requestBody:    `{"chat_id":-1001234567890,"thread_id":42}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "message_thread_id": float64(42)},
			result:         true,
		},
		{
			name:           "reopen topic",
			path:           "/reopen_topic",
			telegramMethod: "reopenForumTopic",
			requestBody:    `{"chat_id":-1001234567890,"thread_id":42}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "message_thread_id": float64(42)},
			result:         true,
		},
		{
			name:           "pin message",
			path:           "/pin_message",
			telegramMethod: "pinChatMessage",
			requestBody:    `{"chat_id":-1001234567890,"message_id":901}`,
			wantTelegram:   map[string]any{"chat_id": float64(-1001234567890), "message_id": float64(901)},
			result:         true,
		},
		{
			name:           "answer callback",
			path:           "/answer_callback",
			telegramMethod: "answerCallbackQuery",
			requestBody:    `{"callback_query_id":"callback-1"}`,
			wantTelegram:   map[string]any{"callback_query_id": "callback-1"},
			result:         true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/bottest-token/"+tc.telegramMethod {
					t.Errorf("Telegram path = %q, want /bottest-token/%s", r.URL.Path, tc.telegramMethod)
				}
				var got map[string]any
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode Telegram request: %v", err)
				}
				if !reflect.DeepEqual(got, tc.wantTelegram) {
					t.Errorf("Telegram request = %#v, want %#v", got, tc.wantTelegram)
				}
				writeTelegramJSON(w, tc.result)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, tc.path, tc.requestBody)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			assertV1JSONHasNoNull(t, rec.Body.Bytes())
		})
	}
}

func TestProxyBridgeV1_MinimalMediaRequestsOmitOptionalFields(t *testing.T) {
	cases := []struct {
		name      string
		path      string
		method    string
		fileField string
		filename  string
	}{
		{name: "photo", path: "/send_photo", method: "sendPhoto", fileField: "photo", filename: "photo.png"},
		{name: "document", path: "/send_document", method: "sendDocument", fileField: "document", filename: "notes.txt"},
		{name: "audio", path: "/send_audio", method: "sendAudio", fileField: "audio", filename: "voice.ogg"},
		{name: "video", path: "/send_video", method: "sendVideo", fileField: "video", filename: "clip.mp4"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/bottest-token/"+tc.method {
					t.Errorf("Telegram path = %q, want /bottest-token/%s", r.URL.Path, tc.method)
				}
				if err := r.ParseMultipartForm(1 << 20); err != nil {
					t.Fatalf("parse Telegram multipart form: %v", err)
				}
				if !reflect.DeepEqual(r.MultipartForm.Value, map[string][]string{"chat_id": {"-1001234567890"}}) {
					t.Errorf("Telegram optional fields were not omitted: %#v", r.MultipartForm.Value)
				}
				file, header, err := r.FormFile(tc.fileField)
				if err != nil {
					t.Fatalf("Telegram file = %v", err)
				}
				defer file.Close()
				if header.Filename != tc.filename {
					t.Errorf("Telegram filename = %q, want %q", header.Filename, tc.filename)
				}
				data, err := io.ReadAll(file)
				if err != nil {
					t.Fatalf("read Telegram file: %v", err)
				}
				if string(data) != "media-🌍" {
					t.Errorf("Telegram media = %q, want media-🌍", data)
				}
				writeTelegramJSON(w, map[string]any{"message_id": 902})
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
			sender := telegram.NewSender("test-token", api.URL)
			req := multipartRequest(t, tc.path, map[string]string{"chat_id": "-1001234567890"}, tc.fileField, tc.filename, []byte("media-🌍"), true)
			rec := httptest.NewRecorder()
			proxyContractMux(poller, sender).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
			}
			assertV1JSONHasNoNull(t, rec.Body.Bytes())
		})
	}
}

func TestProxyBridgeV1_FileDownloadContract(t *testing.T) {
	const fileBody = "downloaded-🌍"
	var cdnCalls int

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/bottest-token/getFile":
			if r.Method != http.MethodPost {
				t.Errorf("getFile method = %s, want POST", r.Method)
			}
			writeTelegramJSON(w, map[string]any{
				"file_id":   "file-🌍",
				"file_size": len([]byte(fileBody)),
				"file_path": "documents/report-🌍.txt",
			})
		case "/file/bottest-token/documents/report-🌍.txt":
			cdnCalls++
			if r.Method != http.MethodGet {
				t.Errorf("CDN method = %s, want GET", r.Method)
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Header().Set("Content-Length", "15")
			_, _ = io.WriteString(w, fileBody)
		default:
			t.Errorf("unexpected Telegram path %q", r.URL.Path)
			writeTelegramFailure(w, http.StatusNotFound, "unexpected path", nil)
		}
	}))
	defer api.Close()

	poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
	rec := httptest.NewRecorder()
	proxyContractMux(poller, telegram.NewSender("test-token", api.URL)).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/file/file-🌍", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /file status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !bytes.Equal(rec.Body.Bytes(), []byte(fileBody)) {
		t.Errorf("download body = %q, want %q", rec.Body.Bytes(), fileBody)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("download Content-Type = %q, want UTF-8 text type", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "15" {
		t.Errorf("download Content-Length = %q, want 15", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="report-🌍.txt"` {
		t.Errorf("download Content-Disposition = %q, want UTF-8 filename", got)
	}
	if cdnCalls != 1 {
		t.Errorf("CDN calls = %d, want 1", cdnCalls)
	}
}

func TestProxyBridgeV1_ErrorEnvelopesAndFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		name       string
		errorCode  int
		wantStatus int
	}{
		{name: "bad gateway", errorCode: contract.ErrCodeTelegramUnreachable, wantStatus: http.StatusBadGateway},
		{name: "not polling", errorCode: contract.ErrCodeNotPolling, wantStatus: http.StatusServiceUnavailable},
		{name: "gateway timeout", errorCode: contract.ErrCodeTelegramTimeout, wantStatus: http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeTelegramFailure(w, tc.errorCode, "documented failure", nil)
			}))
			defer api.Close()

			poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
			sender := telegram.NewSender("test-token", api.URL)
			rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"hello"}`)
			if rec.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%q", rec.Code, tc.wantStatus, rec.Body.String())
			}
			assertJSONContentType(t, rec)
			assertV1JSONHasNoNull(t, rec.Body.Bytes())
			got := decodeProxyError(t, rec)
			if got.ErrorCode != tc.errorCode || got.Description != "documented failure" {
				t.Errorf("error = %+v, want code=%d description=%q", got, tc.errorCode, "documented failure")
			}
		})
	}
}

func TestProxyBridgeV1_InternalAndTelegramErrorMapping(t *testing.T) {
	t.Run("Telegram errors preserve contract code and retry metadata", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			writeTelegramFailure(w, contract.ErrCodeRateLimit, "Too Many Requests", intPointer(3))
		}))
		defer api.Close()

		poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
		sender := telegram.NewSender("test-token", api.URL)
		rec := proxyJSONRequest(t, proxyContractMux(poller, sender), http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"hello"}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("Telegram rate-limit status = %d, want 429; body=%q", rec.Code, rec.Body.String())
		}
		assertJSONContentType(t, rec)
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeRateLimit || got.Description != "Too Many Requests" || got.RetryAfter == nil || *got.RetryAfter != 3 {
			t.Errorf("Telegram error = %+v, want 429 with retry_after=3", got)
		}
	})

	t.Run("Telegram unreachable maps to 502", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		apiURL := api.URL
		api.Close()

		poller := telegram.NewPoller("test-token", apiURL, "v1-test", "abc123", "")
		rec := proxyJSONRequest(t, proxyContractMux(poller, telegram.NewSender("test-token", apiURL)), http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"hello"}`)
		if rec.Code != http.StatusBadGateway {
			t.Fatalf("unreachable status = %d, want 502; body=%q", rec.Code, rec.Body.String())
		}
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeTelegramUnreachable {
			t.Errorf("unreachable error_code = %d, want %d", got.ErrorCode, contract.ErrCodeTelegramUnreachable)
		}
	})

	t.Run("Telegram deadline maps to 504", func(t *testing.T) {
		api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(100 * time.Millisecond)
		}))
		defer api.Close()

		poller := telegram.NewPoller("test-token", api.URL, "v1-test", "abc123", "")
		sender := telegram.NewSender("test-token", api.URL)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		defer cancel()
		rec := proxyJSONRequestWithContext(t, proxyContractMux(poller, sender), ctx, http.MethodPost, "/send", `{"chat_id":-1001234567890,"text":"hello"}`)
		if rec.Code != http.StatusGatewayTimeout {
			t.Fatalf("deadline status = %d, want 504; body=%q", rec.Code, rec.Body.String())
		}
		got := decodeProxyError(t, rec)
		if got.ErrorCode != contract.ErrCodeTelegramTimeout {
			t.Errorf("deadline error_code = %d, want %d", got.ErrorCode, contract.ErrCodeTelegramTimeout)
		}
	})
}

func TestProxyBridgeV1_BridgeSenderRequestShapes(t *testing.T) {
	const chatID int64 = -1009876543210
	var mu sync.Mutex
	var jsonRequests []struct {
		method string
		body   map[string]any
	}
	var mediaRequests []struct {
		method string
		fields map[string][]string
	}

	telegramAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := strings.TrimPrefix(r.URL.Path, "/bottest-token/")
		switch method {
		case "sendMessage", "editMessageText", "sendChatAction", "createForumTopic", "editForumTopic", "closeForumTopic":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode %s request: %v", method, err)
			}
			if got, ok := body["chat_id"].(float64); !ok || int64(got) != chatID {
				t.Errorf("%s chat_id = %#v, want signed %d", method, body["chat_id"], chatID)
			}
			if method != "editForumTopic" && method != "closeForumTopic" && method != "sendChatAction" {
				if _, ok := body["message_thread_id"]; ok {
					t.Errorf("%s unexpectedly included message_thread_id for nil bridge thread: %#v", method, body)
				}
			}
			mu.Lock()
			jsonRequests = append(jsonRequests, struct {
				method string
				body   map[string]any
			}{method: method, body: body})
			mu.Unlock()
			if method == "sendMessage" || method == "editMessageText" {
				writeTelegramJSON(w, map[string]any{"message_id": 903})
			} else if method == "createForumTopic" {
				writeTelegramJSON(w, map[string]any{"message_thread_id": 43, "name": "topic"})
			} else {
				writeTelegramJSON(w, true)
			}
		case "sendPhoto", "sendDocument", "sendAudio", "sendVideo":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("parse %s request: %v", method, err)
				return
			}
			if got := r.FormValue("chat_id"); got != "-1009876543210" {
				t.Errorf("%s chat_id = %q, want signed %d", method, got, chatID)
			}
			if len(r.MultipartForm.Value) != 1 {
				t.Errorf("%s optional fields = %#v, want only chat_id", method, r.MultipartForm.Value)
			}
			mu.Lock()
			mediaRequests = append(mediaRequests, struct {
				method string
				fields map[string][]string
			}{method: method, fields: r.MultipartForm.Value})
			mu.Unlock()
			writeTelegramJSON(w, map[string]any{"message_id": 904})
		default:
			t.Errorf("unexpected Telegram method %q", method)
			writeTelegramFailure(w, 400, "unexpected method", nil)
		}
	}))
	defer telegramAPI.Close()

	poller := telegram.NewPoller("test-token", telegramAPI.URL, "v1-test", "abc123", "")
	proxy := httptest.NewServer(proxyContractMux(poller, telegram.NewSender("test-token", telegramAPI.URL)))
	defer proxy.Close()

	bridgeSender, err := bridge.NewSender(proxy.URL, filepath.Join(t.TempDir(), "sender.db"))
	if err != nil {
		t.Fatalf("create bridge sender: %v", err)
	}
	defer bridgeSender.Close()

	ctx := context.Background()
	if err := bridgeSender.SendResponse(ctx, chatID, nil, 77, "bridge → proxy 🌍"); err != nil {
		t.Fatalf("bridge SendResponse: %v", err)
	}
	if err := bridgeSender.EditMessage(ctx, chatID, 903, "edited"); err != nil {
		t.Fatalf("bridge EditMessage: %v", err)
	}
	bridgeSender.SendTyping(ctx, chatID, nil)
	if err := bridgeSender.SendPhoto(ctx, chatID, nil, 0, "", "photo.png", []byte("photo")); err != nil {
		t.Fatalf("bridge SendPhoto: %v", err)
	}
	if err := bridgeSender.SendDocument(ctx, chatID, nil, 0, "", "document.txt", []byte("document")); err != nil {
		t.Fatalf("bridge SendDocument: %v", err)
	}
	if err := bridgeSender.SendAudio(ctx, chatID, nil, 0, "", "audio.ogg", []byte("audio")); err != nil {
		t.Fatalf("bridge SendAudio: %v", err)
	}
	if err := bridgeSender.SendVideo(ctx, chatID, nil, 0, "", "video.mp4", []byte("video")); err != nil {
		t.Fatalf("bridge SendVideo: %v", err)
	}
	if _, err := bridgeSender.CreateTopic(ctx, chatID, "topic", contract.IconColorLightBlue); err != nil {
		t.Fatalf("bridge CreateTopic: %v", err)
	}
	if err := bridgeSender.EditTopicIconColor(ctx, chatID, 43, contract.IconColorGreen); err != nil {
		t.Fatalf("bridge EditTopicIconColor: %v", err)
	}
	if err := bridgeSender.CloseTopic(ctx, chatID, 43); err != nil {
		t.Fatalf("bridge CloseTopic: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(jsonRequests) != 6 {
		t.Errorf("JSON requests = %d, want send/edit/chat-action/create/edit-topic/close-topic", len(jsonRequests))
	}
	if len(mediaRequests) != 4 {
		t.Errorf("media requests = %d, want photo/document/audio/video", len(mediaRequests))
	}
}
