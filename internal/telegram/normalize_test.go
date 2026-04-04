package telegram

import (
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// helpers

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }
func int64Ptr(i int64) *int64 { return &i }

func makeUser(id int64, first string) User {
	return User{ID: id, FirstName: first}
}

func makeChat(id int64) Chat {
	return Chat{ID: id, Type: "supergroup"}
}

// assertNoErr fatals if err is non-nil.
func assertNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// assertNotNil fatals if v is nil.
func assertNotNil(t *testing.T, label string, v any) {
	t.Helper()
	if v == nil {
		t.Fatalf("%s is nil", label)
	}
}

// ---- Text message ----

func TestNormalizeUpdate_TextMessage(t *testing.T) {
	username := "testuser"
	text := "Hello, world!"
	threadID := int64(42)

	raw := Update{
		UpdateID: 100,
		Message: &Message{
			MessageID:       1,
			From:            &User{ID: 7, FirstName: "Alice", Username: &username},
			Chat:            makeChat(-1001111111111),
			Date:            1700000000,
			MessageThreadID: &threadID,
			Text:            &text,
			Entities: []MessageEntity{
				{Type: "bold", Offset: 0, Length: 5},
				{Type: "url", Offset: 7, Length: 6, URL: strPtr("https://example.com")},
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	assertNotNil(t, "update", u)

	if u.UpdateID != 100 {
		t.Errorf("UpdateID = %d, want 100", u.UpdateID)
	}
	if u.Type != "message" {
		t.Errorf("Type = %q, want \"message\"", u.Type)
	}
	if u.ChatID != -1001111111111 {
		t.Errorf("ChatID = %d, want -1001111111111", u.ChatID)
	}
	if u.ThreadID == nil || *u.ThreadID != 42 {
		t.Errorf("ThreadID = %v, want 42", u.ThreadID)
	}
	if u.FromUser.ID != 7 || u.FromUser.FirstName != "Alice" {
		t.Errorf("FromUser = %+v", u.FromUser)
	}
	if u.FromUser.Username == nil || *u.FromUser.Username != "testuser" {
		t.Errorf("Username = %v, want testuser", u.FromUser.Username)
	}
	if u.MessageID != 1 {
		t.Errorf("MessageID = %d, want 1", u.MessageID)
	}
	if u.Timestamp != 1700000000 {
		t.Errorf("Timestamp = %d, want 1700000000", u.Timestamp)
	}
	if u.Service != nil {
		t.Error("Service should be nil for text message")
	}

	c := u.Content
	assertNotNil(t, "Content", c)
	if c.Type != contract.ContentTypeText {
		t.Errorf("Content.Type = %q, want %q", c.Type, contract.ContentTypeText)
	}
	if c.Text == nil || *c.Text != text {
		t.Errorf("Content.Text = %v, want %q", c.Text, text)
	}
	if len(c.Entities) != 2 {
		t.Fatalf("Entities len = %d, want 2", len(c.Entities))
	}
	if c.Entities[1].Extra == nil || *c.Entities[1].Extra != "https://example.com" {
		t.Errorf("Entity[1].Extra = %v, want URL", c.Entities[1].Extra)
	}
}

// ---- Edited message ----

func TestNormalizeUpdate_EditedMessage(t *testing.T) {
	text := "edited text"
	raw := Update{
		UpdateID: 200,
		EditedMessage: &Message{
			MessageID: 5,
			From:      &User{ID: 9, FirstName: "Bob"},
			Chat:      makeChat(-1002222222222),
			Date:      1700001000,
			Text:      &text,
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	assertNotNil(t, "update", u)
	if u.Type != "edited_message" {
		t.Errorf("Type = %q, want \"edited_message\"", u.Type)
	}
	if u.UpdateID != 200 {
		t.Errorf("UpdateID = %d, want 200", u.UpdateID)
	}
}

// ---- Photo message ----

func TestNormalizeUpdate_PhotoMessage(t *testing.T) {
	fileSize := int64(51200)
	raw := Update{
		UpdateID: 300,
		Message: &Message{
			MessageID: 10,
			From:      &User{ID: 11, FirstName: "Carol"},
			Chat:      makeChat(-1003333333333),
			Date:      1700002000,
			Photo: []PhotoSize{
				{FileID: "small", Width: 90, Height: 60, FileSize: int64Ptr(1024)},
				{FileID: "medium", Width: 320, Height: 213},
				{FileID: "large", Width: 800, Height: 533, FileSize: &fileSize},
				{FileID: "xlarge", Width: 1280, Height: 853},
				{FileID: "huge", Width: 2560, Height: 1706},
			},
			Caption: strPtr("A photo"),
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	assertNotNil(t, "update", u)

	c := u.Content
	assertNotNil(t, "Content", c)
	if c.Type != contract.ContentTypePhoto {
		t.Errorf("Content.Type = %q, want %q", c.Type, contract.ContentTypePhoto)
	}
	// Best ≤1280 long-edge is xlarge (1280x853)
	if c.FileID == nil || *c.FileID != "xlarge" {
		t.Errorf("FileID = %v, want \"xlarge\"", c.FileID)
	}
	if c.Width == nil || *c.Width != 1280 {
		t.Errorf("Width = %v, want 1280", c.Width)
	}
	if c.Height == nil || *c.Height != 853 {
		t.Errorf("Height = %v, want 853", c.Height)
	}
	if c.Caption == nil || *c.Caption != "A photo" {
		t.Errorf("Caption = %v, want \"A photo\"", c.Caption)
	}
}

// ---- Photo selection: all exceed 1280 ----

func TestSelectPhoto_AllExceedLimit(t *testing.T) {
	sizes := []PhotoSize{
		{FileID: "a", Width: 1500, Height: 1000},
		{FileID: "b", Width: 3000, Height: 2000},
		{FileID: "c", Width: 2000, Height: 1500},
	}
	got := selectPhoto(sizes)
	// Largest by area: b (3000×2000 = 6,000,000)
	if got.FileID != "b" {
		t.Errorf("got %q, want \"b\"", got.FileID)
	}
}

// ---- Photo selection: picks largest within limit ----

func TestSelectPhoto_PicksLargestWithinLimit(t *testing.T) {
	sizes := []PhotoSize{
		{FileID: "s", Width: 90, Height: 60},
		{FileID: "m", Width: 320, Height: 213},
		{FileID: "l", Width: 1280, Height: 853},
		{FileID: "xl", Width: 2560, Height: 1706},
	}
	got := selectPhoto(sizes)
	if got.FileID != "l" {
		t.Errorf("got %q, want \"l\"", got.FileID)
	}
}

// ---- Voice message ----

func TestNormalizeUpdate_VoiceMessage(t *testing.T) {
	raw := Update{
		UpdateID: 400,
		Message: &Message{
			MessageID: 20,
			From:      &User{ID: 13, FirstName: "Dave"},
			Chat:      makeChat(-1004444444444),
			Date:      1700003000,
			Voice: &Voice{
				FileID:   "voice_file_id",
				Duration: 15,
				MimeType: strPtr("audio/ogg"),
				FileSize: int64Ptr(30720),
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	c := u.Content
	assertNotNil(t, "Content", c)
	if c.Type != contract.ContentTypeVoice {
		t.Errorf("Type = %q", c.Type)
	}
	if c.FileID == nil || *c.FileID != "voice_file_id" {
		t.Errorf("FileID = %v", c.FileID)
	}
	if c.Duration == nil || *c.Duration != 15 {
		t.Errorf("Duration = %v, want 15", c.Duration)
	}
	if c.MimeType == nil || *c.MimeType != "audio/ogg" {
		t.Errorf("MimeType = %v", c.MimeType)
	}
}

// ---- Audio message ----

func TestNormalizeUpdate_AudioMessage(t *testing.T) {
	raw := Update{
		UpdateID: 500,
		Message: &Message{
			MessageID: 30,
			From:      &User{ID: 15, FirstName: "Eve"},
			Chat:      makeChat(-1005555555555),
			Date:      1700004000,
			Audio: &Audio{
				FileID:    "audio_file_id",
				Duration:  180,
				MimeType:  strPtr("audio/mpeg"),
				Title:     strPtr("My Song"),
				Performer: strPtr("The Band"),
				FileSize:  int64Ptr(3145728),
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	c := u.Content
	if c.Type != contract.ContentTypeAudio {
		t.Errorf("Type = %q", c.Type)
	}
	if c.Title == nil || *c.Title != "My Song" {
		t.Errorf("Title = %v", c.Title)
	}
	if c.Performer == nil || *c.Performer != "The Band" {
		t.Errorf("Performer = %v", c.Performer)
	}
	if c.Duration == nil || *c.Duration != 180 {
		t.Errorf("Duration = %v", c.Duration)
	}
}

// ---- Video message ----

func TestNormalizeUpdate_VideoMessage(t *testing.T) {
	raw := Update{
		UpdateID: 600,
		Message: &Message{
			MessageID: 40,
			From:      &User{ID: 17, FirstName: "Frank"},
			Chat:      makeChat(-1006666666666),
			Date:      1700005000,
			Video: &Video{
				FileID:   "video_file_id",
				Width:    1920,
				Height:   1080,
				Duration: 60,
				MimeType: strPtr("video/mp4"),
				FileSize: int64Ptr(10485760),
			},
			Caption: strPtr("Watch this"),
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	c := u.Content
	if c.Type != contract.ContentTypeVideo {
		t.Errorf("Type = %q", c.Type)
	}
	if c.Width == nil || *c.Width != 1920 {
		t.Errorf("Width = %v", c.Width)
	}
	if c.Height == nil || *c.Height != 1080 {
		t.Errorf("Height = %v", c.Height)
	}
	if c.Caption == nil || *c.Caption != "Watch this" {
		t.Errorf("Caption = %v", c.Caption)
	}
}

// ---- VideoNote message ----

func TestNormalizeUpdate_VideoNoteMessage(t *testing.T) {
	raw := Update{
		UpdateID: 700,
		Message: &Message{
			MessageID: 50,
			From:      &User{ID: 19, FirstName: "Grace"},
			Chat:      makeChat(-1007777777777),
			Date:      1700006000,
			VideoNote: &VideoNote{
				FileID:   "vidnote_file_id",
				Length:   320,
				Duration: 10,
				FileSize: int64Ptr(204800),
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	c := u.Content
	if c.Type != contract.ContentTypeVideoNote {
		t.Errorf("Type = %q", c.Type)
	}
	if c.Length == nil || *c.Length != 320 {
		t.Errorf("Length = %v, want 320", c.Length)
	}
	if c.Duration == nil || *c.Duration != 10 {
		t.Errorf("Duration = %v", c.Duration)
	}
}

// ---- Document message ----

func TestNormalizeUpdate_DocumentMessage(t *testing.T) {
	raw := Update{
		UpdateID: 800,
		Message: &Message{
			MessageID: 60,
			From:      &User{ID: 21, FirstName: "Hank"},
			Chat:      makeChat(-1008888888888),
			Date:      1700007000,
			Document: &Document{
				FileID:   "doc_file_id",
				FileName: strPtr("report.pdf"),
				MimeType: strPtr("application/pdf"),
				FileSize: int64Ptr(1048576),
			},
			Caption: strPtr("See attached"),
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	c := u.Content
	if c.Type != contract.ContentTypeDocument {
		t.Errorf("Type = %q", c.Type)
	}
	if c.FileName == nil || *c.FileName != "report.pdf" {
		t.Errorf("FileName = %v", c.FileName)
	}
	if c.MimeType == nil || *c.MimeType != "application/pdf" {
		t.Errorf("MimeType = %v", c.MimeType)
	}
	if c.Caption == nil || *c.Caption != "See attached" {
		t.Errorf("Caption = %v", c.Caption)
	}
}

// ---- Callback query ----

func TestNormalizeUpdate_CallbackQuery(t *testing.T) {
	threadID := int64(7)
	raw := Update{
		UpdateID: 900,
		CallbackQuery: &CallbackQuery{
			ID:   "cbq_id",
			From: User{ID: 23, FirstName: "Ivy"},
			Message: &Message{
				MessageID:       70,
				Chat:            makeChat(-1009999999999),
				Date:            1700008000,
				MessageThreadID: &threadID,
			},
			Data: strPtr("action:confirm"),
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	assertNotNil(t, "update", u)
	if u.Type != "callback_query" {
		t.Errorf("Type = %q, want \"callback_query\"", u.Type)
	}
	if u.ChatID != -1009999999999 {
		t.Errorf("ChatID = %d", u.ChatID)
	}
	if u.ThreadID == nil || *u.ThreadID != 7 {
		t.Errorf("ThreadID = %v", u.ThreadID)
	}
	c := u.Content
	if c.Type != contract.ContentTypeCallback {
		t.Errorf("Content.Type = %q", c.Type)
	}
	if c.CallbackQueryID == nil || *c.CallbackQueryID != "cbq_id" {
		t.Errorf("CallbackQueryID = %v", c.CallbackQueryID)
	}
	if c.Data == nil || *c.Data != "action:confirm" {
		t.Errorf("Data = %v", c.Data)
	}
}

// ---- Service: forum_topic_created ----

func TestNormalizeUpdate_ForumTopicCreated(t *testing.T) {
	raw := Update{
		UpdateID: 1000,
		Message: &Message{
			MessageID: 80,
			From:      &User{ID: 25, FirstName: "Jack"},
			Chat:      makeChat(-1000111222333),
			Date:      1700009000,
			ForumTopicCreated: &ForumTopicCreated{
				Name:      "Project Alpha",
				IconColor: 7322096,
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	if u.Type != "service" {
		t.Errorf("Type = %q, want \"service\"", u.Type)
	}
	svc := u.Service
	assertNotNil(t, "Service", svc)
	if svc.Type != contract.ServiceTypeForumTopicCreated {
		t.Errorf("Service.Type = %q", svc.Type)
	}
	if svc.Name == nil || *svc.Name != "Project Alpha" {
		t.Errorf("Service.Name = %v", svc.Name)
	}
	if svc.IconColor == nil || *svc.IconColor != 7322096 {
		t.Errorf("Service.IconColor = %v", svc.IconColor)
	}
	if u.Content != nil {
		t.Error("Content should be nil for service message")
	}
}

// ---- Service: forum_topic_edited ----

func TestNormalizeUpdate_ForumTopicEdited(t *testing.T) {
	raw := Update{
		UpdateID: 1001,
		Message: &Message{
			MessageID: 81,
			From:      &User{ID: 25, FirstName: "Jack"},
			Chat:      makeChat(-1000111222333),
			Date:      1700009100,
			ForumTopicEdited: &ForumTopicEdited{
				Name: strPtr("Project Beta"),
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	svc := u.Service
	if svc.Type != contract.ServiceTypeForumTopicEdited {
		t.Errorf("Service.Type = %q", svc.Type)
	}
	if svc.Name == nil || *svc.Name != "Project Beta" {
		t.Errorf("Service.Name = %v", svc.Name)
	}
}

// ---- Service: forum_topic_closed ----

func TestNormalizeUpdate_ForumTopicClosed(t *testing.T) {
	ftc := &ForumTopicClosed{}
	raw := Update{
		UpdateID: 1002,
		Message: &Message{
			MessageID:        82,
			From:             &User{ID: 25, FirstName: "Jack"},
			Chat:             makeChat(-1000111222333),
			Date:             1700009200,
			ForumTopicClosed: ftc,
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	if u.Service == nil || u.Service.Type != contract.ServiceTypeForumTopicClosed {
		t.Errorf("unexpected service: %+v", u.Service)
	}
}

// ---- Service: forum_topic_reopened ----

func TestNormalizeUpdate_ForumTopicReopened(t *testing.T) {
	ftr := &ForumTopicReopened{}
	raw := Update{
		UpdateID: 1003,
		Message: &Message{
			MessageID:          83,
			From:               &User{ID: 25, FirstName: "Jack"},
			Chat:               makeChat(-1000111222333),
			Date:               1700009300,
			ForumTopicReopened: ftr,
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	if u.Service == nil || u.Service.Type != contract.ServiceTypeForumTopicReopened {
		t.Errorf("unexpected service: %+v", u.Service)
	}
}

// ---- Reply tracking ----

func TestNormalizeUpdate_ReplyToMessage(t *testing.T) {
	text := "reply text"
	raw := Update{
		UpdateID: 1100,
		Message: &Message{
			MessageID: 90,
			From:      &User{ID: 27, FirstName: "Kim"},
			Chat:      makeChat(-1001234500000),
			Date:      1700010000,
			Text:      &text,
			ReplyToMessage: &Message{
				MessageID: 55,
				Chat:      makeChat(-1001234500000),
				Date:      1699999999,
			},
		},
	}

	u, err := NormalizeUpdate(raw)
	assertNoErr(t, err)
	if u.ReplyToMessageID == nil || *u.ReplyToMessageID != 55 {
		t.Errorf("ReplyToMessageID = %v, want 55", u.ReplyToMessageID)
	}
}

// ---- Unknown update type returns (nil, nil) ----

func TestNormalizeUpdate_Unknown(t *testing.T) {
	raw := Update{UpdateID: 9999}
	u, err := NormalizeUpdate(raw)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if u != nil {
		t.Errorf("expected nil update for unknown type, got %+v", u)
	}
}

// ---- Message without From returns error ----

func TestNormalizeUpdate_NoFrom_Error(t *testing.T) {
	text := "hello"
	raw := Update{
		UpdateID: 1200,
		Message: &Message{
			MessageID: 100,
			Chat:      makeChat(-1001111111111),
			Date:      1700011000,
			Text:      &text,
			// From is nil — should produce an error for a regular message
		},
	}

	_, err := NormalizeUpdate(raw)
	if err == nil {
		t.Error("expected error for message with no From field")
	}
}
