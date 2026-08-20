// Package bridge implements the bridge-side components that connect to the proxy.
package bridge

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // register sqlite driver

	"github.com/jedarden/telegram-claude-bridge/internal/contract"
)

// Icon color constants for topic status states.
const (
	ColorActive    = contract.IconColorLightBlue // 0x6FB9F0
	ColorComplete  = contract.IconColorGreen     // 0x8EEE98
	ColorBlocked   = contract.IconColorYellow    // 0xFFD67E
	ColorError     = contract.IconColorRedOrange // 0xFB6F5F
	ColorReview    = contract.IconColorPink      // 0xFF93B2
	ColorResearch  = contract.IconColorPurple    // 0xCB86DB
)

// DB wraps a SQLite database providing all state persistence for the bridge.
type DB struct {
	db *sql.DB
}

// Group represents a configured Telegram group/supergroup.
type Group struct {
	ChatID             int64
	Name               string
	CWD                string
	DefaultModel       string
	MaxBudget          float64
	TimeoutSec         int
	PermissionMode     string
	AllowedTools       string // JSON array of tool names, or empty for all tools
	DisallowedTools    string // JSON array of tool names, or empty for no restrictions
	MaxSubtasks        int    // Maximum concurrent subtasks per topic (default 5)
	MaxWorkers         int    // Maximum concurrent spawn_worker per topic (default 5)
	ProgressIntervalSec int    // Progress ticker interval in seconds (0 = disabled, default 120)
	DispatcherMode     int    // 1 = orchestrator system prompt injected (default), 0 = direct mode
	TranscriptVerify   bool   // true = require user approval before sending audio transcription to Claude
	CreatedAt          time.Time
}

// Session represents an active Claude Code session mapped to a (chat_id, thread_id) pair.
type Session struct {
	ChatID           int64
	ThreadID         int64
	SessionID        string
	CWD              string
	Model            string
	Status           string
	IconColor        int
	CreatedAt        time.Time
	LastActive       time.Time
	MessageCount     int
	PinnedMessageID  int64  // ID of the pinned metadata message in this topic
	TotalCostUSD     float64 // Total cost of all messages in this session (USD)
	Summary          string // Summary of the session, generated on close
	NotificationMode string // Notification mode: "live" (default), "summary", "quiet"
	TimeoutSec       int    // Per-topic timeout override (0 = use group timeout)
	DispatcherMode   int    // 1 = dispatcher enabled (default), 0 = direct mode; -1 = use group default
	TopicName        string // Name of the forum topic (stored for /context lookup)
	LastFromUserID   int64  // Telegram user ID who sent the last message in this session
}

// AllowedUser represents a user permitted to interact with the bot.
type AllowedUser struct {
	UserID  int64
	Role    string
	AddedAt time.Time
}

// UserInfo represents Telegram user information for display.
type UserInfo struct {
	UserID   int64
	Username string // Telegram username (without @ prefix)
	FirstName string
	LastName string
}

// SentMessage tracks messages sent by the bot for deduplication / editing.
type SentMessage struct {
	ChatID    int64
	ThreadID  int64
	MessageID int64
	Purpose   string
	CreatedAt time.Time
}

// CostEvent represents a single API invocation cost record.
type CostEvent struct {
	ID                   int64
	ChatID               int64
	ThreadID             int64
	CostUSD              float64
	InputTokens          int
	OutputTokens         int
	CacheReadTokens      int
	CacheCreationTokens  int
	Model                string
	FromUserID           int64 // Telegram user ID who triggered this cost event
	CreatedAt            time.Time
}

// Subtask represents a parallel sub-task spawned by the SubtaskOrchestrator.
type Subtask struct {
	ID           string    // Unique subtask ID
	ChatID       int64     // Parent chat ID
	ThreadID     int64     // Parent thread ID
	ParentMsgID  int64     // Message ID of the parent message that spawned this subtask
	Prompt       string    // The prompt for this subtask
	SessionID    string    // Optional session ID to resume
	Status       string    // "running", "complete", "error"
	Result       string    // Result text if complete
	Error        string    // Error message if failed
	StartedAt    time.Time // When the subtask started
	FinishedAt   *time.Time // When the subtask finished (nil if running)
}

// Worker represents a spawned worker process from spawn_worker synthetic tool.
type Worker struct {
	ID         string    // Unique worker ID
	ChatID     int64     // Parent chat ID
	ThreadID   int64     // Parent thread ID
	ParentMsg  int64     // Message ID of the parent message that spawned this worker
	Prompt     string    // The prompt for this worker
	SessionID  string    // Optional session ID from the worker invocation
	Model      string    // Model used by this worker
	Status     string    // "running", "done", "failed"
	Result     string    // Result text if complete
	Error      string    // Error message if failed
	StartedAt  time.Time // When the worker started
	FinishedAt *time.Time // When the worker finished (nil if running)
}

// BackgroundJob represents a running background shell process.
// This is the database version - the background_jobs.go file has the in-memory version with Cmd.
type BackgroundJob struct {
	ID        string    // Unique job identifier (8-character hex)
	ChatID    int64     // Telegram chat ID
	ThreadID  int64     // Telegram thread ID
	Command   string    // The full command being executed
	Status    string    // "running", "complete", "error", "interrupted"
	ExitCode  *int      // Exit code (nil if still running or interrupted)
	StartedAt time.Time // When the job was started
}

// Snippet represents a reusable context snippet for a chat.
type Snippet struct {
	ID        int64     // Auto-increment ID
	ChatID    int64     // Telegram chat ID
	Name      string    // Unique snippet name within the chat
	Content   string    // Snippet content
	CreatedAt time.Time // When the snippet was created
}

// ConversationMessage is a stored Telegram exchange — one user turn or one
// assistant turn. The bridge writes these independently of Claude sessions so
// conversation history survives session loss, restarts, or --resume failures.
type ConversationMessage struct {
	ID        int64
	ChatID    int64
	ThreadID  int64
	Role      string    // "user" or "assistant"
	Content   string
	TgMsgID   int64     // Telegram message ID (user messages only; 0 for assistant)
	CreatedAt time.Time
}

// UpdateFailure represents a failed update check, persisted to surface silent updater failures.
type UpdateFailure struct {
	ID         int64     // Auto-increment ID
	ErrorType  string    // 'build_failed', 'go_not_found', 'git_error', 'uncommitted_changes'
	ErrorMsg   string    // Detailed error message
	AttemptedAt time.Time // When the failure occurred
	Resolved   bool      // Whether the failure has been resolved (success after failure)
}

const schemaVersion = 26

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

	// Version 2 — add permission_mode to groups
	`ALTER TABLE groups ADD COLUMN permission_mode TEXT NOT NULL DEFAULT 'acceptEdits';`,

	// Version 3 — add icon_color to sessions
	`ALTER TABLE sessions ADD COLUMN icon_color INTEGER NOT NULL DEFAULT 7322096;`, // 0x6FB9F0 (light blue)

		// Version 4 — add pinned_message_id to sessions
		`ALTER TABLE sessions ADD COLUMN pinned_message_id INTEGER NOT NULL DEFAULT 0;`,

		// Version 5 — add total_cost_usd to sessions
		`ALTER TABLE sessions ADD COLUMN total_cost_usd REAL NOT NULL DEFAULT 0;`,

		// Version 6 — add summary to sessions
		`ALTER TABLE sessions ADD COLUMN summary TEXT;`,

		// Version 7 — add cost_events table for detailed cost tracking
		`CREATE TABLE IF NOT EXISTS cost_events (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			chat_id     INTEGER NOT NULL,
			thread_id   INTEGER NOT NULL,
			cost_usd    REAL NOT NULL,
			input_tokens  INTEGER,
			output_tokens INTEGER,
			cache_read_tokens INTEGER,
			cache_creation_tokens INTEGER,
			model       TEXT,
			created_at  TEXT NOT NULL DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_cost_events_chat_thread ON cost_events(chat_id, thread_id);
		CREATE INDEX IF NOT EXISTS idx_cost_events_created_at ON cost_events(created_at);`,
		// Version 8 — add notification_mode to sessions
		`ALTER TABLE sessions ADD COLUMN notification_mode TEXT NOT NULL DEFAULT 'live';`,

		// Version 9 — add tool restrictions to groups
		`ALTER TABLE groups ADD COLUMN allowed_tools TEXT;
		 ALTER TABLE groups ADD COLUMN disallowed_tools TEXT;`,

		// Version 10 — raise default timeout from 300s to 1800s for existing groups.
		// Only updates groups still at the old hardcoded default (300); groups
		// explicitly configured to another value are left untouched.
		// timeout_sec = 0 is now the sentinel for "no timeout".
		`UPDATE groups SET timeout_sec = 1800 WHERE timeout_sec = 300;`,
		// Version 11 — add timeout_sec to sessions for per-topic timeout override.
		// Default 0 means "use group timeout" (group-level fallback).
		`ALTER TABLE sessions ADD COLUMN timeout_sec INTEGER NOT NULL DEFAULT 0;`,

		// Version 12 — add max_subtasks to groups for parallel subtask limiting.
		`ALTER TABLE groups ADD COLUMN max_subtasks INTEGER NOT NULL DEFAULT 5;`,

		// Version 13 — add subtasks table for parallel task orchestration.
		`CREATE TABLE IF NOT EXISTS subtasks (
			id           TEXT PRIMARY KEY,
			chat_id      INTEGER NOT NULL,
			thread_id    INTEGER NOT NULL,
			parent_msg   INTEGER NOT NULL,
			prompt       TEXT NOT NULL,
			session_id   TEXT,
			status       TEXT NOT NULL DEFAULT 'running',
			result       TEXT,
			error        TEXT,
			started_at   TEXT NOT NULL DEFAULT (datetime('now')),
			finished_at  TEXT
		);

		CREATE INDEX IF NOT EXISTS idx_subtasks_chat_thread ON subtasks(chat_id, thread_id);
		CREATE INDEX IF NOT EXISTS idx_subtasks_status ON subtasks(status);`,

			// Version 14 — add background_jobs table for background shell job runner.
			`CREATE TABLE IF NOT EXISTS background_jobs (
				id          TEXT PRIMARY KEY,
				chat_id     INTEGER NOT NULL,
				thread_id   INTEGER NOT NULL,
				command     TEXT NOT NULL,
				status      TEXT NOT NULL DEFAULT 'running',
				exit_code   INTEGER,
				started_at  TEXT NOT NULL DEFAULT (datetime('now')),
				finished_at TEXT
			);

			CREATE INDEX IF NOT EXISTS idx_background_jobs_chat_thread ON background_jobs(chat_id, thread_id);
			CREATE INDEX IF NOT EXISTS idx_background_jobs_status ON background_jobs(status);`,

			// Version 15 — add progress_interval_sec to groups for progress ticker.
			`ALTER TABLE groups ADD COLUMN progress_interval_sec INTEGER NOT NULL DEFAULT 120;`,

			// Version 16 — add workers table for spawn_worker synthetic tool and max_workers to groups.
			`ALTER TABLE groups ADD COLUMN max_workers INTEGER NOT NULL DEFAULT 5;

			CREATE TABLE IF NOT EXISTS workers (
				id           TEXT PRIMARY KEY,
				chat_id      INTEGER NOT NULL,
				thread_id    INTEGER NOT NULL,
				parent_msg   INTEGER NOT NULL,
				prompt       TEXT NOT NULL,
				session_id   TEXT,
				model        TEXT,
				status       TEXT NOT NULL DEFAULT 'running',
				result       TEXT,
				error        TEXT,
				started_at   TEXT NOT NULL DEFAULT (datetime('now')),
				finished_at  TEXT
			);

			CREATE INDEX IF NOT EXISTS idx_workers_chat_thread ON workers(chat_id, thread_id);
			CREATE INDEX IF NOT EXISTS idx_workers_status ON workers(status);`,

			// Version 17 — add dispatcher_mode to groups and sessions for orchestrator system prompt injection.
			`ALTER TABLE groups ADD COLUMN dispatcher_mode INTEGER NOT NULL DEFAULT 1;
			 ALTER TABLE sessions ADD COLUMN dispatcher_mode INTEGER NOT NULL DEFAULT -1;`,

			// Version 18 — add from_user_id to cost_events for per-user attribution.
			`ALTER TABLE cost_events ADD COLUMN from_user_id INTEGER NOT NULL DEFAULT 0;`,

			// Version 19 — add snippets table for context snippet management (Phase 5.2).
			`CREATE TABLE IF NOT EXISTS snippets (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				chat_id    INTEGER NOT NULL,
				name       TEXT NOT NULL,
				content    TEXT NOT NULL,
				created_at TEXT NOT NULL DEFAULT (datetime('now')),
				UNIQUE(chat_id, name)
			);

			CREATE INDEX IF NOT EXISTS idx_snippets_chat_id ON snippets(chat_id);`,

			// Version 20 — conversation history table, independent of Claude sessions.
			// Stores every user message and assistant response so history survives
			// session loss, binary restarts, or --resume failures.
			`CREATE TABLE IF NOT EXISTS conversation_messages (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				chat_id    INTEGER NOT NULL,
				thread_id  INTEGER NOT NULL,
				role       TEXT NOT NULL,
				content    TEXT NOT NULL,
				tg_msg_id  INTEGER NOT NULL DEFAULT 0,
				created_at TEXT NOT NULL DEFAULT (datetime('now'))
			);

			CREATE INDEX IF NOT EXISTS idx_conv_msgs_topic
				ON conversation_messages (chat_id, thread_id, created_at);`,

			// Version 21 — add topic_name to sessions for /context command lookup.
			`ALTER TABLE sessions ADD COLUMN topic_name TEXT;`,

			// Version 22 — add transcript_verify to groups for opt-in audio transcription verification.
			`ALTER TABLE groups ADD COLUMN transcript_verify INTEGER NOT NULL DEFAULT 0;`,

				// Version 23 — add last_from_user_id to sessions for per-user last message attribution.
				`ALTER TABLE sessions ADD COLUMN last_from_user_id INTEGER NOT NULL DEFAULT 0;`,

				// Version 24 — add processed_updates table for update deduplication.
				// Tracks which Telegram update_ids have been processed to prevent replay
				// after proxy restarts or offset loss. The update_id is the unique,
				// monotonically-increasing identifier from Telegram's getUpdates API.
				`CREATE TABLE IF NOT EXISTS processed_updates (
					update_id INTEGER PRIMARY KEY,
					processed_at TEXT NOT NULL DEFAULT (datetime('now'))
				);

				CREATE INDEX IF NOT EXISTS idx_processed_updates_at
					ON processed_updates (processed_at);`,

				// Version 25 — add budget_alerts table for tracking one-time budget threshold alerts.
				// Tracks which groups have already received alerts at 80% and 100% budget thresholds.
				`CREATE TABLE IF NOT EXISTS budget_alerts (
					chat_id      INTEGER NOT NULL,
					threshold    INTEGER NOT NULL, -- 80 for 80%, 100 for 100%
					alerted_at   TEXT NOT NULL DEFAULT (datetime('now')),
					PRIMARY KEY (chat_id, threshold)
				);

				CREATE INDEX IF NOT EXISTS idx_budget_alerts_chat ON budget_alerts(chat_id);`,

			// Version 26 — add update_failures table for tracking update check failures.
			// Surfaces silent updater failures to operators via /status and /update commands.
			`CREATE TABLE IF NOT EXISTS update_failures (
				id         INTEGER PRIMARY KEY AUTOINCREMENT,
				error_type TEXT NOT NULL,    -- 'build_failed', 'go_not_found', 'git_error', 'uncommitted_changes'
				error_msg  TEXT NOT NULL,     -- detailed error message
				attempted_at TEXT NOT NULL DEFAULT (datetime('now')),
				resolved   INTEGER NOT NULL DEFAULT 0  -- 1 if resolved, 0 if still failing
			);

			CREATE INDEX IF NOT EXISTS idx_update_failures_created_at ON update_failures(attempted_at);
			CREATE INDEX IF NOT EXISTS idx_update_failures_resolved ON update_failures(resolved);`,
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

// SqlDB returns the underlying *sql.DB for use with other packages.
func (d *DB) SqlDB() *sql.DB {
	return d.db
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
		`SELECT chat_id, COALESCE(name,''), cwd, default_model, max_budget, timeout_sec,
		        COALESCE(permission_mode,'acceptEdits'),
		        COALESCE(allowed_tools,''), COALESCE(disallowed_tools,''),
		        COALESCE(max_subtasks,5), COALESCE(max_workers,5), COALESCE(progress_interval_sec,120),
		        COALESCE(dispatcher_mode,1), COALESCE(transcript_verify,0), created_at
		 FROM groups WHERE chat_id = ?`, chatID)
	return scanGroup(row)
}

// UpsertGroup inserts or replaces a group record.
func (d *DB) UpsertGroup(ctx context.Context, g *Group) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO groups (chat_id, name, cwd, default_model, max_budget, timeout_sec, permission_mode, allowed_tools, disallowed_tools, max_subtasks, max_workers, progress_interval_sec, dispatcher_mode, transcript_verify, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(chat_id) DO UPDATE SET
		   name                = excluded.name,
		   cwd                  = excluded.cwd,
		   default_model       = excluded.default_model,
		   max_budget          = excluded.max_budget,
		   timeout_sec         = excluded.timeout_sec,
		   permission_mode      = excluded.permission_mode,
		   allowed_tools        = excluded.allowed_tools,
		   disallowed_tools     = excluded.disallowed_tools,
		   max_subtasks         = excluded.max_subtasks,
		   max_workers          = excluded.max_workers,
		   progress_interval_sec = excluded.progress_interval_sec,
		   dispatcher_mode      = excluded.dispatcher_mode,
		   transcript_verify    = excluded.transcript_verify
		 WHERE chat_id = excluded.chat_id`,
		g.ChatID, g.Name, g.CWD, g.DefaultModel, g.MaxBudget, g.TimeoutSec, g.PermissionMode,
		nullableString(g.AllowedTools), nullableString(g.DisallowedTools),
		g.MaxSubtasks, g.MaxWorkers, g.ProgressIntervalSec, g.DispatcherMode,
		boolToInt(g.TranscriptVerify),
		g.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// ListGroups returns all configured groups.
func (d *DB) ListGroups(ctx context.Context) ([]*Group, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT chat_id, COALESCE(name,''), cwd, default_model, max_budget, timeout_sec,
		        COALESCE(permission_mode,'acceptEdits'),
		        COALESCE(allowed_tools,''), COALESCE(disallowed_tools,''),
		        COALESCE(max_subtasks,5), COALESCE(max_workers,5), COALESCE(progress_interval_sec,120),
		        COALESCE(dispatcher_mode,1), COALESCE(transcript_verify,0), created_at
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
	var transcriptVerify int
	err := s.Scan(&g.ChatID, &g.Name, &g.CWD, &g.DefaultModel, &g.MaxBudget, &g.TimeoutSec, &g.PermissionMode, &g.AllowedTools, &g.DisallowedTools, &g.MaxSubtasks, &g.MaxWorkers, &g.ProgressIntervalSec, &g.DispatcherMode, &transcriptVerify, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	g.TranscriptVerify = transcriptVerify != 0
	g.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &g, nil
}

// ── sessions ──────────────────────────────────────────────────────────────────

// GetSession returns the session for (chatID, threadID), or (nil, nil) if not found.
func (d *DB) GetSession(ctx context.Context, chatID, threadID int64) (*Session, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
		        created_at, last_active, message_count, icon_color, pinned_message_id, total_cost_usd,
		        COALESCE(summary,''), COALESCE(notification_mode,'live'), timeout_sec, COALESCE(dispatcher_mode,-1),
			        COALESCE(topic_name,''), COALESCE(last_from_user_id,0)
		 FROM sessions WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return scanSession(row)
}

// GetSessionByTopicName returns the session for (chatID, topicName), or (nil, nil) if not found.
// This is used by the /context command to resolve topic names to thread IDs.
func (d *DB) GetSessionByTopicName(ctx context.Context, chatID int64, topicName string) (*Session, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
		        created_at, last_active, message_count, icon_color, pinned_message_id, total_cost_usd,
		        COALESCE(summary,''), COALESCE(notification_mode,'live'), timeout_sec, COALESCE(dispatcher_mode,-1),
			        COALESCE(topic_name,''), COALESCE(last_from_user_id,0)
		 FROM sessions WHERE chat_id = ? AND topic_name = ?`, chatID, topicName)
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
	if s.IconColor == 0 {
		s.IconColor = 7322096 // Default to light blue (0x6FB9F0)
	}
	if s.NotificationMode == "" {
		s.NotificationMode = "live"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO sessions
		   (chat_id, thread_id, session_id, cwd, model, status, icon_color, created_at, last_active, message_count, pinned_message_id, total_cost_usd, summary, notification_mode, timeout_sec, dispatcher_mode, topic_name, last_from_user_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ChatID, s.ThreadID, s.SessionID, s.CWD, nullableString(s.Model), s.Status,
		s.IconColor,
		s.CreatedAt.UTC().Format(time.RFC3339),
		s.LastActive.UTC().Format(time.RFC3339),
		s.MessageCount,
		s.PinnedMessageID,
		s.TotalCostUSD,
		nullableString(s.Summary),
		s.NotificationMode,
		s.TimeoutSec,
		s.DispatcherMode,
		nullableString(s.TopicName),
		s.LastFromUserID,
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
		     icon_color    = ?,
		     last_active   = ?,
		     message_count = ?,
		     pinned_message_id = ?,
		     total_cost_usd    = ?,
		     summary           = ?,
		     notification_mode = ?,
		     timeout_sec       = ?,
		     dispatcher_mode   = ?,
			     topic_name        = ?,
			     last_from_user_id = ?
		 WHERE chat_id = ? AND thread_id = ?`,
		s.SessionID, s.CWD, nullableString(s.Model), s.Status,
		s.IconColor,
		s.LastActive.UTC().Format(time.RFC3339),
		s.MessageCount,
		s.PinnedMessageID,
		s.TotalCostUSD,
		nullableString(s.Summary),
		s.NotificationMode,
		s.TimeoutSec,
		s.DispatcherMode,
			nullableString(s.TopicName),
			s.LastFromUserID,
		s.ChatID, s.ThreadID,
	)
	return err
}

// SetSessionIconColor updates only the icon_color field for a session.
func (d *DB) SetSessionIconColor(ctx context.Context, chatID, threadID int64, color int) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET icon_color = ? WHERE chat_id = ? AND thread_id = ?`,
		color, chatID, threadID,
	)
	return err
}

// SetSessionNotificationMode updates only the notification_mode field for a session.
// Valid modes are: "live" (default), "summary", "quiet".
func (d *DB) SetSessionNotificationMode(ctx context.Context, chatID, threadID int64, mode string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET notification_mode = ? WHERE chat_id = ? AND thread_id = ?`,
		mode, chatID, threadID,
	)
	return err
}

// SetSessionDispatcherMode updates only the dispatcher_mode field for a session.
// Valid values: 1 (enabled), 0 (disabled), -1 (use group default).
func (d *DB) SetSessionDispatcherMode(ctx context.Context, chatID, threadID int64, mode int) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET dispatcher_mode = ? WHERE chat_id = ? AND thread_id = ?`,
		mode, chatID, threadID,
	)
	return err
}

// GetSessionIconColor returns the current icon_color for a session.
// Returns the default active color if session not found.
func (d *DB) GetSessionIconColor(ctx context.Context, chatID, threadID int64) int {
	session, err := d.GetSession(ctx, chatID, threadID)
	if err != nil || session == nil {
		return ColorActive
	}
	return session.IconColor
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
		        created_at, last_active, message_count, icon_color, pinned_message_id, total_cost_usd,
		        COALESCE(summary,''), COALESCE(notification_mode,'live'), timeout_sec, COALESCE(dispatcher_mode,-1),
		        COALESCE(topic_name,''), COALESCE(last_from_user_id,0)
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

// ListAllSessions returns all sessions across all groups, ordered by last_active descending.
func (d *DB) ListAllSessions(ctx context.Context) ([]*Session, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
		        created_at, last_active, message_count, icon_color, pinned_message_id, total_cost_usd,
		        COALESCE(summary,''), COALESCE(notification_mode,'live'), timeout_sec, COALESCE(dispatcher_mode,-1),
		        COALESCE(topic_name,''), COALESCE(last_from_user_id,0)
			 FROM sessions ORDER BY last_active DESC`)
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

// CloseSession marks a session as closed.
func (d *DB) CloseSession(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET status = 'closed' WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID)
	return err
}

// DeleteSession removes a session record.
func (d *DB) DeleteSession(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM sessions WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return err
}

// SetSessionPinnedMessageID updates the pinned_message_id for a session.
func (d *DB) SetSessionPinnedMessageID(ctx context.Context, chatID, threadID int64, messageID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET pinned_message_id = ? WHERE chat_id = ? AND thread_id = ?`,
		messageID, chatID, threadID,
	)
	return err
}

// ListStaleSessions returns sessions where last_active is older than ttl.
// Only returns sessions with status='active' — already inactive/closed sessions are excluded.
func (d *DB) ListStaleSessions(ctx context.Context, ttl time.Duration) ([]*Session, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT chat_id, thread_id, session_id, cwd, COALESCE(model,''), status,
			        created_at, last_active, message_count, icon_color, pinned_message_id, total_cost_usd,
			        COALESCE(summary,''), COALESCE(notification_mode,'live'), timeout_sec, COALESCE(dispatcher_mode,-1)
			 FROM sessions
			 WHERE status = 'active'
			   AND datetime(last_active) < datetime('now', '-' || ? || ' seconds')
			 ORDER BY last_active ASC`,
		int64(ttl.Seconds()),
	)
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

// SetSessionStatus updates the status field for a session.
func (d *DB) SetSessionStatus(ctx context.Context, chatID, threadID int64, status string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET status = ? WHERE chat_id = ? AND thread_id = ?`,
		status, chatID, threadID,
	)
	return err
}

// UpdateSessionCost adds to the total_cost_usd for a session.
func (d *DB) UpdateSessionCost(ctx context.Context, chatID, threadID int64, costUSD float64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET total_cost_usd = total_cost_usd + ? WHERE chat_id = ? AND thread_id = ?`,
		costUSD, chatID, threadID,
	)
	return err
}

// ── cost_events ───────────────────────────────────────────────────────────────

// RecordCostEvent inserts a new cost event record.
func (d *DB) RecordCostEvent(ctx context.Context, e *CostEvent) error {
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO cost_events
		   (chat_id, thread_id, cost_usd, input_tokens, output_tokens,
		    cache_read_tokens, cache_creation_tokens, model, from_user_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ChatID, e.ThreadID, e.CostUSD, e.InputTokens, e.OutputTokens,
		e.CacheReadTokens, e.CacheCreationTokens, nullableString(e.Model),
		e.FromUserID,
		e.CreatedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// GetGroupTotalCost returns the sum of all costs for a group (all threads).
func (d *DB) GetGroupTotalCost(ctx context.Context, chatID int64) (float64, error) {
	var total float64
	row := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM cost_events WHERE chat_id = ?`, chatID)
	err := row.Scan(&total)
	return total, err
}

// GetTopicTotalCost returns the sum of all costs for a specific topic/thread.
func (d *DB) GetTopicTotalCost(ctx context.Context, chatID, threadID int64) (float64, error) {
	var total float64
	row := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM cost_events WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID)
	err := row.Scan(&total)
	return total, err
}

// TopicCostSummary holds cost breakdown for a single topic.
type TopicCostSummary struct {
	ThreadID    int64
	TotalCost   float64
	EventCount  int
}

// GetCostsByTopic returns a list of topics with their costs for a group.
func (d *DB) GetCostsByTopic(ctx context.Context, chatID int64) ([]*TopicCostSummary, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT thread_id,
		        COALESCE(SUM(cost_usd), 0) as total_cost,
		        COUNT(*) as event_count
		 FROM cost_events
		 WHERE chat_id = ?
		 GROUP BY thread_id
		 ORDER BY total_cost DESC`,
		chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*TopicCostSummary
	for rows.Next() {
		var s TopicCostSummary
		if err := rows.Scan(&s.ThreadID, &s.TotalCost, &s.EventCount); err != nil {
			return nil, err
		}
		summaries = append(summaries, &s)
	}
	return summaries, rows.Err()
}

// DailyCostSummary holds cost data for a single day.
type DailyCostSummary struct {
	Date    string
	TotalCost float64
}

// GetDailyCosts returns daily cost totals for a group within the last N days.
func (d *DB) GetDailyCosts(ctx context.Context, chatID int64, days int) ([]*DailyCostSummary, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT DATE(created_at) as date,
		        COALESCE(SUM(cost_usd), 0) as total_cost
		 FROM cost_events
		 WHERE chat_id = ?
		   AND DATE(created_at) >= DATE('now', '-' || ? || ' days')
		 GROUP BY DATE(created_at)
		 ORDER BY date DESC`,
		chatID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*DailyCostSummary
	for rows.Next() {
		var s DailyCostSummary
		if err := rows.Scan(&s.Date, &s.TotalCost); err != nil {
			return nil, err
		}
		summaries = append(summaries, &s)
	}
	return summaries, rows.Err()
}

// UserCostSummary holds cost data for a single user.
type UserCostSummary struct {
	UserID     int64
	TotalCost  float64
	EventCount int
}

// GetUserTotalCost returns the sum of all costs for a user across all chats.
func (d *DB) GetUserTotalCost(ctx context.Context, userID int64) (float64, error) {
	var total float64
	row := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM cost_events WHERE from_user_id = ?`, userID)
	err := row.Scan(&total)
	return total, err
}

// GetUserTopicCost returns the sum of all costs for a user in a specific topic.
func (d *DB) GetUserTopicCost(ctx context.Context, chatID, threadID, userID int64) (float64, error) {
	var total float64
	row := d.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(cost_usd), 0) FROM cost_events WHERE chat_id = ? AND thread_id = ? AND from_user_id = ?`,
		chatID, threadID, userID)
	err := row.Scan(&total)
	return total, err
}

// GetCostsByUser returns a list of users with their costs for a group.
func (d *DB) GetCostsByUser(ctx context.Context, chatID int64) ([]*UserCostSummary, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT from_user_id,
			        COALESCE(SUM(cost_usd), 0) as total_cost,
			        COUNT(*) as event_count
			 FROM cost_events
			 WHERE chat_id = ?
			 GROUP BY from_user_id
			 ORDER BY total_cost DESC`,
		chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*UserCostSummary
	for rows.Next() {
		var s UserCostSummary
		if err := rows.Scan(&s.UserID, &s.TotalCost, &s.EventCount); err != nil {
			return nil, err
		}
		summaries = append(summaries, &s)
	}
	return summaries, rows.Err()
}

// GetUserRecentCosts returns recent cost events for a user across all groups.
func (d *DB) GetUserRecentCosts(ctx context.Context, userID int64, limit int) ([]*CostEvent, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, cost_usd, input_tokens, output_tokens,
		        cache_read_tokens, cache_creation_tokens, model, from_user_id, created_at
		 FROM cost_events
		 WHERE from_user_id = ?
		 ORDER BY created_at DESC LIMIT ?`,
		userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []*CostEvent
	for rows.Next() {
		var e CostEvent
		var model sql.NullString
		var createdAt string
		err := rows.Scan(&e.ID, &e.ChatID, &e.ThreadID, &e.CostUSD,
			&e.InputTokens, &e.OutputTokens, &e.CacheReadTokens, &e.CacheCreationTokens,
			&model, &e.FromUserID, &createdAt)
		if err != nil {
			return nil, err
		}
		e.Model = model.String
		e.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		events = append(events, &e)
	}
	return events, rows.Err()
}

// GetSessionParticipants returns the user IDs who have participated in a session.
func (d *DB) GetSessionParticipants(ctx context.Context, chatID, threadID int64) ([]int64, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT DISTINCT from_user_id FROM cost_events WHERE chat_id = ? AND thread_id = ? AND from_user_id > 0`,
		chatID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var users []int64
	for rows.Next() {
		var userID int64
		if err := rows.Scan(&userID); err != nil {
			return nil, err
		}
		users = append(users, userID)
	}
	return users, rows.Err()
}

// GetSessionUserCosts returns cost breakdown by user for a session.
func (d *DB) GetSessionUserCosts(ctx context.Context, chatID, threadID int64) ([]*UserCostSummary, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT from_user_id,
			        COALESCE(SUM(cost_usd), 0) as total_cost,
			        COUNT(*) as event_count
			 FROM cost_events
			 WHERE chat_id = ? AND thread_id = ? AND from_user_id > 0
			 GROUP BY from_user_id
			 ORDER BY total_cost DESC`,
		chatID, threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var summaries []*UserCostSummary
	for rows.Next() {
		var s UserCostSummary
		if err := rows.Scan(&s.UserID, &s.TotalCost, &s.EventCount); err != nil {
			return nil, err
		}
		summaries = append(summaries, &s)
	}
	return summaries, rows.Err()
}

// UpdateSessionSummary updates the summary field for a session.
func (d *DB) UpdateSessionSummary(ctx context.Context, chatID, threadID int64, summary string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET summary = ? WHERE chat_id = ? AND thread_id = ?`,
		nullableString(summary), chatID, threadID,
	)
	return err
}

// UpdateSessionLastUser updates the last_from_user_id field for a session.
// This should be called when a user sends a message that triggers a Claude invocation.
func (d *DB) UpdateSessionLastUser(ctx context.Context, chatID, threadID, userID int64) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE sessions SET last_from_user_id = ? WHERE chat_id = ? AND thread_id = ?`,
		userID, chatID, threadID,
	)
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
		&sess.Model, &sess.Status, &createdAt, &lastActive, &sess.MessageCount, &sess.IconColor,
		&sess.PinnedMessageID, &sess.TotalCostUSD, &sess.Summary, &sess.NotificationMode, &sess.TimeoutSec,
		&sess.DispatcherMode, &sess.TopicName, &sess.LastFromUserID,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sess.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	sess.LastActive, _ = time.Parse(time.RFC3339, lastActive)
	if sess.NotificationMode == "" {
		sess.NotificationMode = "live"
	}
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

// GetUserRole returns the role for a user ("admin" or "user").
// Returns empty string if user not found.
func (d *DB) GetUserRole(ctx context.Context, userID int64) (string, error) {
	var role string
	err := d.db.QueryRowContext(ctx,
		`SELECT role FROM allowed_users WHERE user_id = ?`, userID,
	).Scan(&role)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return role, err
}

// IsUserAdmin returns true if the user has an "admin" role.
func (d *DB) IsUserAdmin(ctx context.Context, userID int64) (bool, error) {
	role, err := d.GetUserRole(ctx, userID)
	if err != nil {
		return false, err
	}
	return role == "admin", nil
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

// EnsureAdminUser ensures a user exists as an admin in the database.
// If the user doesn't exist, they are added with the admin role.
// If they exist with a different role, their role is updated to admin.
func (d *DB) EnsureAdminUser(ctx context.Context, userID int64) error {
	u := &AllowedUser{
		UserID:  userID,
		Role:    "admin",
		AddedAt: time.Now().UTC(),
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO allowed_users (user_id, role, added_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET role = excluded.role`,
		u.UserID, u.Role, u.AddedAt.UTC().Format(time.RFC3339),
	)
	return err
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

// boolToInt returns 1 for true, 0 for false, suitable for INTEGER columns storing booleans.
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ── subtasks ───────────────────────────────────────────────────────────────────

// CreateSubtask inserts a new subtask record.
func (d *DB) CreateSubtask(ctx context.Context, s *Subtask) error {
	if s.StartedAt.IsZero() {
		s.StartedAt = time.Now().UTC()
	}
	if s.Status == "" {
		s.Status = "running"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO subtasks (id, chat_id, thread_id, parent_msg, prompt, session_id, status, result, error, started_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.ChatID, s.ThreadID, s.ParentMsgID, s.Prompt,
		nullableString(s.SessionID), s.Status,
		nullableString(s.Result), nullableString(s.Error),
		s.StartedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// UpdateSubtask updates status, result, and error for a subtask.
// Also sets finished_at to the current time.
func (d *DB) UpdateSubtask(ctx context.Context, id string, status, result, errorMsg string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE subtasks
		 SET status = ?, result = ?, error = ?, finished_at = datetime('now')
		 WHERE id = ?`,
		status, nullableString(result), nullableString(errorMsg), id,
	)
	return err
}

// GetSubtask retrieves a subtask by ID.
func (d *DB) GetSubtask(ctx context.Context, id string) (*Subtask, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id,
		        status, result, error, started_at, finished_at
		 FROM subtasks WHERE id = ?`, id)

	return scanSubtask(row)
}

// ListSubtasksByStatus returns all subtasks for a topic with the given status.
func (d *DB) ListSubtasksByStatus(ctx context.Context, chatID, threadID int64, status string) ([]*Subtask, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id,
		        status, result, error, started_at, finished_at
		 FROM subtasks
		 WHERE chat_id = ? AND thread_id = ? AND status = ?
		 ORDER BY started_at ASC`,
		chatID, threadID, status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subtasks []*Subtask
	for rows.Next() {
		s, err := scanSubtask(rows)
		if err != nil {
			return nil, err
		}
		subtasks = append(subtasks, s)
	}
	return subtasks, rows.Err()
}

// ListSubtasks returns all subtasks for a topic, ordered by started_at.
func (d *DB) ListSubtasks(ctx context.Context, chatID, threadID int64) ([]*Subtask, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id,
		        status, result, error, started_at, finished_at
		 FROM subtasks
		 WHERE chat_id = ? AND thread_id = ?
		 ORDER BY started_at ASC`,
		chatID, threadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subtasks []*Subtask
	for rows.Next() {
		s, err := scanSubtask(rows)
		if err != nil {
			return nil, err
		}
		subtasks = append(subtasks, s)
	}
	return subtasks, rows.Err()
}

// DeleteSubtask removes a subtask by ID.
func (d *DB) DeleteSubtask(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM subtasks WHERE id = ?`, id)
	return err
}

// DeleteSubtasksForTopic removes all subtasks for a topic.
func (d *DB) DeleteSubtasksForTopic(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM subtasks WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return err
}

type subtaskScanner interface {
	Scan(dest ...any) error
}

func scanSubtask(s subtaskScanner) (*Subtask, error) {
	var subtask Subtask
	var startedAt string
	var finishedAt sql.NullString
	var sessionID, result, errMsg sql.NullString
	err := s.Scan(
		&subtask.ID, &subtask.ChatID, &subtask.ThreadID, &subtask.ParentMsgID,
		&subtask.Prompt, &sessionID, &subtask.Status,
		&result, &errMsg, &startedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	subtask.SessionID = sessionID.String
	subtask.Result = result.String
	subtask.Error = errMsg.String
	subtask.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339, finishedAt.String)
		subtask.FinishedAt = &t
	}
	if subtask.Status == "" {
		subtask.Status = "running"
	}
	return &subtask, nil
}

// ── background_jobs ───────────────────────────────────────────────────────────

// CreateBackgroundJob inserts a new background job record.
func (d *DB) CreateBackgroundJob(ctx context.Context, job *BackgroundJob) error {
	if job.StartedAt.IsZero() {
		job.StartedAt = time.Now().UTC()
	}
	if job.Status == "" {
		job.Status = "running"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO background_jobs (id, chat_id, thread_id, command, status, started_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		job.ID, job.ChatID, job.ThreadID, job.Command, job.Status,
		job.StartedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// UpdateBackgroundJob updates status, exit_code, and finished_at for a background job.
func (d *DB) UpdateBackgroundJob(ctx context.Context, job *BackgroundJob) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE background_jobs
		 SET status = ?, exit_code = ?, finished_at = datetime('now')
		 WHERE id = ?`,
		job.Status, job.ExitCode, job.ID,
	)
	return err
}

// GetBackgroundJob retrieves a background job by ID.
func (d *DB) GetBackgroundJob(ctx context.Context, id string) (*BackgroundJob, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT id, chat_id, thread_id, command, status, exit_code, started_at, finished_at
		 FROM background_jobs WHERE id = ?`, id)

	return scanBackgroundJob(row)
}

// ListBackgroundJobsForTopic returns all background jobs for a topic, ordered by started_at descending.
func (d *DB) ListBackgroundJobsForTopic(ctx context.Context, chatID, threadID int64) ([]*BackgroundJob, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, command, status, exit_code, started_at, finished_at
		 FROM background_jobs
		 WHERE chat_id = ? AND thread_id = ?
		 ORDER BY started_at DESC`,
		chatID, threadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*BackgroundJob
	for rows.Next() {
		job, err := scanBackgroundJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// ListBackgroundJobsByStatus returns all background jobs with a given status.
func (d *DB) ListBackgroundJobsByStatus(ctx context.Context, status string) ([]*BackgroundJob, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, command, status, exit_code, started_at, finished_at
		 FROM background_jobs
		 WHERE status = ?
		 ORDER BY started_at ASC`,
		status,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var jobs []*BackgroundJob
	for rows.Next() {
		job, err := scanBackgroundJob(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

// DeleteBackgroundJob removes a background job by ID.
func (d *DB) DeleteBackgroundJob(ctx context.Context, id string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM background_jobs WHERE id = ?`, id)
	return err
}

// DeleteBackgroundJobsForTopic removes all background jobs for a topic.
func (d *DB) DeleteBackgroundJobsForTopic(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM background_jobs WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return err
}

type backgroundJobScanner interface {
	Scan(dest ...any) error
}

func scanBackgroundJob(s backgroundJobScanner) (*BackgroundJob, error) {
	var job BackgroundJob
	var startedAt string
	var finishedAt sql.NullString
	var exitCode sql.NullInt32
	err := s.Scan(
		&job.ID, &job.ChatID, &job.ThreadID, &job.Command, &job.Status,
		&exitCode, &startedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	job.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	if exitCode.Valid {
		code := int(exitCode.Int32)
		job.ExitCode = &code
	}
	if job.Status == "" {
		job.Status = "running"
	}
	return &job, nil
}

// ── workers ────────────────────────────────────────────────────────────────────

// CreateWorker inserts a new worker record.
func (d *DB) CreateWorker(ctx context.Context, w *Worker) error {
	if w.StartedAt.IsZero() {
		w.StartedAt = time.Now().UTC()
	}
	if w.Status == "" {
		w.Status = "running"
	}
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO workers (id, chat_id, thread_id, parent_msg, prompt, session_id, model, status, result, error, started_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.ChatID, w.ThreadID, w.ParentMsg, w.Prompt,
		nullableString(w.SessionID), nullableString(w.Model), w.Status,
		nullableString(w.Result), nullableString(w.Error),
		w.StartedAt.UTC().Format(time.RFC3339),
	)
	return err
}

// UpdateWorker updates status, result, error, session_id, and finished_at for a worker.
func (d *DB) UpdateWorker(ctx context.Context, id, status, result, errorMsg string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE workers
			 SET status = ?, result = ?, error = ?, finished_at = datetime('now')
			 WHERE id = ?`,
		status, nullableString(result), nullableString(errorMsg), id,
	)
	return err
}

// UpdateWorkerSessionID sets the session_id for a worker.
func (d *DB) UpdateWorkerSessionID(ctx context.Context, id, sessionID string) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE workers SET session_id = ? WHERE id = ?`,
		nullableString(sessionID), id,
	)
	return err
}

// GetWorker retrieves a worker by ID.
func (d *DB) GetWorker(ctx context.Context, id string) (*Worker, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id, model,
		        status, result, error, started_at, finished_at
			 FROM workers WHERE id = ?`, id)
	return scanWorker(row)
}

// CountRunningWorkers returns the count of running workers for a topic.
func (d *DB) CountRunningWorkers(ctx context.Context, chatID, threadID int64) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workers WHERE chat_id = ? AND thread_id = ? AND status = 'running'`,
		chatID, threadID,
	).Scan(&count)
	return count, err
}

// CountRunningWorkersGlobal returns the count of all running workers across all topics.
func (d *DB) CountRunningWorkersGlobal(ctx context.Context) (int, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workers WHERE status = 'running'`,
	).Scan(&count)
	return count, err
}

// ListWorkersForTopic returns all workers for a topic, ordered by started_at descending.
func (d *DB) ListWorkersForTopic(ctx context.Context, chatID, threadID int64) ([]*Worker, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id, model,
		        status, result, error, started_at, finished_at
			 FROM workers
			 WHERE chat_id = ? AND thread_id = ?
			 ORDER BY started_at DESC`,
		chatID, threadID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []*Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

// DeleteWorkersForTopic removes all workers for a topic.
func (d *DB) DeleteWorkersForTopic(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM workers WHERE chat_id = ? AND thread_id = ?`, chatID, threadID)
	return err
}

// ListStaleWorkers returns workers stuck in 'running' status past the worker TTL.
func (d *DB) ListStaleWorkers(ctx context.Context, ttl time.Duration) ([]*Worker, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, parent_msg, prompt, session_id, model,
				        status, result, error, started_at, finished_at
				 FROM workers
				 WHERE status = 'running'
				   AND datetime(started_at) < datetime('now', '-' || ? || ' seconds')
				 ORDER BY started_at ASC`,
		int64(ttl.Seconds()),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workers []*Worker
	for rows.Next() {
		w, err := scanWorker(rows)
		if err != nil {
			return nil, err
		}
		workers = append(workers, w)
	}
	return workers, rows.Err()
}

type workerScanner interface {
	Scan(dest ...any) error
}

func scanWorker(s workerScanner) (*Worker, error) {
	var w Worker
	var startedAt string
	var finishedAt sql.NullString
	var sessionID, model, result, errMsg sql.NullString
	err := s.Scan(
		&w.ID, &w.ChatID, &w.ThreadID, &w.ParentMsg,
		&w.Prompt, &sessionID, &model, &w.Status,
		&result, &errMsg, &startedAt, &finishedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	w.SessionID = sessionID.String
	w.Model = model.String
	w.Result = result.String
	w.Error = errMsg.String
	w.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339, finishedAt.String)
		w.FinishedAt = &t
	}
	if w.Status == "" {
		w.Status = "running"
	}
	return &w, nil
}

	// ── snippets ─────────────────────────────────────────────────────────────────────

// CreateSnippet inserts a new snippet record.
func (d *DB) CreateSnippet(ctx context.Context, s *Snippet) error {
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now().UTC()
	}
	result, err := d.db.ExecContext(ctx,
		`INSERT INTO snippets (chat_id, name, content, created_at) VALUES (?, ?, ?, ?)`,
		s.ChatID, s.Name, s.Content, s.CreatedAt.UTC().Format(time.RFC3339),
	)
	if err != nil {
		return err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return err
	}
	s.ID = id
	return nil
}

// GetSnippet retrieves a snippet by chat_id and name.
// Returns (nil, nil) if not found.
func (d *DB) GetSnippet(ctx context.Context, chatID int64, name string) (*Snippet, error) {
	row := d.db.QueryRowContext(ctx,
		`SELECT id, chat_id, name, content, created_at FROM snippets WHERE chat_id = ? AND name = ?`,
		chatID, name)

	var s Snippet
	var createdAt string
	err := row.Scan(&s.ID, &s.ChatID, &s.Name, &s.Content, &createdAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
	return &s, nil
}

// ListSnippets returns all snippets for a chat, ordered by name.
func (d *DB) ListSnippets(ctx context.Context, chatID int64) ([]*Snippet, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, name, content, created_at FROM snippets WHERE chat_id = ? ORDER BY name`,
		chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var snippets []*Snippet
	for rows.Next() {
		var s Snippet
		var createdAt string
		if err := rows.Scan(&s.ID, &s.ChatID, &s.Name, &s.Content, &createdAt); err != nil {
			return nil, err
		}
		s.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		snippets = append(snippets, &s)
	}
	return snippets, rows.Err()
}

// UpdateSnippet updates the content of an existing snippet.
func (d *DB) UpdateSnippet(ctx context.Context, s *Snippet) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE snippets SET content = ? WHERE chat_id = ? AND name = ?`,
		s.Content, s.ChatID, s.Name,
	)
	return err
}

// DeleteSnippet removes a snippet by chat_id and name.
func (d *DB) DeleteSnippet(ctx context.Context, chatID int64, name string) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM snippets WHERE chat_id = ? AND name = ?`,
		chatID, name)
	return err
}

// ── conversation history ──────────────────────────────────────────────────────

// AddConversationMessage appends a user or assistant turn to the stored history
// for a topic. tgMsgID should be the Telegram message ID for user turns (0 for
// assistant turns).
func (d *DB) AddConversationMessage(ctx context.Context, chatID, threadID int64, role, content string, tgMsgID int64) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO conversation_messages (chat_id, thread_id, role, content, tg_msg_id)
		 VALUES (?, ?, ?, ?, ?)`,
		chatID, threadID, role, content, tgMsgID)
	return err
}

// GetConversationHistory returns up to limit messages for a topic, ordered
// oldest-first (ready to prepend as context).
func (d *DB) GetConversationHistory(ctx context.Context, chatID, threadID int64, limit int) ([]*ConversationMessage, error) {
	rows, err := d.db.QueryContext(ctx,
		`SELECT id, chat_id, thread_id, role, content, tg_msg_id, created_at
		 FROM conversation_messages
		 WHERE chat_id = ? AND thread_id = ?
		 ORDER BY created_at DESC
		 LIMIT ?`,
		chatID, threadID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var msgs []*ConversationMessage
	for rows.Next() {
		m := &ConversationMessage{}
		var createdAt string
		if err := rows.Scan(&m.ID, &m.ChatID, &m.ThreadID, &m.Role, &m.Content, &m.TgMsgID, &createdAt); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse("2006-01-02T15:04:05Z", createdAt)
		msgs = append(msgs, m)
	}
	// Reverse to oldest-first order.
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, rows.Err()
}

// DeleteConversationHistory removes all stored messages for a topic (e.g. on /reset).
func (d *DB) DeleteConversationHistory(ctx context.Context, chatID, threadID int64) error {
	_, err := d.db.ExecContext(ctx,
		`DELETE FROM conversation_messages WHERE chat_id = ? AND thread_id = ?`,
		chatID, threadID)
	return err
}

// ── processed_updates (update deduplication) ───────────────────────────────────

// IsUpdateProcessed returns true if the given update_id has already been processed.
// This prevents replay attacks after proxy restarts or offset loss.
func (d *DB) IsUpdateProcessed(ctx context.Context, updateID int64) (bool, error) {
	var count int
	err := d.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM processed_updates WHERE update_id = ?`, updateID,
	).Scan(&count)
	return count > 0, err
}

// MarkUpdateProcessed records that an update has been processed.
// Uses INSERT OR IGNORE to be idempotent.
func (d *DB) MarkUpdateProcessed(ctx context.Context, updateID int64) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT OR IGNORE INTO processed_updates (update_id) VALUES (?)`, updateID,
	)
	return err
}

// ── update_failures (persistent update failure tracking) ─────────────────────────────

// RecordUpdateFailure records a failed update check. This surfaces silent updater
// failures to operators via /status and /update commands instead of burying them
// in journald logs where they're invisible to users.
func (d *DB) RecordUpdateFailure(ctx context.Context, errorType, errorMsg string) error {
	_, err := d.db.ExecContext(ctx,
		`INSERT INTO update_failures (error_type, error_msg, resolved) VALUES (?, ?, 0)`,
		errorType, errorMsg,
	)
	return err
}

// ListRecentUpdateFailures returns the N most recent update failures, ordered
// by attempted_at descending (newest first). Only returns unresolved failures
// unless includeResolved is true.
func (d *DB) ListRecentUpdateFailures(ctx context.Context, limit int, includeResolved bool) ([]*UpdateFailure, error) {
	query := `SELECT id, error_type, error_msg, attempted_at, resolved
		FROM update_failures`
	if !includeResolved {
		query += ` WHERE resolved = 0`
	}
	query += ` ORDER BY attempted_at DESC LIMIT ?`

	rows, err := d.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var failures []*UpdateFailure
	for rows.Next() {
		f := &UpdateFailure{}
		var attemptedAt string
		var resolvedInt int
		if err := rows.Scan(&f.ID, &f.ErrorType, &f.ErrorMsg, &attemptedAt, &resolvedInt); err != nil {
			return nil, err
		}
		f.AttemptedAt, _ = time.Parse("2006-01-02T15:04:05Z", attemptedAt)
		f.Resolved = resolvedInt != 0
		failures = append(failures, f)
	}
	return failures, rows.Err()
}

// MarkUpdateFailuresResolved marks all unresolved update failures as resolved.
// Called after a successful update to clear the failure state.
func (d *DB) MarkUpdateFailuresResolved(ctx context.Context) error {
	_, err := d.db.ExecContext(ctx,
		`UPDATE update_failures SET resolved = 1 WHERE resolved = 0`,
	)
	return err
}

