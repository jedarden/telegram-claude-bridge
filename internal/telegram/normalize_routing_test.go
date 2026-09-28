package telegram

import (
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func TestNormalizeUpdate_PreservesRoutingMetadataAcrossEnvelopeKinds(t *testing.T) {
	const (
		chatID      int64 = -1009876543210
		messageID   int64 = 4321
		timestamp   int64 = 1700012345
		replyID     int64 = 987
		threadValue int64 = 42
	)

	threadID := threadValue
	text := "route this update"
	callbackData := "approve:route"
	callbackQueryID := "callback-route"
	username := "router-user"

	message := func() *Message {
		return &Message{
			MessageID:       messageID,
			From:            &User{ID: 101, FirstName: "Router", Username: &username},
			Chat:            Chat{ID: chatID, Type: "supergroup"},
			Date:            timestamp,
			MessageThreadID: &threadID,
			Text:            &text,
			ReplyToMessage:  &Message{MessageID: replyID},
		}
	}

	cases := []struct {
		name            string
		raw             Update
		wantType        string
		wantFromUserID  int64
		wantContentType contract.ContentType
		wantServiceType string
		wantReply       bool
	}{
		{
			name:            "message",
			raw:             Update{UpdateID: 7001, Message: message()},
			wantType:        "message",
			wantFromUserID:  101,
			wantContentType: contract.ContentTypeText,
			wantReply:       true,
		},
		{
			name: "edited message",
			raw: Update{
				UpdateID:      7002,
				EditedMessage: message(),
			},
			wantType:        "edited_message",
			wantFromUserID:  101,
			wantContentType: contract.ContentTypeText,
			wantReply:       true,
		},
		{
			name: "service",
			raw: Update{
				UpdateID: 7003,
				Message: func() *Message {
					m := message()
					m.Text = nil
					m.ForumTopicClosed = &ForumTopicClosed{}
					return m
				}(),
			},
			wantType:        "service",
			wantFromUserID:  101,
			wantServiceType: contract.ServiceTypeForumTopicClosed,
			wantReply:       true,
		},
		{
			name: "callback query",
			raw: Update{
				UpdateID: 7004,
				CallbackQuery: &CallbackQuery{
					ID:   callbackQueryID,
					From: User{ID: 202, FirstName: "Callback", Username: &username},
					Message: &Message{
						MessageID:       messageID,
						Chat:            Chat{ID: chatID, Type: "supergroup"},
						Date:            timestamp,
						MessageThreadID: &threadID,
					},
					Data: &callbackData,
				},
			},
			wantType:        "callback_query",
			wantFromUserID:  202,
			wantContentType: contract.ContentTypeCallback,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeUpdate(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeUpdate: %v", err)
			}
			if got == nil {
				t.Fatal("NormalizeUpdate returned nil")
			}

			if got.UpdateID != tc.raw.UpdateID || got.Type != tc.wantType {
				t.Errorf("envelope identity = update_id %d/type %q, want %d/%q", got.UpdateID, got.Type, tc.raw.UpdateID, tc.wantType)
			}
			if got.ChatID != chatID {
				t.Errorf("ChatID = %d, want signed chat ID %d", got.ChatID, chatID)
			}
			if got.ThreadID == nil || *got.ThreadID != threadValue {
				t.Errorf("ThreadID = %v, want %d", got.ThreadID, threadValue)
			}
			if got.MessageID != messageID || got.Timestamp != timestamp {
				t.Errorf("routing metadata = message_id %d/timestamp %d, want %d/%d", got.MessageID, got.Timestamp, messageID, timestamp)
			}
			if got.FromUser.ID != tc.wantFromUserID || got.FromUser.FirstName == "" {
				t.Errorf("FromUser = %+v, want user %d", got.FromUser, tc.wantFromUserID)
			}
			if tc.wantReply {
				if got.ReplyToMessageID == nil || *got.ReplyToMessageID != replyID {
					t.Errorf("ReplyToMessageID = %v, want %d", got.ReplyToMessageID, replyID)
				}
			} else if got.ReplyToMessageID != nil {
				t.Errorf("ReplyToMessageID = %v, want nil", got.ReplyToMessageID)
			}

			if tc.wantServiceType != "" {
				if got.Content != nil {
					t.Errorf("service Content = %+v, want nil", got.Content)
				}
				if got.Service == nil || got.Service.Type != tc.wantServiceType {
					t.Errorf("Service = %+v, want type %q", got.Service, tc.wantServiceType)
				}
				return
			}

			if got.Service != nil {
				t.Errorf("Service = %+v, want nil", got.Service)
			}
			if got.Content == nil || got.Content.Type != tc.wantContentType {
				t.Fatalf("Content = %+v, want type %q", got.Content, tc.wantContentType)
			}
		})
	}
}

func TestNormalizeUpdate_NormalizesExplicitGeneralTopicAcrossEnvelopeKinds(t *testing.T) {
	const chatID int64 = -1009876543210
	generalThreadID := int64(1)
	text := "general"
	callbackData := "general-action"

	message := func() *Message {
		return &Message{
			MessageID:       1,
			From:            &User{ID: 1, FirstName: "User"},
			Chat:            Chat{ID: chatID, Type: "supergroup"},
			Date:            1700012000,
			MessageThreadID: &generalThreadID,
			Text:            &text,
		}
	}

	cases := []struct {
		name string
		raw  Update
	}{
		{name: "message", raw: Update{UpdateID: 7101, Message: message()}},
		{name: "edited message", raw: Update{UpdateID: 7102, EditedMessage: message()}},
		{
			name: "service",
			raw: Update{
				UpdateID: 7103,
				Message: func() *Message {
					m := message()
					m.Text = nil
					m.ForumTopicReopened = &ForumTopicReopened{}
					return m
				}(),
			},
		},
		{
			name: "callback query",
			raw: Update{
				UpdateID: 7104,
				CallbackQuery: &CallbackQuery{
					ID:      "general-callback",
					From:    User{ID: 1, FirstName: "User"},
					Message: message(),
					Data:    &callbackData,
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeUpdate(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeUpdate: %v", err)
			}
			if got == nil {
				t.Fatal("NormalizeUpdate returned nil")
			}
			if got.ThreadID != nil {
				t.Errorf("ThreadID = %d, want omitted for General topic", *got.ThreadID)
			}
		})
	}
}

func TestNormalizeUpdate_AllMemberServiceVariants(t *testing.T) {
	const chatID int64 = -1009876543210
	threadID := int64(42)
	joinedUsername := "joined-user"
	leftUsername := "left-user"

	cases := []struct {
		name        string
		raw         Update
		serviceType string
		check       func(*testing.T, *contract.Service)
	}{
		{
			name: "new chat members",
			raw: Update{
				UpdateID: 7201,
				Message: &Message{
					MessageID:       10,
					From:            &User{ID: 1, FirstName: "Admin"},
					Chat:            Chat{ID: chatID, Type: "supergroup"},
					Date:            1700013000,
					MessageThreadID: &threadID,
					NewChatMembers: []User{
						{ID: 2, FirstName: "Joined", Username: &joinedUsername},
						{ID: 3, FirstName: "Second"},
					},
				},
			},
			serviceType: contract.ServiceTypeNewChatMembers,
			check: func(t *testing.T, service *contract.Service) {
				if len(service.Members) != 2 || service.Members[0].ID != 2 || service.Members[1].ID != 3 {
					t.Errorf("Members = %+v, want both joined users", service.Members)
				}
				if service.Members[0].Username == nil || *service.Members[0].Username != joinedUsername {
					t.Errorf("first member username = %v, want %q", service.Members[0].Username, joinedUsername)
				}
			},
		},
		{
			name: "left chat member",
			raw: Update{
				UpdateID: 7202,
				Message: &Message{
					MessageID:       11,
					From:            &User{ID: 1, FirstName: "Admin"},
					Chat:            Chat{ID: chatID, Type: "supergroup"},
					Date:            1700013001,
					MessageThreadID: &threadID,
					LeftChatMember:  &User{ID: 4, FirstName: "Left", Username: &leftUsername},
				},
			},
			serviceType: contract.ServiceTypeLeftChatMember,
			check: func(t *testing.T, service *contract.Service) {
				if service.Member == nil || service.Member.ID != 4 {
					t.Errorf("Member = %+v, want user 4", service.Member)
				}
			},
		},
		{
			name: "my chat member",
			raw: Update{
				UpdateID: 7203,
				MyChatMember: &ChatMemberUpdated{
					Chat:          Chat{ID: chatID, Type: "supergroup"},
					From:          User{ID: 5, FirstName: "Owner"},
					Date:          1700013002,
					OldChatMember: ChatMember{Status: "left"},
					NewChatMember: ChatMember{Status: "administrator"},
				},
			},
			serviceType: contract.ServiceTypeMyChatMember,
			check: func(t *testing.T, service *contract.Service) {
				if service.OldStatus == nil || *service.OldStatus != "left" || service.NewStatus == nil || *service.NewStatus != "administrator" {
					t.Errorf("status transition = old:%v new:%v, want left -> administrator", service.OldStatus, service.NewStatus)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeUpdate(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeUpdate: %v", err)
			}
			if got == nil || got.Type != "service" {
				t.Fatalf("envelope = %+v, want service update", got)
			}
			if got.ChatID != chatID {
				t.Errorf("ChatID = %d, want signed chat ID %d", got.ChatID, chatID)
			}
			if got.Content != nil {
				t.Errorf("Content = %+v, want nil for service update", got.Content)
			}
			if got.Service == nil || got.Service.Type != tc.serviceType {
				t.Fatalf("Service = %+v, want type %q", got.Service, tc.serviceType)
			}
			tc.check(t, got.Service)
		})
	}
}
