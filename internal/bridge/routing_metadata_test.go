package bridge

import (
	"context"
	"reflect"
	"testing"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

func TestRouter_PreservesNormalizedRoutingMetadata(t *testing.T) {
	const (
		adminUserID int64 = 9001
		chatID      int64 = -1009876543210
		messageID   int64 = 4321
		timestamp   int64 = 1700012345
		replyID     int64 = 987
		threadValue int64 = 42
	)

	db := openTestDB(t)
	seedGroup(t, db, chatID)
	seedSession(t, db, chatID, threadValue)

	r := NewRouter(db, nil, chatID)
	r.SetAdminUserID(adminUserID)

	username := "router-user"
	messageText := "/status details"
	editedText := "edited details"
	callbackData := "approve:route"
	threadID := threadValue
	replyTo := replyID

	command := contract.Update{
		UpdateID:         7301,
		Type:             "message",
		ChatID:           chatID,
		FromUser:         contract.FromUser{ID: adminUserID, FirstName: "Router", Username: &username},
		MessageID:        messageID,
		Timestamp:        timestamp,
		ReplyToMessageID: &replyTo,
		Content: &contract.Content{
			Type:     contract.ContentTypeText,
			Text:     &messageText,
			Entities: []contract.Entity{{Type: "bot_command", Offset: 0, Length: len("/status")}},
		},
	}
	edited := contract.Update{
		UpdateID:         7302,
		Type:             "edited_message",
		ChatID:           chatID,
		ThreadID:         &threadID,
		FromUser:         contract.FromUser{ID: adminUserID, FirstName: "Router", Username: &username},
		MessageID:        messageID + 1,
		Timestamp:        timestamp + 1,
		ReplyToMessageID: &replyTo,
		Content: &contract.Content{
			Type: contract.ContentTypeText,
			Text: &editedText,
		},
	}
	service := contract.Update{
		UpdateID:  7303,
		Type:      "service",
		ChatID:    chatID,
		ThreadID:  &threadID,
		FromUser:  contract.FromUser{ID: adminUserID, FirstName: "Router", Username: &username},
		MessageID: messageID + 2,
		Timestamp: timestamp + 2,
		Service:   &contract.Service{Type: contract.ServiceTypeForumTopicClosed},
	}
	callback := contract.Update{
		UpdateID:  7304,
		Type:      "callback_query",
		ChatID:    chatID,
		ThreadID:  &threadID,
		FromUser:  contract.FromUser{ID: adminUserID, FirstName: "Router", Username: &username},
		MessageID: messageID + 3,
		Timestamp: timestamp + 3,
		Content: &contract.Content{
			Type:            contract.ContentTypeCallback,
			CallbackQueryID: strPtr("callback-route"),
			Data:            &callbackData,
		},
	}

	expected := map[string]contract.Update{
		"message":        command,
		"edited_message": edited,
		"service":        service,
		"callback_query": callback,
	}
	got := make(map[string]contract.Update, len(expected))
	r.OnCommand = func(_ context.Context, update contract.Update, group *Group) {
		if group == nil {
			t.Errorf("OnCommand received nil group for registered chat")
		}
		got[update.Type] = update
	}
	r.OnSession = func(_ context.Context, update contract.Update, session *Session, group *Group) {
		if session == nil || group != nil {
			t.Errorf("OnSession context = session:%v group:%v, want existing session only", session, group)
		}
		got[update.Type] = update
	}
	r.OnService = func(_ context.Context, update contract.Update) {
		got[update.Type] = update
	}
	r.OnCallback = func(_ context.Context, update contract.Update) {
		got[update.Type] = update
	}

	for _, update := range []contract.Update{command, edited, service, callback} {
		r.Route(context.Background(), update)
	}

	if len(got) != len(expected) {
		t.Fatalf("routed update kinds = %v, want %v", got, expected)
	}
	for kind, want := range expected {
		gotUpdate, ok := got[kind]
		if !ok {
			t.Errorf("no handler call for %q", kind)
			continue
		}
		if !reflect.DeepEqual(gotUpdate, want) {
			t.Errorf("%s update changed during routing:\n got: %+v\nwant: %+v", kind, gotUpdate, want)
		}
	}
}
