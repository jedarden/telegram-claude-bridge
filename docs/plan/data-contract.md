# Proxy ↔ Bridge Data Contract

Version: 1.0  
Reviewed: 2026-09-27

## Transport

- **Protocol:** HTTP/1.1 over Tailscale
- **Base URL:** `http://telegram-proxy:8080` (Tailscale hostname)
- **JSON requests:** `application/json` for `/send`, `/edit`, and topic-management requests
- **JSON responses:** `application/json` for every successful JSON endpoint and every proxy-generated JSON error
- **Media uploads:** `multipart/form-data` with a boundary; the file is one named part and metadata is made up of text parts
- **File downloads:** `GET /file/{file_id}` returns raw bytes on success, not JSON
- **Auth:** None — Tailscale ACLs restrict access to the EX44 node only
- **Encoding:** UTF-8

## Conventions

- All timestamps are Unix epoch integers (matching Telegram API)
- `chat_id` is a signed 64-bit integer (supergroups are negative)
- `thread_id` is a positive integer for a named forum topic; the canonical v1 envelope omits it for General. Telegram's raw General-topic ID (`1`) is normalized to omission.
- `message_id` is a positive integer
- `user_id` is a positive 64-bit integer
- Empty optional fields are omitted (not sent as `null`)
- The proxy normalizes Telegram updates into the typed v1 envelope below; it does not expose raw Telegram field names to the bridge

### Thread routing

`thread_id` is the bridge-facing name for Telegram's
`message_thread_id`. For `/send`, `/send_chat_action`, and the media upload
endpoints, the proxy forwards it to Telegram to route the message into that
forum topic. Omit it for the General topic; named topics must carry their
positive thread ID. The proxy does not infer a topic from `chat_id`, and it
does not validate or rewrite outbound `thread_id` values. The raw Telegram
General ID (`1`) may be forwarded by older clients, but new clients should
use omission to match the canonical v1 envelope.

`/edit` identifies the target by `chat_id` and `message_id`; it has no
`thread_id` field. Topic-management endpoints use `thread_id` to identify an
existing topic in the supplied chat. The `reply_to_message_id` field is
forwarded unchanged and should refer to a message in the same chat/topic.

## Error behavior

```json
{
  "ok": false,
  "error_code": 400,
  "description": "Bad Request: message text is empty"
}
```

For errors produced by a JSON or multipart handler, the body is an
`ErrorResponse` with `ok: false`, `error_code`, and `description`. Telegram
errors preserve Telegram's `error_code`, `description`, and (for 429)
`retry_after`, but the HTTP status is mapped by the proxy. In particular,
Telegram 400/401/403/409 responses are returned as HTTP 502 with their
original error code in the JSON body; do not use the HTTP status alone to
identify the Telegram error.

The common HTTP mappings are:

| HTTP status / `error_code` | Meaning |
|---|---|
| 400 | Malformed JSON or multipart form, missing/invalid media `chat_id`, or missing file part |
| 404 | File ID/path is unavailable or expired (file endpoint only) |
| 413 | Media upload or file download exceeds the proxy limit |
| 429 | Telegram rate limit; `retry_after` is seconds when Telegram supplies it |
| 502 | Telegram API unreachable, invalid upstream response, or a Telegram error such as 400/401/403/409 (the body keeps Telegram's original `error_code`) |
| 503 | Proxy not connected to Telegram (polling not started) |
| 504 | Telegram API timeout |

HTTP 405 method errors are emitted by `net/http` as a plain-text response;
they are not `ErrorResponse` JSON. A missing route also uses the standard
`net/http` response. JSON/multipart validation errors and all mapped Telegram
errors use the JSON shape above.

---

## Endpoints

### GET /health

Health check. No parameters.

**Response 200:**
```json
{
  "ok": true,
  "polling": true,
  "last_update_id": 123456789,
  "uptime_seconds": 3600
}
```

| Field | Type | Description |
|---|---|---|
| `ok` | boolean | Proxy is operational |
| `polling` | boolean | Actively polling Telegram |
| `last_update_id` | integer \| null | Most recent update_id received |
| `uptime_seconds` | integer | Seconds since proxy started |

---

### GET /updates

Long-polls Telegram and returns pending updates as normalized envelopes.

**Query parameters:**

| Param | Type | Default | Description |
|---|---|---|---|
| `timeout` | integer | 30 | Long-poll timeout in seconds. Proxy adds 5s to its Telegram poll to ensure the Telegram response arrives first. |
| `ack` | integer | omitted | Cumulative acknowledgement: discard retained updates with `update_id` less than or equal to this value before returning the response. Omit on the first poll or when no update has been durably recorded. |

**Response 200:**
```json
{
  "ok": true,
  "updates": [ <Update> ... ]
}
```

The bridge should call this in a loop. The proxy tracks Telegram's upstream `offset` internally, but bridge-facing delivery is governed by an explicit cumulative acknowledgement. `GET /updates` is non-destructive: every retained update is returned on every call until the bridge sends `ack=<update_id>` on a later call. There is no implicit acknowledgement when the bridge fetches the next batch.

#### Acknowledgement and delivery semantics

The canonical v1 request sequence is:

```text
GET /updates?timeout=30
GET /updates?timeout=30&ack=<highest-durably-recorded-update-id>
```

The `ack` value is a cumulative high-water mark. A valid positive value acknowledges every retained update whose `update_id` is less than or equal to it; it is not an acknowledgement of only one item, and the proxy applies it to the whole retained buffer even if a replay made that buffer temporarily non-monotonic. The bridge must advance it only through the contiguous portion of the response it has durably recorded; it must not send the maximum ID from a partially handled batch. A missing, non-positive, or malformed value acknowledges nothing. The proxy acknowledges updates to Telegram when it receives them, then retains its normalized copy for this bridge-facing protocol.

#### Crash and replay behavior

- If the bridge crashes after receiving a response but before the next request carries the acknowledgement, the same updates are returned again.
- If the acknowledgement request or its response is lost, retrying without a newer acknowledgement is safe; the replay is expected and the bridge deduplicates by `update_id`.
- If the proxy restarts, its persisted `offset` and `unacked` buffer are reloaded from `OFFSET_FILE_PATH`, so unacknowledged updates remain eligible for replay. The pair is written as one fsynced temporary JSON file followed by an atomic rename; a restart sees the previous complete pair or the next complete pair.
- The retained buffer is capped at 10,000 updates by default. On overflow, the proxy drops and logs the oldest retained updates; those updates cannot be recovered because they were already acknowledged upstream.

#### Compatibility

This explicit-ack behavior is the v1 contract; the old implicit-next-poll description was stale documentation, not a second supported mode. A client that omits `ack` can make an initial request, but it does not progress the buffer and is not compatible with reliable ongoing consumption. An old bridge that expects implicit acknowledgement will receive the same retained updates repeatedly and may duplicate work.

An updated bridge can make requests to an old proxy because the old proxy ignores the new query parameter, but that mixed-version deployment retains the old destructive-delivery crash-loss behavior. An offset-only state file written by an older proxy remains readable by the new proxy, but it contains no retained replay buffer; updates already acknowledged upstream cannot be reconstructed. Do not downgrade a proxy while a new-format state file contains retained updates, because the old proxy ignores that field and may lose them.

#### Update envelope

Every Telegram update is normalized into this envelope. The proxy strips all auth context and resolves file references.

```json
{
  "update_id": 123456789,
  "type": "message",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user": {
    "id": 789,
    "first_name": "Jed",
    "username": "jedarden"
  },
  "message_id": 1001,
  "timestamp": 1712169600,
  "content": {
    "type": "text",
    "text": "refactor the error handling in main.py",
    "entities": [
      { "type": "bot_command", "offset": 0, "length": 7 }
    ]
  },
  "reply_to_message_id": 999
}
```

The canonical v1 fields are deliberately different from the earliest plan
draft. The sender is always the `from_user` object, content is always a
discriminated `content` object, and media metadata is part of that object
rather than a top-level `media` array. A message, edited message, or callback
query has `content`; a service update has `service` instead and omits
`content`. Optional fields are omitted, not sent as `null`.

General-topic normalization is also part of the contract: an absent raw
`message_thread_id` and Telegram's explicit General ID (`1`) both produce an
omitted `thread_id`. Named topic IDs are preserved. Consumers may accept
`thread_id: 1` while mixed versions are being upgraded, but producers must
emit the canonical omission.

The old `from_user_id` and top-level `media` names are not aliases emitted by
v1. They came from a pre-implementation plan example. A client still sending
that shape must translate it to `from_user` and the typed `content` object
before handing it to a v1 bridge; unknown legacy fields are not a supported
compatibility mode.

| Field | Type | Required | Description |
|---|---|---|---|
| `update_id` | integer | yes | Telegram update ID |
| `type` | string | yes | One of: `message`, `edited_message`, `callback_query`, `service` |
| `chat_id` | integer | yes | Telegram chat/group ID |
| `thread_id` | integer | no | Named forum topic ID. Omitted = General topic; `1` is accepted only as a legacy input |
| `from_user` | object | yes | Sender info |
| `from_user.id` | integer | yes | Telegram user ID |
| `from_user.first_name` | string | yes | User's first name |
| `from_user.username` | string \| null | no | User's @username |
| `message_id` | integer | yes | Telegram message ID (for replies/edits) |
| `timestamp` | integer | yes | Unix epoch of the message |
| `content` | object | conditional | Required for `message`, `edited_message`, and `callback_query`; omitted for `service` |
| `reply_to_message_id` | integer | no | Message ID this is replying to |
| `service` | object | conditional | Required for `service`; omitted for other update types |

#### Content types

The `content` object has a `type` discriminator field.

**Text:**
```json
{
  "type": "text",
  "text": "refactor the error handling in main.py",
  "entities": [
    {
      "type": "bot_command",
      "offset": 0,
      "length": 7
    }
  ]
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"text"` | Discriminator |
| `text` | string | Message text (1–4096 chars) |
| `entities` | array | Telegram MessageEntity objects, preserved as-is |

**Photo:**
```json
{
  "type": "photo",
  "file_id": "AgACAgIAAxk...",
  "width": 800,
  "height": 600,
  "file_size": 52400,
  "caption": "screenshot of the error",
  "caption_entities": []
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"photo"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference (use with `GET /file`) |
| `width` | integer | Pixel width of selected resolution |
| `height` | integer | Pixel height of selected resolution |
| `file_size` | integer \| null | Size in bytes |
| `caption` | string \| null | Photo caption |
| `caption_entities` | array | Entities in caption |

The proxy selects the best resolution that does not exceed 1280px on the long edge. If only smaller sizes exist, the largest is used.

**Voice:**
```json
{
  "type": "voice",
  "file_id": "AwACAgIAAxk...",
  "duration": 12,
  "file_size": 19200,
  "mime_type": "audio/ogg"
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"voice"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference |
| `duration` | integer | Duration in seconds |
| `file_size` | integer \| null | Size in bytes |
| `mime_type` | string | Always `audio/ogg` for voice messages |

**Audio:**
```json
{
  "type": "audio",
  "file_id": "CQACAgIAAxk...",
  "duration": 240,
  "file_size": 3840000,
  "mime_type": "audio/mpeg",
  "title": "meeting-notes.mp3"
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"audio"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference |
| `duration` | integer | Duration in seconds |
| `file_size` | integer \| null | Size in bytes |
| `mime_type` | string | MIME type |
| `title` | string \| null | Audio title metadata |
| `performer` | string \| null | Performer metadata |

**Video:**
```json
{
  "type": "video",
  "file_id": "BAACAgIAAxk...",
  "width": 1920,
  "height": 1080,
  "duration": 30,
  "file_size": 5242880,
  "mime_type": "video/mp4"
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"video"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference |
| `width` | integer | Pixel width |
| `height` | integer | Pixel height |
| `duration` | integer | Duration in seconds |
| `file_size` | integer \| null | Size in bytes |
| `mime_type` | string | MIME type |
| `caption` | string \| null | Video caption |
| `caption_entities` | array | Entities in caption |

**Video note (round video):**
```json
{
  "type": "video_note",
  "file_id": "DQACAgIAAxk...",
  "length": 240,
  "duration": 15,
  "file_size": 1048576
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"video_note"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference |
| `length` | integer | Diameter in pixels (square) |
| `duration` | integer | Duration in seconds |
| `file_size` | integer \| null | Size in bytes |

**Document:**
```json
{
  "type": "document",
  "file_id": "BQACAgIAAxk...",
  "file_name": "config.yaml",
  "mime_type": "text/yaml",
  "file_size": 2048
}
```

| Field | Type | Description |
|---|---|---|
| `type` | `"document"` | Discriminator |
| `file_id` | string | Proxy-scoped file reference |
| `file_name` | string \| null | Original filename |
| `mime_type` | string \| null | MIME type |
| `file_size` | integer \| null | Size in bytes |
| `caption` | string \| null | Document caption |
| `caption_entities` | array | Entities in caption |

**Callback query (inline keyboard press):**

When `type` is `callback_query`, the envelope uses the same envelope fields and
places callback-specific data in the typed `content` object:

```json
{
  "update_id": 123456790,
  "type": "callback_query",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user": { "id": 789, "first_name": "Jed", "username": "jedarden" },
  "message_id": 2001,
  "timestamp": 1712169700,
  "content": {
    "type": "callback",
    "callback_query_id": "abc123def456",
    "data": "approve_tool_xyz"
  }
}
```

| Field | Type | Description |
|---|---|---|
| `content.type` | `"callback"` | Discriminator |
| `content.callback_query_id` | string | Must be passed to `POST /answer_callback` |
| `content.data` | string | The `callback_data` from the inline button (1–64 bytes) |

`message_id` refers to the message containing the inline keyboard that was pressed.

#### Service types

When `type` is `service`, `content` is omitted and the `service` field is populated:

```json
{
  "type": "service",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user": { "id": 789, "first_name": "Jed", "username": "jedarden" },
  "message_id": 1002,
  "timestamp": 1712169800,
  "service": {
    "type": "forum_topic_created",
    "name": "fix auth middleware",
    "icon_color": 7322096
  }
}
```

| Service type | Fields | Description |
|---|---|---|
| `forum_topic_created` | `name`, `icon_color`, `icon_custom_emoji_id` | New topic created |
| `forum_topic_edited` | `name`, `icon_color`, `icon_custom_emoji_id` | Topic name/icon changed |
| `forum_topic_closed` | (none) | Topic was closed |
| `forum_topic_reopened` | (none) | Topic was reopened |
| `new_chat_members` | `members: [{id, first_name, username}]` | Users joined |
| `left_chat_member` | `member: {id, first_name, username}` | User left |

---

### GET /file/{file_id}

Download a file that was referenced in an update's `file_id` field.

The proxy first calls Telegram `getFile`, then streams the resolved file path
from Telegram's file CDN. It does not cache the bytes or expose the bot token
to the bridge. The request has no body and does not require a
`Content-Type` header.

**Path parameters:**

| Param | Type | Description |
|---|---|---|
| `file_id` | string | The `file_id` from a content object |

**Response 200:**
- `Content-Type`: the file's MIME type (e.g., `image/jpeg`, `audio/ogg`, `application/pdf`)
- `Content-Length`: copied when the CDN supplies it
- `Content-Disposition`: `attachment; filename="<basename of Telegram's file_path>"`
- Body: raw file bytes

**Response 404:**
```json
{
  "ok": false,
  "error_code": 404,
  "description": "file not found or expired"
}
```

**Response 413:**
```json
{
  "ok": false,
  "error_code": 413,
  "description": "file exceeds 20MB limit"
}
```

The proxy returns 404 when Telegram rejects the file ID, does not return a
`file_path`, or the CDN returns 404. A Telegram-reported file size over 20 MiB
is rejected before the CDN request with 413. Telegram file links expire after
about one hour, so the bridge should download files promptly after receiving
the update. The proxy does not retry `getFile` or the CDN request.

---

### POST /send

Send a text message.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "thread_id": 42,
  "text": "Here is the refactored code...",
  "parse_mode": "HTML",
  "reply_to_message_id": 1001
}
```

Send the request with `Content-Type: application/json`. Optional fields that
are not used should be omitted; `null` is accepted by Go's JSON decoder for
pointer fields but is not forwarded to Telegram.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic. Omit for General topic |
| `text` | string | yes | Message text (1–4096 chars) |
| `parse_mode` | string | no | `"HTML"` or `"MarkdownV2"`. Omit for plain text |
| `reply_to_message_id` | integer | no | Message to reply to |
| `reply_markup` | object | no | Inline keyboard (see Inline Keyboard below) |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2001
}
```

The response has `Content-Type: application/json` and contains the
`message_id` of the sent message. The bridge stores this for subsequent edits.

---

### POST /edit

Edit an existing text message. Used for progressive streaming.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "message_id": 2001,
  "text": "Updated response content...",
  "parse_mode": "HTML"
}
```

Send the request with `Content-Type: application/json`. `/edit` routes by
`chat_id` and `message_id`; it does not accept `thread_id`.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Chat containing the message |
| `message_id` | integer | yes | Message to edit |
| `text` | string | yes | New text (1–4096 chars) |
| `parse_mode` | string | no | `"HTML"` or `"MarkdownV2"` |
| `reply_markup` | object | no | Inline keyboard; omit to leave the existing markup unchanged, or send an empty keyboard to remove it |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2001
}
```

**Telegram no-op (text unchanged):**
```json
{
  "ok": false,
  "error_code": 400,
  "description": "Bad Request: message is not modified"
}
```

Because this is a Telegram-originated 400, the proxy sends the body above with
HTTP 502. Consumers should inspect `error_code` and `description` and treat
this specific response as a no-op. Other Telegram 400 errors are failures.

---

### POST /send_photo

Send a photo.

**Request:** `multipart/form-data; boundary=...`

The metadata fields are ordinary text parts; integer values use their decimal
string representation. The required file part is named `photo` and its
multipart filename is forwarded to Telegram. The proxy does not inspect the
file MIME type; Telegram performs media validation.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `photo` | file | yes | Image file, max 10 MiB |
| `caption` | string | no | Caption (0–1024 chars) |
| `parse_mode` | string | no | Parse mode for caption |
| `reply_to_message_id` | integer | no | Message to reply to |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2002
}
```

The response is `application/json` and uses the common `SendResponse` schema.
Validation failures are JSON `400`/`413` responses; Telegram failures use the
error mapping in [Error behavior](#error-behavior).

---

### POST /send_document

Send a file/document.

**Request:** `multipart/form-data; boundary=...`

The required file part is named `document`. Its multipart filename is used by
default; the optional `file_name` text field overrides that filename before
the proxy calls Telegram.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `document` | file | yes | File to send, max 50 MiB |
| `caption` | string | no | Caption (0–1024 chars) |
| `parse_mode` | string | no | Parse mode for caption |
| `reply_to_message_id` | integer | no | Message to reply to |
| `file_name` | string | no | Override filename |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2003
}
```

The response is `application/json` and uses the common `SendResponse` schema.

---

### POST /send_audio

Send an audio file.

**Request:** `multipart/form-data; boundary=...`

The required file part is named `audio`; its multipart filename is forwarded
to Telegram.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `audio` | file | yes | Audio file, max 50 MiB |
| `caption` | string | no | Caption |
| `parse_mode` | string | no | Parse mode for caption |
| `duration` | integer | no | Duration in seconds |
| `title` | string | no | Track title |
| `reply_to_message_id` | integer | no | Message to reply to |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2004
}
```

The response is `application/json` and uses the common `SendResponse` schema.

---

### POST /send_video

Send a video file.

**Request:** `multipart/form-data; boundary=...`

The required file part is named `video`; its multipart filename is forwarded
to Telegram.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `video` | file | yes | Video file, max 50 MiB |
| `caption` | string | no | Caption |
| `parse_mode` | string | no | Parse mode for caption |
| `duration` | integer | no | Duration in seconds |
| `width` | integer | no | Video width |
| `height` | integer | no | Video height |
| `reply_to_message_id` | integer | no | Message to reply to |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2005
}
```

The response is `application/json` and uses the common `SendResponse` schema.

For all four media endpoints, the proxy limits the entire multipart request
to the file limit plus a small 4096-byte allowance for form fields. Multipart
parsing keeps up to 32 MiB in memory and may spool the remainder to temporary
storage. A missing file, malformed form, or invalid/missing `chat_id` returns
HTTP 400; an oversized request returns HTTP 413. Malformed optional integer
fields are treated as absent by the current handler, so clients should send
valid decimal values rather than rely on that behavior.

---

### POST /send_chat_action

Send a typing indicator. Telegram shows it for 5 seconds. The bridge should re-send every 4 seconds during long processing.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "thread_id": 42,
  "action": "typing"
}
```

Send the request with `Content-Type: application/json`. The action is routed
to `thread_id` when present; omit it for General.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `action` | string | yes | One of: `typing`, `upload_photo`, `upload_document`, `upload_video`, `upload_voice` |

**Response 200:**
```json
{
  "ok": true
}
```

---

### POST /create_topic

Create a new forum topic in a supergroup.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "name": "fix auth middleware",
  "icon_color": 7322096
}
```

Send the request with `Content-Type: application/json`. The proxy forwards
`chat_id`, `name`, and the optional `icon_color` to Telegram's
`createForumTopic` method.

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Supergroup chat ID |
| `name` | string | yes | Topic name (1–128 chars) |
| `icon_color` | integer | no | One of six preset colors (see below) |

**Icon color presets:**

| Color | Decimal | Hex |
|---|---|---|
| Light blue | 7322096 | `0x6FB9F0` |
| Yellow | 16766846 | `0xFFD67E` |
| Purple | 13338587 | `0xCB86DB` |
| Green | 9371288 | `0x8EEE98` |
| Pink | 16749490 | `0xFF93B2` |
| Red/orange | 16478046 | `0xFB6F5F` |

**Response 200:**
```json
{
  "ok": true,
  "thread_id": 43,
  "name": "fix auth middleware",
  "icon_color": 7322096
}
```

---

### POST /edit_topic

Edit a forum topic's name or icon.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "thread_id": 43,
  "name": "fix auth middleware [DONE]",
  "icon_color": 9371288
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Supergroup chat ID |
| `thread_id` | integer | yes | Topic to edit |
| `name` | string | no | New name (1–128 chars) |
| `icon_color` | integer | no | New icon color |

Telegram requires at least one of `name` or `icon_color`; the proxy forwards
the request and lets Telegram return the validation error if both are absent.

**Response 200:**
```json
{
  "ok": true
}
```

---

### POST /close_topic

Close a forum topic (makes it read-only).

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "thread_id": 43
}
```

Send the request with `Content-Type: application/json`. `thread_id` is the
topic to close in `chat_id`.

**Response 200:**
```json
{
  "ok": true
}
```

---

### POST /reopen_topic

Reopen a previously closed forum topic.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "thread_id": 43
}
```

Same request/response schema as `/close_topic`.

The response is `application/json`; `thread_id` is the topic to reopen in
`chat_id`.

---

### POST /pin_message

Pin a message in a chat or topic.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "message_id": 2001,
  "disable_notification": true
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Chat containing the message |
| `message_id` | integer | yes | Message to pin |
| `disable_notification` | boolean | no | Suppress pin notification (default: false) |

**Response 200:**
```json
{
  "ok": true
}
```

---

### POST /answer_callback

Acknowledge an inline keyboard button press. Must be called within 10 seconds of receiving the callback query.

**Request body:**
```json
{
  "callback_query_id": "abc123def456",
  "text": "Tool approved",
  "show_alert": false
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `callback_query_id` | string | yes | From the callback content in the update |
| `text` | string | no | Notification text shown to user (0–200 chars) |
| `show_alert` | boolean | no | Show as alert dialog vs. top notification |

**Response 200:**
```json
{
  "ok": true
}
```

---

## Inline Keyboard Schema

Used in `reply_markup` field of `/send` and `/edit`.

```json
{
  "inline_keyboard": [
    [
      { "text": "Approve", "callback_data": "approve_tool_abc123" },
      { "text": "Deny", "callback_data": "deny_tool_abc123" }
    ],
    [
      { "text": "View Details", "callback_data": "details_tool_abc123" }
    ]
  ]
}
```

- Outer array = rows, inner arrays = buttons per row
- `callback_data` is 1–64 bytes, returned in the callback query update
- The bridge encodes session and action context into `callback_data`

---

## Retries and rate limits

The proxy makes one Telegram request per API call. It does not perform a
retry or deduplicate an outbound request. A successful Telegram side effect
followed by a lost response can therefore be repeated by a caller; clients
should account for possible duplicate sends when retrying `/send`, media, or
topic creation.

The bridge's JSON sender applies the following retry policy to `/send`,
`/edit`, `/create_topic`, `/edit_topic`, `/close_topic`, `/reopen_topic`, and
`/pin_message`:

- transport failures and proxy HTTP 502/503/504 responses are retried up to
  five times after the initial attempt;
- exponential delays are 1, 2, 4, 8, and 16 seconds (capped at 30 seconds);
- HTTP 429 waits for `retry_after` seconds when present, or one second when it
  is absent, and that wait does not consume one of the five retry attempts;
- other JSON API errors are returned immediately.

`/send_chat_action`, all four media upload endpoints, and `GET /file/{file_id}`
are single-attempt operations in the current bridge client. A caller that
chooses to retry a media upload or topic creation should expect duplicates if
the first request reached Telegram before the failure was observed. File IDs
can also expire while a retry is pending.

Telegram supplies the authoritative rate-limit response:

```json
{
  "ok": false,
  "error_code": 429,
  "description": "Too Many Requests: retry after 3",
  "retry_after": 3
}
```

The bridge must respect `retry_after` and not retry before that interval. The
proxy has no local message/request throttle, so callers should debounce
progressive `/edit` calls and queue sends where needed rather than relying on
the proxy to enforce a fixed Telegram quota.

## Limits

The proxy forwards JSON values to Telegram and does not locally validate most
Telegram field limits; the following limits are the values callers should
observe, and violations normally come back as a Telegram error mapped to HTTP
502:

| Value | Limit |
|---|---|
| `/send` and `/edit` `text` | 1–4096 characters |
| Media `caption` | 0–1024 characters |
| Forum topic `name` | 1–128 characters |
| Inline-button `callback_data` | 1–64 bytes |
| `/answer_callback` `text` | 0–200 characters |
| `/send_photo` file | 10 MiB; the proxy rejects an oversized multipart request with 413 |
| `/send_document`, `/send_audio`, `/send_video` file | 50 MiB; the proxy rejects an oversized multipart request with 413 |
| `GET /file/{file_id}` | 20 MiB according to Telegram's reported `file_size`; the proxy rejects it with 413 |
| Retained unacknowledged updates | 10,000 by default; oldest entries are dropped on overflow |

The multipart request limit is the media limit plus a 4096-byte allowance for
form fields. Up to 32 MiB is used for multipart parsing in memory; larger
requests may use temporary storage. The bridge chunks longer Claude output
before `/send` and uploads an oversized code block as a document.

---

## Versioning

The contract version is returned in the `/health` response and can be checked on startup:

```json
{
  "ok": true,
  "polling": true,
  "last_update_id": 123456789,
  "uptime_seconds": 3600,
  "contract_version": "1.0"
}
```

Breaking changes increment the major version. The bridge should check `contract_version` on startup and warn if mismatched.
