# telegram-claude-bridge

A bridge that connects a Telegram bot to [Claude Code](https://github.com/anthropics/claude-code) CLI sessions, letting users interact with Claude directly from Telegram group forum topics. Each topic gets a persistent Claude Code session running in tmux; the bridge routes messages in, streams responses back, and keeps sessions warm between turns. Uses Anthropic subscription billing (interactive PTY), not API credits.

> **Security-sensitive defaults:** `ALLOWED_CHAT_ID=0` accepts every chat, and
> new groups default to `bypassPermissions`, which launches Claude with
> `--dangerously-skip-permissions`. Before running the bridge, restrict it to a
> trusted chat, set `ADMIN_USER_ID`, choose the least-permissive workable mode,
> and run it under an OS account that cannot access unrelated repositories or
> credentials.

Authorization is enforced in the bridge before an update reaches a command,
session, service, or callback handler. If `ALLOWED_CHAT_ID` is non-zero, the
update's chat ID must match it exactly. The configured `ADMIN_USER_ID` is the
bootstrap administrator and remains authorized in that chat; other users must
be present in the database allowlist. Unauthorized users and chats are dropped
silently so the bot does not disclose its configuration.

---

## How it works

The system is split into two processes:

**Proxy** — A lightweight container that holds the Telegram bot token, long-polls the Telegram `getUpdates` endpoint, and exposes an internal HTTP API (`/updates`, `/send`, `/edit`, etc.) for the bridge to consume. It holds no session state. Its persisted state is the Telegram polling offset and the retained update buffer, stored in a JSON file (`OFFSET_FILE_PATH`, default `/data/offset.json`).

#### Update delivery and acknowledgement

The proxy uses an explicit, cumulative acknowledgement protocol. `GET /updates` is at-least-once delivery: it returns every retained update, including updates returned by earlier calls, until the bridge acknowledges them. The first poll may omit the acknowledgement; subsequent polls send `?ack=<update_id>`, where the value is the highest update ID the bridge has durably recorded. The proxy discards retained updates with IDs up to and including that value before returning the next batch. There is no implicit acknowledgement when the next poll starts.

If the bridge or its connection fails after a batch is returned but before the acknowledgement reaches the proxy, the batch is returned again. The bridge's SQLite update-ID deduplication makes that replay safe. The proxy persists the retained buffer with the Telegram offset, so a proxy restart replays anything not acknowledged through the API. The buffer is bounded at 10,000 updates by default; if it overflows during a bridge outage, the oldest updates are dropped and logged because Telegram has already acknowledged them and they cannot be recovered.

This is the v1 protocol. A client that omits `ack` is accepted for an initial/legacy poll but does not acknowledge anything and will receive the retained batch repeatedly; clients written for implicit next-poll acknowledgement must be upgraded before relying on this API. A new bridge talking to an old proxy still works at the HTTP level because the old proxy ignores the unknown query parameter, but its crash-loss behavior is the old destructive-delivery behavior. Do not downgrade a proxy while its new state file contains retained updates: an old proxy ignores that buffer and can lose those updates.

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

The mode is configurable via `/config permission_mode <mode>` or `/permission <mode>` and takes effect at the next Claude spawn. All spawn sites (topic panes, workers, `/parallel` subtasks, service handlers) resolve flags through `resolvePermissionArgs()` in `internal/bridge/session_manager.go`, which is the source of truth. New groups default to `bypassPermissions`; if the stored mode is empty, the Go-side fallback is also `bypassPermissions`.

**Note:** Only the CLI flag reflects the configured mode. Interactive approval of individual tool calls from Telegram (inline approve/deny keyboard on a `plan`-mode prompt) is not implemented — there is no PTY-output prompt detection, so `plan` and `dontAsk` modes will block on Claude's own prompt rather than surfacing it to the chat.

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

**Systemd unit updates:** The self-updater automatically copies `deploy/telegram-claude-bridge.service` to `~/.config/systemd/user/telegram-claude-bridge.service` and runs `systemctl --user daemon-reload` before each update. This ensures service configuration changes (like StartLimit settings, watchdog timeout, environment variables) are applied without manual intervention. The live unit file is always kept in sync with the template in the repo.

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
- **whisper** — [OpenAI Whisper CLI](https://github.com/openai/whisper) (`pip install openai-whisper`), or a CLI that accepts the same flags (`--model turbo --output_format txt --output_dir`) — whisper.cpp's CLI does not (required only for voice/audio transcription)
- **git** — required only if self-update is enabled

The proxy runs as a Docker container and has no host-level dependencies beyond a container runtime.

---

## Configuration

### Proxy environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `BOT_TOKEN` (or `TELEGRAM_TOKEN`) | — | **Required.** Telegram bot token. |
| `PROXY_LISTEN_ADDR` | `:8080` | HTTP listen address |
| `OFFSET_FILE_PATH` | `/data/offset.json` | JSON state file for the Telegram polling offset and retained unacked updates; the path must be writable or both are lost on restart |
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
| `ADMIN_CHAT_ID` | `0` | Chat ID for crash-loop and PTY canary alerts |
| `EVENT_PUBLISHING_ENABLED` | `false` | Enable dashboard Unix socket events |
| `EVENT_SOCKET_PATH` | `/tmp/telegram-bridge-events.sock` | Dashboard socket path |
| `MAX_GLOBAL_WORKERS` | `10` | Maximum concurrent `spawn_worker` Claude processes across all topics (`0` = disabled) |
| `UPDATE_INTERVAL_MINUTES` | `5` | Self-update check interval (`0` = disabled) |
| `CANARY_ENABLED` | `true` | Run the throwaway PTY/Claude drift check at startup |
| `CANARY_INTERVAL_MINUTES` | `0` | Repeat the PTY canary periodically (`0` = startup only) |
| `HEALTH_ADDR` | `127.0.0.1:9091` | Bind address of the health/metrics HTTP server. Point at a Tailscale interface to allow external `/metrics` scraping |

### Metrics endpoint

The health server (default `127.0.0.1:9091`) exposes **Prometheus text-format metrics** at `GET /metrics`:

| Metric | Meaning |
|--------|---------|
| `bridge_sessions_active` | Sessions currently in `active` status |
| `bridge_cost_usd_today` | Total API cost (USD) recorded today (UTC) |
| `bridge_last_update_success_timestamp_seconds` | Unix time of the last self-update verified healthy after restart; **absent** when no update has ever been verified — combine with `absent()` or staleness alerting to catch a stalled updater (ADR-001) |
| `bridge_build_info{version,commit}` | Version/commit the running binary was built from |
| `bridge_uptime_seconds` | Process uptime |

Unlike `/health` and `/livez` (localhost-origin requests only), `/metrics` answers any source — it is a side-effect-free read, so external scrapers (e.g. over Tailscale) can poll it once `HEALTH_ADDR` is bound to a non-loopback interface:

```bash
curl -s http://<host>:9091/metrics
```

Verified self-updates are persisted in the `update_history` table (migration v27), so the last-success timestamp survives restarts.

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
| `/budget [amount]` | View or set the group budget (one-time alerts are pushed to the topic at 80% and 100% usage; changing the budget re-arms them) |
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
| `/cwd <path>` | Set group working directory (also registers the group); `/cwd` is readable by all allowed users |
| `/permission [mode]` | Read the permission mode; changing it requires admin access |
| `/config [setting] [value]` | Read configuration; changing `permission_mode`, tool restrictions, or limits requires admin access |
| `/budget [amount]` | Read the budget; changing it requires admin access |
| `/update [do]` | Check for or apply a self-update |
| `/adduser <id> [role]` | Add an allowed user |
| `/removeuser <id>` | Remove a user |
| `/users` | List allowed users |
| `/usage [user_id]` | Show per-user cost usage |

### Authorization matrix

| Actor | Chat gate | User gate | Capabilities |
|-------|-----------|-----------|--------------|
| Unknown Telegram user | Must match `ALLOWED_CHAT_ID` | Rejected | No response; update is silently dropped |
| Allowed user (`role=user`) | Must match `ALLOWED_CHAT_ID` | `allowed_users` row | Messages, sessions, topic controls, user commands, and read-only forms of `/cwd`, `/permission`, `/config`, and `/budget` |
| Database admin (`role=admin`) | Must match `ALLOWED_CHAT_ID` | `allowed_users` row | Everything an allowed user can do, plus admin-only writes and `/update`, `/adduser`, `/removeuser`, `/users`, and `/usage` |
| `ADMIN_USER_ID` | Must match `ALLOWED_CHAT_ID` | Environment-configured bootstrap identity | All administrator capabilities; it is re-established as an admin on startup |

`ADMIN_USER_ID` and database administrator status are checked for every
privileged command. In particular, an allowed non-admin user cannot change the
group permission mode through either `/permission <mode>` or
`/config permission_mode <mode>`. The documented `bypassPermissions` default
therefore remains unchanged unless an administrator explicitly changes it.

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

The proxy runs as a stateless container. It only needs the bot token and a writable path for its offset file (`OFFSET_FILE_PATH`, default `/data/offset.json`).

```yaml
# Example docker-compose snippet
services:
  proxy:
    image: telegram-claude-bridge:<version>
    environment:
      BOT_TOKEN: "<your-telegram-bot-token>"
      PROXY_LISTEN_ADDR: ":8080"
      OFFSET_FILE_PATH: "/data/offset.json"
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

---

Part of [jedarden.com](https://jedarden.com)

*This GitHub repo is a read-only mirror of git.ardenone.com/jedarden/telegram-claude-bridge — issues and PRs are welcome here either way.*
