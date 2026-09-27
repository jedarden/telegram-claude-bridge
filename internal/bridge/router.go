package bridge

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
	"github.com/jedarden/telegram-claude-bridge/internal/events"
)

// Rate limiter configuration
const (
	// maxMessagesPerMinute is the maximum number of messages a single user can send per minute
	maxMessagesPerMinute = 30
	// rateLimitWindow is the time window for rate limiting
	rateLimitWindow = time.Minute
)

// userLimiter tracks message count for a single user within a sliding window.
type userLimiter struct {
	messages []time.Time
	mu       sync.Mutex
}

// check returns true if the user is within the rate limit, false if exceeded.
func (ul *userLimiter) check() bool {
	ul.mu.Lock()
	defer ul.mu.Unlock()

	now := time.Now()
	// Remove messages older than the rate limit window
	cutoff := now.Add(-rateLimitWindow)
	valid := ul.messages[:0]
	for _, t := range ul.messages {
		if t.After(cutoff) {
			valid = append(valid, t)
		}
	}
	ul.messages = valid

	// Check if user has exceeded the rate limit
	if len(ul.messages) >= maxMessagesPerMinute {
		return false
	}

	// Add current message
	ul.messages = append(ul.messages, now)
	return true
}

// rateLimiter manages per-user rate limiting.
type rateLimiter struct {
	limiters map[int64]*userLimiter
	mu       sync.RWMutex
}

// newRateLimiter creates a new rate limiter.
func newRateLimiter() *rateLimiter {
	return &rateLimiter{
		limiters: make(map[int64]*userLimiter),
	}
}

// check returns true if the user is within the rate limit, false if exceeded.
func (rl *rateLimiter) check(userID int64) bool {
	rl.mu.RLock()
	ul, exists := rl.limiters[userID]
	rl.mu.RUnlock()

	if !exists {
		rl.mu.Lock()
		// Double-check after acquiring write lock
		if ul, exists = rl.limiters[userID]; !exists {
			ul = &userLimiter{
				messages: make([]time.Time, 0, maxMessagesPerMinute),
			}
			rl.limiters[userID] = ul
		}
		rl.mu.Unlock()
	}

	return ul.check()
}

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
	db             *DB
	rateLimiter    *rateLimiter
	eventPublisher events.Publishable
	authorizer     *Authorizer

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
// eventPublisher may be nil if event publishing is disabled. The optional
// allowedChatID argument is intended for the bridge's ALLOWED_CHAT_ID setting;
// omitting it or passing zero accepts updates from every chat.
func NewRouter(db *DB, eventPublisher events.Publishable, allowedChatID ...int64) *Router {
	configuredChatID := int64(0)
	if len(allowedChatID) > 0 {
		configuredChatID = allowedChatID[0]
	}
	r := &Router{
		db:             db,
		rateLimiter:    newRateLimiter(),
		eventPublisher: eventPublisher,
		authorizer:     NewAuthorizer(db, configuredChatID, 0),
	}
	return r
}

// SetAuthorizer installs the process-wide authorization policy. It is used by
// main so routing and command/session handlers evaluate the same identity and
// chat boundary.
func (r *Router) SetAuthorizer(authorizer *Authorizer) {
	if authorizer != nil {
		r.authorizer = authorizer
	}
}

// SetAdminUserID configures the administrator bootstrapped by ADMIN_USER_ID.
// Additional administrators remain controlled by the allowed_users table.
func (r *Router) SetAdminUserID(userID int64) {
	r.authorizer.SetAdminUserID(userID)
}

// Route classifies update and dispatches it to the registered handler.
//
// Routing order:
//  1. Drop updates from unauthorized users silently.
//  2. Drop callback queries from non-admin users silently.
//  3. Rate limiting for non-callback, non-service messages.
//  4. callback_query → OnCallback
//  5. service message → OnService
//  6. General topic (thread_id nil or 1) + command → OnCommand
//  7. Named topic (thread_id non-nil, non-1) → look up session, then OnSession;
//     if group is unregistered, drop silently.
//
// Non-command messages in the General topic are silently ignored.
func (r *Router) Route(ctx context.Context, update contract.Update) {
	// ── 1. Chat authorization ────────────────────────────────────────────────────
	// Apply the chat boundary before touching user authorization or dispatching
	// any callback/service update. Rejected chats are intentionally silent so
	// they cannot use the bot to enumerate its configured chat or users.
	allowed, err := r.authorizer.CanReceiveUpdate(ctx, update)
	if err != nil {
		log.Printf("[router] auth check failed for user %d: %v", update.FromUser.ID, err)
		return
	}
	if !allowed {
		return // silently drop
	}

	// ── 2. Callback authorization ─────────────────────────────────────────────────
	// Callback actions can approve tool use or submit transcribed content. They
	// are privileged even when the originating user is otherwise allow-listed.
	if update.Type == "callback_query" {
		allowed, err := r.authorizer.CanApproveCallback(ctx, update)
		if err != nil {
			log.Printf("[router] callback auth check failed for user %d: %v", update.FromUser.ID, err)
			return
		}
		if !allowed {
			return // silently drop
		}
	}

	// ── 3. Rate limiting ──────────────────────────────────────────────────────────
	// Apply rate limits to non-callback, non-service messages
	if update.Type != "callback_query" && update.Type != "service" {
		if !r.rateLimiter.check(update.FromUser.ID) {
			// User has exceeded the rate limit - log and drop
			username := ""
			if update.FromUser.Username != nil {
				username = *update.FromUser.Username
			}
			log.Printf("[router] rate limit exceeded for user %d (@%s), dropping message",
				update.FromUser.ID, username)
			return
		}
	}

	// ── 5. Callback query ────────────────────────────────────────────────────────
	if update.Type == "callback_query" {
		if r.OnCallback != nil {
			r.OnCallback(ctx, update)
		}
		return
	}

	// ── 6. Service message ───────────────────────────────────────────────────────
	if update.Type == "service" {
		if r.OnService != nil {
			r.OnService(ctx, update)
		}
		return
	}

	// ── 7. General topic ─────────────────────────────────────────────────────────
	isGeneral := update.ThreadID == nil || *update.ThreadID == generalTopicID
	if isGeneral {
		if update.Content != nil && update.Content.IsCommand() && r.OnCommand != nil {
			group, err := r.db.GetGroup(ctx, update.ChatID)
			if err != nil {
				log.Printf("[router] get group for chat %d: %v", update.ChatID, err)
				return
			}
			// Publish command event (Phase 6 format)
			if r.eventPublisher != nil {
				command, args := update.Content.ExtractCommandAndArgs()
				username := ""
				if update.FromUser.Username != nil {
					username = *update.FromUser.Username
				}
				r.eventPublisher.PublishCommand(
					update.ChatID,
					command,
					args,
					"General",
					events.FormatUsername(username, update.FromUser.FirstName),
					"ok",
				)
			}
			r.OnCommand(ctx, update, group)
		}
		// Non-command messages in General topic are silently ignored.
		return
	}

	// ── 8. Named topic ───────────────────────────────────────────────────────────
	tid := *update.ThreadID

	// Publish message received event (Phase 6 format)
	if r.eventPublisher != nil && update.Content != nil {
		username := ""
		if update.FromUser.Username != nil {
			username = *update.FromUser.Username
		}
		user := events.FormatUsername(username, update.FromUser.FirstName)

		// Extract preview from text content
		preview := ""
		if update.Content.Text != nil {
			preview = events.TruncatePreview(*update.Content.Text, 100)
		} else if update.Content.Caption != nil {
			preview = events.TruncatePreview(*update.Content.Caption, 100)
		} else {
			preview = update.Content.Type
		}

		r.eventPublisher.PublishMessageIn(
			update.ChatID,
			tid,
			events.FormatTopicID(tid),
			user,
			preview,
		)
	}

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
