# Telegram-Claude Bridge: Implementation Plan

## Overview

A two-component system where the bridge acts as a **dispatcher** — routing Telegram conversations to headless Claude Code CLI instances and coordinating their results back to the thread. The bridge is not a pipe; it is an orchestrator of Claude processes.

Each Telegram forum topic maps to an **orchestrator** Claude session: a persistent, resumed `claude` interactive process that understands the user's intent, delegates parallelisable sub-tasks to **worker** instances, and synthesises responses. Workers are short-lived, fresh-session Claude processes spawned by the orchestrator and managed by the bridge. The bridge routes worker results back to both the Telegram thread and the orchestrator's context.

A lightweight proxy on ardenone-cluster isolates the Telegram bot token. The bridge on Hetzner EX44 handles all dispatch logic, session management, worker lifecycle, media processing, and state persistence.

```
Telegram API  <--token-->  Proxy (ardenone-cluster)  <--Tailscale-->  Bridge (EX44) [dispatcher]
                                                                              │
                                                             ┌────────────────┴────────────────┐
                                                             │                                 │
                                                      Orchestrator Claude              Worker Claude(s)
                                                      (resumed session)                (fresh sessions)
                                                      understands + delegates          execute sub-tasks
                                                      synthesises + responds           report results back
```

---

## Component 1: Proxy (ardenone-cluster)

### Purpose

Hold the Telegram bot token and act as a dumb authenticated pipe. No routing, no state, no interpretation of message content.

### Technology

- **Language:** Go (same as bridge — shared module, single build system)
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

The bridge is the dispatcher and message bus. It routes Telegram messages to the correct orchestrator session, intercepts synthetic tool calls from the orchestrator stream (`spawn_worker`, `update_progress`) to manage worker lifecycles, routes worker results back to the thread and to the orchestrator's context, and handles all state, media, and formatting. No Claude logic runs in the bridge itself — it only manages processes and routes data.

### Technology

- **Language:** Go
- **HTTP:** stdlib `net/http` for client to proxy
- **Claude integration:** Interactive Claude CLI via PTY, one tmux pane per active topic. Each session runs `claude --resume <session_id>` in a persistent tmux pane within the `telegram-bridge` session. Prompts are injected via PTY write (bracketed paste mode); responses are extracted by reading PTY output and scraping the VT100 screen via a pyte emulator. Billing uses subscription (not API credits) because `claude` runs in a real TTY context, not headless `-p` mode.
- **Media processing:** `whisper` CLI for audio transcription, `ffmpeg` CLI for video frame/audio extraction (both invoked as subprocesses)
- **State:** SQLite via `modernc.org/sqlite` (pure Go, no CGo)
- **Runtime:** single static binary, systemd unit on EX44

### Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                          Bridge (EX44) — Dispatcher                  │
│                                                                      │
│  ┌──────────┐  ┌──────────────┐  ┌─────────────────────────────────┐ │
│  │  Poller   │→│   Router     │→│         Session Manager          │ │
│  │ /updates  │  │ chat/thread  │  │                                 │ │
│  └──────────┘  └──────────────┘  │  TopicWorker (per topic)        │ │
│                                  │  ├─ PTY goroutine                │ │
│                                  │  │    tmux pane (1 per topic)    │ │
│                                  │  │    claude --resume session_id │ │
│                                  │  │    --append-system-prompt     │ │
│                                  │  │    PTY write (bracketed paste)│ │
│                                  │  │    pyte screen scrape         │ │
│                                  │  │                               │ │
│                                  │  │  Intercepts synthetic tools:  │ │
│                                  │  │  ├─ spawn_worker → WorkerPool │ │
│                                  │  │  └─ update_progress → Sender  │ │
│                                  │  │                               │ │
│                                  │  └─ WorkerPool                   │ │
│                                  │       Worker pane (per worker)   │ │
│                                  │       claude (fresh session)     │ │
│                                  │       result → thread + context  │ │
│                                  └─────────────────────────────────┘ │
│                                                                      │
│  ┌──────────────┐  ┌──────────────────────────────────────────────┐  │
│  │  Command     │  │  State (SQLite)                              │  │
│  │  Handler     │  │  groups · sessions · workers · cost_events   │  │
│  │  /new /close │  └──────────────────────────────────────────────┘  │
│  │  /model etc  │                                                    │
│  └──────────────┘  ┌──────────────┐  ┌──────────────┐               │
│                    │  Media       │  │  Sender       │               │
│                    │  Processor   │  │  POST /send   │               │
│                    │  Whisper     │  │  POST /edit   │               │
│                    │  ffmpeg      │  │  POST /send_* │               │
│                    └──────────────┘  └──────────────┘               │
└──────────────────────────────────────────────────────────────────────┘
```

### Module Breakdown

#### 1. Poller

Polls the proxy's `/updates` endpoint in a loop. Maintains its own long-poll timeout matching the proxy's Telegram poll. On connection failure, retries with exponential backoff (1s, 2s, 4s, 8s, max 30s). Deserializes update envelopes and dispatches to the Router.

```go
func pollLoop(ctx context.Context, proxyURL string, router *Router) {
    backoff := time.Second
    for {
        updates, err := fetchUpdates(ctx, proxyURL, 35*time.Second)
        if err != nil {
            time.Sleep(backoff)
            backoff = min(backoff*2, 30*time.Second)
            continue
        }
        backoff = time.Second
        for _, update := range updates {
            router.Route(ctx, update)
        }
    }
}
```

#### 2. Router

Looks up `(chat_id, thread_id)` in the SQLite routing table:

- **General topic (thread_id == 1 or None):** dispatch to Command Handler
- **Known topic:** dispatch to Session Manager with existing session
- **Unknown topic:** create new session via Session Manager, register in routing table
- **Callback queries:** dispatch to the session that sent the inline keyboard
- **Service messages** (`forum_topic_closed`, `forum_topic_reopened`, `forum_topic_created`): dispatch to Topic Lifecycle handler

#### 3. Session Manager

The Session Manager implements the dispatcher pattern. It manages two categories of Claude CLI process per topic:

**Orchestrator** — the on-demand, resumed session for a topic. Spawns a tmux pane within the `telegram-bridge` session only when a message arrives. After the response is complete, a short idle timer (`pane_idle_ttl`, default 5 min) keeps the pane warm for follow-up messages. When the timer expires the pane is killed; the `session_id` persists in SQLite so the next message resumes the conversation via `--resume`. Understands user intent, decides what to delegate, posts progress updates, and synthesises the final response.

**Worker** — a short-lived, fresh-session process dispatched by the orchestrator via the `spawn_worker` synthetic tool. Also runs in its own tmux pane, torn down immediately on completion. Executes one concrete sub-task (research, code generation, analysis, etc.). Multiple workers can run in parallel across panes. Results are posted to the thread immediately and injected back into the orchestrator's context on the next turn.

**Pane lifecycle (dynamic load/unload):**

```
Message arrives for topic
        │
        ▼
Pane alive for topic?
   ├── Yes (warm)  → inject prompt via PTY write immediately
   └── No  (cold) → spawn pane:
                     tmux new-window -t telegram-bridge -n "topic-<thread_id>"
                       "claude --resume <session_id>   # or no --resume on first ever message
                               --dangerously-skip-permissions
                               --model <topic_model>
                               --cwd <project_dir>
                               --append-system-prompt '<dispatcher context>'"
                     wait for startup handshake (~3s: DA query + trust dismiss + ready)
                     inject prompt via bracketed paste

Response scraped
        │
        ▼
Reset idle timer (pane_idle_ttl = 5 min)
        │
        ▼  (timer fires with no new message)
tmux kill-pane  →  pane_state = idle  (session_id kept in SQLite)
```

Cold-start latency is ~3–4s (claude startup + PTY handshake). Warm injection is near-instant. The `pane_idle_ttl` is configurable per group — set higher for active development sessions, lower for rarely-used topics.

**Worker invocation (transient tmux pane, spawned on spawn_worker tool call):**

```
# Fresh pane per worker, torn down on completion
tmux new-window -t telegram-bridge -n "worker-<worker_id>" \
  "claude --dangerously-skip-permissions \
          --model <worker_model_or_default> \
          --cwd <project_dir> \
          --disallowed-tools spawn_worker"

# Prompt injected via bracketed paste; result extracted via pyte scrape
# Pane closed after result captured
```

Workers do not get `--resume` (fresh session each time) and cannot call `spawn_worker` (depth limit = 1).

**Key CLI flags and PTY mechanics:**
- Interactive mode (no `-p`) — claude runs in REPL mode, billing via subscription
- `--resume <session_id>` — orchestrator only; restores full conversation context across pane restarts
- `--append-system-prompt` — injects dispatcher context into orchestrator (see Phase 9)
- `--disallowed-tools spawn_worker` — workers cannot recursively spawn
- `--model` — per-session or per-worker model override
- `--allowed-tools` / `--disallowed-tools` — tool restrictions per group
- Bracketed paste mode (`\x1b[200~` … `\x1b[201~`) — injects multi-line prompts without triggering premature submission
- pyte VT100 screen emulator — extracts response text from PTY byte stream; watches for response-start sentinel (`●`) and end-of-response separators (`────`)
- `PRE_RESP_TIMEOUT = 120s` — wait for first `●` after prompt injection
- `IDLE_TIMEOUT = 30s` — session considered done after 30s silence past last response chunk

**Synthetic tools intercepted by the bridge** (not real Anthropic tools — described via system prompt):

| Tool | Bridge action |
|---|---|
| `spawn_worker(prompt, model?)` | Insert workers row, spawn goroutine, return `{worker_id, status: "dispatched"}` immediately |
| `update_progress(message)` | POST message to thread, return `{ok: true}` immediately |

The scanner loop in `invokeClaudeAPI` intercepts `tool_use` blocks with these names before they reach the `result` event, handles them synchronously (DB write + goroutine spawn), and writes the `tool_result` back to the subprocess stdin so the orchestrator continues without stalling.

**PTY output parsing (replacing NDJSON stream):**
- Response start detected by `●` (U+25CF) sentinel in PTY byte stream
- Tool use detected by watching for known tool-start patterns in the VT100 screen buffer
- Response end detected by `────` separator bars and subsequent idle gap
- Full response extracted by pyte `_SCREEN_ROWS=2000` screen emulator after end-of-response detected
- Session ID captured from the initial pane startup output (claude prints it on attach/resume)
- Cost/usage not available in interactive mode (subscription billing — no per-call token counts)

**Worker goroutine on completion:**
1. Update `workers` row: `status=done`, `result=...`, `finished_at=now`
2. Post to thread: `⚙️ Worker [N] done: <truncated result>`
3. Append to `SessionManager.pendingWorkerResults[(chatID,threadID)]`

**Next orchestrator prompt construction:**
```
[Worker results available]
Worker 1 (<model>): <result>
Worker 2 (<model>): error — <error>

[User message]
<user text>
```

The `pendingWorkerResults` map is drained atomically at the start of each `processBatch`.

**Orchestrator session lifecycle:**
- **Create:** First message to a new topic — launch pane (no `--resume`), capture `session_id` from startup output, store in SQLite, start idle timer
- **Warm resume:** Message arrives while pane is alive — inject prompt directly, reset idle timer
- **Cold resume:** Message arrives after pane was culled — relaunch with `--resume <session_id>`, wait for startup handshake, inject prompt, start idle timer
- **Idle cull:** `pane_idle_ttl` expires — `tmux kill-pane`, set `pane_state=idle`; `session_id` retained in SQLite
- **Close:** `/close`, topic deletion, or natural language close intent — `tmux kill-pane`, mark `status=closed` in SQLite; session permanently ended

**Concurrency model:**
- One orchestrator goroutine per topic (non-blocking worker, messages buffer in `pending` slice)
- N worker goroutines per topic (parallel, bounded by `groups.max_workers`)
- Workers post to thread and inject context independently of the orchestrator

**Timeout:**
- Orchestrator: `groups.timeout_sec` (default 1800s), `0` = no deadline
- Workers: inherit group default; can be overridden per `spawn_worker` call
- Bridge-side progress ticker fires every `groups.progress_interval_sec` (default 120s) if no output sent

```go
// PTY read loop with timeout
select {
case resp := <-ptyResponseCh:
    // response extracted from pyte screen
case <-time.After(timeout):
    // kill the pane, notify user in Telegram
    tmuxKillPane(paneID)
}
```

**File/image attachments:** Files downloaded from the proxy are saved to a temp directory and referenced in the prompt text. Claude Code's `Read` tool can process images, PDFs, code files, and notebooks from the filesystem. The prompt includes the file path:

```
[User sent an image: /tmp/telegram-bridge/12345/photo.jpg]
Please analyze this screenshot.
```

Claude Code will use its `Read` tool to open the file.

#### 4. Media Processor

Handles non-text message types before they reach Claude.

**Images:**
- Download from proxy via `GET /file/<file_id>`
- Resize to ~800px on the long edge (save tokens, ~1334 tokens per 1000x1000)
- Save to `/tmp/telegram-bridge/<chat_id>/<message_id>.jpg`
- Reference in prompt text so Claude Code's `Read` tool can open it

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
- If Claude-compatible (text, code, PDF, images, notebooks): save to temp dir, reference path in prompt
- If incompatible (Office docs, archives): attempt conversion or notify user of limitation

**Cleanup:** Temp files cleaned up after session processes the message. Configurable retention for debugging.

#### 5. Command Handler

Processes bot commands. Commands are recognized in both the General topic and non-General topics, but the available commands differ by context.

**General topic commands (group-level):**

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
| `/version` | Show bridge and proxy versions (semver + commit hash) |
| `/update` | (admin) Trigger self-update check and apply if available |

**Topic commands (session-level):**

| Command | Action |
|---|---|
| `/model [name]` | View or set the model for this topic. Overrides group default. |
| `/haiku` | Shortcut: set this topic to `claude-haiku-4-5` |
| `/sonnet` | Shortcut: set this topic to `claude-sonnet-4-6` |
| `/opus` | Shortcut: set this topic to `claude-opus-4-6` |
| `/info` | Show session info: model, cwd, session_id, cost, message count |

All other text in non-General topics is passed through to Claude as the prompt — but the bridge scans it for model-change intent first (see below).

**Natural language model switching:**

Before dispatching a message to Claude, the bridge checks for model-change phrases in the message text. If detected, the bridge updates the topic's model, confirms the change with a short reply, and then forwards the rest of the message (if any) to Claude using the new model.

Detection is done by normalizing the message to lowercase and checking for known phrase substrings:

| Phrase | Action |
|---|---|
| "use opus", "switch to opus", "let's use opus", "need opus for this" | Set model to `claude-opus-4-6` |
| "use sonnet", "switch to sonnet", "back to sonnet" | Set model to `claude-sonnet-4-6` |
| "use haiku", "switch to haiku", "quick mode" | Set model to `claude-haiku-4-5` |
| "use a smarter model", "this needs more power", "think harder" | Escalate one tier (haiku→sonnet, sonnet→opus) |
| "use a faster model", "keep it simple", "quick answer" | De-escalate one tier (opus→sonnet, sonnet→haiku) |

Implementation: `strings.ToLower()` then `strings.Contains()` against a table of known phrases. No regex. The phrases are deliberately specific — a verb of intent ("use", "switch to", "need") paired with a model name or tier keyword — so the detector doesn't misfire on messages that happen to contain "opus" in a different context.

When a model change is detected:
1. Update `sessions.model` in SQLite
2. Reply with a short confirmation: `Model → claude-opus-4-6`
3. Update the pinned metadata message
4. If the message contains additional text beyond the model-change phrase, strip the intent phrase and forward the remainder to Claude with the new model. If the message is *only* a model-change request, no Claude invocation is made.

**Model resolution order:** The bridge resolves which model to use for each CLI invocation in this order (first non-null wins):

1. `sessions.model` — topic-level override (set via `/model`, `/haiku`, `/sonnet`, `/opus`, or natural language in the topic)
2. `groups.default_model` — group-level default (set via `/model` in General topic)
3. Hardcoded fallback: `claude-sonnet-4-6`

When a topic-level model is set, the bridge updates the pinned metadata message to reflect the current model.

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
    chat_id         INTEGER PRIMARY KEY,
    name            TEXT,
    cwd             TEXT NOT NULL,
    default_model   TEXT DEFAULT 'claude-sonnet-4-6',
    max_budget      REAL DEFAULT 5.0,
    timeout_sec     INTEGER DEFAULT 300,
    pane_idle_ttl   INTEGER DEFAULT 300,  -- seconds to keep pane alive after last response (0 = kill immediately)
    created_at      TEXT NOT NULL DEFAULT (datetime('now'))
);

-- Maps each topic to a Claude Code session
CREATE TABLE sessions (
    chat_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    session_id    TEXT NOT NULL,
    cwd           TEXT NOT NULL,
    model         TEXT,
    status        TEXT NOT NULL DEFAULT 'active',  -- active, closed, error
    pane_state    TEXT NOT NULL DEFAULT 'idle',    -- idle (no pane), warm (pane alive), processing
    tmux_pane_id  TEXT,                            -- tmux pane target (e.g. "telegram-bridge:topic-42"), null when idle
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

- User messages are passed to Claude via stdin pipe to the subprocess, never interpolated into shell commands or CLI arguments.
- File paths from Telegram are never used directly — files are saved with generated names in a sandboxed temp directory.

#### Claude Code Permissions

- Default `--permission-mode plan` — Claude proposes changes before executing.
- Configurable per group. Options: `plan` (default), `acceptEdits` (auto-approve file edits), `dontAsk` (full autonomy for trusted contexts).
- `--allowedTools` and `--disallowedTools` configurable per group to restrict what Claude can do.

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
Model: claude-sonnet-4-6 (group default)
Started: 2026-04-03 18:30 UTC
```

Updated when settings change (including model override via `/model`, `/haiku`, `/sonnet`, `/opus`). When a topic-level model override is active, the pinned message shows:

```
Model: claude-opus-4-6 (topic override)
```

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
- Go binary (`cmd/proxy/main.go`), stdlib `net/http`
- Long-poll Telegram, expose `/updates`, `/send`, `/edit`, `/health`
- Token from env var (OpenBao injection at pod start)
- `FROM scratch` container, deploy to ardenone-cluster via ArgoCD
- Verify: curl from EX44 over Tailscale returns updates

### 1.3 — Bridge MVP
- Go binary (`cmd/bridge/main.go`)
- Poller → Router → Session Manager → Sender pipeline
- SQLite state with `groups` and `sessions` tables (`modernc.org/sqlite`)
- General topic: `/cwd` command to register a group
- Non-General topics: create tmux pane + session on first message, inject into existing pane on subsequent
- PTY manager component: launches/tracks tmux panes in the `telegram-bridge` session (one pane per active topic), injects prompts via bracketed paste, scrapes responses via pyte VT100 emulator
- Text-only (no media processing)
- No streaming initially — wait for full response from PTY, send as single message (chunked if >4096)
- Single static binary, systemd unit on EX44
- Verify: send message in topic, receive Claude response

### 1.4 — Basic Commands
- `/status`, `/help`, `/cwd`, `/sessions` in General topic
- `/close` to end a session
- User allowlist (single admin user initially)

**Deliverable:** Working text-in, text-out bridge with topic-based conversation isolation.

---

## Phase 2: Streaming and Formatting

### 2.1 — Progressive Streaming
- Poll pyte screen buffer while PTY output is arriving (after `●` sentinel, before end-of-response separator)
- Extract incrementally-growing response text from screen on each poll tick (~500ms)
- Send placeholder message, edit in-place as new text appears
- Debounce edits to 1/second
- Handle overflow (>4096 chars) by sending new messages

### 2.2 — Response Formatting
- Markdown → HTML converter
- Code-block-aware message chunking
- Oversized code blocks sent as document attachments

### 2.3 — Permission Handling
- Default `--permission-mode plan` means Claude proposes before executing
- The interactive CLI shows a permission prompt in the pane — the bridge detects this pattern in PTY output and sends inline keyboards to Telegram
- Callback query response writes approval/denial via PTY write to the pane
- Alternative: use `--permission-mode acceptEdits` or `--permission-mode dontAsk` for trusted groups to skip interactive approval

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

## Phase 6: TUI Dashboard

### Purpose

A terminal UI that provides real-time visibility into the bridge's operation. Runs in a tmux session on EX44, showing messages in flight, active sessions, command processing, and system health at a glance.

### Technology

- **Language:** Go (same module as bridge)
- **TUI framework:** `charmbracelet/bubbletea` + `charmbracelet/lipgloss` for layout and styling
- **Data source:** Bridge exposes an internal event stream (Unix socket or localhost HTTP SSE) that the TUI consumes. The TUI is a read-only observer — it never modifies bridge state.
- **Binary:** `cmd/dashboard/main.go`, separate binary from the bridge

### Layout

```
┌─ Telegram-Claude Bridge Dashboard ──────────────────────────────────────┐
│                                                                          │
│  ┌─ Active Sessions ─────────────────────┐  ┌─ System Health ─────────┐ │
│  │ #trading-bot   opus   ▶ processing    │  │ Proxy: ✓ 210ms          │ │
│  │ #refactor-api  sonnet   idle 2m       │  │ Bridge: ✓ uptime 4h12m  │ │
│  │ #fix-auth      haiku    idle 15m      │  │ DB: ✓ 6ms               │ │
│  │ #new-feature   sonnet ▶ streaming     │  │ Claude: ✓ installed     │ │
│  │                                       │  │ Telegram: ✓ polling     │ │
│  └───────────────────────────────────────┘  └─────────────────────────┘ │
│                                                                          │
│  ┌─ Messages In Flight ─────────────────────────────────────────────────┐│
│  │ 09:31:02 → #new-feature  @jed "refactor the error handling"         ││
│  │ 09:31:03 ← #new-feature  claude streaming... (1.2s, 340 tokens)     ││
│  │ 09:30:45 ← #trading-bot  claude complete (4.8s, $0.032)             ││
│  │ 09:30:12 → #trading-bot  @jed "add stop-loss logic"                 ││
│  └─────────────────────────────────────────────────────────────────────┘│
│                                                                          │
│  ┌─ Command Log ────────────────────────────────────────────────────────┐│
│  │ 09:31:05  /model opus          #new-feature  @jed  → OK             ││
│  │ 09:30:00  /new trading-bot     General       @jed  → created        ││
│  │ 09:28:15  /cwd /home/coding    General       @jed  → set            ││
│  └─────────────────────────────────────────────────────────────────────┘│
│                                                                          │
│  ┌─ Cost Tracker ───────────────────────────────────────────────────────┐│
│  │ Session totals today: $0.48  │  This hour: $0.12  │  Active: 4      ││
│  └─────────────────────────────────────────────────────────────────────┘│
└──────────────────────────────────────────────────────────────────────────┘
```

### Panels

#### Active Sessions
- Lists all sessions with status (processing/streaming/idle/error), model, topic name
- Color-coded: green=idle, blue=processing, yellow=streaming, red=error
- Shows idle duration for inactive sessions
- Sorted by last activity (most recent first)

#### System Health
- Real-time health checks mirroring the bridge's internal health checker
- Proxy latency, bridge uptime, DB response time, Claude CLI availability
- Telegram polling status and last update ID

#### Messages In Flight
- Scrolling log of inbound (→) and outbound (←) messages
- Shows topic name, user, message preview (truncated to terminal width)
- For outbound: streaming progress, token count, cost, duration
- Ring buffer of last ~100 messages

#### Command Log
- Scrolling log of `/commands` processed by the bridge
- Shows command, topic, user, result (OK/error)
- Ring buffer of last ~50 commands

#### Cost Tracker
- Aggregated cost from session `total_cost_usd` fields
- Today's total, current hour, and active session count
- Per-session breakdown available by selecting a session in the Active Sessions panel

### Event Stream

The bridge publishes events to a local Unix socket at `/tmp/telegram-bridge-events.sock` (or configurable path). The event protocol is NDJSON with event types:

```json
{"type":"message_in","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","user":"@jed","preview":"add stop-loss logic"}
{"type":"message_out","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","status":"streaming","tokens":340,"elapsed_ms":1200}
{"type":"message_out","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#trading-bot","status":"complete","tokens":1200,"cost_usd":0.032,"elapsed_ms":4800}
{"type":"command","timestamp":"...","command":"/model","args":"opus","topic":"#new-feature","user":"@jed","result":"ok"}
{"type":"session_update","timestamp":"...","chat_id":...,"thread_id":...,"topic":"#new-feature","status":"active","model":"claude-opus-4-6"}
{"type":"health","timestamp":"...","proxy_ok":true,"proxy_latency_ms":210,"db_ok":true,"db_latency_ms":6}
```

The bridge emits events regardless of whether the TUI is connected. If no listener is present, writes are silently dropped (non-blocking).

### Running

```bash
# In a tmux session
tmux new -s dashboard
./bin/dashboard

# Or with a custom socket path
./bin/dashboard --socket /tmp/telegram-bridge-events.sock
```

The dashboard is optional — the bridge functions identically without it. It's a pure observer for operational visibility.

**Deliverable:** Real-time terminal dashboard for monitoring bridge activity, session state, and costs.

## Phase 7: Foundational Infrastructure for Dispatcher

### Purpose

Prerequisite pieces that the Phase 9 dispatcher architecture depends on. The bridge must support long-running invocations, cancellation, and non-blocking operation before the orchestrator/worker model can work reliably. Phases 7 and 9 are designed together — Phase 7 is the infrastructure layer, Phase 9 is the dispatch layer built on top of it.

**Status of 7.1–7.2:** Implemented (under `claude -p` model). These need revisiting under the interactive PTY model — cancellation now means writing `\x03` (Ctrl-C) to the pane's PTY rather than sending SIGTERM to a subprocess, and timeout means abandoning the PTY read loop and killing the pane.

### 7.1 — In-flight cancellation

**Problem:** A stuck or long-running invocation holds the topic PTY read loop with no escape.

**Solution:** Track the active pane ID in `topicWorker`. A `/cancel` command writes `\x03` (Ctrl-C) to the pane's PTY to interrupt the running Claude turn.

```go
type topicWorker struct {
    ch          chan sessionMsg
    cancel      context.CancelFunc
    activePaneID string   // guarded by activeMu
    activeMu    sync.Mutex
}
```

- Set `activePaneID` when pane is launched; clear it after PTY read loop exits.
- `/cancel [thread_id]` command: acquire `activeMu`, write `\x03` to the pane PTY via `tmux send-keys -t <paneID> C-c`.
- Edit the "Thinking…" placeholder to "⚠️ Cancelled" on cancellation.
- Topic color → red on cancel.

### 7.2 — Extended timeouts + per-topic timeout override

**Problem:** `defaultSessionTimeout = 300` (5 min) is too low for agentic tasks. Per-topic override is missing.

**Changes:**
- Raise `defaultSessionTimeout` from `300` → `1800` (30 min).
- Add `timeout_sec` column to `sessions` table (mirrors `groups.timeout_sec`). Per-topic override takes precedence over group default.
- Add `/timeout [N]` as a topic-level command (currently only group-level via `/config`).
- `N=0` disables the timeout entirely (no `context.WithTimeout`).
- Add `/timeout` to `/help` output.

**Timeout resolution order** (first non-zero wins):
1. `sessions.timeout_sec` — topic-level override
2. `groups.timeout_sec` — group default
3. `defaultSessionTimeout` — hardcoded fallback (1800s)

### 7.3 — Tool-use progress notifications

**Problem:** During long agentic runs the user sees only "Thinking…" with no visibility into what Claude is doing.

**Solution:** Parse `tool_use` and `tool_result` events from the stream-json NDJSON and send interim status messages to the topic.

Stream-json emits `stream_event` lines containing `message_delta` and tool events. Parse:
```json
{"type":"stream_event","event":{"type":"content_block_start","content_block":{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}}}
```

Behavior:
- On `tool_use` start: edit the placeholder → `⚙️ Running Bash: \`go test ./...\`` (truncated to 100 chars)
- On `tool_result`: edit → `✓ Bash done (exit 0)` or `✗ Bash failed (exit 1)`
- On final result: replace with full streamed response as now
- Debounce tool notifications at 500ms (faster than text streaming since these are discrete events)
- Long tool commands truncated in the notification but full output still goes to Claude

### 7.4 — Sub-task orchestrator (multi-instance fan-out/fan-in)

**Core feature:** Allow Claude or the user to dispatch multiple parallel interactive Claude instances (each in its own tmux pane) from a single topic, with results funneled back to that topic.

#### SQLite schema addition

```sql
CREATE TABLE subtasks (
    id           TEXT PRIMARY KEY,          -- uuid
    chat_id      INTEGER NOT NULL,
    thread_id    INTEGER NOT NULL,
    parent_msg   INTEGER,                   -- message_id that triggered the subtask
    prompt       TEXT NOT NULL,
    session_id   TEXT,                      -- claude session_id for this subtask
    status       TEXT NOT NULL DEFAULT 'running',  -- running, done, failed, cancelled
    result       TEXT,
    error        TEXT,
    started_at   TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at  TEXT,
    FOREIGN KEY (chat_id, thread_id) REFERENCES sessions(chat_id, thread_id)
);
```

#### SubtaskOrchestrator

```go
type SubtaskOrchestrator struct {
    db      *DB
    sender  *Sender
    ptyMgr  *PTYManager  // manages tmux panes for subtask workers
}

type SubtaskRequest struct {
    ChatID   int64
    ThreadID int64
    MsgID    int64
    Prompts  []string  // one per parallel instance
    Group    *Group
    Session  *Session  // parent session; each subtask gets its own pane
}

func (o *SubtaskOrchestrator) Run(ctx context.Context, req SubtaskRequest)
```

Behavior:
1. For each prompt, insert a `subtasks` row with `status=running`.
2. Spawn a goroutine per prompt; each goroutine launches a fresh tmux pane running `claude` (no `--resume`), injects the prompt via bracketed paste, and scrapes the response via pyte.
3. As each goroutine completes, update the `subtasks` row and post the result to the originating topic.
4. Post results as they arrive (fan-in is non-blocking).
5. A final "All N sub-tasks complete" summary is sent when all goroutines exit.

#### Trigger mechanisms

**User-initiated via `/parallel`:**
```
/parallel
Research the three biggest failure modes in distributed SQLite replication.
---
List the top 5 Go libraries for distributed locking with pros/cons.
---
Summarize the WAL mode write amplification problem.
```
Delimiter `---` splits prompts. Up to 5 parallel instances (configurable per group via `/config max_subtasks N`).

**Claude-initiated via tool:**
The bridge exposes a synthetic tool `spawn_subtask` that Claude can call in its stream. When the bridge sees a `tool_use` block with `name="spawn_subtask"`, it:
1. Extracts `{"prompt": "...", "session_inherit": true/false}` from the tool input.
2. Inserts a `subtasks` row.
3. Spawns the sub-task goroutine.
4. Returns a synthetic `tool_result` to Claude's stdin: `{"subtask_id": "...", "status": "dispatched"}`.
5. The sub-task result is posted to the topic when complete.

This lets Claude self-decompose complex tasks into parallel workstreams without the user having to structure the prompt.

#### Resource limits

- Max concurrent subtasks per topic: 5 (configurable)
- Sub-tasks share the group's budget cap
- Sub-task timeout: inherits group `timeout_sec`
- A topic worker is not blocked while sub-tasks run — the worker goroutine exits `processBatch` after dispatching, and sub-task goroutines post back to the topic via `sender` directly

### 7.5 — Background shell job runner

**Problem:** Sometimes the user wants to fire a long-running shell process (deploy, test suite, build) and get notified when it completes without tying up a Claude session.

**`/bg <command>` command:**

```go
type BackgroundJob struct {
    ID        string
    ChatID    int64
    ThreadID  int64
    Command   string
    Cmd       *exec.Cmd
    Status    string // running, done, failed
    StartedAt time.Time
}
```

Behavior:
- `/bg go test ./...` — launches the command in a goroutine under the group's CWD.
- Sends a "⏳ Background job started: `go test ./...`" confirmation immediately.
- Streams stdout/stderr line-by-line back to the topic (same debounce as streaming, 1s).
- On completion: "✓ Job done (exit 0)" or "✗ Job failed (exit 1)" with last N lines of output.
- `/jobs` command lists running background jobs for this topic.
- `/kill <job_id>` sends SIGTERM to a running job.

Background jobs are stored in a `background_jobs` SQLite table for persistence across bridge restarts (partially — running jobs are re-marked as `interrupted` on startup).

```sql
CREATE TABLE background_jobs (
    id          TEXT PRIMARY KEY,
    chat_id     INTEGER NOT NULL,
    thread_id   INTEGER NOT NULL,
    command     TEXT NOT NULL,
    status      TEXT NOT NULL DEFAULT 'running',
    exit_code   INTEGER,
    started_at  TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at TEXT
);
```

### New commands summary

| Command | Context | Action |
|---|---|---|
| `/cancel [thread_id]` | General or topic | Cancel the active Claude invocation for a topic |
| `/timeout [N]` | Topic | View or set timeout for this topic (0 = no timeout) |
| `/parallel` | Topic | Dispatch multi-prompt parallel sub-tasks |
| `/bg <command>` | Topic | Run a shell command in background, stream output back |
| `/jobs` | Topic | List running background jobs for this topic |
| `/kill <job_id>` | Topic | Send SIGTERM to a background job |

### Updated architecture diagram

```
┌─────────────────────────────────────────────────────────────────────┐
│                          Bridge (EX44)                               │
│                                                                      │
│  ┌──────────┐  ┌──────────────┐  ┌──────────────────────────────┐   │
│  │  Poller   │→│   Router     │→│      Session Manager          │   │
│  └──────────┘  └──────────────┘  │                              │   │
│                                  │  TopicWorker (per topic)     │   │
│                                  │  ├─ PTY pane (current)       │   │
│                                  │  └─ SubtaskOrchestrator      │   │
│                                  │       ├─ pane per prompt     │   │
│                                  │       └─ fan-in → sender     │   │
│                                  └──────────────────────────────┘   │
│                                                                      │
│  ┌──────────────────────┐    ┌──────────────────────────────────┐   │
│  │  BackgroundJobRunner  │    │  State (SQLite)                  │   │
│  │  /bg <cmd>           │    │  groups, sessions, subtasks,     │   │
│  │  stdout → topic      │    │  background_jobs, sent_messages  │   │
│  └──────────────────────┘    └──────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────┘
```

**Deliverable:** Bridge supports long-running agentic tasks via cancellation, higher timeouts, tool-use visibility, parallel sub-task dispatch, and background shell jobs — all routing output back to the originating topic.

---

## Phase 9: Dispatcher Architecture

### Purpose

The bridge is primarily a **dispatcher**, not a pipe. Each topic's Claude session is an **orchestrator** — it understands the user's request, delegates parallelisable sub-tasks to **worker** Claude instances, and responds to the user with progress updates and final synthesis. The bridge is the message bus that routes user messages to the orchestrator, worker spawn requests from the orchestrator to new subprocesses, and worker results back to both the orchestrator context and the Telegram thread.

### Target architecture

```
User message
    │
    ▼
Bridge (dispatcher)
    │  ──── dispatches to ────►  Orchestrator Claude (-p --resume session_id)
    │                                │  understands request
    │                                │  posts "Starting X workers..." to thread
    │                                │  calls spawn_worker tool N times
    │                                ▼
    │         ┌──────────────────────────────────────┐
    │         │  Worker pool (bridge-managed)              │
    │         │  Worker 1: tmux pane, claude (fresh)       │
    │         │  Worker 2: tmux pane, claude (fresh)       │
    │         │  Worker N: tmux pane, claude (fresh)       │
    │         └──────────────────────────────────────┘
    │                                │
    │         worker completes       │
    │         ├─── post result to Telegram thread
    │         └─── inject result as tool_result into orchestrator stdin
    │                                │
    │                         Orchestrator synthesises
    │                         posts final response to thread
    ▼
Thread receives: progress updates + individual worker results + final synthesis
```

### Orchestrator vs worker

| | Orchestrator | Worker |
|---|---|---|
| Session | Persisted (`--resume session_id`) | Fresh per invocation (no `--resume`) |
| Purpose | Understand, delegate, synthesise, respond to user | Execute one concrete sub-task |
| Model | Configured per topic (default sonnet) | Inherits from orchestrator, or overridden per spawn |
| Result routing | Posts synthesis to thread | Result posted to thread + injected back to orchestrator |
| Spawning | Calls `spawn_worker` tool | Cannot spawn further workers (depth limit 1) |

### 9.1 — Orchestrator system prompt injection

The bridge appends a system prompt to every orchestrator invocation (`--append-system-prompt`) that makes the dispatcher pattern explicit to Claude:

```
You are running as an orchestrator in a Telegram Claude Bridge.
You have access to two bridge-provided tools:

spawn_worker(prompt, model?)
  Dispatches a new headless Claude instance to execute `prompt` independently.
  Returns {worker_id}. The worker result will be delivered to you as a tool_result
  and also posted directly to the Telegram thread.

update_progress(message)
  Posts a status message to the Telegram thread immediately.
  Use this to keep the user informed during long-running work.
  Returns {ok: true}.

Guidelines:
- Use spawn_worker to parallelise independent sub-tasks (research, analysis, code review, etc.)
- Use update_progress when more than ~30 seconds have passed without user-visible output.
- Synthesise worker results into a final response rather than forwarding raw outputs.
- You do not need to spawn workers for simple, fast requests.
```

This is injected via `--append-system-prompt` when `groups.dispatcher_mode = true` (default on, opt-out per group).

### 9.2 — `spawn_worker` synthetic tool

The bridge intercepts `tool_use` blocks with `name="spawn_worker"` from the orchestrator's stream before forwarding them to Claude as `tool_result`. No actual Anthropic tool definition needed — the system prompt description is sufficient for Claude to call it.

**Bridge handling:**
1. Parse `tool_use` block: `{name: "spawn_worker", input: {prompt, model?}}`
2. Insert row into `workers` table (`status=running`)
3. Spawn goroutine: launch fresh tmux pane running `claude` (no `--resume`) with the given prompt, inherit CWD, use specified model or orchestrator default
4. Return `tool_result` immediately: `{worker_id: "uuid", status: "dispatched"}` — orchestrator continues without waiting
5. Worker goroutine on completion:
   - Updates `workers` row to `status=done`
   - Posts worker result to Telegram thread: `⚙️ Worker [N] complete: <truncated result>`
   - Writes worker result into a pending-injection buffer for the orchestrator
6. Next time the user sends a follow-up (or orchestrator posts an update_progress that triggers re-entry), the worker results are injected as additional context prepended to the new prompt

**Schema:**
```sql
CREATE TABLE workers (
    id            TEXT PRIMARY KEY,
    chat_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    parent_msg    INTEGER,
    prompt        TEXT NOT NULL,
    session_id    TEXT,
    model         TEXT,
    status        TEXT NOT NULL DEFAULT 'running',
    result        TEXT,
    error         TEXT,
    started_at    TEXT NOT NULL DEFAULT (datetime('now')),
    finished_at   TEXT
);

ALTER TABLE groups ADD COLUMN dispatcher_mode INTEGER NOT NULL DEFAULT 1;
ALTER TABLE groups ADD COLUMN max_workers INTEGER NOT NULL DEFAULT 5;
```

### 9.3 — `update_progress` synthetic tool

The bridge intercepts `tool_use` blocks with `name="update_progress"` and posts the message immediately to the Telegram thread without invoking any subprocess.

**Bridge handling:**
1. Parse: `{name: "update_progress", input: {message}}`
2. Send `message` to the Telegram thread (new message, not an edit of the placeholder)
3. Return `tool_result`: `{ok: true}` — orchestrator continues immediately

This costs ~0ms and lets the orchestrator drive its own update cadence rather than relying on bridge-side timers.

### 9.4 — Bridge-side progress ticker

Independent of `update_progress` calls, the bridge posts a heartbeat to the thread when a long invocation is running with no output:

- If no Telegram message has been sent for `progress_interval` seconds (default 120s), post: `⏳ Still working… (Xm elapsed)`
- Reset timer on every `flushEdit` or `update_progress` call
- Configurable per group via `groups.progress_interval_sec` (0 = disabled)

This is a safety net for invocations where Claude never calls `update_progress`.

**Schema:**
```sql
ALTER TABLE groups ADD COLUMN progress_interval_sec INTEGER NOT NULL DEFAULT 120;
```

### 9.5 — Worker result injection on next prompt

When the user sends a follow-up message while workers are still running (or just completed), the bridge prepends the worker results to the new prompt before sending to the orchestrator:

```
[Worker results from previous invocation]
Worker 1 (analyze X): <result>
Worker 2 (check Y): <result>

[User message]
<user message text>
```

Workers complete asynchronously — results accumulate in `pendingWorkerResults` map keyed by `(chatID, threadID)`. The next `processBatch` drains and prepends them. This closes the loop: the orchestrator always has access to worker outputs without needing to poll.

### 9.6 — Per-topic dispatcher opt-out

Some topics should remain in direct mode (no system prompt injection, no worker spawning). Add `/dispatch [on|off]` command to toggle `sessions.dispatcher_mode` per topic, overriding the group default.

**Deliverable:** Bridge is a true dispatcher. Orchestrator Claude instances coordinate worker instances. User sees real-time progress updates and individual worker results as they complete, followed by a synthesised final response.

---

## Self-Updating

Both components update themselves from the repo without manual intervention.

### Bridge (EX44)

The bridge binary includes a self-update mechanism:

1. **Check:** On a configurable interval (default: 5 minutes), the bridge calls `git -C <repo-path> fetch origin main --dry-run` to check for new commits.
2. **Pull:** If new commits exist, run `git -C <repo-path> pull origin main`.
3. **Build:** Run `go build -o <tmp-path> ./cmd/bridge/` in the repo directory.
4. **Swap:** Replace the running binary: rename the new binary over the current one.
5. **Restart:** The bridge exec's itself (or signals systemd to restart via `systemctl restart telegram-bridge`). Active Claude subprocesses are allowed to finish before shutdown (graceful drain with configurable timeout).
6. **Notify:** Post a message to each group's General topic: `Bridge updated to <commit-hash> — <commit-subject>`.
7. **Rollback:** If the new binary fails to start (systemd detects crash within `RestartSec`), systemd's `StartLimitBurst` prevents restart loops. The admin is notified via Telegram (the proxy is still running and can relay a hardcoded alert). Manual rollback: `git revert` + push triggers the next update cycle.

The update check can also be triggered manually via `/update` command (admin only).

**Systemd integration:**

```ini
[Service]
ExecStart=/home/coding/telegram-claude-bridge/bin/bridge
Restart=on-failure
RestartSec=5
StartLimitBurst=3
StartLimitIntervalSec=60
```

If the bridge crashes 3 times within 60 seconds after an update, systemd stops restarting. This prevents a bad build from looping.

### Proxy (ardenone-cluster)

The proxy updates via the existing GitOps pipeline:

1. CI builds a new container image on push to `main` (GitHub Actions, tagged with commit SHA)
2. Image pushed to container registry
3. Manifest in `declarative-config` references the new image tag (updated by CI or image automation)
4. ArgoCD syncs the updated manifest → rolling restart of the proxy pod

No self-update logic in the proxy binary — ArgoCD handles it. This is consistent with how all other workloads on ardenone-cluster deploy.

### Versioning

Both components follow semver (`MAJOR.MINOR.PATCH`). The version is embedded at build time via Go linker flags:

```go
// Set at build time
var (
    Version   = "dev"
    CommitSHA = "unknown"
    BuildDate = "unknown"
)
```

```bash
go build -ldflags "-X main.Version=0.3.1 -X main.CommitSHA=$(git rev-parse --short HEAD) -X main.BuildDate=$(date -u +%Y-%m-%dT%H:%M:%SZ)" ./cmd/bridge/
```

Version source of truth: git tags (`v0.1.0`, `v0.2.0`, etc.). The self-update build step reads the latest tag reachable from `main` via `git describe --tags --always` and injects it.

**`/version` command output:**

```
Bridge: v0.3.1 (abc1234) built 2026-04-03T18:30:00Z
Proxy:  v0.3.1 (def5678) uptime 4h12m
Contract: 1.0
```

The bridge queries the proxy's `/health` endpoint (which returns `contract_version` and can be extended with `version` and `commit`) to display the proxy version alongside its own.

**Version bump convention:**
- `PATCH` — bug fixes, dependency updates, minor behavior changes
- `MINOR` — new commands, new media types, new features
- `MAJOR` — breaking data contract changes (proxy ↔ bridge API), SQLite schema changes that aren't auto-migratable

### Update Safety

- **Bridge:** The build happens locally on EX44 using the same Go toolchain that built the original. No downloading pre-built binaries from external sources.
- **No auto-update on tag/release-only branches** — updates track `main` directly since this is a single-operator system.
- **Build failure is a no-op** — if `go build` fails, the bridge logs the error, notifies via Telegram, and continues running the current binary.
- **SQLite migrations:** If an update includes schema changes, the bridge runs migrations on startup before accepting messages. Migrations are idempotent and forward-only.

---

## Deployment Summary

| Component | Where | How | Config Source |
|---|---|---|---|
| Proxy | ardenone-cluster, `telegram-bridge` namespace | Deployment via ArgoCD, `FROM scratch` ~10MB image | `declarative-config` repo |
| Bridge | Hetzner EX44 | Single Go binary, systemd unit, self-updating from git | Local config file + SQLite |
| Bot token | OpenBao on ardenone-cluster | Fetched at proxy startup | `secret/data/telegram-claude-bridge/bot-token` |
| Manifests | `declarative-config` repo | GitOps via ArgoCD | `k8s/ardenone-cluster/telegram-bridge/` |
| Source | `telegram-claude-bridge` repo | Go monorepo: `cmd/proxy/`, `cmd/bridge/` | This repo |

---

## Open Decisions

| Decision | Options | Leaning | Notes |
|---|---|---|---|
| Proxy language | Go | **Decided: Go** | Same language as bridge, single binary, minimal container |
| Bridge language | Go | **Decided: Go** | See language evaluation below |
| Claude integration | Interactive CLI via PTY | **Decided: Interactive CLI** | `claude` in interactive mode (no `-p`), one persistent tmux pane per topic within the `telegram-bridge` session. Prompt injection via bracketed paste, response extraction via pyte VT100 emulator. Subscription billing (not API credits). `--resume` for session continuity on pane restart. |
| Message format proxy→bridge | Normalized envelope vs raw Telegram JSON | Normalized | Keeps bridge decoupled from Telegram API specifics, proxy handles schema changes |
| Media transfer proxy→bridge | Inline in update vs separate `/file` endpoint | Separate `/file` | Keeps update payloads small, avoids large base64 blobs |
| Whisper model | turbo vs base vs small | turbo | Best accuracy-to-speed ratio |
| Private chat topics | Support vs groups only | Groups only (initially) | Bot API 9.4 supports private chat topics, but groups are the primary use case |
| Response format | HTML vs MarkdownV2 | HTML | MarkdownV2 escaping is unreliable — universal consensus from existing implementations |
| Permission mode | plan vs acceptEdits vs dontAsk | plan (default) | Configurable per group. `plan` gives interactive approval via Telegram inline keyboards. |

---

## Language Evaluation

The bridge and proxy are fundamentally I/O-bound glue: HTTP client, subprocess management, NDJSON line parsing, and SQLite. The language needs strong async I/O, reliable subprocess streaming, and minimal operational overhead.

### Candidates

#### Go

**Strengths:**
- Goroutines map 1:1 to the concurrency model: one goroutine per topic polling Claude subprocess stdout, one for the proxy HTTP poll loop, one per pending send. No callback chains or async/await coloring.
- `os/exec` provides first-class subprocess management with `cmd.StdoutPipe()` returning an `io.Reader` — NDJSON line scanning is `bufio.NewScanner(pipe)` in a loop.
- `net/http` client and server in stdlib — no third-party HTTP library needed for either component.
- `database/sql` with `modernc.org/sqlite` (pure Go SQLite, no CGo) — single binary with no system dependencies.
- Compiles to a single static binary. Deploy to EX44 is `scp` + `systemctl restart`. Container for the proxy is a `FROM scratch` image at ~10MB.
- Memory footprint is minimal (~10-20MB for the bridge). Relevant on the proxy side where resource limits are 64Mi.
- `encoding/json` handles all serialization. `json.Decoder` with `Token()` for streaming JSON parsing.
- Error handling is explicit — no hidden exceptions from subprocess or HTTP failures.
- Whisper and ffmpeg are invoked as CLI subprocesses regardless of language, so no Python library advantage.

**Weaknesses:**
- No REPL for quick iteration during development.
- Verbose error handling boilerplate.
- JSON struct tags are slightly more ceremony than dynamic languages.

#### Python

**Strengths:**
- Fastest to prototype. `asyncio.create_subprocess_exec` for Claude CLI, `aiohttp` for HTTP, `aiosqlite` for SQLite.
- Whisper can be imported as a library (`import whisper`) rather than shelled out — lower latency for transcription.
- Largest ecosystem of Telegram bot libraries (`python-telegram-bot`, `aiogram`, `telethon`).
- Most existing implementations in the research survey are Python.

**Weaknesses:**
- Runtime dependency: Python 3.11+, pip, virtualenv. Must be maintained on EX44 alongside system Python.
- Async subprocess streaming in Python requires careful handling of `asyncio.StreamReader` — easy to deadlock on large stdout/stderr buffers.
- `asyncio` function coloring: every function in the call chain must be async. Mixing sync and async (e.g., SQLite without aiosqlite) blocks the event loop.
- Higher memory footprint (~50-100MB with dependencies loaded).
- Dependency management (pip, requirements.txt or pyproject.toml) is more fragile than a compiled binary.
- Containerizing Python is heavier (~200MB+ image with dependencies).

#### TypeScript (Node.js / Bun)

**Strengths:**
- Good async primitives (`child_process.spawn` with stream events, `fetch` for HTTP).
- Several surveyed implementations use TypeScript (the official Anthropic Telegram plugin runs on Bun).
- Strong typing with TypeScript catches contract mismatches at compile time.
- npm ecosystem has Telegram bot libraries (`telegraf`, `grammy`).

**Weaknesses:**
- Runtime dependency: Node.js or Bun must be installed and maintained on EX44.
- `child_process` subprocess streaming is callback-based, slightly less ergonomic than Go's `io.Reader`.
- `better-sqlite3` (native addon) requires build tools; `sql.js` (WASM) is slower.
- Heavier container images than Go.
- npm dependency tree adds supply chain surface area.

#### Rust

**Strengths:**
- Performance and memory safety. `tokio::process::Command` for async subprocess. Single binary like Go.
- Excellent error handling with `Result<T, E>`.

**Weaknesses:**
- Significantly more development time for what is I/O-bound glue code.
- Async Rust has a steeper learning curve (lifetimes + async = complexity).
- Compile times are slow for iteration speed.
- Overkill — the bridge is not compute-bound, and the safety guarantees don't add much over Go for this workload.

### Decision Matrix

| Criterion | Go | Python | TypeScript | Rust |
|---|---|---|---|---|
| Subprocess streaming | `os/exec` + `bufio.Scanner` — excellent | `asyncio.create_subprocess_exec` — good, deadlock risk | `child_process.spawn` — good, callback-based | `tokio::process` — excellent |
| HTTP client/server | stdlib `net/http` — no deps | `aiohttp` — third-party | `fetch` / `express` — third-party | `reqwest` / `axum` — third-party |
| SQLite | `modernc.org/sqlite` pure Go — no CGo | `aiosqlite` — third-party | `better-sqlite3` — native addon | `rusqlite` — C binding |
| JSON/NDJSON parsing | `encoding/json` stdlib | `json` stdlib | `JSON.parse` built-in | `serde_json` — third-party |
| Binary/deployment | Single static binary, `FROM scratch` | Virtualenv + pip + runtime | Node/Bun runtime + node_modules | Single static binary |
| Memory footprint | ~10-20MB | ~50-100MB | ~40-80MB | ~5-15MB |
| Container size | ~10MB | ~200MB+ | ~150MB+ | ~10MB |
| Development speed | Moderate | Fast | Moderate | Slow |
| Operational overhead | Minimal — just the binary | Runtime + deps to maintain | Runtime + deps to maintain | Minimal — just the binary |
| Concurrency model | Goroutines — natural fit | asyncio — function coloring | Event loop — natural fit | Tokio — steeper learning curve |
| Whisper/ffmpeg | Shell out (same as all others) | Can import as library | Shell out | Shell out |

### Recommendation: Go

The bridge is subprocess management + HTTP + SQLite + NDJSON parsing. Go's stdlib covers all of these without third-party dependencies. Single binary deployment eliminates runtime management on EX44. The proxy is the same workload at smaller scale, so both components share a language, build system, and deployment model.

Whisper is the one area where Python has an advantage (library import vs CLI). But Whisper-as-CLI (`whisper` command or `faster-whisper` CLI) is well-supported and avoids coupling the bridge to Python's runtime. The latency difference (subprocess spawn vs library call) is negligible compared to transcription time itself.

The bridge and proxy share a Go module (monorepo with `cmd/proxy/` and `cmd/bridge/`), build to two binaries, and deploy independently.

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
| PTY screen scraping fragility | Response extraction breaks on claude UI changes | Pin sentinel characters (`●`, `────`) and test on each claude update; fall back to full screen dump if partial extraction fails |
| tmux session missing on bridge restart | All warm panes lost | Expected and safe — dynamic model means panes are ephemeral; on next message the bridge does a cold resume via `--resume <session_id>`; no manual recovery needed |
| claude interactive mode UI changes | Bracketed paste or startup behavior changes break injection | Keep PTY integration logic isolated in PTYManager; sentinel + timing constants are the fragile surface — document and test them |
