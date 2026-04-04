# Proxy ↔ Bridge Data Contract

Version: 1.0  
Date: 2026-04-03

## Transport

- **Protocol:** HTTP/1.1 over Tailscale
- **Base URL:** `http://telegram-proxy:8080` (Tailscale hostname)
- **Content-Type:** `application/json` for all JSON endpoints
- **Media uploads:** `multipart/form-data`
- **Auth:** None — Tailscale ACLs restrict access to the EX44 node only
- **Encoding:** UTF-8

## Conventions

- All timestamps are Unix epoch integers (matching Telegram API)
- `chat_id` is a signed 64-bit integer (supergroups are negative)
- `thread_id` is a positive integer (forum topic ID); `null` or absent means General topic
- `message_id` is a positive integer
- `user_id` is a positive 64-bit integer
- Empty optional fields are omitted (not sent as `null`)
- The proxy never interprets message content — it is opaque bytes/JSON to the proxy

## Error Response (all endpoints)

```json
{
  "ok": false,
  "error_code": 400,
  "description": "Bad Request: message text is empty"
}
```

`error_code` mirrors the Telegram API error code when the error originates from Telegram. Proxy-internal errors use:

| Code | Meaning |
|---|---|
| 502 | Telegram API unreachable |
| 503 | Proxy not connected to Telegram (polling not started) |
| 504 | Telegram API timeout |

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

**Response 200:**
```json
{
  "ok": true,
  "updates": [ <Update> ... ]
}
```

The bridge should call this in a loop. The proxy tracks `offset` internally — each call returns only new updates since the last acknowledged batch. Updates are acknowledged implicitly when the bridge fetches the next batch.

#### Update envelope

Every Telegram update is normalized into this envelope. The proxy strips all auth context and resolves file references.

```json
{
  "update_id": 123456789,
  "type": "message | edited_message | callback_query | service",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user": {
    "id": 789,
    "first_name": "Jed",
    "username": "jedarden"
  },
  "message_id": 1001,
  "timestamp": 1712169600,
  "content": { <ContentObject> },
  "reply_to_message_id": 999,
  "service": null
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `update_id` | integer | yes | Telegram update ID |
| `type` | string | yes | One of: `message`, `edited_message`, `callback_query`, `service` |
| `chat_id` | integer | yes | Telegram chat/group ID |
| `thread_id` | integer \| null | no | Forum topic ID. `null` = General topic |
| `from_user` | object | yes | Sender info |
| `from_user.id` | integer | yes | Telegram user ID |
| `from_user.first_name` | string | yes | User's first name |
| `from_user.username` | string \| null | no | User's @username |
| `message_id` | integer | yes | Telegram message ID (for replies/edits) |
| `timestamp` | integer | yes | Unix epoch of the message |
| `content` | object | yes | Message content (see Content types below) |
| `reply_to_message_id` | integer \| null | no | Message ID this is replying to |
| `service` | object \| null | no | Service message data (see Service types below) |

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
      "length": 7,
      "extra": null
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
  "title": "meeting-notes.mp3",
  "performer": null
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
  "mime_type": "video/mp4",
  "caption": null,
  "caption_entities": []
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
  "file_size": 2048,
  "caption": null,
  "caption_entities": []
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

When `type` is `callback_query`, the envelope differs slightly:

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
  },
  "reply_to_message_id": null,
  "service": null
}
```

| Field | Type | Description |
|---|---|---|
| `content.type` | `"callback"` | Discriminator |
| `content.callback_query_id` | string | Must be passed to `POST /answer_callback` |
| `content.data` | string | The `callback_data` from the inline button (1–64 bytes) |

`message_id` refers to the message containing the inline keyboard that was pressed.

#### Service types

When `type` is `service`, the `content` is `null` and the `service` field is populated:

```json
{
  "type": "service",
  "chat_id": -1001234567890,
  "thread_id": 42,
  "from_user": { "id": 789, "first_name": "Jed", "username": "jedarden" },
  "message_id": 1002,
  "timestamp": 1712169800,
  "content": null,
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

**Path parameters:**

| Param | Type | Description |
|---|---|---|
| `file_id` | string | The `file_id` from a content object |

**Response 200:**
- `Content-Type`: the file's MIME type (e.g., `image/jpeg`, `audio/ogg`, `application/pdf`)
- `Content-Length`: file size in bytes
- `Content-Disposition`: `attachment; filename="<original_filename>"` (if available)
- Body: raw file bytes

**Response 404:**
```json
{
  "ok": false,
  "error_code": 404,
  "description": "File not found or expired"
}
```

**Response 413:**
```json
{
  "ok": false,
  "error_code": 413,
  "description": "File exceeds 20MB download limit"
}
```

Note: Telegram file links expire after ~1 hour. The proxy fetches from Telegram on each request — it does not cache files. The bridge should download files promptly after receiving the update.

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
  "reply_to_message_id": 1001,
  "reply_markup": null
}
```

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

The proxy returns the `message_id` of the sent message. The bridge stores this for subsequent edits.

---

### POST /edit

Edit an existing text message. Used for progressive streaming.

**Request body:**
```json
{
  "chat_id": -1001234567890,
  "message_id": 2001,
  "text": "Updated response content...",
  "parse_mode": "HTML",
  "reply_markup": null
}
```

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Chat containing the message |
| `message_id` | integer | yes | Message to edit |
| `text` | string | yes | New text (1–4096 chars) |
| `parse_mode` | string | no | `"HTML"` or `"MarkdownV2"` |
| `reply_markup` | object | no | Updated inline keyboard, or `null` to remove |

**Response 200:**
```json
{
  "ok": true,
  "message_id": 2001
}
```

**Response 400 (text unchanged):**
```json
{
  "ok": false,
  "error_code": 400,
  "description": "Bad Request: message is not modified"
}
```

The bridge should treat this as a no-op, not an error.

---

### POST /send_photo

Send a photo.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `photo` | file | yes | Image file (JPEG/PNG, max 10MB) |
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

---

### POST /send_document

Send a file/document.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `document` | file | yes | File to send (max 50MB) |
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

---

### POST /send_audio

Send an audio file.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `audio` | file | yes | Audio file (max 50MB) |
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

---

### POST /send_video

Send a video file.

**Request:** `multipart/form-data`

| Field | Type | Required | Description |
|---|---|---|---|
| `chat_id` | integer | yes | Target chat |
| `thread_id` | integer | no | Target forum topic |
| `video` | file | yes | Video file (max 50MB) |
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

At least one of `name` or `icon_color` must be provided.

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
  "ok": true
}
```

Same request/response schema as `/close_topic`.

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

## Rate Limiting

The proxy enforces Telegram's rate limits and returns 429 with retry information:

```json
{
  "ok": false,
  "error_code": 429,
  "description": "Too Many Requests: retry after 3",
  "retry_after": 3
}
```

The bridge must respect `retry_after` and not retry before that interval.

Telegram rate limits:
- 1 message/second per chat (private)
- 20 messages/minute per group
- ~30 requests/second global across all chats

The bridge is responsible for debouncing edit calls (max 1/second) and queuing sends per chat.

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
