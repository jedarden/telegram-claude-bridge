package bridge

import (
	"context"

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// Authorizer is the single source of truth for the bridge's Telegram access
// boundary and administrator identity. The router uses it for update-level
// admission, while handlers use it for privileged mutations.
//
// A zero allowedChatID intentionally means "all chats" for backwards
// compatibility. A zero adminUserID means there is no environment bootstrap
// identity; database administrators can still be used.
type Authorizer struct {
	db            *DB
	allowedChatID int64
	adminUserID   int64
}

// NewAuthorizer creates an authorization policy for the bridge process.
func NewAuthorizer(db *DB, allowedChatID, adminUserID int64) *Authorizer {
	return &Authorizer{
		db:            db,
		allowedChatID: allowedChatID,
		adminUserID:   adminUserID,
	}
}

// SetAdminUserID updates the environment-configured bootstrap administrator.
func (a *Authorizer) SetAdminUserID(userID int64) {
	if a == nil {
		return
	}
	a.adminUserID = userID
}

// ChatAllowed reports whether an update's chat is inside the configured
// boundary. Zero is the explicitly open-chat configuration.
func (a *Authorizer) ChatAllowed(chatID int64) bool {
	return a == nil || a.allowedChatID == 0 || a.allowedChatID == chatID
}

// IsAdmin reports whether userID is the bootstrap administrator or a database
// administrator. The bootstrap identity remains privileged even if its row is
// accidentally removed from the database.
func (a *Authorizer) IsAdmin(ctx context.Context, userID int64) (bool, error) {
	if a == nil {
		return false, nil
	}
	if a.adminUserID > 0 && userID == a.adminUserID {
		return true, nil
	}
	return a.db.IsUserAdmin(ctx, userID)
}

// CanReceiveUpdate applies the user gate used before any update handler. The
// chat check is intentionally performed first so blocked chats cannot probe
// whether a user or callback is known to the bridge.
func (a *Authorizer) CanReceiveUpdate(ctx context.Context, update contract.Update) (bool, error) {
	if !a.ChatAllowed(update.ChatID) {
		return false, nil
	}

	allowed, err := a.db.IsUserAllowed(ctx, update.FromUser.ID)
	if err != nil {
		return false, err
	}
	if allowed {
		return true, nil
	}

	admin, err := a.IsAdmin(ctx, update.FromUser.ID)
	return admin, err
}

// CanApproveCallback restricts callback actions that can approve tool use or
// submit a transcript to administrators. Allow-listed users may still use
// normal messages and read-only commands, but an inline callback must not be
// able to authorize work on another user's session.
func (a *Authorizer) CanApproveCallback(ctx context.Context, update contract.Update) (bool, error) {
	if !a.ChatAllowed(update.ChatID) {
		return false, nil
	}
	return a.IsAdmin(ctx, update.FromUser.ID)
}
