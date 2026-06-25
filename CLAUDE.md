# CLAUDE.md - telegram-claude-bridge

This document describes the Go module structure, key files, and testing approach for the telegram-claude-bridge project.

## Project Overview

telegram-claude-bridge is a Go application that connects Telegram forum topics to Claude Code CLI sessions. It uses a **proxy + bridge architecture**:

- **Proxy**: A lightweight HTTP server that holds the Telegram bot token and exposes an internal API for the bridge to consume.
- **Bridge**: The stateful service that manages Claude Code sessions in tmux, routes messages, and handles all business logic.
- **Dashboard**: An optional Bubbletea TUI for real-time monitoring via Unix socket events.

## Go Module Structure

### Module

```
module github.com/jedarden/telegram-claude-bridge
```

Go version: `1.25.0`

### Key Dependencies

- `modernc.org/sqlite` - Pure Go SQLite driver (no CGo)
- `github.com/charmbracelet/bubbletea` - TUI framework for dashboard
- `github.com/charmbracelet/lipgloss` - Styling for dashboard
- `github.com/coreos/go-systemd/v22` - systemd integration (sd_notify, watchdog)
- `github.com/google/uuid` - UUID generation

## Directory Structure

```
cmd/
├── bridge/          # Main bridge binary entry point
│   └── main.go      # Wire all bridge components
├── dashboard/       # Dashboard TUI binary
│   └── main.go      # Bubbletea UI
└── proxy/           # Proxy HTTP server
    └── main.go      # Telegram API handler

internal/
├── bridge/          # Core bridge logic
│   ├── state.go     # SQLite database layer, migrations, data types
│   ├── session_manager.go  # Claude Code session lifecycle
│   ├── commands.go  # Command handlers (/new, /model, /close, etc.)
│   ├── sender.go    # Proxy client for sending messages
│   ├── poller.go    # Long-poll updates from proxy
│   ├── router.go    # Update routing (commands, sessions, callbacks)
│   ├── pty_manager.go  # tmux session and pane management
│   ├── dispatcher_test.go  # Subtask orchestrator tests
│   ├── audio.go     # Whisper transcription
│   ├── video.go     # Video keyframe extraction
│   ├── image.go     # Image handling
│   ├── document.go  # Document attachment handling
│   ├── background_jobs.go  # Background shell process management
│   ├── subtask_orchestrator.go  # Parallel subtask spawning
│   ├── worker_pool.go  # spawn_worker tool execution pool
│   ├── callback_handler.go  # Inline keyboard button interactions
│   ├── cleanup.go   # Session cleanup
│   └── *_test.go    # Unit and integration tests
├── config/          # Configuration loading
│   └── config.go    # Proxy and bridge config from env vars
├── contract/        # Shared types for Proxy ↔ Bridge HTTP API
│   ├── types.go     # Update, Content, Service, request/response types
│   └── errors.go    # Error types
├── events/          # Event publishing to Unix socket for dashboard
│   ├── publisher.go # NDJSON event streaming
│   └── publisher_test.go
├── health/          # Health checking and systemd watchdog
│   ├── health.go    # Health checker for proxy/db
│   ├── health_test.go
│   └── watchdog.go  # systemd watchdog implementation
├── telegram/        # Telegram API client (proxy-side)
│   ├── poller.go    # Telegram getUpdates long-polling
│   ├── sender.go    # sendMessage, editMessageText, etc.
│   ├── types.go     # Telegram API types
│   ├── markdown.go  # MarkdownV2 escaping
│   └── normalize.go # Update normalization
└── updater/         # Self-update mechanism
    ├── updater.go   # Git-based version checking
    └── updater_test.go

deploy/
└── telegram-claude-bridge.service  # systemd unit file

scripts/
├── bridge-crash-alert.sh   # ExecStopPost hook
└── bridge-stop-hook.sh     # Cleanup on stop

docs/                       # (Empty - docs in README.md)
notes/                      # Development notes
```

## Key Files by Component

### Bridge Entry Point

**`cmd/bridge/main.go`**

The main bridge binary initialization:
- Loads config from environment variables
- Opens SQLite database
- Initializes health checker, sender, updater
- Creates event publisher for dashboard
- Wires together command handler, session manager, PTY manager, background job manager, subtask orchestrator
- Starts HTTP health server on localhost:9091
- Starts systemd watchdog
- Enters poll loop for updates

### Database Layer

**`internal/bridge/state.go`**

Core SQLite database operations:
- `OpenDB()` - Opens database with foreign key support
- Schema migrations (versioned, applied on startup)
- `Group`, `Session`, `AllowedUser`, `SentMessage`, `CostEvent`, `Subtask`, `Worker`, `BackgroundJob`, `Snippet`, `ConversationMessage` types
- CRUD operations for all entities
- Transaction handling for multi-step operations

Key migrations: groups, sessions, allowed_users, sent_messages, cost_events, subtasks, workers, background_jobs, snippets, conversation_messages, and various feature additions.

### Session Management

**`internal/bridge/session_manager.go`**

Manages Claude Code sessions:
- One session per `(chat_id, thread_id)` pair
- Spawns `claude` in tmux panes
- Streams responses via stop-hook or PTY screen-scraping fallback
- Restores context from conversation history on session loss
- Handles timeouts, cancellation, and summary generation on close

### PTY Management

**`internal/bridge/pty_manager.go`**

tmux integration:
- Manages tmux session `telegram-bridge`
- Creates one pane per active session
- Writes user input to panes
- Reads Claude output from panes or stop-hook files
- Cleans up stale panes

### Command Handling

**`internal/bridge/commands.go`**

Implements all `/` commands:
- User commands: `/new`, `/cwd`, `/model`, `/haiku`, `/sonnet`, `/opus`, `/color`, `/notify`, `/context`, `/snippet`, `/snippets`, `/info`, `/status`, `/sessions`, `/close`, `/cancel`, `/dispatch`, `/timeout`, `/cost`, `/budget`, `/parallel`, `/bg`, `/jobs`, `/kill`, `/ping`, `/version`, `/help`
- Admin commands: `/cwd` (set), `/permission`, `/config`, `/update`, `/adduser`, `/removeuser`, `/users`

### Router

**`internal/bridge/router.go`**

Routes incoming updates to handlers:
- `OnCommand` - Commands starting with `/`
- `OnSession` - Regular messages to sessions
- `OnService` - Service events (topic created, member changes)
- `OnCallback` - Inline keyboard button interactions

### Sender (Proxy Client)

**`internal/bridge/sender.go`**

HTTP client for proxy API:
- `SendMessage()`, `EditMessageText()`, `SendChatAction()`
- `CreateForumTopic()`, `EditForumTopic()`, `CloseForumTopic()`, `ReopenForumTopic()`
- `PinChatMessage()`, `AnswerCallbackQuery()`
- `GetMessage()` - Fetch message content
- Media upload: `SendPhoto()`, `SendDocument()`, `SendAudio()`, `SendVideo()`
- File download via `GET /file/{file_id}`
- Rate limit handling with exponential backoff
- Message deduplication via sent_messages table

### Intent Detection

**`internal/bridge/intent_test.go`**

Natural language intent recognition (test-driven examples):
- Cancel phrases: "cancel", "stop", "abort"
- Model switching: "use opus", "think harder", "fast mode"
- Notification mode: "stream updates", "quiet mode"
- Cost/status queries
- Session control
- Timeout adjustments

### Media Processing

**`internal/bridge/audio.go`** - Whisper transcription for voice/audio
**`internal/bridge/video.go`** - Keyframe extraction via ffmpeg
**`internal/bridge/image.go`** - Image handling
**`internal/bridge/document.go`** - Document attachment handling

### Background Jobs

**`internal/bridge/background_jobs.go`**

Run shell commands in background with output streaming:
- Process management via `os/exec`
- Output line buffering and streaming
- Job status tracking (running, complete, error, interrupted)
- `/bg` command, `/jobs`, `/kill`

### Subtask Orchestrator

**`internal/bridge/subtask_orchestrator.go`**

Parallel subtask execution:
- Parses `---` delimiter to split prompts
- Spawns headless Claude instances for each subtask
- Reassembles results into final response

### Worker Pool

**`internal/bridge/worker_pool.go`**

Implements the `spawn_worker` synthetic tool:
- Manages concurrent worker processes
- Each worker runs a headless Claude instance
- Returns result to parent session

### Event Publishing

**`internal/events/publisher.go`**

NDJSON event streaming to Unix socket:
- Non-blocking sends (drop if no listener)
- Event types: `message_in`, `message_out`, `command`, `session_update`, `health`
- Helper functions for formatting usernames, topic IDs, previews

### Health Checking

**`internal/health/health.go`**

- Checks proxy health (`GET /health`)
- Checks database connectivity
- Publishes health events
- Provides HTTP health endpoint on localhost:9091

**`internal/health/watchdog.go`**

- systemd watchdog integration
- Pings health endpoint periodically
- Notifies systemd if watchdog fails

### Configuration

**`internal/config/config.go`**

Loads config from environment variables:
- `LoadProxyConfig()` - Proxy configuration
- `LoadBridgeConfig()` - Bridge configuration
- Optional OpenBao secret fetching for bot token

### Contract Types

**`internal/contract/types.go`**

Shared types for Proxy ↔ Bridge HTTP API:
- `Update`, `Content`, `Service`
- Request/response types for all proxy endpoints
- `ContentType` constants
- `ServiceType` constants
- Helper methods: `IsCommand()`, `ExtractCommandAndArgs()`, `IsGeneralTopic()`

### Proxy Entry Point

**`cmd/proxy/main.go`**

HTTP server implementing the proxy API:
- Telegram polling and update normalization
- Handlers: `/health`, `/updates`, `/send`, `/edit`, `/send_chat_action`, `/create_topic`, `/edit_topic`, `/close_topic`, `/reopen_topic`, `/pin_message`, `/get_message`, `/answer_callback`, `/file/{file_id}`, `/send_photo`, `/send_document`, `/send_audio`, `/send_video`

### Telegram API Client

**`internal/telegram/poller.go`** - Telegram `getUpdates` long-polling
**`internal/telegram/sender.go`** - Telegram Bot API client (sendMessage, editMessageText, etc.)
**`internal/telegram/normalize.go`** - Update normalization to contract types
**`internal/telegram/markdown.go`** - MarkdownV2 escaping

### Updater

**`internal/updater/updater.go`**

Self-update mechanism:
- Git-based version checking
- Compares `RunningCommitSHA` with remote
- Triggers update via `/update do`

## Testing Approach

### Unit Tests

Located alongside source files as `*_test.go`:

- **`internal/bridge/state_test.go`** - Database operations, CRUD, migrations
- **`internal/bridge/sender_test.go`** - Proxy client, rate limiting, deduplication
- **`internal/bridge/poller_test.go`** - Update polling from proxy
- **`internal/bridge/router_test.go`** - Update routing logic
- **`internal/bridge/notification_mode_test.go`** - Notification mode parsing
- **`internal/bridge/context_test.go`** - Context injection
- **`internal/bridge/snippet_test.go`** - Snippet CRUD
- **`internal/bridge/commands_notify_test.go`** - Notify command parsing
- **`internal/bridge/intent_test.go`** - Natural language intent detection (table-driven examples)
- **`internal/contract/types_test.go`** - Contract type methods
- **`internal/telegram/markdown_test.go`** - MarkdownV2 escaping
- **`internal/telegram/normalize_test.go`** - Update normalization
- **`internal/telegram/poller_test.go`** - Telegram poller
- **`internal/config/config_test.go`** - Configuration loading
- **`internal/events/publisher_test.go`** - Event publishing
- **`internal/health/health_test.go`** - Health checking
- **`internal/updater/updater_test.go`** - Self-update logic

### Integration Tests

**`internal/bridge/integration_test.go`**

Full-stack integration tests covering:
- Session creation and lifecycle
- Message routing and response streaming
- Command execution
- Media handling
- Background job execution
- Subtask orchestration
- Cost tracking

Uses test doubles for tmux, Claude, and proxy where needed.

### Running Tests

```bash
# Run all tests
make test
# or
go test ./...

# Run tests in a specific package
go test ./internal/bridge
go test ./internal/config

# Run with coverage
go test -cover ./...

# Run with race detection
go test -race ./...

# Verbose output
go test -v ./...
```

### Test Conventions

1. **Table-driven tests** for multiple scenarios (e.g., intent detection, notification modes)
2. **Test doubles** for external dependencies (tmux, Claude, Telegram API)
3. **Integration tests** for end-to-end flows
4. **Coverage** focuses on business logic over HTTP handlers
5. **No CI** - GitHub Actions are disabled; tests run locally or via `make test`

## Build Process

**`Makefile`**

```makefile
VERSION := $(shell git describe --tags --always 2>/dev/null || echo "dev")
COMMITSHA := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILDDATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -s -w -X main.Version=$(VERSION) -X main.CommitSHA=$(COMMITSHA) -X main.BuildDate=$(BUILDDATE)
```

Build commands:
- `make build` - Build all three binaries (proxy, bridge, dashboard)
- `make proxy` - Build proxy only
- `make bridge` - Build bridge only
- `make dashboard` - Build dashboard only
- `make test` - Run tests
- `make vet` - Run `go vet`
- `make docker` - Build Docker image

Version info injected via ldflags at build time.

## CI/CD

No CI is configured. GitHub Actions are disabled (`.github/workflows/ci.yml.disabled`). All testing is local.

## Deployment Architecture

The project uses **Docker + systemd**, not Kubernetes:

- **Proxy**: Docker container (scratch-based, ~5 MB)
- **Bridge**: systemd service on bare metal (needs tmux, claude CLI, whisper)
- **Dashboard**: Optional TUI, runs standalone

See `README.md` for full deployment instructions.
