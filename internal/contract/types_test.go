package contract

import (
	"encoding/json"
	"testing"
)

// roundTrip marshals v to JSON, unmarshals into a new value of the same type,
// and returns whether the re-marshalled JSON is byte-identical to the original.
func roundTrip[T any](t *testing.T, v T) T {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	var got T
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	data2, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshal failed: %v", err)
	}
	if string(data) != string(data2) {
		t.Errorf("round-trip mismatch:\n  before: %s\n  after:  %s", data, data2)
	}
	return got
}

func ptr[T any](v T) *T { return &v }

func TestUpdate_RoundTrip(t *testing.T) {
	threadID := int64(42)
	replyTo := int64(7)
	text := "hello world"

	u := Update{
		UpdateID:         1001,
		Type:             "message",
		ChatID:           -100123456789,
		ThreadID:         &threadID,
		FromUser:         FromUser{ID: 9999, FirstName: "Alice", Username: ptr("alice")},
		MessageID:        5,
		Timestamp:        1700000000,
		ReplyToMessageID: &replyTo,
		Content: &Content{
			Type:     ContentTypeText,
			Text:     &text,
			Entities: []Entity{{Type: "bold", Offset: 0, Length: 5}},
		},
	}
	roundTrip(t, u)
}

func TestUpdate_NoOptionals_RoundTrip(t *testing.T) {
	u := Update{
		UpdateID:  2,
		Type:      "message",
		ChatID:    123,
		FromUser:  FromUser{ID: 1, FirstName: "Bob"},
		MessageID: 1,
		Timestamp: 1700000001,
	}
	got := roundTrip(t, u)
	if got.ThreadID != nil {
		t.Errorf("expected nil ThreadID, got %v", got.ThreadID)
	}
	if got.Content != nil {
		t.Errorf("expected nil Content, got %v", got.Content)
	}
	if got.Service != nil {
		t.Errorf("expected nil Service, got %v", got.Service)
	}
}

func TestUpdate_WithService_RoundTrip(t *testing.T) {
	u := Update{
		UpdateID:  3,
		Type:      "service",
		ChatID:    -100111,
		FromUser:  FromUser{ID: 2, FirstName: "System"},
		MessageID: 10,
		Timestamp: 1700000002,
		Service: &Service{
			Type:      ServiceTypeForumTopicCreated,
			Name:      ptr("General"),
			IconColor: ptr(IconColorLightBlue),
		},
	}
	roundTrip(t, u)
}

func TestUpdate_WithCallbackQuery_RoundTrip(t *testing.T) {
	u := Update{
		UpdateID:  4,
		Type:      "callback_query",
		ChatID:    123,
		FromUser:  FromUser{ID: 3, FirstName: "Carol"},
		MessageID: 20,
		Timestamp: 1700000003,
		Content: &Content{
			Type:            ContentTypeCallback,
			CallbackQueryID: ptr("abc123"),
			Data:            ptr("action:confirm"),
		},
	}
	roundTrip(t, u)
}

func TestContent_AllTypes_RoundTrip(t *testing.T) {
	cases := []Content{
		{
			Type: ContentTypeText,
			Text: ptr("simple text"),
		},
		{
			Type:     ContentTypePhoto,
			FileID:   ptr("file-id-photo"),
			FileSize: ptr(int64(204800)),
			Width:    ptr(1280),
			Height:   ptr(720),
			Caption:  ptr("a photo"),
		},
		{
			Type:     ContentTypeVoice,
			FileID:   ptr("file-id-voice"),
			FileSize: ptr(int64(8192)),
			MimeType: ptr("audio/ogg"),
			Duration: ptr(15),
		},
		{
			Type:      ContentTypeAudio,
			FileID:    ptr("file-id-audio"),
			Duration:  ptr(180),
			Title:     ptr("Song Title"),
			Performer: ptr("Artist"),
		},
		{
			Type:     ContentTypeVideo,
			FileID:   ptr("file-id-video"),
			Duration: ptr(30),
			Width:    ptr(1920),
			Height:   ptr(1080),
		},
		{
			Type:   ContentTypeVideoNote,
			FileID: ptr("file-id-vnote"),
			Length: ptr(240),
		},
		{
			Type:     ContentTypeDocument,
			FileID:   ptr("file-id-doc"),
			FileName: ptr("report.pdf"),
			MimeType: ptr("application/pdf"),
			FileSize: ptr(int64(1048576)),
		},
	}

	for _, c := range cases {
		t.Run(c.Type, func(t *testing.T) {
			roundTrip(t, c)
		})
	}
}

func TestSendRequest_RoundTrip(t *testing.T) {
	r := SendRequest{
		ChatID:           -100123456,
		ThreadID:         ptr(int64(5)),
		Text:             "Hello from the bridge",
		ParseMode:        ptr("Markdown"),
		ReplyToMessageID: ptr(int64(3)),
		ReplyMarkup: &InlineKeyboard{
			InlineKeyboard: [][]InlineButton{
				{{Text: "Yes", CallbackData: "yes"}, {Text: "No", CallbackData: "no"}},
			},
		},
	}
	roundTrip(t, r)
}

func TestSendRequest_Minimal_RoundTrip(t *testing.T) {
	r := SendRequest{ChatID: 1, Text: "hi"}
	got := roundTrip(t, r)
	if got.ThreadID != nil {
		t.Errorf("expected nil ThreadID")
	}
	if got.ReplyMarkup != nil {
		t.Errorf("expected nil ReplyMarkup")
	}
}

func TestEditRequest_RoundTrip(t *testing.T) {
	r := EditRequest{
		ChatID:    100,
		MessageID: 42,
		Text:      "edited text",
		ParseMode: ptr("HTML"),
	}
	roundTrip(t, r)
}

func TestErrorResponse_RoundTrip(t *testing.T) {
	t.Run("with retry_after", func(t *testing.T) {
		e := ErrorResponse{
			OK:          false,
			ErrorCode:   ErrCodeRateLimit,
			Description: "Too Many Requests",
			RetryAfter:  ptr(30),
		}
		got := roundTrip(t, e)
		if got.RetryAfter == nil || *got.RetryAfter != 30 {
			t.Errorf("unexpected RetryAfter: %v", got.RetryAfter)
		}
	})

	t.Run("without retry_after", func(t *testing.T) {
		e := ErrorResponse{
			OK:          false,
			ErrorCode:   ErrCodeTelegramUnreachable,
			Description: "Telegram unreachable",
		}
		got := roundTrip(t, e)
		if got.RetryAfter != nil {
			t.Errorf("expected nil RetryAfter, got %v", got.RetryAfter)
		}
	})
}

func TestErrorResponse_Error(t *testing.T) {
	e := &ErrorResponse{ErrorCode: 503, Description: "not polling"}
	want := "proxy error 503: not polling"
	if e.Error() != want {
		t.Errorf("Error() = %q, want %q", e.Error(), want)
	}
}

func TestUpdatesResponse_RoundTrip(t *testing.T) {
	text := "hi"
	r := UpdatesResponse{
		OK: true,
		Updates: []Update{
			{
				UpdateID:  1,
				Type:      "message",
				ChatID:    1,
				FromUser:  FromUser{ID: 1, FirstName: "A"},
				MessageID: 1,
				Timestamp: 1700000000,
				Content:   &Content{Type: ContentTypeText, Text: &text},
			},
		},
	}
	roundTrip(t, r)
}

func TestHealthResponse_RoundTrip(t *testing.T) {
	lastID := int64(999)
	h := HealthResponse{
		OK:              true,
		Polling:         true,
		LastUpdateID:    &lastID,
		UptimeSeconds:   3600,
		ContractVersion: ContractVersion,
	}
	got := roundTrip(t, h)
	if got.ContractVersion != ContractVersion {
		t.Errorf("ContractVersion = %q, want %q", got.ContractVersion, ContractVersion)
	}
}

func TestCreateTopicRequest_RoundTrip(t *testing.T) {
	r := CreateTopicRequest{
		ChatID:    -100123,
		Name:      "Support",
		IconColor: ptr(IconColorGreen),
	}
	roundTrip(t, r)
}

func TestTopicRequest_RoundTrip(t *testing.T) {
	r := TopicRequest{ChatID: -100123, ThreadID: 7}
	roundTrip(t, r)
}

func TestAnswerCallbackRequest_RoundTrip(t *testing.T) {
	r := AnswerCallbackRequest{
		CallbackQueryID: "query-id-xyz",
		Text:            ptr("Done!"),
		ShowAlert:       ptr(true),
	}
	roundTrip(t, r)
}

func TestIsGeneralTopic(t *testing.T) {
	u := &Update{ThreadID: nil}
	if !u.IsGeneralTopic() {
		t.Error("expected IsGeneralTopic() true when ThreadID is nil")
	}
	u.ThreadID = ptr(int64(1))
	if u.IsGeneralTopic() {
		t.Error("expected IsGeneralTopic() false when ThreadID is set")
	}
}

func TestIsCommand(t *testing.T) {
	cases := []struct {
		name    string
		content Content
		want    bool
	}{
		{
			name:    "command",
			content: Content{Type: ContentTypeText, Text: ptr("/start"), Entities: []Entity{{Type: "bot_command", Offset: 0, Length: 6}}},
			want:    true,
		},
		{
			name:    "not command type",
			content: Content{Type: ContentTypeText, Text: ptr("hello"), Entities: []Entity{{Type: "bold", Offset: 0, Length: 5}}},
			want:    false,
		},
		{
			name:    "non-zero offset",
			content: Content{Type: ContentTypeText, Text: ptr("hey /start"), Entities: []Entity{{Type: "bot_command", Offset: 4, Length: 6}}},
			want:    false,
		},
		{
			name:    "no entities",
			content: Content{Type: ContentTypeText, Text: ptr("/start")},
			want:    false,
		},
		{
			name:    "non-text type",
			content: Content{Type: ContentTypePhoto},
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.content.IsCommand(); got != tc.want {
				t.Errorf("IsCommand() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestExtractCommandAndArgs(t *testing.T) {
	cases := []struct {
		name        string
		content     Content
		wantCmd     string
		wantArgs    string
	}{
		{
			name: "command only",
			content: Content{
				Type:     ContentTypeText,
				Text:     ptr("/start"),
				Entities: []Entity{{Type: "bot_command", Offset: 0, Length: 6}},
			},
			wantCmd: "/start", wantArgs: "",
		},
		{
			name: "command with args",
			content: Content{
				Type:     ContentTypeText,
				Text:     ptr("/set timeout 30"),
				Entities: []Entity{{Type: "bot_command", Offset: 0, Length: 4}},
			},
			wantCmd: "/set", wantArgs: "timeout 30",
		},
		{
			name: "command with @BotName suffix",
			content: Content{
				Type:     ContentTypeText,
				Text:     ptr("/help@mybot"),
				Entities: []Entity{{Type: "bot_command", Offset: 0, Length: 11}},
			},
			wantCmd: "/help", wantArgs: "",
		},
		{
			name:        "not a command",
			content:     Content{Type: ContentTypeText, Text: ptr("hello")},
			wantCmd:     "",
			wantArgs:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args := tc.content.ExtractCommandAndArgs()
			if cmd != tc.wantCmd {
				t.Errorf("command = %q, want %q", cmd, tc.wantCmd)
			}
			if args != tc.wantArgs {
				t.Errorf("args = %q, want %q", args, tc.wantArgs)
			}
		})
	}
}
