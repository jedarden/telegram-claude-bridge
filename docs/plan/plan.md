# Telegram-Claude Bridge: Implementation Plan

## Overview

A two-component system that bridges Telegram group conversations to headless Claude Code CLI sessions. A lightweight proxy on ardenone-cluster isolates the Telegram bot token. A bridge script on Hetzner EX44 handles all routing, session management, media processing, and Claude Code orchestration. Telegram forum topics provide per-conversation granularity.

```
Telegram API  <--token-->  Proxy Pod (ardenone-cluster)  <--Tailscale-->  Bridge (EX44)  -->  Claude Code CLI
```

---

## Component 1: Proxy (ardenone-cluster)

### Purpose

Hold the Telegram bot token and act as a dumb authenticated pipe. No routing, no state, no interpretation of message content.

### Technology

- **Language:** Go (small binary, low memory, easy to containerize) or Python (consistency with bridge)
- **Runtime:** Single-container Deployment in its own namespace on ardenone-cluster
- **Networking:** Tailscale sidecar or host networking for Tailscale access; no public ingress

### Telegram Communication

The proxy long-polls Telegram using `getUpdates` with:
- `timeout=30` (long-poll interval)
- `offset` tracking (last `update_id + 1`)
- `allowed_updates` filter to reduce noise (only `message`, `edited_message`, `callback_query`, `my_chat_member`, and forum topic service messages)

Long-polling chosen over webhooks because:
- No public IP or HTTPS endpoint needed
- Auto-recovers from transient network failures
- Simpler deployment (no TLS cert management)

### Proxy HTTP API

Exposed only over Tailscale. No authentication on the proxy API itself — Tailscale ACLs restrict access to the EX44 node.

#### Inbound (Telegram → Bridge)

```
GET /updates?timeout=30
```

Returns a JSON array of pending Telegram updates, stripped of any auth context. The proxy translates each Telegram Update object into a normalized envelope:

```json
{
  "update_id": 123456789,
  "type": "message",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user_id": 789,
  "message_id": 1001,
  "timestamp": 1712169600,
  "content": {
    "text": "refactor the error handling",
    "entities": [...]
  },
  "media": [],
  "reply_to_message_id": null,
  "service": null
}
```

For media messages, the proxy downloads the file from Telegram (using the token) and either:
- **Option A:** Streams the file bytes in the response (for small files <5MB)
- **Option B:** Saves to a shared volume or returns a proxy-local URL the bridge can fetch

```
GET /file/<file_id>
```

Returns the raw file bytes. The bridge never constructs a Telegram file URL (which would require the token).

#### Outbound (Bridge → Telegram)

```
POST /send
{
  "chat_id": -1001234567890,
  "thread_id": 42,
  "text": "Here's the refactored code...",
  "parse_mode": "HTML",
  "reply_to_message_id": 1001
}

POST /send_photo
POST /send_document
POST /send_video
POST /send_audio
```

Each accepts a JSON body with chat routing fields. Media endpoints accept multipart form data. The proxy injects the bot token and forwards to the Telegram Bot API. Returns the Telegram API response (including `message_id` for subsequent edits).

```
POST /edit
{
  "chat_id": -1001234567890,
  "message_id": 2001,
  "text": "Updated streaming response...",
  "parse_mode": "HTML"
}
```

For progressive streaming updates via `editMessageText`.

#### Topic Management (Bridge → Telegram)

```
POST /create_topic
{ "chat_id": ..., "name": "...", "icon_color": 7322096 }

POST /close_topic
{ "chat_id": ..., "thread_id": ... }

POST /reopen_topic
{ "chat_id": ..., "thread_id": ... }

POST /edit_topic
{ "chat_id": ..., "thread_id": ..., "name": "...", "icon_color": ... }
```

Thin wrappers around `createForumTopic`, `closeForumTopic`, `reopenForumTopic`, `editForumTopic`.

#### Callback Queries

```
POST /answer_callback
{ "callback_query_id": "...", "text": "Approved" }
```

For responding to inline keyboard button presses.

### Token Storage

Token fetched from OpenBao at pod startup, held only in memory. Never written to disk, never in the pod spec or environment variable literals in manifests.

OpenBao path: `secret/data/telegram-claude-bridge/bot-token`

Fallback: Kubernetes Secret object referenced via `secretKeyRef` in the Deployment env, with the Secret managed by sealed-secrets.

### Deployment

- Namespace: `telegram-bridge` on ardenone-cluster
- Manifests in `declarative-config` repo under `k8s/ardenone-cluster/telegram-bridge/`
- ArgoCD Application for GitOps sync
- Resource limits: 64Mi memory, 100m CPU (this is a tiny workload)
- Health check: `GET /health` returns 200 if Telegram polling is active
- Single replica (no HA needed — bridge handles reconnection)

---

## Component 2: Bridge (Hetzner EX44)

### Purpose

All intelligence lives here: routing, session management, media processing, Claude Code orchestration, state persistence, and response formatting.

### Technology

- **Language:** Python 3.11+ (async, best ecosystem for Claude Agent SDK and media processing)
- **Framework:** `asyncio` with `aiohttp` for HTTP client to proxy
- **Claude integration:** `claude-agent-sdk` (Python) for native async streaming, type-safe sessions, custom permission callbacks
- **Media processing:** `openai-whisper` for audio transcription, `ffmpeg` for video frame/audio extraction
- **State:** SQLite via `aiosqlite`
- **Runtime:** systemd unit on EX44

### Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      Bridge (EX44)                          │
│                                                             │
│  ┌──────────┐  ┌──────────────┐  ┌───────────────────────┐ │
│  │  Poller   │→│   Router     │→│   Session Manager      │ │
│  │          │  │              │  │                        │ │
│  │ GET      │  │ (chat_id,   │  │ Claude Agent SDK       │ │
│  │ /updates │  │  thread_id) │  │ --resume session_id    │ │
│  │          │  │  → handler  │  │ --cwd project_dir      │ │
│  └──────────┘  └──────────────┘  └───────────────────────┘ │
│                       │                      │              │
│                       ▼                      ▼              │
│              ┌──────────────┐      ┌──────────────────┐     │
│              │  Command     │      │  Response         │     │
│              │  Handler     │      │  Formatter        │     │
│              │              │      │                   │     │
│              │  /status     │      │  Markdown → HTML  │     │
│              │  /cwd        │      │  Chunk splitting  │     │
│              │  /new        │      │  Stream editing   │     │
│              │  /close      │      │  Media responses  │     │
│              └──────────────┘      └──────────────────┘     │
│                                            │                │
│  ┌──────────────┐  ┌──────────────┐        ▼                │
│  │  Media       │  │  State       │  ┌──────────────┐       │
│  │  Processor   │  │  (SQLite)    │  │  Sender      │       │
│  │              │  │              │  │              │       │
│  │  Whisper     │  │  sessions    │  │ POST /send   │       │
│  │  ffmpeg      │  │  groups      │  │ POST /edit   │       │
│  │  image resize│  │  users       │  │ POST /send_* │       │
│  └──────────────┘  └──────────────┘  └──────────────┘       │
└─────────────────────────────────────────────────────────────┘
```

### Module Breakdown

#### 1. Poller

Polls the proxy's `/updates` endpoint in a loop. Maintains its own long-poll timeout matching the proxy's Telegram poll. On connection failure, retries with exponential backoff (1s, 2s, 4s, 8s, max 30s). Deserializes update envelopes and dispatches to the Router.

```python
async def poll_loop(proxy_url: str, router: Router):
    backoff = 1
    while True:
        try:
            updates = await fetch_updates(proxy_url, timeout=35)
            backoff = 1
            for update in updates:
                await router.route(update)
        except ConnectionError:
            await asyncio.sleep(backoff)
            backoff = min(backoff * 2, 30)
```

#### 2. Router

Looks up `(chat_id, thread_id)` in the SQLite routing table:

- **General topic (thread_id == 1 or None):** dispatch to Command Handler
- **Known topic:** dispatch to Session Manager with existing session
- **Unknown topic:** create new session via Session Manager, register in routing table
- **Callback queries:** dispatch to the session that sent the inline keyboard
- **Service messages** (`forum_topic_closed`, `forum_topic_reopened`, `forum_topic_created`): dispatch to Topic Lifecycle handler

#### 3. Session Manager

Manages Claude Code sessions via the Claude Agent SDK.

Each active session is a `ClaudeSDKClient` instance with:
- `resume=session_id` (from SQLite)
- `cwd=project_dir` (from group config)
- `permission_mode="plan"` (default — ask before destructive actions)
- `max_budget_usd` (configurable per group)
- `include_partial_messages=True` (for streaming)

**Session lifecycle:**
- **Create:** On first message to a new topic. Generate session ID, store in SQLite, instantiate SDK client.
- **Resume:** On subsequent messages. Load session ID from SQLite, instantiate SDK client with `resume=session_id`.
- **Destroy:** On topic close/delete or `/close` command. Remove from SQLite. Claude Code session persists on disk but is no longer routed to.

**Concurrency:** One active Claude session per topic. Messages arriving while a session is processing are queued per-topic and batched into the next prompt (prevents race conditions and preserves message ordering).

**Timeouts:** 5-minute default per prompt. Configurable via `/timeout` command. On timeout, send error message to topic and leave session intact for retry.

```python
class SessionManager:
    def __init__(self, state: StateDB, sender: Sender):
        self.active: dict[tuple[int, int], asyncio.Task] = {}
        self.queues: dict[tuple[int, int], asyncio.Queue] = {}

    async def handle(self, update: Update, session_info: SessionInfo):
        key = (update.chat_id, update.thread_id)
        if key in self.active:
            await self.queues[key].put(update)
            return
        self.active[key] = asyncio.create_task(
            self._run_session(update, session_info)
        )
```

#### 4. Media Processor

Handles non-text message types before they reach Claude.

**Images:**
- Download from proxy via `GET /file/<file_id>`
- Resize to ~800px on the long edge (save tokens, ~1334 tokens per 1000x1000)
- Save to `/tmp/telegram-bridge/<chat_id>/<message_id>.jpg`
- Pass to Claude via SDK file attachment or `--add-file`

**Voice/Audio:**
- Download from proxy
- Transcribe via Whisper (`whisper --model turbo --output_format txt`)
- Prepend transcription to the text prompt: `[Voice message transcription]: <text>`
- Retain original audio path for reference

**Video:**
- Download from proxy
- Extract keyframes: `ffmpeg -i video.mp4 -vf "fps=0.5,scale=800:-1" frame_%04d.jpg`
- Extract audio track: `ffmpeg -i video.mp4 -vn -acodec pcm_s16le audio.wav`
- Transcribe audio via Whisper
- Combine: attach keyframes as images + prepend audio transcription as text

**Documents:**
- Download from proxy
- If Claude-compatible (text, code, PDF, images, notebooks): pass directly via file attachment
- If incompatible (Office docs, archives): attempt conversion or notify user of limitation

**Cleanup:** Temp files cleaned up after session processes the message. Configurable retention for debugging.

#### 5. Command Handler

Processes bot commands sent in the General topic.

| Command | Action |
|---|---|
| `/status` | List active sessions in this group with last activity time |
| `/sessions` | List all sessions across all groups |
| `/cwd [path]` | View or set the default working directory for this group |
| `/new [name]` | Create a new topic and Claude session |
| `/close [topic]` | Close a topic and optionally end its Claude session |
| `/model [name]` | View or set the default model for this group |
| `/timeout [seconds]` | View or set the prompt timeout for this group |
| `/budget [usd]` | View or set the max budget per session for this group |
| `/help` | Show available commands |
| `/ping` | Health check — responds with latency to proxy and Claude |

Commands in non-General topics are passed through to Claude as regular messages (so Claude can interpret `/` prefixed text naturally).

#### 6. Response Formatter

Converts Claude's output to Telegram-compatible format.

**Markdown → HTML conversion:**
- Claude outputs markdown; Telegram needs HTML (chosen over MarkdownV2 for reliability)
- Convert: `**bold**` → `<b>`, `*italic*` → `<i>`, `` `code` `` → `<code>`, code fences → `<pre><code class="language-X">`, `[text](url)` → `<a href="url">text</a>`
- Escape `<`, `>`, `&` in plain text
- Strip unsupported markdown features gracefully

**Message chunking:**
- Telegram limit: 4096 characters per message
- Split at natural boundaries in priority order: paragraph breaks, code block boundaries, sentence endings
- Never split inside a code block — if a code block exceeds 4096 chars, send it as a document attachment instead
- First chunk is a reply to the user's message; subsequent chunks are standalone in the same topic

**Streaming (progressive updates):**
- Send initial "Thinking..." placeholder message
- Edit the message with new content as Claude streams (debounce: max 1 edit/second to stay under Telegram rate limits)
- On completion, send final formatted message (replacing the streamed version if formatting differs)
- If response exceeds 4096 chars during streaming, stop editing and send new messages for overflow

#### 7. Sender

Thin async HTTP client that posts to the proxy's outbound endpoints. Handles:
- Rate limiting: respect Telegram's 1 msg/sec per chat, 20 msg/min per group, ~30 global
- Retry on 429 with `retry_after` from Telegram
- Retry on proxy connection failure with exponential backoff
- Response tracking: store `message_id` of sent messages for subsequent edits

#### 8. State (SQLite)

```sql
-- Which groups are registered and their default settings
CREATE TABLE groups (
    chat_id       INTEGER PRIMARY KEY,
    name          TEXT,
    cwd           TEXT NOT NULL,
    default_model TEXT DEFAULT 'claude-sonnet-4-6',
    max_budget    REAL DEFAULT 5.0,
    timeout_sec   INTEGER DEFAULT 300,
    created_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Maps each topic to a Claude Code session
CREATE TABLE sessions (
    chat_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    session_id    TEXT NOT NULL,
    cwd           TEXT NOT NULL,
    model         TEXT,
    status        TEXT NOT NULL DEFAULT 'active',  -- active, closed, error
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    last_active   TEXT NOT NULL DEFAULT (datetime('now')),
    message_count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (chat_id, thread_id),
    FOREIGN KEY (chat_id) REFERENCES groups(chat_id)
);

-- Authorized Telegram user IDs
CREATE TABLE allowed_users (
    user_id       INTEGER PRIMARY KEY,
    role          TEXT NOT NULL DEFAULT 'user',  -- admin, user
    added_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Tracks sent message IDs for edit-in-place streaming
CREATE TABLE sent_messages (
    chat_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    message_id    INTEGER NOT NULL,
    purpose       TEXT NOT NULL,  -- streaming, response, command
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    PRIMARY KEY (chat_id, thread_id, message_id)
);
```

### Security

#### User Authorization

- `allowed_users` table is the allowlist. Only messages from authorized `from_user_id` values are processed.
- Admin role can use `/cwd`, `/model`, `/budget`, `/timeout`, and manage group settings.
- User role can send messages to topics and use `/status`, `/help`, `/new`, `/close`.
- Unknown users are silently ignored (no error response to prevent enumeration).

#### Input Handling

- User messages are passed to Claude via SDK (not shell), eliminating shell injection.
- No user-controlled input is interpolated into shell commands.
- File paths from Telegram are never used directly — files are saved with generated names in a sandboxed temp directory.

#### Claude Code Permissions

- Default `permission_mode="plan"` — Claude proposes changes before executing.
- Configurable per group. Options: `plan` (default), `acceptEdits` (auto-approve file edits), `dontAsk` (full autonomy for trusted contexts).
- `allowedTools` and `disallowedTools` configurable per group to restrict what Claude can do.

### Topic Lifecycle Management

#### Automatic Registration

When the bridge receives a message for an unknown `(chat_id, thread_id)`:
1. Check if `chat_id` is in the `groups` table. If not, ignore (group must be registered first via `/cwd` in General topic).
2. Create a new session with a generated session ID.
3. Insert into `sessions` table with the group's default `cwd` and `model`.
4. Send an initial context message to Claude including the topic name as task context.

#### Status Tracking via Icon Colors

The bridge updates topic icon colors to reflect session status:

| Status | Color | Hex |
|---|---|---|
| Active (processing) | Light blue | `0x6FB9F0` |
| Complete (closed) | Green | `0x8EEE98` |
| Blocked/waiting | Yellow | `0xFFD67E` |
| Error | Red/orange | `0xFB6F5F` |
| Review needed | Pink | `0xFF93B2` |
| Research/exploration | Purple | `0xCB86DB` |

Updated via `POST /edit_topic` to the proxy when session status changes.

#### Pinned Metadata

On session creation, the bridge pins a message in the topic:

```
Session: abc123
Project: ~/telegram-claude-bridge
Model: claude-sonnet-4-6
Started: 2026-04-03 18:30 UTC
```

Updated when settings change. Provides at-a-glance context.

#### Close/Reopen

- Topic closed in Telegram → bridge marks session as `closed` in SQLite, optionally frees SDK resources.
- Topic reopened → bridge marks session as `active`, resumes via `--resume session_id`.
- `/close` command → bridge closes the topic via proxy (`POST /close_topic`) and marks session closed.

---

## Phase 1: Minimal Viable Bridge

Goal: Send a text message in a Telegram topic, get a Claude response back.

### 1.1 — Telegram Bot Setup
- Create bot via BotFather
- Store token in OpenBao on ardenone-cluster
- Create a supergroup with forum mode enabled
- Grant bot admin with `can_manage_topics`

### 1.2 — Proxy MVP
- Single Python file (FastAPI or aiohttp)
- Long-poll Telegram, expose `/updates`, `/send`, `/edit`, `/health`
- Token from env var (OpenBao injection at pod start)
- Containerize, deploy to ardenone-cluster via ArgoCD
- Verify: curl from EX44 over Tailscale returns updates

### 1.3 — Bridge MVP
- Poller → Router → Session Manager → Sender pipeline
- SQLite state with `groups` and `sessions` tables
- General topic: `/cwd` command to register a group
- Non-General topics: create session on first message, `--resume` on subsequent
- Text-only (no media processing)
- No streaming — wait for full response, send as single message (chunked if >4096)
- Systemd unit on EX44
- Verify: send message in topic, receive Claude response

### 1.4 — Basic Commands
- `/status`, `/help`, `/cwd`, `/sessions` in General topic
- `/close` to end a session
- User allowlist (single admin user initially)

**Deliverable:** Working text-in, text-out bridge with topic-based conversation isolation.

---

## Phase 2: Streaming and Formatting

### 2.1 — Progressive Streaming
- Use Claude Agent SDK `include_partial_messages=True`
- Send placeholder message, edit in-place as text streams
- Debounce edits to 1/second
- Handle overflow (>4096 chars) by sending new messages

### 2.2 — Response Formatting
- Markdown → HTML converter
- Code-block-aware message chunking
- Oversized code blocks sent as document attachments

### 2.3 — Inline Keyboards for Tool Approval
- When Claude requests permission (plan mode), send an inline keyboard with Approve/Deny buttons
- Map `callback_query` back to the correct session
- Timeout auto-deny after configurable period

**Deliverable:** Streamed responses with proper formatting and interactive tool approval.

---

## Phase 3: Media Processing

### 3.1 — Image Support
- Download photos from proxy
- Resize to ~800px
- Pass to Claude as file attachments
- Send image responses (screenshots, generated diagrams) back via `/send_photo`

### 3.2 — Voice/Audio Support
- Download voice messages and audio files from proxy
- Transcribe via Whisper (turbo model)
- Inject transcription into prompt
- Consider: send transcription back to user for verification before prompting Claude

### 3.3 — Document Support
- Download documents from proxy
- Route to Claude based on file type (text/code/PDF/image → direct, others → notify limitation)
- Send file responses back via `/send_document`

### 3.4 — Video Support (stretch)
- Extract keyframes via ffmpeg
- Extract and transcribe audio
- Combine into multi-modal prompt

**Deliverable:** Full multi-modal input support — images, voice, documents, and optionally video.

---

## Phase 4: Topic Lifecycle and Operations

### 4.1 — Topic Auto-Creation
- `/new [name]` command creates a Telegram topic and registers a session
- Claude can suggest creating a new topic (bridge parses structured output or uses a tool)

### 4.2 — Status Colors
- Update topic icon color based on session state (active/complete/error/blocked)
- Color updated automatically on session events

### 4.3 — Pinned Metadata
- Pin session info message on topic creation
- Update on setting changes

### 4.4 — Session Cleanup
- Configurable TTL for inactive sessions (e.g., 7 days)
- Stale sessions marked inactive, topics optionally closed
- `/sessions` command shows age and activity

### 4.5 — Monitoring
- Health check endpoint on bridge (systemd watchdog or HTTP)
- Logging with structured JSON (no secrets in logs)
- Proxy health: `GET /health` checks Telegram polling is active
- Bridge health: proxy reachable + SQLite writable + at least one session resumable
- Reconnection notifications in General topic when bridge recovers from downtime

**Deliverable:** Self-managing topic lifecycle with operational visibility.

---

## Phase 5: Multi-User and Advanced Features

### 5.1 — Multi-User Support
- Multiple entries in `allowed_users` with role-based permissions
- Per-user session attribution (track who sent what)
- Admin vs user command separation

### 5.2 — Cross-Topic Context
- `/context <topic>` command to pull summary from another session
- Shared context snippets via pinned messages

### 5.3 — Per-Group Configuration
- Different models, budgets, timeouts, permission modes per group
- Stored in `groups` table, managed via General topic commands

### 5.4 — Notification Controls
- Configurable verbosity per topic (full streaming vs. final-only)
- Quiet mode for long-running tasks (only notify on completion or error)

**Deliverable:** Production-ready multi-user bridge with fine-grained configuration.

---

## Deployment Summary

| Component | Where | How | Config Source |
|---|---|---|---|
| Proxy | ardenone-cluster, `telegram-bridge` namespace | Deployment via ArgoCD | `declarative-config` repo |
| Bridge | Hetzner EX44 | systemd unit | Local config file + SQLite |
| Bot token | OpenBao on ardenone-cluster | Fetched at proxy startup | `secret/data/telegram-claude-bridge/bot-token` |
| Manifests | `declarative-config` repo | GitOps via ArgoCD | `k8s/ardenone-cluster/telegram-bridge/` |
| Bridge code | `telegram-claude-bridge` repo | git pull + systemd restart | This repo |

---

## Open Decisions

| Decision | Options | Leaning | Notes |
|---|---|---|---|
| Proxy language | Go vs Python | Python | Consistency with bridge, simpler for a thin proxy |
| Claude integration | Agent SDK vs CLI subprocess | Agent SDK | Native async streaming, type-safe sessions, lower overhead |
| SDK fallback to CLI | Yes vs SDK-only | Yes | CLI subprocess as fallback if SDK fails — pattern from existing implementations |
| Message format proxy→bridge | Normalized envelope vs raw Telegram JSON | Normalized | Keeps bridge decoupled from Telegram API specifics, proxy handles schema changes |
| Media transfer proxy→bridge | Inline in update vs separate `/file` endpoint | Separate `/file` | Keeps update payloads small, avoids large base64 blobs |
| Whisper model | turbo vs base vs small | turbo | Best accuracy-to-speed ratio |
| Private chat topics | Support vs groups only | Groups only (initially) | Bot API 9.4 supports private chat topics, but groups are the primary use case |
| Response format | HTML vs MarkdownV2 | HTML | MarkdownV2 escaping is unreliable — universal consensus from existing implementations |

---

## Risk Register

| Risk | Impact | Mitigation |
|---|---|---|
| Proxy pod restart loses polling offset | Missed messages | Store last `update_id` in a ConfigMap or PV; Telegram retains unacked updates for 24h |
| Claude session corruption | Lost conversation context | Sessions are stateless from bridge perspective — create new session, close old topic |
| Whisper transcription errors | Garbled prompts | Send transcription to user for verification before prompting Claude (opt-in) |
| Telegram rate limiting (429) | Delayed responses | Respect `retry_after`, debounce edits, queue sends per chat |
| Long-running Claude prompts | Telegram typing indicator expires (5s) | Re-send `sendChatAction` every 4 seconds during processing |
| SQLite corruption on EX44 | Lost routing state | WAL mode, regular backups, sessions recoverable from Claude Code's own session storage |
| Proxy unreachable (Tailscale disruption) | Bridge can't send/receive | Exponential backoff retry, notification on recovery, messages queued locally |
| Token leak via Claude Code output | Bot token exposed | Impossible by design — token never reaches EX44 |
