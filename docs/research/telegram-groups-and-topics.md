# Telegram Groups, Supergroups, Forum Topics, and Bot API Research

Research conducted 2026-04-03. Sources: Telegram Bot API docs, Telegram MTProto API docs,
official blog posts, and community references.

---

## Part 1: Telegram Group Types

### Basic Groups

- **Member limit**: 200 members maximum
- **Privacy**: Always private (cannot be made public, no public username)
- **Admins**: Limited admin controls; no granular per-admin permissions
- **Pinned messages**: Only 1 pinned message allowed
- **Message storage**: Messages share a single message ID and PTS sequence with all other
  basic groups and private chats in the user's message box
- **Bots**: Up to 20 bots per group (count toward member limit)
- **Message history for new members**: Not configurable
- **Forum topics**: Not supported (enabling topics triggers automatic upgrade)

### Supergroups

- **Member limit**: Up to 200,000 members
- **Privacy**: Can be public (with username/invite link) or private
- **Admins**: Up to 50 admins with granular per-admin permission sets
- **Pinned messages**: Unlimited
- **Message storage**: Dedicated message ID space per supergroup; stores up to 1,000,000
  most recent messages visibly
- **Bots**: Up to 20 bots per group
- **Message history for new members**: Configurable (visible or hidden)
- **Forum topics**: Supported (must be explicitly enabled)
- **Slow mode**: Supported (configurable cooldown per member)
- **Admin logs**: Full action audit trail
- **Internal representation**: Technically a "channel" with the `megagroup` flag

### Gigagroups (Broadcast Groups)

- **Member limit**: Unlimited (exceeds 200,000)
- **One-way conversion**: From supergroup via `channels.convertToGigagroup` (irreversible)
- **Restrictions**: Only admins can post (when `send_messages` globally disabled)
- **Voice chats**: Participants muted by default
- **Direct invites**: Not supported; join via link only

### Channels

- **Subscriber limit**: Unlimited
- **Purpose**: One-to-many broadcasting (not group conversation)
- **Message attribution**: Anonymous by default, can enable text signatures or full sender info
- **Discussion groups**: Can link a supergroup for comment threads
- **Not relevant for this bridge project** (channels are broadcast-only)

### Migration: Basic Group to Supergroup

Migration is **one-way and irreversible**. It can be triggered:

- **Manually** via `messages.migrateChat`
- **Automatically** when performing any supergroup-only action:
  - Exceeding 200 members
  - Switching to public
  - Changing chat history visibility for new members
  - Assigning custom admin roles
  - Altering member permissions
  - Enabling slow mode
  - Linking a channel
  - **Enabling forum topics**

After migration, both the old basic group ID and the new supergroup ID exist. Clients must
merge message history from both.

### Admin Permissions (Supergroup)

Granular per-admin rights include:

| Permission | Description |
|---|---|
| `can_manage_chat` | General admin access |
| `can_post_messages` | Post in channels (channel-only) |
| `can_edit_messages` | Edit others' messages |
| `can_delete_messages` | Delete any message |
| `can_restrict_members` | Ban/restrict users |
| `can_promote_members` | Add new admins |
| `can_change_info` | Edit group info |
| `can_invite_users` | Add members via invite |
| `can_pin_messages` | Pin messages |
| `can_manage_topics` | Create, rename, close, reopen, delete forum topics |
| `can_manage_video_chats` | Manage voice/video chats |

---

## Part 2: Forum Topics

### Overview

Forum topics split a supergroup into multiple sub-conversations, each functioning as an
independent chat with its own message history, media, pinned messages, and notification
settings. They were designed for large community management but are equally useful for
structured bot interactions.

### History

- **November 5, 2022**: Topics in Groups launched (Telegram blog post), coinciding with
  Bot API 6.3 which added full forum topic support
- **December 30, 2022**: Bot API 6.4 added General topic management methods
- **October 31, 2024**: Bot API 7.11 added `unpinAllGeneralForumTopicMessages`
- **February 9, 2026**: Bot API 9.4 enabled bots to create topics in **private chats**

### Enabling Forum Mode

- Only available on **supergroups** (basic groups auto-upgrade if topics are enabled)
- Group owner or admin with appropriate permissions enables via Group Settings > Permissions > Topics toggle
- Programmatically: `channels.toggleForum` with `enabled=true`
- The `is_forum` field on the Chat object indicates forum mode is active
- **Private chat topics**: Bots can now also enable forum mode in 1-on-1 private chats
  (`has_topics_enabled` on User object)

### The General Topic

Every forum has a mandatory, non-deletable **General** topic with `id=1`.

Special behaviors:
- **Cannot be deleted** (only custom topics can be deleted)
- **Can be hidden**: Unlike other topics, General supports a "hidden" state in addition to
  closed (via `hideGeneralForumTopic` / `unhideGeneralForumTopic`)
- **Default destination**: Messages sent without a `message_thread_id` go to General
- **Reopening auto-unhides**: Reopening a closed General topic automatically unhides it
- **Standard messaging**: Users send to General using normal `sendMessage` without
  specifying a thread ID (same as messaging a regular supergroup)
- **Thread behavior**: The General topic uniquely supports reply threads within it
  (other topics do not)

### Creating Topics

Parameters for topic creation:
- **title** (required): 1-128 UTF-8 characters
- **icon_color** (optional): One of 6 preset RGB values:
  - `0x6FB9F0` (light blue) = 7322096
  - `0xFFD67E` (yellow) = 16766590
  - `0xCB86DB` (purple) = 13338331
  - `0x8EEE98` (green) = 9367192
  - `0xFF93B2` (pink) = 16749490
  - `0xFB6F5F` (red/orange) = 16478047
- **icon_custom_emoji_id** (optional): Custom emoji as icon (Telegram Premium users can
  use any emoji; free users limited to default topic icon sticker pack)

Each created topic gets a unique ID equal to the `message_id` of its
`messageActionTopicCreate` service message.

### Editing Topics

- Name and icon can be changed after creation
- Bot must be admin with `can_manage_topics`, OR be the topic creator
- Topic creators can always edit their own topics without admin rights

### Closing and Reopening Topics

- **Closed topics** reject new messages (read-only state)
- Topics can be reopened to resume conversation
- Bot must be admin with `can_manage_topics`, OR be the topic creator
- Useful for marking completed tasks/conversations

### Deleting Topics

- **Deletes the topic AND all its messages permanently**
- Only non-General topics can be deleted
- Bot must be admin with `can_delete_messages` permission
- Irreversible operation

### Topic Hierarchy and Nesting

**Topics are flat -- there is no native sub-topic or nesting support.**

- All topics exist at the same level within a forum
- There is no parent-child relationship between topics
- Experimental support for nested sub-topics (depth=2) and topic-level slow mode has been
  referenced in API development, but is **not yet shipped** as of April 2026
- If sub-topics ship, the limit may become 500 parent + 500 children (speculative)

### Reply Behavior Within Topics

- **Non-General topics cannot have message threads.** Replies within a topic stay flat --
  they do not create nested reply threads. You can reply to specific messages, but it does
  not spawn a sub-thread.
- **The General topic CAN have reply threads**, behaving like a normal supergroup.
- When replying within a topic, the topic ID is passed as `top_msg_id` to keep the reply
  in the correct topic even if the original message is deleted.

### Limits

| Limit | Value |
|---|---|
| Maximum topics per forum | ~1,000,000 (server-enforced) |
| Topic title length | 1-128 UTF-8 characters |
| Pinned topics | Up to 5 (configurable via `topics_pinned_limit`) |
| Pinned messages per topic | Unlimited (same as supergroup) |
| Icon colors | 6 preset values only |
| Custom emoji icons | Telegram Premium only (free users use default pack) |

### Visual Presentation

- Each topic appears as a separate "chat" in the forum's topic list
- Topics display their icon (color circle or custom emoji) + title
- Forums support two UI modes:
  - **List-based**: Scrollable list of topics
  - **Tabbed**: Tab bar for quick switching (toggled via `channels.toggleForum` with `tabs` flag)
- "View as messages" mode: Users can toggle a flat view showing all topics' messages as a
  single stream (per-account setting via `channels.toggleViewForumAsMessages`)

### Per-Topic Features

- **Notification settings**: Each topic has independent `notify_settings` -- users can mute
  busy topics while keeping others active
- **Pinned messages**: Each topic maintains its own set of pinned messages
- **Shared media**: Each topic has its own media gallery/files section
- **Draft messages**: Per-topic draft support
- **Unread counts**: Independent unread message, mention, and reaction counts per topic
- **Search**: Messages can be searched within a specific topic using `messages.search` with
  the `top_msg_id` parameter

---

## Part 3: Bot API for Forum Topics

### Key Message Fields

**On the Message object:**

```
message_thread_id (Integer, optional)
  Unique identifier of a message thread or forum topic to which the message
  belongs; for supergroups and private chats only.

is_topic_message (Boolean, optional)
  True if the message is sent to a topic in a forum supergroup or a private
  chat with the bot.
```

**On the Chat object:**

```
is_forum (Boolean, optional)
  True if the supergroup chat is a forum (has topics enabled).
```

**On the User object:**

```
has_topics_enabled (Boolean, optional)
  True if the bot has forum topic mode enabled in private chats.
```

### ForumTopic Object (Bot API)

Returned by `createForumTopic`:

| Field | Type | Description |
|---|---|---|
| `message_thread_id` | Integer | Unique identifier of the forum topic |
| `name` | String | Name of the topic |
| `icon_color` | Integer | Color of the topic icon in RGB format |
| `icon_custom_emoji_id` | String (optional) | Custom emoji ID shown as icon |

### Forum Topic Management Methods

#### createForumTopic

Creates a topic in a forum supergroup chat or private chat with a user.

**Parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `chat_id` | Integer or String | Yes | Target chat ID or @username |
| `name` | String | Yes | Topic name, 1-128 characters |
| `icon_color` | Integer | No | RGB color (one of 6 preset values) |
| `icon_custom_emoji_id` | String | No | Custom emoji ID for icon |

**Returns:** ForumTopic object

**Requirements:** Bot must be admin with `can_manage_topics` in supergroups. In private
chats, the bot can create topics if forum mode is enabled.

#### editForumTopic

Edits name and icon of a topic.

**Parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `chat_id` | Integer or String | Yes | Target chat ID |
| `message_thread_id` | Integer | Yes | Topic identifier |
| `name` | String | No | New topic name, 1-128 characters |
| `icon_custom_emoji_id` | String | No | New custom emoji ID |

**Returns:** True on success

**Requirements:** Admin with `can_manage_topics`, OR topic creator.

#### closeForumTopic

Closes an open topic (makes it read-only).

**Parameters:**

| Parameter | Type | Required | Description |
|---|---|---|---|
| `chat_id` | Integer or String | Yes | Target chat ID |
| `message_thread_id` | Integer | Yes | Topic identifier |

**Returns:** True on success

#### reopenForumTopic

Reopens a closed topic.

**Parameters:** Same as closeForumTopic.

**Returns:** True on success

#### deleteForumTopic

Deletes a topic and ALL its messages permanently.

**Parameters:** Same as closeForumTopic.

**Returns:** True on success

**Requirements:** Admin with `can_delete_messages`.

#### unpinAllForumTopicMessages

Clears all pinned messages in a specific topic.

**Parameters:** Same as closeForumTopic.

**Returns:** True on success

### General Topic Management Methods

These methods manage the special General topic (id=1) and do not require a
`message_thread_id` parameter:

| Method | Description |
|---|---|
| `editGeneralForumTopic` | Edit General topic name (takes `chat_id` + `name`) |
| `closeGeneralForumTopic` | Close the General topic (takes `chat_id`) |
| `reopenGeneralForumTopic` | Reopen the General topic (takes `chat_id`) |
| `hideGeneralForumTopic` | Hide the General topic from topic list (takes `chat_id`) |
| `unhideGeneralForumTopic` | Unhide the General topic (takes `chat_id`) |
| `unpinAllGeneralForumTopicMessages` | Clear pinned messages in General (takes `chat_id`) |

All require admin with `can_manage_topics`.

### Utility Methods

#### getForumTopicIconStickers

Returns a list of custom emoji stickers that can be used as forum topic icons by any user
(the default icon pack). Takes no parameters. Returns Array of Sticker.

### Sending Messages to Topics

The `message_thread_id` parameter is supported on all major send methods:

- `sendMessage`, `sendPhoto`, `sendVideo`, `sendAnimation`, `sendAudio`, `sendDocument`
- `sendSticker`, `sendVideoNote`, `sendVoice`, `sendLocation`, `sendVenue`, `sendContact`
- `sendPoll`, `sendDice`, `sendInvoice`, `sendGame`, `sendMediaGroup`
- `sendPaidMedia`
- `copyMessage`, `copyMessages`, `forwardMessage`, `forwardMessages`
- `sendChatAction` (typing indicators per topic)

**If `message_thread_id` is omitted**, the message goes to the General topic.

### Service Messages

The Bot API delivers service messages when topic lifecycle events occur:

| Field on Message | Type | Event |
|---|---|---|
| `forum_topic_created` | ForumTopicCreated | New topic created |
| `forum_topic_edited` | ForumTopicEdited | Topic name/icon changed |
| `forum_topic_closed` | ForumTopicClosed | Topic closed (empty object) |
| `forum_topic_reopened` | ForumTopicReopened | Topic reopened (empty object) |
| `general_forum_topic_hidden` | GeneralForumTopicHidden | General topic hidden (empty) |
| `general_forum_topic_unhidden` | GeneralForumTopicUnhidden | General topic unhidden (empty) |

**ForumTopicCreated fields:**
- `name` (String): Topic name
- `icon_color` (Integer): RGB color
- `icon_custom_emoji_id` (String, optional): Custom emoji
- `is_name_implicit` (Boolean, optional): True if name wasn't explicitly specified

**ForumTopicEdited fields:**
- `name` (String, optional): New name if edited
- `icon_custom_emoji_id` (String, optional): New emoji if changed

### Bot Permissions in Forum Groups

Minimum permissions needed for full forum topic management:

| Capability | Required Permission |
|---|---|
| Create topics | `can_manage_topics` |
| Edit any topic | `can_manage_topics` |
| Edit own topics | No admin needed (topic creator) |
| Close/reopen topics | `can_manage_topics` (or topic creator) |
| Delete topics | `can_delete_messages` |
| Send to topics | Basic send permission |
| Pin in topics | `can_pin_messages` |
| Manage General topic | `can_manage_topics` |

---

## Part 4: Architecture Patterns for Context Management

### Pattern A: One Topic = One Claude Code Conversation/Session

Each forum topic maps to a separate Claude Code CLI conversation using `--resume`.

```
Forum Group: "Claude Code Bridge"
  |
  +-- General (id=1)     -> Control plane / bot commands
  +-- "Fix auth bug"     -> claude --resume conv_abc123
  +-- "Refactor DB"      -> claude --resume conv_def456
  +-- "Write tests"      -> claude --resume conv_ghi789
```

**Implementation:**
- When a user creates a new topic (or sends a message in an empty topic), the bot spawns
  a new Claude Code session and stores the mapping: `topic_id -> conversation_id`
- Subsequent messages in that topic are routed to the same conversation via `--resume`
- The `message_thread_id` on incoming updates identifies which topic/conversation to route to
- Closing a topic can trigger session cleanup; reopening restores it

**Advantages:**
- Natural 1:1 mapping between visual topics and conversation contexts
- Users can easily see all active conversations at a glance
- Per-topic notifications let users mute noisy long-running tasks
- Up to ~1M topics means effectively unlimited conversations

### Pattern B: One Topic = One Project/Working Directory

Each topic is bound to a specific `--cwd` for Claude Code.

```
Forum Group: "Claude Code Bridge"
  |
  +-- General                    -> Default project or help
  +-- "telegram-claude-bridge"   -> claude --cwd ~/telegram-claude-bridge
  +-- "ardenone-cluster"         -> claude --cwd ~/ardenone-cluster
  +-- "kalshi-weather"           -> claude --cwd ~/kalshi-weather
```

**Implementation:**
- Bot maintains a mapping: `topic_id -> {cwd, conversation_id}`
- Topic name or a pinned configuration message defines the working directory
- Multiple conversations can exist per project by creating multiple topics with the same
  project prefix

**Advantages:**
- Clean separation of project contexts
- Each topic's Claude instance operates in the correct directory
- Can combine with Pattern A (topic = project + conversation)

### Pattern C: Topic Naming Conventions for Metadata

Encode operational metadata in topic names and icons:

```
Naming patterns:
  "[project] task description"           -> "[bridge] Fix auth flow"
  "[project/model] task description"     -> "[bridge/opus] Refactor handler"
  "emoji project: task"                  -> "🔧 bridge: Fix auth flow"

Icon color coding:
  Blue (0x6FB9F0)    -> Active/in-progress tasks
  Green (0x8EEE98)   -> Completed (before closing)
  Yellow (0xFFD67E)  -> Waiting/blocked
  Red (0xFB6F5F)     -> Failed/needs attention
  Pink (0xFF93B2)    -> Review/testing
  Purple (0xCB86DB)  -> Research/exploration
```

**Implementation:**
- Parse topic name on creation to extract project, model, and task description
- Use `editForumTopic` to update icon color as task status changes
- Bot can enforce naming conventions when auto-creating topics

### Pattern D: Topic Lifecycle Management

```
New task request (in General)
  -> Bot creates topic via createForumTopic
  -> Bot spawns Claude Code session
  -> Messages flow between topic and CLI
  -> Task completes
  -> Bot closes topic via closeForumTopic
  -> Session resources freed

Resuming work
  -> Bot reopens topic via reopenForumTopic
  -> Resumes CLI session via --resume
  -> Work continues
```

**Closing vs deleting:**
- **Close**: Preserves history, can reopen. Use for completed tasks.
- **Delete**: Destroys all messages. Use only for cleanup of abandoned/test topics.

### Pattern E: General Topic as Control Plane

The General topic serves as the bot's command interface:

```
General topic handles:
  /new [project] [description]  -> Create new topic + session
  /list                         -> List active topics/sessions
  /status                       -> Show resource usage, active CLI processes
  /close [topic_name]           -> Close a topic from General
  /model [topic_id] [model]     -> Change model for a session
  /help                         -> Show available commands

All other topics handle:
  Direct conversation with Claude Code
  File uploads -> passed to CLI
  /stop -> Gracefully terminate CLI session
  /restart -> Kill and restart CLI session
  /context -> Show current conversation context info
```

**Implementation:**
- Check `message_thread_id`: if absent or equal to the General topic's thread ID, treat as
  command; otherwise route to the appropriate CLI session
- General topic can be hidden if desired (to reduce clutter once topics are set up)

### Pattern F: Cross-Topic Context Sharing

Since topics are isolated conversations, cross-referencing requires explicit mechanisms:

**Approach 1: Mention by topic name**
```
User in topic "Fix auth": "Use the same approach we discussed in the 'Refactor DB' topic"
Bot: Fetches recent messages from 'Refactor DB' topic, injects as context
```

**Approach 2: Pinned context messages**
```
Each topic has a pinned message with:
- Project path
- Key decisions/artifacts from other topics
- Links to related topics
```

**Approach 3: Shared context store**
```
Bot maintains a shared context DB:
- Key artifacts (file paths, decisions, code snippets) tagged by topic
- Any topic can query: /context from "Refactor DB"
- Bot retrieves and injects relevant context
```

**Approach 4: Fork topics**
```
User: /fork "Fix auth" "Fix auth - part 2"
Bot: Creates new topic, seeds it with conversation summary from original
```

---

## Part 5: Practical Considerations

### Handling the General Topic

The General topic requires special handling because:

1. **It always exists** and cannot be deleted
2. **Default message destination**: Messages without `message_thread_id` go here
3. **Has reply threads**: Unlike other topics, General supports nested reply threads, which
   complicates message routing
4. **Can be hidden**: Use `hideGeneralForumTopic` to declutter the topic list if using it
   only as a control plane

**Recommended approach**: Use General exclusively as a control plane for bot commands. Hide
it from the topic list if the bot auto-creates topics for all conversations. Alternatively,
keep it visible as the default "quick chat" destination.

### Thread Behavior Within Topics

- **General topic**: Supports reply threads (messages can spawn sub-threads). The bot must
  handle `message_thread_id` carefully here -- it could refer to the General topic itself
  OR a reply thread within General.
- **Custom topics**: No reply threads. All messages are flat within the topic. Replies to
  specific messages are just quoted replies, not separate threads. This simplifies routing
  significantly.
- **Implication for bridge**: Custom topics are easier to work with because every message
  in the topic is guaranteed to belong to that single conversation context. No ambiguity.

### Notification Behavior

- **Per-topic mute**: Users can mute individual topics without muting the entire group
- **Per-topic notification settings**: Independent `notify_settings` per topic (sound,
  show preview, mute duration)
- **Mentions**: Up to 50 mentions per message; only the first 5 trigger notifications
- **Consideration**: Long-running Claude Code output may generate many messages. Users
  should be able to mute noisy task topics while keeping important ones active.

### Search Within Topics

- Messages within a specific topic can be searched using `messages.search` with `top_msg_id`
  set to the topic ID
- Bot API does not expose a direct search method, but the bot can iterate messages
- Telegram clients provide native in-topic search UI

### Pinned Messages Per Topic

- Each topic maintains its own pinned message list (unlimited)
- Useful for storing topic configuration, status, or key artifacts
- Bot can pin a "topic metadata" message on creation:
  ```
  Project: telegram-claude-bridge
  CWD: /home/coding/telegram-claude-bridge
  Model: opus
  Session: conv_abc123
  Started: 2026-04-03 14:30 UTC
  ```
- Use `unpinAllForumTopicMessages` to clean up

### Media Handling Per Topic

- Each topic has its own shared media gallery
- File uploads within a topic stay scoped to that topic
- Useful for passing files to Claude Code (screenshots, logs, config files)
- Bot receives file uploads with the correct `message_thread_id`, so routing to the
  right CLI session is straightforward

### Auto-Creating Topics from User Requests

Bots can automatically create topics based on user input:

```python
# Pseudocode: User sends "/new Fix the login bug" in General
if message.text.startswith("/new") and is_general_topic(message):
    topic_name = message.text[5:]  # "Fix the login bug"
    result = bot.create_forum_topic(
        chat_id=chat_id,
        name=topic_name,
        icon_color=0x6FB9F0  # Blue = active
    )
    # result.message_thread_id is the new topic's ID
    # Start Claude Code session, store mapping
    bot.send_message(
        chat_id=chat_id,
        message_thread_id=result.message_thread_id,
        text="Session started. Send your messages here."
    )
```

**Requirements:**
- Bot must be admin with `can_manage_topics`
- Topic name must be 1-128 characters, non-empty
- Icon color must be one of the 6 preset values (or omitted for default)

### Private Chat Topics (Bot API 9.4+)

As of February 2026, bots can enable forum mode in 1-on-1 private chats:

- Bot owners configure this via the BotFather Mini App
- `has_topics_enabled` field on User indicates if enabled
- Bots can create topics in private chats via `createForumTopic`
- Bot owners can prevent users from creating/deleting topics
- All `message_thread_id` routing works the same as in groups

**Use case for the bridge**: Instead of a group, a single user could have a private chat
with the bot where each topic is a separate Claude Code session. This is simpler (no group
management) but loses multi-user collaboration.

### Rate Limits and Performance

- Topic creation is subject to standard Bot API rate limits (~30 messages/second to
  different chats, ~1 message/second to same chat)
- Creating many topics rapidly may hit flood limits
- The 1M topic limit is generous; practical limits are more likely hit by UI performance
  in Telegram clients with many open topics
- Recommend periodically closing completed topics to keep the list manageable

### Error Handling

Key errors to handle:

| Error | Cause | Handling |
|---|---|---|
| `CHANNEL_FORUM_MISSING` | Chat is not a forum | Prompt admin to enable topics |
| `TOPIC_TITLE_EMPTY` | Empty topic name | Validate before creation |
| `CHAT_WRITE_FORBIDDEN` | Bot lacks write permission | Check bot admin status |
| `PREMIUM_ACCOUNT_REQUIRED` | Custom emoji without Premium | Fall back to color icons |
| Topic already closed | Sending to closed topic | Reopen before sending, or notify user |

---

## Summary: Recommended Architecture for telegram-claude-bridge

Based on this research, the optimal architecture for bridging Telegram to Claude Code CLI:

1. **Use a supergroup with forum mode enabled** (not private chat topics, to support
   future multi-user scenarios)
2. **General topic = control plane** for bot commands (`/new`, `/list`, `/status`, etc.)
3. **Each custom topic = one Claude Code session** with a unique `--resume` conversation ID
4. **Topic names encode task description**; icon colors indicate status
5. **Pinned message per topic** stores session metadata (cwd, model, conversation ID)
6. **Close topics when tasks complete**; reopen to resume
7. **Bot needs admin with**: `can_manage_topics`, `can_delete_messages`, `can_pin_messages`
8. **Route by `message_thread_id`**: Every incoming message's `message_thread_id` maps to
   a conversation; General topic messages are bot commands

This gives clean context isolation per conversation, visual task management, per-topic
notifications, and the full power of Claude Code CLI behind each topic.
