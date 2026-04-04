// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register sqlite driver
)

// DB wraps a SQLite database providing all state persistence for the bridge.
type DB struct {
	db *sql.DB
}

// Group represents a configured Telegram group/supergroup.
type Group struct {
	ChatID       int64
	Name         string
	CWD          string
	DefaultModel string
	MaxBudget    float64
	TimeoutSec   int
	CreatedAt    time.Time
}

// Session represents an active Claude Code session mapped to a (chat_id, thread_id) pair.
type Session struct {
	ChatID       int64
	ThreadID     int64
	SessionID    string
	CWD          string
	Model        string
	Status       string
	CreatedAt    time.Time
	LastActive   time.Time
	MessageCount int
}

// AllowedUser represents a user permitted to interact with the bot.
type AllowedUser struct {
	UserID  int64
	Role    string
	AddedAt time.Time
}

// SentMessage tracks messages sent by the bot for deduplication / editing.
type SentMessage struct {
	ChatID    int64
	ThreadID  int64
	MessageID int64
	Purpose   string
	CreatedAt time.Time
}

const schemaVersion = 1

// migrations is an ordered list of SQL statements applied once on startup.
// Each entry is applied inside a single transaction. Migrations are idempotent
// because schema_version prevents re-application.
var migrations = []string{
	// Version 1 — initial schema
	`CREATE TABLE IF NOT EXISTS groups (
		chat_id       INTEGER PRIMARY KEY,
		name          TEXT,
		cwd           TEXT NOT NULL,
		default_model TEXT NOT NULL DEFAULT 'claude-sonnet-4-6',
		max_budget    REAL NOT NULL DEFAULT 5.0,
		timeout_sec   INTEGER NOT NULL DEFAULT 300,
		created_at    TEXT NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS sessions (
		chat_id       INTEGER NOT NULL,
		thread_id     INTEGER NOT NULL,
		session_id    TEXT NOT NULL,
		cwd           TEXT NOT NULL,
		model         TEXT,
		status        TEXT NOT NULL DEFAULT 'active',
		created_at    TEXT NOT NULL DEFAULT (datetime('now')),
		last_active   TEXT NOT NULL DEFAULT (datetime('now')),
		message_count INTEGER NOT NULL DEFAULT 0,
		PRIMARY KEY (chat_id, thread_id),
		FOREIGN KEY (chat_id) REFERENCES groups(chat_id)
	);

	CREATE TABLE IF NOT EXISTS allowed_users (
		user_id  INTEGER PRIMARY KEY,
		role     TEXT NOT NULL DEFAULT 'user',
		added_at TEXT NOT NULL DEFAULT (datetime('now'))
	);

	CREATE TABLE IF NOT EXISTS sent_messages (
		chat_id    INTEGER NOT NULL,
		thread_id  INTEGER NOT NULL,
		message_id INTEGER NOT NULL,
		purpose    TEXT NOT NULL,
		created_at TEXT NOT NULL DEFAULT (datetime('now')),
		PRIMARY KEY (chat_id, thread_id, message_id)
	);`,
}

// OpenDB opens (or creates) the SQLite database at path, enables WAL mode,
// and applies any pending migrations.
func OpenDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %q: %w", path, err)
	}

	// SQLite works best with a single writer connection.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if _, err := db.Exec("PRAGMA foreign_keys=ON;"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	d := &DB{db: db}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return d, nil
}

// Close closes the underlying database connection.
func (d *DB) Close() error {
	return d.db.Close()
}

// migrate creates the schema_version table if needed, then applies any
// migrations whose version number exceeds the current schema version.
func (d *DB) migrate() error {
	_, err := d.db.Exec(`CREATE TABLE IF NOT EXISTS schema_version (
		version    INTEGER NOT NULL,
		applied_at TEXT NOT NULL DEFAULT (datetime('now'))
	);`)
	if err != nil {
		return fmt.Errorf("create schema_version: %w", err)
	}

	var current int
	row := d.db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_version`)
	if err := row.Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	for i, ddl := range migrations {
		version := i + 1
		if version <= current {
			continue
		}

		tx, err := d.db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %d: %w", version, err)
		}
		if _, err := tx.Exec(ddl); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %d: %w", version, err)
		}
		if _, err := tx.Exec(
			`INSERT INTO schema_version (version) VALUES (?)`, version,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %d: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %d: %w", version, err)
		}
	}
	return nil
}

// ── groups ────────────────────────────────────────────────────────────────────

// GetGroup returns the group with the given chat_id, or (nil, nil) if not found.
func (d *DB) GetGroup(ctx context.Context, chatID int64) (*Group, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, COALESCE(name,''), cwd, default_model, max_budget, timeout_sec, created_at
		 FROM groups WHERE chat_id = ?`, chatID)
	return scanGroup(row)
}

// UpsertGroup inserts or replaces a group record.
func (d *DB) UpsertGroup(ctx context.Context, g *Group) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO groups (chat_id, name, cwd, default_model, max_budget, timeout_sec, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET
		   name          = excluded.name,
		   cwd           = excluded.cwd,
		   default_model = excluded.default_model,
		   max_budget    = excluded.max_budget,
		   timeout_sec   = excluded.timeout_sec`,
		g.ChatID, g.Name, g.CWD, g.DefaultModel, g.MaxBudget, g.TimeoutSec,
		g.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// ListGroups returns all configured groups.
func (d *DB) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT chat_id, COALESCE(name,''), cwd, default_model, max_budget, timeout_sec, created_at
		 FROM groups ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []*Group
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// DeleteGroup removes a group and all its sessions (cascade handled manually).
func (d *DB) DeleteGroup(ctx context.Context, chatID int64) error {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sent_messages WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM groups WHERE chat_id = ?`, chatID); err != nil {
		return err
	}
	return tx.Commit()
}

type groupScanner interface {
	Scan(dest ...any) error
}

func scanGroup(s groupScanner) (*Group, error) {
	var g Group
	var createdAt string
	err := s.Scan(&g.ChatID, &g.Name, &g.CWD, &g.DefaultModel, &g.MaxBudget, &g.TimeoutSec, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &g, nil
}

// ── sessions ──────────────────────────────────────────────────────────────────

// GetSession returns the session for (chatID, threadID), or (nil, nil) if not found.
func (d *DB) GetSession(ctx context.Context, chatID, threadID int64) (*Session, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
		        created_at, last_active, message_count
		 FROM sessions WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return scanSession(row)
}

// CreateSession inserts a new session record.
func (d *DB) CreateSession(ctx context.Context, s *Session) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	if s.LastActive.IsZero() {
		s.LastActive = now
	}
	if s.Status == "" {
		s.Status = "active"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions
		   (chat_id, thread_id, session_id, cwd, model, status, created_at, last_active, message_count)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ChatID, s.ThreadID, s.SessionID, s.CWD, nullableString(s.Model), s.Status,
		s.CreatedAt.UTC().Format(time.RFC3339),
		s.LastActive.UTC().Format(time.RFC3339),
		s.MessageCount,
	)
	return err
}

// UpdateSession updates mutable fields on an existing session.
func (d *DB) UpdateSession(ctx context.Context, s *Session) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions
		 SET session_id    = ?,
		     cwd           = ?,
		     model         = ?,
		     status        = ?,
		     last_active   = ?,
		     message_count = ?
		 WHERE chat_id = ? AND thread_id = ?`,
		s.SessionID, s.CWD, nullableString(s.Model), s.Status,
		s.LastActive.UTC().Format(time.RFC3339),
		s.MessageCount,
		s.ChatID, s.ThreadID,
	)
	return err
}

// TouchSession bumps last_active and increments message_count atomically.
func (d *DB) TouchSession(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions
		 SET last_active   = datetime('now'),
		     message_count = message_count + 1
		 WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID,
	)
	return err
}

// ListSessions returns all sessions for a group, ordered by last_active descending.
func (d *DB) ListSessions(ctx context.Context, chatID int64) ([]*Session, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
		        created_at, last_active, message_count
		 FROM sessions WHERE chat_id = ? ORDER BY last_active DESC`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var sessions []*Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// DeleteSession removes a session record.
func (d *DB) DeleteSession(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return err
}

type sessionScanner interface {
	Scan(dest ...any) error
}

func scanSession(s sessionScanner) (*Session, error) {
	var sess Session
	var createdAt, lastActive string
	err := s.Scan(
		&sess.ChatID, &sess.ThreadID, &sess.SessionID, &sess.CWD,
		&sess.Model, &sess.Status, &createdAt, &lastActive, &sess.MessageCount,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	sess.LastActive, _ = time.Parse(time.RFC3339, lastActive)
	return &sess, nil
}

// ── allowed_users ─────────────────────────────────────────────────────────────

// IsUserAllowed returns true if userID is in the allowed_users table.
func (d *DB) IsUserAllowed(ctx context.Context, userID int64) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM allowed_users WHERE user_id = ?`, userID,
	).Scan(&count)
	return count > 0, err
}

// GetAllowedUser returns the allowed user record, or (nil, nil) if not found.
func (d *DB) GetAllowedUser(ctx context.Context, userID int64) (*AllowedUser, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT user_id, role, added_at FROM allowed_users WHERE user_id = ?`, userID)

	var u AllowedUser
	var addedAt string
	err := row.Scan(&u.UserID, &u.Role, &addedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u.AddedAt, _ = time.Parse(time.RFC3339, addedAt)
	return &u, nil
}

// UpsertAllowedUser inserts or updates an allowed user.
func (d *DB) UpsertAllowedUser(ctx context.Context, u *AllowedUser) error {
	if u.AddedAt.IsZero() {
		u.AddedAt = time.Now().UTC()
	}
	if u.Role == "" {
		u.Role = "user"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO allowed_users (user_id, role, added_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET role = excluded.role`,
		u.UserID, u.Role, u.AddedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// DeleteAllowedUser removes a user from the allow list.
func (d *DB) DeleteAllowedUser(ctx context.Context, userID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM allowed_users WHERE user_id = ?`, userID)
	return err
}

// ListAllowedUsers returns all allowed users ordered by added_at.
func (d *DB) ListAllowedUsers(ctx context.Context) ([]*AllowedUser, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT user_id, role, added_at FROM allowed_users ORDER BY added_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []*AllowedUser
	for rows.Next() {
		var u AllowedUser
		var addedAt string
		if err := rows.Scan(&u.UserID, &u.Role, &addedAt); err != nil {
			return nil, err
		}
		u.AddedAt, _ = time.Parse(time.RFC3339, addedAt)
		users = append(users, &u)
	}
	return users, rows.Err()
}

// ── sent_messages ─────────────────────────────────────────────────────────────

// RecordSentMessage stores a sent message so it can be found for editing later.
func (d *DB) RecordSentMessage(ctx context.Context, m *SentMessage) error {
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO sent_messages (chat_id, thread_id, message_id, purpose, created_at)
		 VALUES (?, ?, ?, ?, ?)`,
		m.ChatID, m.ThreadID, m.MessageID, m.Purpose,
		m.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// GetSentMessage looks up a sent message by (chatID, threadID, messageID).
// Returns (nil, nil) if not found.
func (d *DB) GetSentMessage(ctx context.Context, chatID, threadID, messageID int64) (*SentMessage, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, thread_id, message_id, purpose, created_at
		 FROM sent_messages WHERE chat_id = ? AND thread_id = ? AND message_id = ?`,
		chatID, threadID, messageID)

	var m SentMessage
	var createdAt string
	err := row.Scan(&m.ChatID, &m.ThreadID, &m.MessageID, &m.Purpose, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &m, nil
}

// FindSentMessageByPurpose returns the most recent sent message with the given
// purpose in a (chatID, threadID) conversation.
func (d *DB) FindSentMessageByPurpose(ctx context.Context, chatID, threadID int64, purpose string) (*SentMessage, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, thread_id, message_id, purpose, created_at
		 FROM sent_messages
		 WHERE chat_id = ? AND thread_id = ? AND purpose = ?
		 ORDER BY created_at DESC LIMIT 1`,
		chatID, threadID, purpose)

	var m SentMessage
	var createdAt string
	err := row.Scan(&m.ChatID, &m.ThreadID, &m.MessageID, &m.Purpose, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &m, nil
}

// DeleteSentMessage removes a specific sent message record.
func (d *DB) DeleteSentMessage(ctx context.Context, chatID, threadID, messageID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM sent_messages WHERE chat_id = ? AND thread_id = ? AND message_id = ?`,
		chatID, threadID, messageID)
	return err
}

// ── helpers ───────────────────────────────────────────────────────────────────

// nullableString returns nil for an empty string, suitable for nullable TEXT columns.
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
