# Routing and Session Management

Date: 2026-04-03

## Principle

The proxy on ardenone-cluster is strictly for token isolation — it holds auth and forwards messages, nothing else. All routing, session management, and state live on the bridge script on EX44.

## Routing Model

The bridge maintains a mapping from Telegram's hierarchy to Claude Code sessions:

```
(group_id, topic_id)  →  (cwd, session_id)
```

| Telegram concept | Maps to | Managed by |
|---|---|---|
| `group_id` | `--cwd` (project directory) | Bridge routing table |
| `topic_id` / `message_thread_id` | `--resume` session ID | Bridge routing table |
| General topic (id=1) | Control plane (bot commands) | Bridge command handler |
| Non-General topics | Individual Claude Code conversations | Bridge session manager |

## Telegram Hierarchy → Claude Code Sessions

```
Group: "telegram-claude-bridge"        →  --cwd ~/telegram-claude-bridge
├── General topic                      →  control plane (no Claude session)
│   /new, /status, /cwd, /sessions
├── Topic: "design proxy API"          →  --resume session-abc123
├── Topic: "implement media handling"  →  --resume session-def456
└── Topic: "fix markdown rendering"    →  --resume session-ghi789

Group: "ardenone-cluster"              →  --cwd ~/ardenone-cluster
├── General topic                      →  control plane
├── Topic: "debug openbao sealing"     →  --resume session-jkl012
└── Topic: "update cert-manager"       →  --resume session-mno345
```

## What the Proxy Sees

The proxy passes through message payloads with Telegram metadata intact:

```json
{
  "chat_id": 123456,
  "message_thread_id": 42,
  "from_user_id": 789,
  "text": "refactor the error handling in main.py",
  "media": []
}
```

It does not interpret, route, or store any of this. It's a dumb pipe with auth.

## What the Bridge Does

1. Receives message payload from proxy
2. Looks up `(chat_id, message_thread_id)` in its routing table
3. If General topic → handle as bot command
4. If known topic → dispatch to existing Claude Code session (`--resume`)
5. If unknown topic → create new session, register in routing table
6. Sends Claude Code response back through proxy with the same `chat_id` and `message_thread_id`

## State Persistence

The routing table lives on EX44 as a SQLite database (or JSON file for simplicity). Schema:

```sql
CREATE TABLE sessions (
    chat_id       INTEGER NOT NULL,
    thread_id     INTEGER NOT NULL,
    session_id    TEXT NOT NULL,
    cwd           TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    last_active   TEXT NOT NULL,
    PRIMARY KEY (chat_id, thread_id)
);

CREATE TABLE groups (
    chat_id       INTEGER PRIMARY KEY,
    cwd           TEXT NOT NULL,
    name          TEXT
);
```

The bridge is the only writer. If it restarts, state is recovered from SQLite. Claude Code sessions persist independently (managed by Claude Code itself), so `--resume` picks up where it left off.

## General Topic Commands

Commands sent in the General topic of any group:

| Command | Effect |
|---|---|
| `/status` | List active sessions in this group |
| `/cwd [path]` | View or change the default working directory for this group |
| `/sessions` | List all sessions across all groups |
| `/close` | Close the Claude session for a specific topic |

## Topic Lifecycle

- **New topic created** → bridge registers it on first message, creates a new Claude Code session
- **Topic closed in Telegram** → bridge receives `forum_topic_closed` service message, can optionally end the Claude session
- **Topic reopened** → bridge resumes the existing session if still valid
- **Topic deleted** → bridge removes the routing entry

## Open Questions

- Auto-create topics from the bridge side? (e.g., Claude suggests splitting work into a new topic)
- Topic icon colors for status? (active = green, stalled = yellow, complete = grey)
- Cross-topic context sharing? (reference another session's output)
- Session TTL / cleanup for stale sessions?
