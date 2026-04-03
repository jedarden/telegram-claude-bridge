# Telegram Bot API & Claude Code CLI Research

Research document for the Telegram-to-Claude-Code bridge project. Covers the Telegram Bot API fundamentals, message/media handling, response mechanics, and Claude Code CLI headless integration.

---

## Part 1: Telegram Bot Fundamentals

### 1.1 Creating a Bot via BotFather

To create a Telegram bot:

1. Open a conversation with [@BotFather](https://t.me/BotFather) in Telegram.
2. Send `/newbot`.
3. Provide a **display name** (human-readable, e.g. "My Bridge Bot").
4. Provide a **username** (5-32 characters, must end in `bot`, e.g. `my_bridge_bot`).
5. BotFather returns an **authentication token** in the format:
   ```
   <bot_id>:<secret_token>
   ```

**Token security:** The token authorizes all requests to the Bot API. Keep it secret. If compromised, use `/token` in BotFather to regenerate.

**Bot limits per account:** Up to 20 bots per Telegram account (40 with Premium).

**Bot commands:** Register up to 100 commands via BotFather (`/setcommands`). Command names are 1-32 characters (lowercase Latin letters, digits, underscores). Descriptions up to 256 characters. Essential global commands: `/start`, `/help`, `/settings`.

### 1.2 Receiving Updates: Long Polling vs Webhooks

The Bot API provides two mutually exclusive methods for receiving updates.

#### Long Polling (`getUpdates`)

**Endpoint:** `POST https://api.telegram.org/bot<token>/getUpdates`

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `offset` | Integer | Identifier of the first update to return. Set to `update_id + 1` of the last processed update to acknowledge receipt. |
| `limit` | Integer | Number of updates to retrieve (1-100, default 100). |
| `timeout` | Integer | Timeout in seconds for long polling (0 = short polling). Recommended: 30+. |
| `allowed_updates` | String[] | List of update types to receive (e.g. `["message", "callback_query"]`). |

**Returns:** Array of `Update` objects.

**Tradeoffs:**
- No infrastructure requirements (no public HTTPS endpoint needed).
- Simple to implement -- just loop and poll.
- Slightly higher latency than webhooks (depends on `timeout` value).
- Must manage `offset` to avoid reprocessing updates.
- Cannot simultaneously use webhooks.
- Good for development, smaller deployments, or servers without public IPs.

#### Webhooks (`setWebhook`)

**Endpoint:** `POST https://api.telegram.org/bot<token>/setWebhook`

**Parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `url` | String | HTTPS URL to receive updates. |
| `certificate` | InputFile | Public key certificate for self-signed certs. |
| `ip_address` | String | Fixed IP for sending webhook requests. |
| `max_connections` | Integer | Max simultaneous HTTPS connections (1-100, default 40). |
| `allowed_updates` | String[] | Update types to receive. |
| `drop_pending_updates` | Boolean | Drop all pending updates on setup. |
| `secret_token` | String | Secret token (1-256 chars) sent in `X-Telegram-Bot-Api-Secret-Token` header. |

**Supported ports:** 443, 80, 88, 8443 only.

**Tradeoffs:**
- Lower latency (push-based, no polling delay).
- Requires a publicly reachable HTTPS endpoint with valid SSL.
- More complex infrastructure (reverse proxy, SSL termination).
- Telegram retries failed deliveries with exponential backoff.
- No redirect support; wildcard certificates not supported.
- Better for production deployments at scale.

**For this project:** Long polling is the better choice. The bridge runs on a Hetzner server connected via Tailscale (no public IP). Long polling requires no inbound connections, simplifying deployment significantly.

### 1.3 Rate Limits

| Scope | Limit |
|-------|-------|
| Private chats | ~1 message/second per chat |
| Group chats | 20 messages/minute per group |
| Bulk notifications (broadcast) | ~30 messages/second (free tier) |
| Paid broadcasts | Up to 1,000 messages/second (requires 100K+ MAU and Stars balance) |
| API requests overall | ~30 requests/second |
| 429 Too Many Requests | Returned when limits exceeded; `retry_after` field indicates wait time in seconds. |

### 1.4 Size Limits

| Resource | Standard Bot API | Local Bot API Server |
|----------|-----------------|---------------------|
| File download (`getFile`) | 20 MB | 2,000 MB |
| File upload (non-photo) | 50 MB | 2,000 MB |
| Photo upload | 10 MB | 2,000 MB |
| Text message length | 4,096 characters | 4,096 characters |
| Caption length | 1,024 characters (4,096 with Premium) | Same |
| Formatting entities per message | 100 | 100 |
| Links per message | 100 | 100 |
| Inline keyboard markup data | 10 KB | 10 KB |
| File name length | 60 characters | 60 characters |
| `/start` payload | 64 bytes | 64 bytes |

### 1.5 Bot Privacy Mode

**Default behavior (privacy mode ON):** In group chats, bots only receive:

1. Messages starting with `/` (commands).
2. Commands specifically addressed to the bot: `/command@this_bot`.
3. Replies to the bot's own messages.
4. Service messages (member joins/leaves, title changes, etc.).
5. Messages sent via the bot's inline mode.

**When privacy mode is OFF:** The bot receives ALL messages in the group.

**Admin bots:** If the bot is added as a group admin, it receives ALL messages regardless of privacy setting.

**Configuration:** Use `/setprivacy` in BotFather to toggle. Group admins can see the bot's privacy setting in the member list.

**For this project:** If the bot is used in private chats only (1:1 with the user), privacy mode is irrelevant -- bots always see all messages in private chats. If group support is needed later, making the bot a group admin is the simplest approach.

---

## Part 2: Message Types and Media Handling

### 2.1 The Update Object

Every incoming event is wrapped in an `Update` object. Exactly one of the optional fields is present per update.

```json
{
  "update_id": 123456789,
  "message": { ... },           // New message
  "edited_message": { ... },    // Edited message
  "channel_post": { ... },      // New channel post
  "callback_query": { ... },    // Inline keyboard button press
  "inline_query": { ... },      // Inline query
  "poll": { ... },              // Poll state change
  "my_chat_member": { ... },    // Bot's chat member status changed
  ...
}
```

### 2.2 The Message Object (key fields)

```json
{
  "message_id": 42,
  "from": { "id": 123, "is_bot": false, "first_name": "User", ... },
  "chat": { "id": 123, "type": "private", ... },
  "date": 1234567890,
  "text": "Hello bot!",
  "entities": [ ... ],
  "reply_to_message": { ... },
  "photo": [ ... ],
  "audio": { ... },
  "voice": { ... },
  "video": { ... },
  "video_note": { ... },
  "document": { ... },
  "caption": "...",
  "caption_entities": [ ... ],
  "reply_markup": { ... }
}
```

---

### 2.3 Text Messages

#### How They Arrive

A text message arrives as an `Update` with the `message` field populated. The text content is in `message.text`. Any special entities (commands, mentions, URLs, formatting) are in `message.entities`.

#### Message Entities

`entities` is an array of `MessageEntity` objects:

```json
{
  "type": "bot_command",
  "offset": 0,
  "length": 6,
  "url": null,
  "user": null,
  "language": null
}
```

**Entity types:**

| Type | Description |
|------|-------------|
| `bot_command` | Bot command (e.g. `/start`) |
| `mention` | @username mention |
| `text_mention` | Mention of a user without a username (includes `user` field) |
| `url` | Clickable URL |
| `text_link` | Clickable text URL (includes `url` field) |
| `email` | Email address |
| `phone_number` | Phone number |
| `bold` | Bold text |
| `italic` | Italic text |
| `underline` | Underlined text |
| `strikethrough` | Strikethrough text |
| `spoiler` | Spoiler text |
| `code` | Inline monospace code |
| `pre` | Pre-formatted code block (includes `language` field) |
| `blockquote` | Block quotation |
| `expandable_blockquote` | Expandable block quotation |
| `hashtag` | Hashtag |
| `cashtag` | Cashtag (e.g. `$USD`) |
| `custom_emoji` | Custom emoji (includes `custom_emoji_id` field) |

#### Maximum Text Length

- **Incoming messages:** 4,096 UTF-8 characters (standard), up to 4,096 characters.
- **Outgoing via `sendMessage`:** 4,096 characters (after entity parsing).
- **Captions:** 1,024 characters (4,096 with Premium).

#### Passing Text to Claude Code CLI

**Direct prompt argument:**
```bash
claude -p "User's message text here"
```

**Via stdin (preferred for longer or multi-line text):**
```bash
echo "User's message text" | claude -p "Process this user message"
```

**Via stdin with context:**
```bash
cat <<'EOF' | claude -p "The user sent the following message. Respond helpfully."
User's multi-line
message text here
EOF
```

The stdin approach is safer for arbitrary user input because it avoids shell injection via prompt arguments. For a bridge application, piping via stdin is the recommended approach.

---

### 2.4 Images / Photos

#### How Photos Arrive

When a user sends a photo, `message.photo` contains an **array of `PhotoSize` objects** -- the same image at multiple resolutions, ordered from smallest to largest.

```json
{
  "photo": [
    { "file_id": "AgAC..._small", "file_unique_id": "AQADAgAT...", "width": 90, "height": 90, "file_size": 1234 },
    { "file_id": "AgAC..._medium", "file_unique_id": "AQADAgAT...", "width": 320, "height": 320, "file_size": 12345 },
    { "file_id": "AgAC..._large", "file_unique_id": "AQADAgAT...", "width": 800, "height": 800, "file_size": 45678 },
    { "file_id": "AgAC..._full", "file_unique_id": "AQADAgAT...", "width": 1280, "height": 1280, "file_size": 98765 }
  ],
  "caption": "Optional caption text"
}
```

#### PhotoSize Object

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier, usable for downloading or reusing |
| `file_unique_id` | String | Unique file identifier, stable across time and bots |
| `width` | Integer | Photo width in pixels |
| `height` | Integer | Photo height in pixels |
| `file_size` | Integer (optional) | File size in bytes |

#### Resolution Tiers

Telegram auto-generates multiple sizes. Typical tiers (approximate):
- **Thumbnail:** ~90px on longest side
- **Small:** ~320px
- **Medium:** ~800px
- **Large:** ~1280px (SD limit; each side <= 1280px)
- **HD (if applicable):** up to 2560px per side

**To get the highest resolution:** Use the **last element** of the `photo` array:
```python
best_photo = message.photo[-1]
file_id = best_photo.file_id
```

#### Downloading Photos

**Step 1 -- Get file path:**
```
GET https://api.telegram.org/bot<token>/getFile?file_id=<file_id>
```

**Response:**
```json
{
  "ok": true,
  "result": {
    "file_id": "AgAC...",
    "file_unique_id": "AQADAgAT...",
    "file_size": 98765,
    "file_path": "photos/file_42.jpg"
  }
}
```

**Step 2 -- Download the file:**
```
GET https://api.telegram.org/file/bot<token>/<file_path>
```

The download link is **valid for at least 1 hour**. Call `getFile` again to refresh.

#### File Size Limits for Photos

- **Upload:** 10 MB via multipart/form-data.
- **Download via `getFile`:** 20 MB (standard API), 2,000 MB (local API server).

#### Claude Code CLI Image Support

Claude Code is multimodal and **can process images**. The `Read` tool handles image files natively:

**In print mode**, the recommended approach is to save the image to disk and reference it in the prompt:

```bash
# Save Telegram photo to /tmp/photo.jpg, then:
claude -p "Describe this image and answer the user's question: <question>" --allowedTools "Read" --cwd /tmp
# Claude's Read tool can read image files (PNG, JPG, etc.) and present them visually.
```

**Via the Agent SDK (Python):**
```python
async for message in query(
    prompt="The user sent an image at /tmp/photo.jpg. Read it and describe what you see.",
    options=ClaudeAgentOptions(
        allowed_tools=["Read"],
        cwd="/tmp"
    ),
):
    ...
```

**Important caveats:**
- The `Read` tool's image handling returns the image as base64 to the model.
- A 1000x1000px image costs approximately 1,334 tokens.
- Images compound in multi-turn conversations (the full conversation is resent each API call), so images in long conversations can exhaust the context window quickly.
- Consider resizing large images before passing to Claude to save tokens: the medium Telegram resolution (~800px) may be sufficient for most use cases.

---

### 2.5 Audio

#### Voice Messages vs Audio Files

Telegram distinguishes between two audio types:

**Voice messages** (`message.voice`) -- recorded via the microphone button:

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier |
| `file_unique_id` | String | Unique file identifier |
| `duration` | Integer | Duration in seconds |
| `mime_type` | String (optional) | MIME type (typically `audio/ogg`) |
| `file_size` | Integer (optional) | File size in bytes |

- **Format:** OGG encoded with Opus codec (`.ogg`).
- **Typical quality:** ~32 kbps, mono, 48 kHz.

**Audio files** (`message.audio`) -- music/audio file uploads:

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier |
| `file_unique_id` | String | Unique file identifier |
| `duration` | Integer | Duration in seconds |
| `performer` | String (optional) | Performer name |
| `title` | String (optional) | Track title |
| `file_name` | String (optional) | Original filename |
| `mime_type` | String (optional) | MIME type |
| `file_size` | Integer (optional) | File size in bytes |
| `thumbnail` | PhotoSize (optional) | Album art thumbnail |

#### Duration and Size Limits

- **File size:** Up to 50 MB for both voice and audio via standard Bot API.
- **Duration:** No explicit API-imposed duration limit; constrained by file size.
- **Download limit:** 20 MB via `getFile` (standard API).

#### Downloading Audio

Same two-step process as photos: `getFile` to get the path, then download via the file URL.

#### Claude Code CLI Audio Support

**Claude Code cannot directly process audio.** The model is text and image multimodal only; it has no audio input capability.

**Recommended approach -- transcribe first with Whisper:**

```bash
# 1. Download OGG from Telegram
# 2. Transcribe with OpenAI Whisper (local, free, no API calls)
whisper /tmp/voice.ogg --model turbo --output_format txt

# 3. Pass transcript to Claude
claude -p "The user sent a voice message. Transcript: $(cat /tmp/voice.txt)"
```

**Whisper setup:**
```bash
pip install openai-whisper
# Requires ffmpeg:
apt install ffmpeg
```

Whisper handles OGG/Opus natively (ffmpeg converts internally). No manual format conversion needed.

**Alternative:** `whisper.cpp` is a C++ port with lower resource usage, also available as an ffmpeg filter for pipeline-based transcription.

---

### 2.6 Video

#### Video Types in Telegram

Three distinct video types:

**Regular videos** (`message.video`):

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier |
| `file_unique_id` | String | Unique file identifier |
| `width` | Integer | Video width |
| `height` | Integer | Video height |
| `duration` | Integer | Duration in seconds |
| `thumbnail` | PhotoSize (optional) | Video thumbnail |
| `file_name` | String (optional) | Original filename |
| `mime_type` | String (optional) | MIME type (typically `video/mp4`) |
| `file_size` | Integer (optional) | File size in bytes |

**Video notes** (`message.video_note`) -- round/circle videos recorded via the camera button:

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier |
| `file_unique_id` | String | Unique file identifier |
| `length` | Integer | Diameter of the circular video (width = height) |
| `duration` | Integer | Duration in seconds |
| `thumbnail` | PhotoSize (optional) | Thumbnail |
| `file_size` | Integer (optional) | File size in bytes |

- **Video note limits:** Up to 1 minute duration, up to 12 MB file size.

**Animations** (`message.animation`) -- GIFs and short silent MP4s:
- Same structure as video, also stored as `document` for backward compatibility.

#### Size and Duration Limits

- **Upload:** Up to 50 MB (standard API), 2,000 MB (local API).
- **Download:** 20 MB via `getFile` (standard), 2,000 MB (local).
- **Video notes:** 1 minute max, 12 MB max.
- **Regular videos:** No explicit duration limit; constrained by file size.

#### Claude Code CLI Video Support

**Claude Code cannot directly process video.** Two practical approaches:

**Approach 1 -- Frame extraction (for visual content):**
```bash
# Extract key frames with ffmpeg
ffmpeg -i /tmp/video.mp4 -vf "fps=1" /tmp/frame_%04d.jpg

# Or extract a single representative frame
ffmpeg -i /tmp/video.mp4 -ss 00:00:01 -frames:v 1 /tmp/thumbnail.jpg

# Pass frame(s) to Claude
claude -p "The user sent a video. Here is a representative frame at /tmp/thumbnail.jpg. Describe what you see." \
  --allowedTools "Read"
```

**Approach 2 -- Audio transcription (for spoken content):**
```bash
# Extract audio track
ffmpeg -i /tmp/video.mp4 -vn -acodec libopus /tmp/audio.ogg

# Transcribe with Whisper
whisper /tmp/audio.ogg --model turbo --output_format txt

# Pass to Claude
claude -p "The user sent a video message. Audio transcript: $(cat /tmp/audio.txt)"
```

**Combined approach** for maximum context: extract both key frames and audio transcript.

---

### 2.7 Documents / Files

#### How Document Uploads Work

Any file that isn't categorized as a photo, audio, video, or voice message arrives as `message.document`:

| Field | Type | Description |
|-------|------|-------------|
| `file_id` | String | File identifier |
| `file_unique_id` | String | Unique file identifier |
| `thumbnail` | PhotoSize (optional) | Document thumbnail |
| `file_name` | String (optional) | Original filename |
| `mime_type` | String (optional) | MIME type |
| `file_size` | Integer (optional) | File size in bytes |

#### Size Limits

- **Upload:** 50 MB (standard API), 2,000 MB (local API server).
- **Download via `getFile`:** 20 MB (standard), 2,000 MB (local).

This is the main constraint: files over 20 MB cannot be downloaded via the standard Bot API `getFile`. For a bridge that handles large files, running a [Local Bot API Server](https://github.com/tdlib/telegram-bot-api) raises the limit to 2 GB.

#### Claude Code CLI File Processing

Claude Code can process many file types via its `Read` tool:

**Directly supported:**
- **Text/code files:** Any plaintext file -- source code, configuration, logs, CSV, JSON, YAML, etc.
- **PDF files:** The `Read` tool can read PDFs. For large PDFs (>10 pages), specify page ranges.
- **Images:** PNG, JPG, GIF, WebP (presented visually to the multimodal model).
- **Jupyter notebooks:** `.ipynb` files with all cells and outputs.

**Not directly supported (require preprocessing):**
- Binary files (executables, archives, etc.)
- Audio/video files
- Proprietary formats (Word, Excel, PowerPoint) -- though they can sometimes be read with degraded quality.

**Integration pattern:**
```bash
# Save Telegram document to disk, then:
claude -p "The user uploaded a file. Read /tmp/uploads/document.pdf and summarize it." \
  --allowedTools "Read" \
  --cwd /tmp/uploads
```

---

## Part 3: Sending Responses Back

### 3.1 Sending Text Messages

**Endpoint:** `POST https://api.telegram.org/bot<token>/sendMessage`

**Key parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `chat_id` | Integer/String | Yes | Target chat ID |
| `text` | String | Yes | Message text (1-4096 characters) |
| `parse_mode` | String | No | `MarkdownV2`, `HTML`, or `Markdown` (legacy) |
| `entities` | MessageEntity[] | No | Pre-parsed entities (alternative to parse_mode) |
| `reply_parameters` | ReplyParameters | No | For reply threading |
| `reply_markup` | Markup | No | Inline keyboard, custom keyboard, etc. |
| `link_preview_options` | Object | No | Link preview control |
| `message_thread_id` | Integer | No | Forum topic ID |

**Message length limit:** 4,096 characters after entity parsing. For longer responses, split into multiple messages.

### 3.2 Formatting: HTML vs MarkdownV2

#### HTML Parse Mode (`parse_mode: "HTML"`)

Preferred for programmatic use because escaping is simpler.

**Supported tags:**

```html
<b>bold</b>, <strong>bold</strong>
<i>italic</i>, <em>italic</em>
<u>underline</u>, <ins>underline</ins>
<s>strikethrough</s>, <strike>strikethrough</strike>, <del>strikethrough</del>
<span class="tg-spoiler">spoiler</span>, <tg-spoiler>spoiler</tg-spoiler>
<b>bold <i>italic bold <s>italic bold strikethrough <span class="tg-spoiler">italic bold strikethrough spoiler</span></s> <u>underline italic bold</u></i> bold</b>
<a href="http://www.example.com/">inline URL</a>
<a href="tg://user?id=123456789">inline mention of a user</a>
<tg-emoji emoji-id="5368324170671202286">custom emoji</tg-emoji>
<code>inline fixed-width code</code>
<pre>pre-formatted fixed-width code block</pre>
<pre><code class="language-python">pre-formatted fixed-width code block written in Python</code></pre>
<blockquote>Block quotation</blockquote>
<blockquote expandable>Expandable block quotation</blockquote>
```

**HTML escaping:** Only these characters need escaping:
- `<` becomes `&lt;`
- `>` becomes `&gt;`
- `&` becomes `&amp;`

#### MarkdownV2 Parse Mode (`parse_mode: "MarkdownV2"`)

More complex escaping rules. Use with caution.

**Syntax:**
```
*bold \*text*
_italic \*text_
__underline__
~strikethrough~
||spoiler||
*bold _italic bold ~italic bold strikethrough ||italic bold strikethrough spoiler||~ __underline italic bold___ bold*
[inline URL](http://www.example.com/)
[inline mention of a user](tg://user?id=123456789)
![emoji](tg://emoji?id=5368324170671202286)
`inline fixed-width code`
```pre-formatted fixed-width code block```
```python
pre-formatted fixed-width code block written in Python
```
>Block quotation started
>Block quotation continued
**>The last line of the block quotation**
```

**Characters requiring escaping** (outside of entities): `_ * [ ] ( ) ~ ` > # + - = | { } . !`

Any character with code 1-126 can be escaped with a preceding `\`.

**Inside `pre` and `code`:** Only `` ` `` and `\` need escaping.
**Inside inline link `(...)`:** Only `)` and `\` need escaping.

**Recommendation for this project:** Use **HTML parse mode**. Claude's responses will contain markdown-like formatting. Converting Claude's markdown to Telegram HTML is far more predictable than trying to escape for MarkdownV2, which rejects messages if any special character is unescaped.

### 3.3 Sending Media Back

**Send a photo:**
```
POST /sendPhoto
  chat_id, photo (file_id | URL | upload), caption, parse_mode, reply_markup, ...
```

**Send a document:**
```
POST /sendDocument
  chat_id, document (file_id | URL | upload), caption, parse_mode, reply_markup, ...
```

**Send a video:**
```
POST /sendVideo
  chat_id, video (file_id | URL | upload), duration, width, height, caption, ...
```

**Send audio:**
```
POST /sendAudio
  chat_id, audio (file_id | URL | upload), duration, performer, title, caption, ...
```

All media send methods accept `caption` (up to 1,024 characters) with `parse_mode`.

### 3.4 Inline Keyboards and Callback Queries

#### InlineKeyboardMarkup

Inline keyboards appear below messages. Structure is a 2D array of buttons (rows of buttons):

```json
{
  "inline_keyboard": [
    [
      { "text": "Option A", "callback_data": "choice_a" },
      { "text": "Option B", "callback_data": "choice_b" }
    ],
    [
      { "text": "Visit Website", "url": "https://example.com" }
    ]
  ]
}
```

#### InlineKeyboardButton Fields

| Field | Type | Description |
|-------|------|-------------|
| `text` | String | Button label |
| `callback_data` | String (optional) | Data sent in callback query (1-64 bytes) |
| `url` | String (optional) | HTTP(S) URL to open |
| `web_app` | WebAppInfo (optional) | Web App to open |
| `login_url` | LoginUrl (optional) | Login URL |
| `switch_inline_query` | String (optional) | Switch to inline mode |
| `pay` | Boolean (optional) | Payment button |

Exactly one optional field must be set per button.

#### CallbackQuery Object

When a user presses an inline keyboard button with `callback_data`, the bot receives an `Update` with `callback_query`:

```json
{
  "id": "unique_query_id",
  "from": { "id": 123, ... },
  "message": { ... },
  "chat_instance": "...",
  "data": "choice_a"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | String | Unique query identifier |
| `from` | User | User who pressed the button |
| `message` | Message (optional) | Message with the keyboard |
| `inline_message_id` | String (optional) | For inline mode messages |
| `chat_instance` | String | Global chat identifier |
| `data` | String (optional) | The `callback_data` from the button (up to 64 bytes) |

**Important:** You must call `answerCallbackQuery` (with the `id`) to dismiss the loading indicator, even if you don't want to show a notification.

#### Use Cases for the Bridge

- **Tool approval:** When Claude wants to use a potentially dangerous tool, show an inline keyboard asking the user to approve or deny.
- **Model selection:** Let the user choose which model to use.
- **Action shortcuts:** Quick-action buttons for common operations.

### 3.5 Message Editing for Streaming / Progressive Responses

**Endpoint:** `POST https://api.telegram.org/bot<token>/editMessageText`

**Key parameters:**

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `chat_id` | Integer/String | Yes* | Target chat |
| `message_id` | Integer | Yes* | Message to edit |
| `text` | String | Yes | New text (1-4096 chars) |
| `parse_mode` | String | No | Formatting mode |
| `reply_markup` | InlineKeyboardMarkup | No | New inline keyboard |

*Either `chat_id` + `message_id` or `inline_message_id` required.

**Streaming pattern for the bridge:**

1. Send an initial placeholder message: `sendMessage` -> get `message_id`.
2. As Claude generates tokens, periodically call `editMessageText` to update the message.
3. Respect rate limits (~1 edit/second per chat to avoid 429 errors).
4. On completion, do a final `editMessageText` with the full response and formatting.

**Practical considerations:**
- Telegram silently ignores edits where text hasn't changed (returns `Bad Request: message is not modified`).
- Buffer updates and only edit every 1-2 seconds to stay within rate limits.
- For very long responses, switch to sending new messages (editing is limited to 4,096 chars).

### 3.6 Reply Threading

Use `reply_parameters` to create reply chains:

```json
{
  "chat_id": 123,
  "text": "Here's my response",
  "reply_parameters": {
    "message_id": 42
  }
}
```

This creates a visual reply thread in the Telegram UI. Useful for maintaining context when multiple conversations are interleaved in a group.

**`ReplyParameters` fields:**

| Field | Type | Description |
|-------|------|-------------|
| `message_id` | Integer | The message to reply to |
| `chat_id` | Integer/String (optional) | Chat containing the message (for cross-chat quotes) |
| `allow_sending_without_reply` | Boolean (optional) | Send even if the replied-to message isn't found |
| `quote` | String (optional) | Quoted part of the message |

---

## Part 4: Claude Code CLI Headless Integration

### 4.1 Print Mode (`-p` / `--print`)

Print mode is the primary mechanism for non-interactive Claude Code usage. It processes a single request and exits.

```bash
claude -p "Your prompt here"
```

**Behavior:**
- Skips the workspace trust dialog.
- Loads the same context as interactive mode (CLAUDE.md, hooks, MCP servers, etc.) unless `--bare` is used.
- Reads stdin if content is piped.
- Exits after the response is complete.

**Stdin piping:**
```bash
echo "Review this code" | claude -p "Analyze the following"
cat error.log | claude -p "What caused this crash?"
git diff | claude -p "Review these changes"
```

### 4.2 Output Formats

#### `--output-format text` (default)

Plain text output to stdout. Best for simple integrations.

#### `--output-format json`

Single JSON object with metadata:

```json
{
  "result": "The text response from Claude",
  "session_id": "uuid-here",
  "is_error": false,
  "duration_ms": 5000,
  "duration_api_ms": 4500,
  "num_turns": 3,
  "total_cost_usd": 0.05,
  "usage": {
    "input_tokens": 1000,
    "output_tokens": 500,
    "cache_creation_input_tokens": 0,
    "cache_read_input_tokens": 0
  },
  "structured_output": null
}
```

Extract the text result with `jq`:
```bash
claude -p "Summarize this project" --output-format json | jq -r '.result'
```

#### `--output-format stream-json`

Newline-delimited JSON (NDJSON) for real-time streaming. Each line is a JSON event.

**Key event types:**

- **`system` events:** Session initialization, API retries, compact boundaries.
- **`assistant` messages:** Complete assistant responses with content blocks.
- **`result` messages:** Final result with metadata.
- **`stream_event` (with `--include-partial-messages`):** Raw API streaming events (text deltas, tool use deltas).

**Streaming text deltas:**
```bash
claude -p "Write a poem" --output-format stream-json --verbose --include-partial-messages | \
  jq -rj 'select(.type == "stream_event" and .event.delta.type? == "text_delta") | .event.delta.text'
```

**API retry events:**

| Field | Type | Description |
|-------|------|-------------|
| `type` | `"system"` | Event type |
| `subtype` | `"api_retry"` | Retry event |
| `attempt` | Integer | Current attempt number |
| `max_retries` | Integer | Total retries permitted |
| `retry_delay_ms` | Integer | Milliseconds until next attempt |
| `error_status` | Integer/null | HTTP status code |
| `error` | String | Error category |

### 4.3 Input Format: `--input-format stream-json`

For multi-turn conversations via stdin, use `--input-format stream-json`. This accepts NDJSON messages on stdin:

```bash
claude -p --input-format stream-json --output-format stream-json
```

Each input line is a JSON message. This enables bidirectional streaming communication -- the bridge can send new user messages while Claude is still generating.

**Stream chaining example:**
```bash
claude --print --output-format stream-json "analyze dataset" | \
  claude --print --input-format stream-json --output-format stream-json "process results"
```

Note: The exact input message schema for `--input-format stream-json` is not fully documented in the official CLI docs. The Agent SDK (Python/TypeScript) provides a better-documented interface for multi-turn conversations.

### 4.4 Session Management

#### Continuing Conversations

```bash
# Continue the most recent conversation in the current directory
claude -p "Follow up question" --continue

# Resume a specific session by ID
claude -p "Follow up" --resume "session-uuid-here"

# Resume by name
claude -p "Follow up" --resume "my-session-name"
```

#### Capturing Session IDs

```bash
session_id=$(claude -p "Start a review" --output-format json | jq -r '.session_id')
claude -p "Continue the review" --resume "$session_id"
```

#### Named Sessions

```bash
# Name a session at creation
claude -p "Start working on auth" --session-id "$(uuidgen)" --name "auth-work"

# Resume by name later
claude -r "auth-work" -p "Continue"
```

#### Fork Sessions

```bash
# Resume but create a new branch of conversation
claude --resume "session-id" --fork-session -p "Try a different approach"
```

### 4.5 Key CLI Flags for Bridge Integration

| Flag | Description | Bridge Use Case |
|------|-------------|----------------|
| `--cwd <dir>` | Set working directory | Point Claude at the user's project directory |
| `--model <model>` | Select model (e.g. `sonnet`, `opus`) | Let users choose model per conversation |
| `--allowedTools <tools>` | Auto-approve specific tools | `"Read,Edit,Bash,Glob,Grep"` for full agent mode |
| `--disallowedTools <tools>` | Block specific tools | Restrict dangerous operations |
| `--permission-mode <mode>` | Permission handling | `acceptEdits` or `bypassPermissions` for headless |
| `--max-budget-usd <amount>` | Spending cap per request | Prevent runaway costs |
| `--max-turns <n>` | Limit agent turns | Prevent infinite loops |
| `--append-system-prompt <text>` | Add to system prompt | Inject bridge-specific instructions |
| `--bare` | Minimal mode, fast startup | Skip hooks/plugins for speed |
| `--no-session-persistence` | Don't save session to disk | For ephemeral one-off queries |
| `--effort <level>` | `low`, `medium`, `high`, `max` | Let users control quality/speed tradeoff |
| `--fallback-model <model>` | Fallback when primary overloaded | Reliability for production bridge |
| `--tools <tools>` | Restrict available tool set | `"Read,Bash"` for read-only mode |
| `--dangerously-skip-permissions` | Skip all permission prompts | For fully automated/sandboxed operation |

### 4.6 The Claude Agent SDK (Programmatic Alternative)

For production use, the Agent SDK is preferred over shelling out to `claude -p`. It provides native Python/TypeScript interfaces with proper streaming, session management, and type safety.

#### Installation

```bash
# Python
pip install claude-agent-sdk

# TypeScript
npm install @anthropic-ai/claude-agent-sdk
```

#### Authentication

```bash
export ANTHROPIC_API_KEY=your-api-key
```

#### Basic Usage (Python)

```python
import asyncio
from claude_agent_sdk import query, ClaudeAgentOptions

async def handle_user_message(text: str, session_id: str = None):
    options = ClaudeAgentOptions(
        allowed_tools=["Read", "Edit", "Bash", "Glob", "Grep"],
        permission_mode="acceptEdits",
        cwd="/home/user/project",
        max_budget_usd=1.00,
    )
    if session_id:
        options.resume = session_id

    result_text = ""
    new_session_id = None

    async for message in query(prompt=text, options=options):
        if hasattr(message, 'result') and message.result:
            result_text = message.result
            new_session_id = message.session_id

    return result_text, new_session_id
```

#### Streaming with the SDK (Python)

```python
from claude_agent_sdk import query, ClaudeAgentOptions
from claude_agent_sdk.types import StreamEvent, ResultMessage

async def stream_response(prompt: str):
    options = ClaudeAgentOptions(
        include_partial_messages=True,
        allowed_tools=["Read", "Bash"],
    )

    async for message in query(prompt=prompt, options=options):
        if isinstance(message, StreamEvent):
            event = message.event
            if event.get("type") == "content_block_delta":
                delta = event.get("delta", {})
                if delta.get("type") == "text_delta":
                    yield delta.get("text", "")  # Stream text chunks
        elif isinstance(message, ResultMessage):
            # Final result with metadata
            pass
```

#### Multi-Turn Conversations with ClaudeSDKClient

```python
from claude_agent_sdk import ClaudeSDKClient, ClaudeAgentOptions, AssistantMessage, TextBlock

async def conversation():
    async with ClaudeSDKClient(options=ClaudeAgentOptions(
        allowed_tools=["Read", "Edit", "Bash"],
        permission_mode="acceptEdits",
    )) as client:
        # First message
        await client.query("What files are in this directory?")
        async for message in client.receive_response():
            if isinstance(message, AssistantMessage):
                for block in message.content:
                    if isinstance(block, TextBlock):
                        print(block.text)

        # Follow-up (retains full context)
        await client.query("Now read the main.py file")
        async for message in client.receive_response():
            ...
```

#### ClaudeAgentOptions (Key Fields)

```python
@dataclass
class ClaudeAgentOptions:
    # Tools
    allowed_tools: list[str]          # Auto-approved tools
    disallowed_tools: list[str]       # Blocked tools
    tools: list[str] | None           # Available tool set

    # Prompts
    system_prompt: str | None         # Replace default system prompt
    
    # Permissions
    permission_mode: str | None       # "default", "acceptEdits", "plan", "dontAsk", "bypassPermissions"
    can_use_tool: Callable | None     # Custom permission callback
    
    # Session
    continue_conversation: bool       # Continue most recent
    resume: str | None                # Resume by session ID
    fork_session: bool                # Fork when resuming
    
    # Limits
    max_budget_usd: float | None      # Spending cap
    max_turns: int | None             # Turn limit
    
    # Model
    model: str | None                 # Model selection
    fallback_model: str | None        # Fallback model
    effort: str | None                # "low", "medium", "high", "max"
    
    # Environment
    cwd: str | None                   # Working directory
    env: dict[str, str]               # Environment variables
    add_dirs: list[str]               # Additional directories
    
    # Streaming
    include_partial_messages: bool     # Enable streaming events
    
    # MCP
    mcp_servers: dict | None          # MCP server configuration
    
    # Agents
    agents: dict | None               # Custom subagent definitions
```

#### SDK Message Types

| Type | Description |
|------|-------------|
| `SystemMessage` | Session init, API retries, compact boundaries |
| `AssistantMessage` | Complete response with `content` blocks (TextBlock, ToolUseBlock, etc.) |
| `ResultMessage` | Final result: `result` (text), `session_id`, `total_cost_usd`, `usage`, `is_error` |
| `StreamEvent` | Raw API streaming events (when `include_partial_messages=True`) |
| `UserMessage` | Echoed user messages |
| `RateLimitEvent` | Rate limit info with `status`, `resets_at`, `utilization` |

#### SDK vs CLI for the Bridge

| Consideration | CLI (`claude -p`) | Agent SDK |
|---------------|-------------------|-----------|
| Setup complexity | Lower | Higher (Python/TS project) |
| Streaming | Via `stream-json` + jq | Native async generators |
| Session management | Via `--resume` flag | Programmatic, type-safe |
| Error handling | Parse stderr/exit codes | Exception-based |
| Custom permissions | Limited | Full callback support |
| Process isolation | Each call is separate process | Shared process, lower overhead |
| Multi-turn | Requires session ID management | Built-in with `ClaudeSDKClient` |

**Recommendation for this project:** Use the **Python Agent SDK** (`claude-agent-sdk`). It provides:
- Native async streaming for progressive Telegram message updates.
- Type-safe session management for per-user conversation continuity.
- Custom permission callbacks for user-controlled tool approval via Telegram inline keyboards.
- Lower overhead than spawning a new process per message.

---

## Part 5: Integration Architecture Summary

### Recommended Stack

1. **Language:** Python (async) -- best ecosystem for both Telegram bot libraries and Claude Agent SDK.
2. **Telegram library:** `python-telegram-bot` (v20+, async) or raw `aiohttp` calls to the Bot API.
3. **Update method:** Long polling (no public IP required; works over Tailscale).
4. **Claude integration:** `claude-agent-sdk` Python package.
5. **Audio transcription:** `openai-whisper` (local, offline).
6. **Video processing:** `ffmpeg` for frame extraction and audio extraction.

### Message Flow

```
User sends Telegram message
  |
  v
Bot receives Update (long polling)
  |
  v
Classify message type (text / photo / voice / video / document)
  |
  v
Preprocess:
  - Text: pass through
  - Photo: download via getFile, save to temp dir
  - Voice: download, transcribe with Whisper
  - Video: extract frames + transcribe audio
  - Document: download, save to temp dir
  |
  v
Build prompt + pass to Claude Agent SDK
  (include session_id for conversation continuity)
  |
  v
Stream response:
  - Send initial placeholder message
  - editMessageText periodically (~1/sec) with accumulated text
  - Final edit with complete, formatted response
  |
  v
If response > 4096 chars: split into multiple messages
If response includes files: use sendDocument/sendPhoto
```

### Key Design Decisions

1. **One Claude session per Telegram chat:** Map `chat_id` to `session_id` for conversation continuity.
2. **Rate-limited message editing:** Buffer streaming tokens and edit every 1-2 seconds.
3. **HTML parse mode:** Convert Claude's markdown output to Telegram HTML for reliable formatting.
4. **Image size optimization:** Use Telegram's medium resolution (~800px) to save Claude tokens.
5. **Graceful degradation:** If audio transcription fails, inform the user rather than silently dropping the message.
6. **Budget controls:** Use `max_budget_usd` per session to prevent runaway API costs.
