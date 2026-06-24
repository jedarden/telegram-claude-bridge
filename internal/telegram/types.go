// Package telegram provides raw Telegram Bot API types and polling/normalization logic.
package telegram

// GetUpdatesResponse is the Telegram API response for getUpdates.
type GetUpdatesResponse struct {
	OK          bool     `json:"ok"`
	Result      []Update `json:"result"`
	ErrorCode   *int     `json:"error_code,omitempty"`
	Description *string  `json:"description,omitempty"`
}

// Update is a raw Telegram Update object.
type Update struct {
	UpdateID      int64              `json:"update_id"`
	Message       *Message           `json:"message,omitempty"`
	EditedMessage *Message           `json:"edited_message,omitempty"`
	CallbackQuery *CallbackQuery     `json:"callback_query,omitempty"`
	MyChatMember  *ChatMemberUpdated `json:"my_chat_member,omitempty"`
}

// Message is a raw Telegram Message object.
type Message struct {
	MessageID          int64               `json:"message_id"`
	From               *User               `json:"from,omitempty"`
	Chat               Chat                `json:"chat"`
	Date               int64               `json:"date"`
	MessageThreadID    *int64              `json:"message_thread_id,omitempty"`
	Text               *string             `json:"text,omitempty"`
	Entities           []MessageEntity     `json:"entities,omitempty"`
	Photo              []PhotoSize         `json:"photo,omitempty"`
	Voice              *Voice              `json:"voice,omitempty"`
	Audio              *Audio              `json:"audio,omitempty"`
	Video              *Video              `json:"video,omitempty"`
	VideoNote          *VideoNote          `json:"video_note,omitempty"`
	Document           *Document           `json:"document,omitempty"`
	Caption            *string             `json:"caption,omitempty"`
	CaptionEntities    []MessageEntity     `json:"caption_entities,omitempty"`
	ReplyToMessage     *Message            `json:"reply_to_message,omitempty"`
	ForumTopicCreated  *ForumTopicCreated  `json:"forum_topic_created,omitempty"`
	ForumTopicEdited   *ForumTopicEdited   `json:"forum_topic_edited,omitempty"`
	ForumTopicClosed   *ForumTopicClosed   `json:"forum_topic_closed,omitempty"`
	ForumTopicReopened *ForumTopicReopened `json:"forum_topic_reopened,omitempty"`
	NewChatMembers     []User              `json:"new_chat_members,omitempty"`
	LeftChatMember     *User               `json:"left_chat_member,omitempty"`
}

// User is a raw Telegram User object.
type User struct {
	ID        int64   `json:"id"`
	FirstName string  `json:"first_name"`
	Username  *string `json:"username,omitempty"`
}

// Chat is a raw Telegram Chat object.
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}

// MessageEntity is a raw Telegram MessageEntity (mention, URL, bold, etc.).
type MessageEntity struct {
	Type   string  `json:"type"`
	Offset int     `json:"offset"`
	Length int     `json:"length"`
	URL    *string `json:"url,omitempty"`
}

// PhotoSize is one resolution variant of a Telegram photo.
type PhotoSize struct {
	FileID   string `json:"file_id"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	FileSize *int64 `json:"file_size,omitempty"`
}

// Voice is a raw Telegram Voice object.
type Voice struct {
	FileID   string  `json:"file_id"`
	Duration int     `json:"duration"`
	MimeType *string `json:"mime_type,omitempty"`
	FileSize *int64  `json:"file_size,omitempty"`
}

// Audio is a raw Telegram Audio object.
type Audio struct {
	FileID    string  `json:"file_id"`
	Duration  int     `json:"duration"`
	MimeType  *string `json:"mime_type,omitempty"`
	Title     *string `json:"title,omitempty"`
	Performer *string `json:"performer,omitempty"`
	FileSize  *int64  `json:"file_size,omitempty"`
}

// Video is a raw Telegram Video object.
type Video struct {
	FileID   string  `json:"file_id"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
	Duration int     `json:"duration"`
	MimeType *string `json:"mime_type,omitempty"`
	FileSize *int64  `json:"file_size,omitempty"`
}

// VideoNote is a raw Telegram VideoNote object.
type VideoNote struct {
	FileID   string `json:"file_id"`
	Length   int    `json:"length"` // diameter in pixels
	Duration int    `json:"duration"`
	FileSize *int64 `json:"file_size,omitempty"`
}

// Document is a raw Telegram Document object.
type Document struct {
	FileID   string  `json:"file_id"`
	FileName *string `json:"file_name,omitempty"`
	MimeType *string `json:"mime_type,omitempty"`
	FileSize *int64  `json:"file_size,omitempty"`
}

// CallbackQuery is a raw Telegram CallbackQuery object.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    *string  `json:"data,omitempty"`
}

// ForumTopicCreated is sent when a forum topic is created.
type ForumTopicCreated struct {
	Name              string  `json:"name"`
	IconColor         int     `json:"icon_color"`
	IconCustomEmojiID *string `json:"icon_custom_emoji_id,omitempty"`
}

// ForumTopicEdited is sent when a forum topic name or icon is changed.
type ForumTopicEdited struct {
	Name              *string `json:"name,omitempty"`
	IconCustomEmojiID *string `json:"icon_custom_emoji_id,omitempty"`
}

// ForumTopicClosed is sent when a forum topic is closed (empty struct).
type ForumTopicClosed struct{}

// ForumTopicReopened is sent when a forum topic is reopened (empty struct).
type ForumTopicReopened struct{}

// ChatMemberUpdated represents a change in chat member status.
type ChatMemberUpdated struct {
	Chat        Chat             `json:"chat"`
	From        User             `json:"from"`
	Date        int64            `json:"date"`
	OldChatMember ChatMember     `json:"old_chat_member"`
	NewChatMember ChatMember     `json:"new_chat_member"`
	InviteLink  *ChatInviteLink  `json:"invite_link,omitempty"`
}

// ChatMember represents the current status of a chat member.
type ChatMember struct {
	Status           string        `json:"status"`
	User             *User         `json:"user,omitempty"`
	UntilDate        *int64        `json:"until_date,omitempty"`
	CanBeEdited      *bool         `json:"can_be_edited,omitempty"`
	CanChangeInfo    *bool         `json:"can_change_info,omitempty"`
	CanPostMessages  *bool         `json:"can_post_messages,omitempty"`
	CanEditMessages  *bool         `json:"can_edit_messages,omitempty"`
	CanDeleteMessages *bool        `json:"can_delete_messages,omitempty"`
	CanInviteUsers   *bool         `json:"can_invite_users,omitempty"`
	CanRestrictMembers *bool       `json:"can_restrict_members,omitempty"`
	CanPinMessages   *bool         `json:"can_pin_messages,omitempty"`
	CanManageTopics  *bool         `json:"can_manage_topics,omitempty"`
	CanPromoteMembers *bool        `json:"can_promote_members,omitempty"`
	CanManageVideoChats *bool       `json:"can_manage_video_chats,omitempty"`
	CanManageChat    *bool         `json:"can_manage_chat,omitempty"`
	IsAnonymous      *bool         `json:"is_anonymous,omitempty"`
	CustomTitle      *string      `json:"custom_title,omitempty"`
}

// ChatInviteLink represents an invite link for a chat.
type ChatInviteLink struct {
	InviteLink string  `json:"invite_link"`
	Creator    User    `json:"creator"`
	CreatesJoinRequest *bool `json:"creates_join_request,omitempty"`
	IsPrimary  *bool   `json:"is_primary,omitempty"`
	IsRevoked  *bool   `json:"is_revoked,omitempty"`
	ExpireDate *int64  `json:"expire_date,omitempty"`
	MemberLimit *int32 `json:"member_limit,omitempty"`
	Name       *string `json:"name,omitempty"`
	PendingJoinRequestCount *int32 `json:"pending_join_request_count,omitempty"`
}
