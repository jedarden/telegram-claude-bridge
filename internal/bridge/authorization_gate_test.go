package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
)

// countingAuthorizationPublisher embeds the no-op implementation so the test
// only needs to override the two event methods the router can publish while
// dispatching an incoming update.
type countingAuthorizationPublisher struct {
	events.NullPublisher
	commands int
	messages int
}

func (p *countingAuthorizationPublisher) PublishCommand(int64, string, string, string, string, string) {
	p.commands++
}

func (p *countingAuthorizationPublisher) PublishMessageIn(int64, int64, string, string, string) {
	p.messages++
}

func TestRouter_AuthorizationGateShortCircuitsEveryUpdateKind(t *testing.T) {
	const (
		allowedChat = int64(100)
		blockedChat = int64(999)
		userID      = int64(200)
		threadID    = int64(5)
	)

	// The allowlisted user, group, and session deliberately exist in the
	// blocked chat. If authorization runs after routing, each fixture is
	// otherwise sufficient to reach a downstream handler.
	db := openTestDB(t)
	seedUser(t, db, userID)
	if err := db.UpsertGroup(context.Background(), &Group{
		ChatID:         blockedChat,
		CWD:            "/sensitive/project",
		DefaultModel:   "claude-opus-4-6",
		PermissionMode: "dontAsk",
		CreatedAt:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed blocked group: %v", err)
	}
	seedSession(t, db, blockedChat, threadID)

	sender, recorder := newRecordingProxy(t)
	publisher := &countingAuthorizationPublisher{}
	authorizer := NewAuthorizer(db, allowedChat, 0)
	r := NewRouter(db, publisher, allowedChat)
	r.SetAuthorizer(authorizer)

	commandHandler := NewCommandHandler(db, sender, "http://unused", nil, publisher, "v1.0.0", "test", "test")
	commandHandler.SetAuthorizer(authorizer)
	r.OnCommand = commandHandler.Handle

	var sessionCalls, serviceCalls, callbackCalls int
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { sessionCalls++ }
	r.OnService = func(context.Context, contract.Update) { serviceCalls++ }
	r.OnCallback = func(context.Context, contract.Update) { callbackCalls++ }

	updates := []struct {
		name   string
		update contract.Update
	}{
		{
			name:   "command",
			update: commandTextUpdate(userID, blockedChat, 1, "/config"),
		},
		{
			name:   "session message",
			update: textUpdate(userID, blockedChat, int64Ptr(threadID), "secret prompt", false),
		},
		{
			name: "edited message",
			update: func() contract.Update {
				update := textUpdate(userID, blockedChat, int64Ptr(threadID), "edited secret prompt", false)
				update.Type = "edited_message"
				return update
			}(),
		},
		{
			name:   "service message",
			update: serviceUpdate(userID, blockedChat, int64Ptr(threadID), contract.ServiceTypeForumTopicClosed),
		},
		{
			name:   "callback",
			update: callbackUpdate(userID, blockedChat),
		},
	}
	for _, contentType := range []string{
		contract.ContentTypePhoto,
		contract.ContentTypeVoice,
		contract.ContentTypeAudio,
		contract.ContentTypeVideo,
		contract.ContentTypeVideoNote,
		contract.ContentTypeDocument,
	} {
		updates = append(updates, struct {
			name   string
			update contract.Update
		}{
			name:   "media " + contentType,
			update: authorizationMediaUpdate(userID, blockedChat, threadID, contentType),
		})
	}

	for _, tc := range updates {
		t.Run(tc.name, func(t *testing.T) {
			r.Route(context.Background(), tc.update)
		})
	}

	if sessionCalls != 0 || serviceCalls != 0 || callbackCalls != 0 {
		t.Fatalf("blocked update handlers called: session=%d service=%d callback=%d", sessionCalls, serviceCalls, callbackCalls)
	}
	if sends := recorder.all(); len(sends) != 0 {
		t.Fatalf("blocked /config produced proxy responses, potentially disclosing configuration: %+v", sends)
	}
	if publisher.commands != 0 || publisher.messages != 0 {
		t.Fatalf("blocked updates published downstream events: commands=%d messages=%d", publisher.commands, publisher.messages)
	}

	group, err := db.GetGroup(context.Background(), blockedChat)
	if err != nil {
		t.Fatalf("get blocked group: %v", err)
	}
	if group == nil || group.CWD != "/sensitive/project" || group.DefaultModel != "claude-opus-4-6" {
		t.Fatalf("blocked update changed or exposed group configuration: %+v", group)
	}
	session, err := db.GetSession(context.Background(), blockedChat, threadID)
	if err != nil {
		t.Fatalf("get blocked session: %v", err)
	}
	if session == nil || session.Status != "active" {
		t.Fatalf("blocked update changed session state: %+v", session)
	}
}

func TestRouter_ChatAuthorizationRunsBeforeDatabaseLookupForEveryUpdateKind(t *testing.T) {
	const blockedChat = int64(999)
	const userID = int64(200)
	const threadID = int64(5)

	// A nil DB makes any user lookup or downstream named-topic lookup fail
	// immediately. A blocked chat must still be safely dropped for every update
	// kind because the chat boundary is intentionally checked first.
	r := NewRouter(nil, nil, 100)
	var calls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { calls++ }
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { calls++ }
	r.OnService = func(context.Context, contract.Update) { calls++ }
	r.OnCallback = func(context.Context, contract.Update) { calls++ }

	updates := []contract.Update{
		commandTextUpdate(userID, blockedChat, 1, "/help"),
		textUpdate(userID, blockedChat, int64Ptr(threadID), "hello", false),
		serviceUpdate(userID, blockedChat, int64Ptr(threadID), contract.ServiceTypeForumTopicCreated),
		callbackUpdate(userID, blockedChat),
	}
	for _, contentType := range []string{
		contract.ContentTypePhoto,
		contract.ContentTypeVoice,
		contract.ContentTypeAudio,
		contract.ContentTypeVideo,
		contract.ContentTypeVideoNote,
		contract.ContentTypeDocument,
	} {
		updates = append(updates, authorizationMediaUpdate(userID, blockedChat, threadID, contentType))
	}

	for _, update := range updates {
		r.Route(context.Background(), update)
	}
	if calls != 0 {
		t.Fatalf("blocked update reached a handler %d times", calls)
	}
}

func TestRouter_AllowlistedAndBootstrapUsersCanRouteEveryUpdateKind(t *testing.T) {
	const (
		chatID         = int64(100)
		threadID       = int64(5)
		allowlistedID  = int64(200)
		bootstrapAdmin = int64(9001)
	)

	db := openTestDB(t)
	seedUser(t, db, allowlistedID)
	seedGroup(t, db, chatID)
	seedSession(t, db, chatID, threadID)
	r := NewRouter(db, nil, chatID)
	r.SetAdminUserID(bootstrapAdmin)

	var commandCalls, sessionCalls, serviceCalls, callbackCalls int
	r.OnCommand = func(context.Context, contract.Update, *Group) { commandCalls++ }
	r.OnSession = func(context.Context, contract.Update, *Session, *Group) { sessionCalls++ }
	r.OnService = func(context.Context, contract.Update) { serviceCalls++ }
	r.OnCallback = func(context.Context, contract.Update) { callbackCalls++ }

	// Database allowlisting admits normal update kinds but not privileged
	// callback actions. The bootstrap identity is not present in the database,
	// yet it admits every update kind, including callbacks.
	for _, userID := range []int64{allowlistedID, bootstrapAdmin} {
		r.Route(context.Background(), commandTextUpdate(userID, chatID, 1, "/help"))
		r.Route(context.Background(), textUpdate(userID, chatID, int64Ptr(threadID), "hello", false))
		edited := textUpdate(userID, chatID, int64Ptr(threadID), "edited hello", false)
		edited.Type = "edited_message"
		r.Route(context.Background(), edited)
		r.Route(context.Background(), serviceUpdate(userID, chatID, int64Ptr(threadID), contract.ServiceTypeForumTopicCreated))
		for _, contentType := range []string{
			contract.ContentTypePhoto,
			contract.ContentTypeVoice,
			contract.ContentTypeAudio,
			contract.ContentTypeVideo,
			contract.ContentTypeVideoNote,
			contract.ContentTypeDocument,
		} {
			r.Route(context.Background(), authorizationMediaUpdate(userID, chatID, threadID, contentType))
		}
	}
	r.Route(context.Background(), callbackUpdate(bootstrapAdmin, chatID))
	r.Route(context.Background(), callbackUpdate(allowlistedID, chatID))

	if commandCalls != 2 {
		t.Fatalf("command handler calls = %d, want 2", commandCalls)
	}
	if sessionCalls != 16 {
		t.Fatalf("session handler calls = %d, want 16 (text, edited, and six media updates for two users)", sessionCalls)
	}
	if serviceCalls != 2 {
		t.Fatalf("service handler calls = %d, want 2", serviceCalls)
	}
	if callbackCalls != 1 {
		t.Fatalf("callback handler calls = %d, want 1 for the bootstrap administrator", callbackCalls)
	}

	allowed, err := NewAuthorizer(db, chatID, bootstrapAdmin).CanReceiveUpdate(
		context.Background(), callbackUpdate(bootstrapAdmin, chatID),
	)
	if err != nil {
		t.Fatalf("bootstrap authorization: %v", err)
	}
	if !allowed {
		t.Fatal("bootstrap administrator was not admitted without an allowlist row")
	}
}
