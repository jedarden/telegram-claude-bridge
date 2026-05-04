// Package contract defines the shared data types for the Proxy ↔ Bridge HTTP API.
// Version: 1.0
package contract

import "strings"

// FromUser is the sender of a Telegram message.
type FromUser struct {
	ID        int64   `json:"id"`
	FirstName string  `json:"first_name"`
	Username  *string `json:"username,omitempty"`
}

// Entity is a Telegram MessageEntity (mention, URL, bold, etc.).
type Entity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	Extra  *string `json:"extra,omitempty"`
}

// Update is the normalized envelope returned by GET /updates.
// Empty optional fields are omitted rather than sent as null.
type Update struct {
	UpdateID          int64       `json:"update_id"`
	Type              string      `json:"type"` // "message", "edited_message", "callback_query", "service"
	ChatID            int64       `json:"chat_id"`
	ThreadID          *int64      `json:"thread_id,omitempty"`
	FromUser          FromUser    `json:"from_user"`
	MessageID         int64       `json:"message_id"`
	Timestamp         int64       `json:"timestamp"`
	Content           *Content    `json:"content,omitempty"`
	ReplyToMessageID  *int64      `json:"reply_to_message_id,omitempty"`
	Service           *Service    `json:"service,omitempty"`
}

// ContentType discriminates the Content union.
type ContentType = string

const (
	ContentTypeText      ContentType = "text"
	ContentTypePhoto     ContentType = "photo"
	ContentTypeVoice     ContentType = "voice"
	ContentTypeAudio     ContentType = "audio"
	ContentTypeVideo     ContentType = "video"
	ContentTypeVideoNote ContentType = "video_note"
	ContentTypeDocument  ContentType = "document"
	ContentTypeCallback  ContentType = "callback"
)

// Content is a discriminated union over all message content types.
// Only the fields relevant to Type are populated.
type Content struct {
	Type string `json:"type"`

	// text
	Text     *string  `json:"text,omitempty"`
	Entities []Entity `json:"entities,omitempty"`

	// photo
	Width           *int    `json:"width,omitempty"`
	Height          *int    `json:"height,omitempty"`
	Caption         *string `json:"caption,omitempty"`
	CaptionEntities []Entity `json:"caption_entities,omitempty"`

	// voice / audio / video / video_note / document / photo
	FileID   *string `json:"file_id,omitempty"`
	FileSize *int64  `json:"file_size,omitempty"`
	MimeType *string `json:"mime_type,omitempty"`
	Duration *int    `json:"duration,omitempty"`

	// audio
	Title     *string `json:"title,omitempty"`
	Performer *string `json:"performer,omitempty"`

	// video
	// Width/Height shared with photo above

	// video_note
	Length *int `json:"length,omitempty"` // diameter in pixels

	// document
	FileName *string `json:"file_name,omitempty"`

	// callback
	CallbackQueryID *string `json:"callback_query_id,omitempty"`
	Data            *string `json:"data,omitempty"`
}

// Service represents service message data (topic lifecycle events, member changes).
type Service struct {
	Type string `json:"type"` // see ServiceType* constants

	// forum_topic_created / forum_topic_edited
	Name              *string `json:"name,omitempty"`
	IconColor         *int    `json:"icon_color,omitempty"`
	IconCustomEmojiID *string `json:"icon_custom_emoji_id,omitempty"`

	// new_chat_members
	Members []FromUser `json:"members,omitempty"`

	// left_chat_member
	Member *FromUser `json:"member,omitempty"`
}

// ServiceType constants for Service.Type.
const (
	ServiceTypeForumTopicCreated  = "forum_topic_created"
	ServiceTypeForumTopicEdited   = "forum_topic_edited"
	ServiceTypeForumTopicClosed   = "forum_topic_closed"
	ServiceTypeForumTopicReopened = "forum_topic_reopened"
	ServiceTypeNewChatMembers     = "new_chat_members"
	ServiceTypeLeftChatMember     = "left_chat_member"
)

// UpdatesResponse is the body of GET /updates.
type UpdatesResponse struct {
	OK      bool     `json:"ok"`
	Updates []Update `json:"updates"`
}

// HealthResponse is the body of GET /health.
type HealthResponse struct {
	OK              bool   `json:"ok"`
	Polling         bool   `json:"polling"`
	LastUpdateID    *int64 `json:"last_update_id,omitempty"`
	UptimeSeconds   int64  `json:"uptime_seconds"`
	ContractVersion string `json:"contract_version,omitempty"`
	Version         string `json:"version,omitempty"`
	CommitSHA       string `json:"commit,omitempty"`
}

// SendRequest is the body of POST /send.
type SendRequest struct {
	ChatID            int64          `json:"chat_id"`
	ThreadID          *int64         `json:"thread_id,omitempty"`
	Text              string         `json:"text"`
	ParseMode         *string        `json:"parse_mode,omitempty"`
	ReplyToMessageID  *int64         `json:"reply_to_message_id,omitempty"`
	ReplyMarkup       *InlineKeyboard `json:"reply_markup,omitempty"`
}

// SendResponse is the body returned by POST /send and POST /edit.
type SendResponse struct {
	OK        bool  `json:"ok"`
	MessageID int64 `json:"message_id"`
}

// EditRequest is the body of POST /edit.
type EditRequest struct {
	ChatID      int64           `json:"chat_id"`
	MessageID   int64           `json:"message_id"`
	Text        string          `json:"text"`
	ParseMode   *string         `json:"parse_mode,omitempty"`
	ReplyMarkup *InlineKeyboard `json:"reply_markup,omitempty"`
}

// ChatActionRequest is the body of POST /send_chat_action.
type ChatActionRequest struct {
	ChatID   int64  `json:"chat_id"`
	ThreadID *int64 `json:"thread_id,omitempty"`
	Action   string `json:"action"` // "typing", "upload_photo", etc.
}

// OKResponse is a minimal success response with no additional fields.
type OKResponse struct {
	OK bool `json:"ok"`
}

// CreateTopicRequest is the body of POST /create_topic.
type CreateTopicRequest struct {
	ChatID    int64  `json:"chat_id"`
	Name      string `json:"name"`
	IconColor *int   `json:"icon_color,omitempty"`
}

// CreateTopicResponse is the body returned by POST /create_topic.
type CreateTopicResponse struct {
	OK        bool   `json:"ok"`
	ThreadID  int64  `json:"thread_id"`
	Name      string `json:"name"`
	IconColor *int   `json:"icon_color,omitempty"`
}

// EditTopicRequest is the body of POST /edit_topic.
type EditTopicRequest struct {
	ChatID    int64   `json:"chat_id"`
	ThreadID  int64   `json:"thread_id"`
	Name      *string `json:"name,omitempty"`
	IconColor *int    `json:"icon_color,omitempty"`
}

// TopicRequest is used for POST /close_topic and POST /reopen_topic.
type TopicRequest struct {
	ChatID   int64 `json:"chat_id"`
	ThreadID int64 `json:"thread_id"`
}

// PinMessageRequest is the body of POST /pin_message.
type PinMessageRequest struct {
	ChatID              int64  `json:"chat_id"`
	MessageID           int64  `json:"message_id"`
	DisableNotification *bool  `json:"disable_notification,omitempty"`
}

// AnswerCallbackRequest is the body of POST /answer_callback.
type AnswerCallbackRequest struct {
	CallbackQueryID string  `json:"callback_query_id"`
	Text            *string `json:"text,omitempty"`
	ShowAlert       *bool   `json:"show_alert,omitempty"`
}

// GetMessageRequest is the body of GET /get_message.
type GetMessageRequest struct {
	ChatID    int64 `json:"chat_id"`
	MessageID int64 `json:"message_id"`
}

// MessageContent represents the content of a fetched message.
type MessageContent struct {
	Type     ContentType `json:"type"`
	Text     *string     `json:"text,omitempty"`
	Caption  *string     `json:"caption,omitempty"`
	FileName *string     `json:"file_name,omitempty"`
}

// GetMessageResponse is the response from GET /get_message.
type GetMessageResponse struct {
	OK      bool           `json:"ok"`
	Message *MessageContent `json:"message,omitempty"`
	Error   *ErrorResponse  `json:"error,omitempty"`
}

// InlineKeyboard is the reply_markup for inline keyboard buttons.
type InlineKeyboard struct {
	InlineKeyboard [][]InlineButton `json:"inline_keyboard"`
}

// InlineButton is a single button in an InlineKeyboard row.
type InlineButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// IconColor presets for forum topic icons.
const (
	IconColorLightBlue = 7322096  // 0x6FB9F0
	IconColorYellow    = 16766846 // 0xFFD67E
	IconColorPurple    = 13338587 // 0xCB86DB
	IconColorGreen     = 9371288  // 0x8EEE98
	IconColorPink      = 16749490 // 0xFF93B2
	IconColorRedOrange = 16478046 // 0xFB6F5F
)

// ContractVersion is the version of this data contract.
const ContractVersion = "1.0"

// IsGeneralTopic returns true when the update is in the General (main) topic.
// The General topic has no thread_id (nil means General).
func (u *Update) IsGeneralTopic() bool {
	return u.ThreadID == nil
}

// IsCommand returns true when the content is a text message beginning with a bot command.
func (c *Content) IsCommand() bool {
	if c.Type != ContentTypeText || c.Text == nil || len(c.Entities) == 0 {
		return false
	}
	e := c.Entities[0]
	return e.Type == "bot_command" && e.Offset == 0
}

// ExtractCommandAndArgs parses a bot command from the content text.
// It returns the command (e.g. "/start") and the remaining text as args.
// Returns ("", "") if the content is not a command.
func (c *Content) ExtractCommandAndArgs() (command, args string) {
	if !c.IsCommand() {
		return "", ""
	}
	e := c.Entities[0]
	runes := []rune(*c.Text)
	if e.Length > len(runes) {
		return "", ""
	}
	command = string(runes[:e.Length])
	// Strip @BotName suffix if present (e.g. "/start@mybot" → "/start")
	if idx := strings.IndexByte(command, '@'); idx != -1 {
		command = command[:idx]
	}
	args = strings.TrimSpace(string(runes[e.Length:]))
	return command, args
}

// SendPhotoRequest holds metadata for POST /send_photo (multipart/form-data).
// The actual file bytes are sent as a separate multipart part named "photo".
type SendPhotoRequest struct {
	ChatID           int64   `json:"chat_id"`
	ThreadID         *int64  `json:"thread_id,omitempty"`
	Caption          *string `json:"caption,omitempty"`
	ParseMode        *string `json:"parse_mode,omitempty"`
	ReplyToMessageID *int64  `json:"reply_to_message_id,omitempty"`
}

// SendDocumentRequest holds metadata for POST /send_document (multipart/form-data).
// The actual file bytes are sent as a separate multipart part named "document".
type SendDocumentRequest struct {
	ChatID           int64   `json:"chat_id"`
	ThreadID         *int64  `json:"thread_id,omitempty"`
	Caption          *string `json:"caption,omitempty"`
	ParseMode        *string `json:"parse_mode,omitempty"`
	ReplyToMessageID *int64  `json:"reply_to_message_id,omitempty"`
	FileName         *string `json:"file_name,omitempty"`
}

// SendAudioRequest holds metadata for POST /send_audio (multipart/form-data).
// The actual file bytes are sent as a separate multipart part named "audio".
type SendAudioRequest struct {
	ChatID           int64   `json:"chat_id"`
	ThreadID         *int64  `json:"thread_id,omitempty"`
	Caption          *string `json:"caption,omitempty"`
	ParseMode        *string `json:"parse_mode,omitempty"`
	Duration         *int    `json:"duration,omitempty"`
	Title            *string `json:"title,omitempty"`
	ReplyToMessageID *int64  `json:"reply_to_message_id,omitempty"`
}

// SendVideoRequest holds metadata for POST /send_video (multipart/form-data).
// The actual file bytes are sent as a separate multipart part named "video".
type SendVideoRequest struct {
	ChatID           int64   `json:"chat_id"`
	ThreadID         *int64  `json:"thread_id,omitempty"`
	Caption          *string `json:"caption,omitempty"`
	ParseMode        *string `json:"parse_mode,omitempty"`
	Duration         *int    `json:"duration,omitempty"`
	Width            *int    `json:"width,omitempty"`
	Height           *int    `json:"height,omitempty"`
	ReplyToMessageID *int64  `json:"reply_to_message_id,omitempty"`
}
