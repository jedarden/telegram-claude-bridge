# telegram-claude-bridge

A bridge that connects a Telegram bot to [Claude Code](https://github.com/anthropics/claude-code) CLI sessions, letting users interact with Claude directly from Telegram group forum topics. Each topic gets a persistent Claude Code session running in tmux; the bridge routes messages in, streams responses back, and keeps sessions warm between turns. Uses Anthropic subscription billing (interactive PTY), not API credits.

---

## How it works

The system is split into two processes:

**Proxy** — A lightweight container that holds the Telegram bot token, long-polls the Telegram `getUpdates` endpoint, and exposes an internal HTTP API (`/updates`, `/send`, `/edit`, etc.) for the bridge to consume. It holds no session state.

**Bridge** — The stateful brain, running as a systemd service on the bare-metal host. It polls the proxy, manages Claude Code sessions, spawns `claude` inside tmux panes (one pane per forum topic), streams responses back via the proxy, and stores session metadata in a local SQLite database.

**Dashboard** (optional) — A Bubbletea TUI that displays real-time session health, in-flight messages, and cost data over a Unix socket.

### Claude invocation

The bridge maintains a tmux session named `telegram-bridge`. Each active Telegram forum topic maps to one tmux window. Claude Code is launched with configurable permissions:

```
claude <permission_flags> --model <model> [--resume <session_id>]
```

Permission flags are determined by the group's `permission_mode` setting:
- `bypassPermissions` → `--dangerously-skip-permissions`
- `acceptEdits`, `plan`, `dontAsk` → `--permission-mode <mode>`

**Note:** The current implementation always passes `--dangerously-skip-permissions` regardless of the stored `permission_mode` (tracked issue). The `permission_mode` is configurable via `/config permission_mode <mode>` or `/permission <mode>`, but does not yet affect runtime behavior.

Panes stay warm between messages (45 s idle threshold). A Claude stop-hook writes the final response to a file that the bridge polls; it falls back to PTY screen-scraping if the hook is not configured. On session ID loss, up to 40 messages of history are prepended from SQLite to restore context.

---

## Features

### Notification modes

Per-topic, configurable via `/notify` or natural language:

| Mode | Behavior |
|------|----------|
| `live` (default) | Progressively edits one Telegram message as Claude streams (1 s debounce) |
| `summary` | Posts a placeholder, edits to the final response only when done |
| `quiet` | No updates during processing; posts only on completion |

### Natural language intent detection

The bridge intercepts common phrases before forwarding them to Claude:

- **Cancel:** "cancel", "stop", "abort"
- **Model switch:** "use opus", "think harder", "fast mode", etc.
- **Notify mode:** "stream updates", "quiet mode", etc.
- **Cost / status queries**
- **Session control:** "close session", "new session"
- **Timeout adjustments**

### Media support

| Type | Handling |
|------|----------|
| Photos | Injected as image attachments (10 MB limit) |
| Voice messages | Transcribed via `whisper` CLI, text prepended to prompt |
| Audio files (MP3, M4A, FLAC, WAV, OGG) | Same as voice messages |
| Video / video notes | Keyframes extracted + audio transcribed; both fed to Claude |
| Documents | Passed as file attachments (50 MB limit) |

### Dispatcher / orchestrator mode

When enabled (`/dispatch on`), Claude receives two synthetic tools:

- `spawn_worker` — the bridge spawns a headless Claude instance and injects the result back
- `update_progress` — posts a status message to the topic immediately

### Self-update

The bridge checks a configurable remote for new versions on a background interval and applies updates with `/update do`. Update checking can be disabled by setting `UPDATE_INTERVAL_MINUTES=0`.

### Session lifecycle

- One session per `(chat_id, thread_id)` pair
- Messages arriving during processing are batched in a per-topic queue (32 messages deep)
- Sessions resume across restarts via `--resume <session_id>`
- Stale sessions cleaned up automatically (configurable interval and TTL)
- On close: Claude (Haiku) generates a summary and pins it to the topic

---

## Prerequisites

The following must be available on the host running the bridge:

- **tmux** — session multiplexer that hosts Claude Code panes
- **claude** — [Claude Code CLI](https://github.com/anthropics/claude-code), logged in with an active Anthropic subscription
- **whisper** — [whisper.cpp](https://github.com/ggerganov/whisper.cpp) or compatible CLI (required only for voice/audio transcription)
- **git** — required only if self-update is enabled

The proxy runs as a Docker container and has no host-level dependencies beyond a container runtime.

---

## Configuration

### Proxy environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` (or `TELEGRAM_TOKEN`) | — | **Required.** Telegram bot token. |
| `PROXY_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `PROXY_DB_PATH` | `proxy.db` | SQLite DB path for offset tracking |
| `POLL_TIMEOUT` | `30` | Telegram long-poll timeout (seconds) |

### Bridge environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PROXY_URL` | — | **Required.** Base URL of the proxy (e.g. `http://localhost:8080`) |
| `BRIDGE_DB_PATH` | `bridge.db` | SQLite DB path |
| `ALLOWED_CHAT_ID` | `0` (all) | Restrict to a single Telegram chat ID |
| `POLL_TIMEOUT` | `30` | Long-poll timeout (seconds) |
| `SESSION_CLEANUP_INTERVAL_MINUTES` | `60` | Stale session cleanup interval (`0` = disabled) |
| `SESSION_TTL_HOURS` | `168` (7 days) | Age at which a session is considered stale |
| `CLOSE_INACTIVE_TOPICS` | `false` | Close Telegram topics for stale sessions |
| `ADMIN_USER_ID` | `0` | Bootstrap initial admin on startup |
| `ADMIN_CHAT_ID` | `0` | Chat ID for crash-loop alerts (via ExecStopPost) |
| `EVENT_PUBLISHING_ENABLED` | `false` | Enable dashboard Unix socket events |
| `EVENT_SOCKET_PATH` | `/tmp/telegram-bridge-events.sock` | Dashboard socket path |
| `UPDATE_INTERVAL_MINUTES` | `5` | Self-update check interval (`0` = disabled) |

---

## Commands reference

### User commands

| Command | Description |
|---------|-------------|
| `/new <name>` | Create a new forum topic and start a Claude Code session |
| `/cwd` | Show the current working directory for this session |
| `/model [name]` | View or set the active model |
| `/haiku` | Switch to Claude Haiku |
| `/sonnet` | Switch to Claude Sonnet |
| `/opus` | Switch to Claude Opus |
| `/color [name]` | Set topic icon color (`active`, `complete`, `blocked`, `error`, `review`, `research`) |
| `/notify [mode]` | Set notification mode (`live`, `summary`, `quiet`) |
| `/context <thread_id>` | Inject context from another topic into this session |
| `/snippet <name> <content>` | Save a named context snippet |
| `/snippets` | List saved snippets |
| `/info` | Show session details (model, cwd, session ID, message count, cost, notify mode, timeout) |
| `/status` | List active sessions in this group |
| `/sessions` | List all sessions across all groups |
| `/close <thread_id>` | Close a session (generates and pins a summary) |
| `/cancel [thread_id]` | Cancel the running request |
| `/dispatch [on\|off\|default]` | Toggle orchestrator mode |
| `/timeout [N]` | Set per-topic timeout in seconds (`0` = use group default) |
| `/cost` | Show cost breakdown (group total / daily trend / per-topic / per-user) |
| `/budget [amount]` | View or set the group budget |
| `/parallel <prompts>` | Run up to 5 prompts in parallel (separate with `---` on its own line) |
| `/bg <command>` | Run a shell command in the background and stream output to the topic |
| `/jobs` | List background jobs |
| `/kill <job_id>` | Kill a background job |
| `/ping` | Check proxy latency |
| `/version` | Show bridge and proxy versions |
| `/help` | Show help |

### Admin-only commands

| Command | Description |
|---------|-------------|
| `/cwd <path>` | Set group working directory (also registers the group) |
| `/permission [mode]` | Set Claude permission mode |
| `/config [setting] [value]` | View or set `permission_mode`, `allowed_tools`, or `disallowed_tools` |
| `/update [do]` | Check for or apply a self-update |
| `/adduser <id> [role]` | Add an allowed user |
| `/removeuser <id>` | Remove a user |
| `/users` | List allowed users |

Rate limiting: 30 messages per minute per user.

---

## Building

Requires Go 1.25+.

```bash
# Build all three binaries (bin/proxy, bin/bridge, bin/dashboard)
make build

# Build individual components
make proxy
make bridge
make dashboard

# Run tests
make test

# Vet
make vet

# Build the proxy Docker image
make docker
```

The resulting Docker image is scratch-based and approximately 5 MB.

---

## Deployment

### Proxy (Docker container)

The proxy runs as a stateless container. It only needs the bot token and a writable path for its offset-tracking database.

```yaml
# Example docker-compose snippet
services:
  proxy:
    image: telegram-claude-bridge:<version>
    environment:
      BOT_TOKEN: "<your-telegram-bot-token>"
      PROXY_LISTEN_ADDR: ":8080"
      PROXY_DB_PATH: "/data/proxy.db"
    volumes:
      - ./proxy-data:/data
    ports:
      - "8080:8080"
```

### Bridge (systemd service)

The bridge runs directly on the host so it has access to `tmux`, `claude`, and the user's Anthropic session. A sample unit file is in `deploy/telegram-claude-bridge.service`.

```ini
[Unit]
Description=Telegram Claude Bridge
After=network.target

[Service]
Type=notify
WatchdogSec=60
EnvironmentFile=/etc/telegram-claude-bridge/env
ExecStart=/usr/local/bin/bridge
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

The bridge implements `sd_notify` (`Type=notify`) with a 60 s watchdog, so systemd will restart it automatically if it hangs.

### Telegram bot setup

1. Create a bot via [@BotFather](https://t.me/BotFather) and copy the token.
2. Enable **Groups** and **Group Admin** permissions for the bot.
3. In your Telegram group, enable **Topics** (Supergroup setting).
4. Add the bot to the group and promote it so it can manage topics and pin messages.
5. Set `ADMIN_USER_ID` to your Telegram user ID to bootstrap the initial admin on first start.
6. Send `/cwd <path>` from the admin account to register the group and set the working directory.

---

## Version

Current version: **0.3.0**
