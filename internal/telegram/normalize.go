package telegram

import (
	"fmt"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// NormalizeUpdate converts a raw Telegram Update to a contract.Update.
// Returns (nil, nil) for unknown update types — callers should skip these.
func NormalizeUpdate(raw Update) (*contract.Update, error) {
	switch {
	case raw.Message != nil:
		return normalizeMessage(raw.UpdateID, "message", raw.Message)
	case raw.EditedMessage != nil:
		return normalizeMessage(raw.UpdateID, "edited_message", raw.EditedMessage)
	case raw.CallbackQuery != nil:
		return normalizeCallbackQuery(raw.UpdateID, raw.CallbackQuery)
	default:
		return nil, nil
	}
}

func normalizeMessage(updateID int64, updateType string, msg *Message) (*contract.Update, error) {
	u := &contract.Update{
		UpdateID:  updateID,
		Type:      updateType,
		ChatID:    msg.Chat.ID,
		ThreadID:  msg.MessageThreadID,
		MessageID: msg.MessageID,
		Timestamp: msg.Date,
	}

	if msg.From != nil {
		u.FromUser = normalizeUser(*msg.From)
	}

	if msg.ReplyToMessage != nil {
		id := msg.ReplyToMessage.MessageID
		u.ReplyToMessageID = &id
	}

	// Service messages take priority over content classification.
	if svc := normalizeServiceMessage(msg); svc != nil {
		u.Type = "service"
		u.Service = svc
		return u, nil
	}

	// Regular messages require a sender.
	if msg.From == nil {
		return nil, fmt.Errorf("message %d has no From field", msg.MessageID)
	}

	content, err := normalizeContent(msg)
	if err != nil {
		return nil, err
	}
	u.Content = content
	return u, nil
}

func normalizeCallbackQuery(updateID int64, cq *CallbackQuery) (*contract.Update, error) {
	u := &contract.Update{
		UpdateID: updateID,
		Type:     "callback_query",
		FromUser: normalizeUser(cq.From),
	}

	if cq.Message != nil {
		u.ChatID = cq.Message.Chat.ID
		u.ThreadID = cq.Message.MessageThreadID
		u.MessageID = cq.Message.MessageID
		u.Timestamp = cq.Message.Date
	}

	u.Content = &contract.Content{
		Type:            contract.ContentTypeCallback,
		CallbackQueryID: &cq.ID,
		Data:            cq.Data,
	}
	return u, nil
}

func normalizeServiceMessage(msg *Message) *contract.Service {
	switch {
	case msg.ForumTopicCreated != nil:
		color := msg.ForumTopicCreated.IconColor
		return &contract.Service{
			Type:              contract.ServiceTypeForumTopicCreated,
			Name:              &msg.ForumTopicCreated.Name,
			IconColor:         &color,
			IconCustomEmojiID: msg.ForumTopicCreated.IconCustomEmojiID,
		}

	case msg.ForumTopicEdited != nil:
		return &contract.Service{
			Type:              contract.ServiceTypeForumTopicEdited,
			Name:              msg.ForumTopicEdited.Name,
			IconCustomEmojiID: msg.ForumTopicEdited.IconCustomEmojiID,
		}

	case msg.ForumTopicClosed != nil:
		return &contract.Service{Type: contract.ServiceTypeForumTopicClosed}

	case msg.ForumTopicReopened != nil:
		return &contract.Service{Type: contract.ServiceTypeForumTopicReopened}

	case len(msg.NewChatMembers) > 0:
		members := make([]contract.FromUser, len(msg.NewChatMembers))
		for i, m := range msg.NewChatMembers {
			members[i] = normalizeUser(m)
		}
		return &contract.Service{
			Type:    contract.ServiceTypeNewChatMembers,
			Members: members,
		}

	case msg.LeftChatMember != nil:
		member := normalizeUser(*msg.LeftChatMember)
		return &contract.Service{
			Type:   contract.ServiceTypeLeftChatMember,
			Member: &member,
		}

	default:
		return nil
	}
}

func normalizeContent(msg *Message) (*contract.Content, error) {
	switch {
	case msg.Text != nil:
		return &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     msg.Text,
			Entities: normalizeEntities(msg.Entities),
		}, nil

	case len(msg.Photo) > 0:
		best := selectPhoto(msg.Photo)
		return &contract.Content{
			Type:            contract.ContentTypePhoto,
			FileID:          &best.FileID,
			FileSize:        best.FileSize,
			Width:           &best.Width,
			Height:          &best.Height,
			Caption:         msg.Caption,
			CaptionEntities: normalizeEntities(msg.CaptionEntities),
		}, nil

	case msg.Voice != nil:
		v := msg.Voice
		return &contract.Content{
			Type:     contract.ContentTypeVoice,
			FileID:   &v.FileID,
			FileSize: v.FileSize,
			MimeType: v.MimeType,
			Duration: &v.Duration,
		}, nil

	case msg.Audio != nil:
		a := msg.Audio
		return &contract.Content{
			Type:      contract.ContentTypeAudio,
			FileID:    &a.FileID,
			FileSize:  a.FileSize,
			MimeType:  a.MimeType,
			Duration:  &a.Duration,
			Title:     a.Title,
			Performer: a.Performer,
		}, nil

	case msg.Video != nil:
		v := msg.Video
		return &contract.Content{
			Type:            contract.ContentTypeVideo,
			FileID:          &v.FileID,
			FileSize:        v.FileSize,
			MimeType:        v.MimeType,
			Duration:        &v.Duration,
			Width:           &v.Width,
			Height:          &v.Height,
			Caption:         msg.Caption,
			CaptionEntities: normalizeEntities(msg.CaptionEntities),
		}, nil

	case msg.VideoNote != nil:
		vn := msg.VideoNote
		return &contract.Content{
			Type:     contract.ContentTypeVideoNote,
			FileID:   &vn.FileID,
			FileSize: vn.FileSize,
			Duration: &vn.Duration,
			Length:   &vn.Length,
		}, nil

	case msg.Document != nil:
		d := msg.Document
		return &contract.Content{
			Type:            contract.ContentTypeDocument,
			FileID:          &d.FileID,
			FileSize:        d.FileSize,
			MimeType:        d.MimeType,
			FileName:        d.FileName,
			Caption:         msg.Caption,
			CaptionEntities: normalizeEntities(msg.CaptionEntities),
		}, nil

	default:
		// Unknown content type — pass through with nil content.
		return nil, nil
	}
}

// selectPhoto picks the best PhotoSize with long edge ≤ 1280px.
// If all sizes exceed 1280px it returns the one with the largest pixel area.
func selectPhoto(sizes []PhotoSize) PhotoSize {
	const maxLongEdge = 1280

	var best *PhotoSize
	for i := range sizes {
		s := &sizes[i]
		longEdge := s.Width
		if s.Height > longEdge {
			longEdge = s.Height
		}
		if longEdge > maxLongEdge {
			continue
		}
		if best == nil {
			best = s
			continue
		}
		bestLong := best.Width
		if best.Height > bestLong {
			bestLong = best.Height
		}
		if longEdge > bestLong {
			best = s
		}
	}

	// Fallback: all sizes exceed 1280px — pick the largest by area.
	if best == nil {
		for i := range sizes {
			s := &sizes[i]
			if best == nil || s.Width*s.Height > best.Width*best.Height {
				best = s
			}
		}
	}

	if best == nil {
		return PhotoSize{}
	}
	return *best
}

func normalizeUser(u User) contract.FromUser {
	return contract.FromUser{
		ID:        u.ID,
		FirstName: u.FirstName,
		Username:  u.Username,
	}
}

func normalizeEntities(entities []MessageEntity) []contract.Entity {
	if len(entities) == 0 {
		return nil
	}
	result := make([]contract.Entity, len(entities))
	for i, e := range entities {
		result[i] = contract.Entity{
			Type:   e.Type,
			Offset: e.Offset,
			Length: e.Length,
			Extra:  e.URL,
		}
	}
	return result
}
