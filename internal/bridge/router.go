package bridge

import (
	"context"
	"log"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// generalTopicID is Telegram's thread_id for the General topic in a forum group.
// The General topic also appears as nil thread_id (message sent without thread context).
const generalTopicID = int64(1)

// CommandHandlerFunc handles bot commands sent in the General topic.
// group is the registered group for the chat, or nil if the group is not registered
// (which can happen when the first /cwd command registers it).
type CommandHandlerFunc func(ctx context.Context, update contract.Update, group *Group)

// ServiceHandlerFunc handles service messages such as forum topic lifecycle events
// and member changes.
type ServiceHandlerFunc func(ctx context.Context, update contract.Update)

// SessionHandlerFunc handles messages in a named forum topic.
// session is nil when no session exists for this topic yet — the handler is
// responsible for creating one if appropriate.
// group is always non-nil when this handler is called.
type SessionHandlerFunc func(ctx context.Context, update contract.Update, session *Session, group *Group)

// CallbackHandlerFunc handles callback_query updates (inline keyboard button presses).
type CallbackHandlerFunc func(ctx context.Context, update contract.Update)

// Router classifies incoming updates and dispatches them to the appropriate handler.
// It is the security boundary: updates from unauthorized users are dropped here
// before reaching any handler.
type Router struct {
	db *DB

	// OnCommand is called for bot commands in the General topic.
	OnCommand CommandHandlerFunc

	// OnService is called for service messages (topic lifecycle, member changes).
	OnService ServiceHandlerFunc

	// OnSession is called for messages in named forum topics.
	OnSession SessionHandlerFunc

	// OnCallback is called for callback_query updates.
	OnCallback CallbackHandlerFunc
}

// NewRouter returns a Router backed by db.
func NewRouter(db *DB) *Router {
	return &Router{db: db}
}

// Route classifies update and dispatches it to the registered handler.
//
// Routing order:
//  1. Drop updates from unauthorized users silently.
//  2. callback_query → OnCallback
//  3. service message → OnService
//  4. General topic (thread_id nil or 1) + command → OnCommand
//  5. Named topic (thread_id non-nil, non-1) → look up session, then OnSession;
//     if group is unregistered, drop silently.
//
// Non-command messages in the General topic are silently ignored.
func (r *Router) Route(ctx context.Context, update contract.Update) {
	// ── 1. Authorization ────────────────────────────────────────────────────────
	allowed, err := r.db.IsUserAllowed(ctx, update.FromUser.ID)
	if err != nil {
		log.Printf("[router] auth check failed for user %d: %v", update.FromUser.ID, err)
		return
	}
	if !allowed {
		return // silently drop
	}

	// ── 2. Callback query ────────────────────────────────────────────────────────
	if update.Type == "callback_query" {
		if r.OnCallback != nil {
			r.OnCallback(ctx, update)
		}
		return
	}

	// ── 3. Service message ───────────────────────────────────────────────────────
	if update.Type == "service" {
		if r.OnService != nil {
			r.OnService(ctx, update)
		}
		return
	}

	// ── 4. General topic ─────────────────────────────────────────────────────────
	isGeneral := update.ThreadID == nil || *update.ThreadID == generalTopicID
	if isGeneral {
		if update.Content != nil && update.Content.IsCommand() && r.OnCommand != nil {
			group, err := r.db.GetGroup(ctx, update.ChatID)
			if err != nil {
				log.Printf("[router] get group for chat %d: %v", update.ChatID, err)
				return
			}
			r.OnCommand(ctx, update, group)
		}
		// Non-command messages in General topic are silently ignored.
		return
	}

	// ── 5. Named topic ───────────────────────────────────────────────────────────
	tid := *update.ThreadID

	session, err := r.db.GetSession(ctx, update.ChatID, tid)
	if err != nil {
		log.Printf("[router] get session for (%d, %d): %v", update.ChatID, tid, err)
		return
	}

	if session != nil {
		// Existing session — dispatch directly.
		if r.OnSession != nil {
			r.OnSession(ctx, update, session, nil)
		}
		return
	}

	// No session — check whether this group is registered before creating one.
	group, err := r.db.GetGroup(ctx, update.ChatID)
	if err != nil {
		log.Printf("[router] get group for chat %d: %v", update.ChatID, err)
		return
	}
	if group == nil {
		return // unregistered group — silently ignore
	}

	// Registered group, new topic — let the session handler create the session.
	if r.OnSession != nil {
		r.OnSession(ctx, update, nil, group)
	}
}
