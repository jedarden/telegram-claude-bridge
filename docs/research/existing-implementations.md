# Existing Telegram-to-Claude/LLM Bridge Implementations

Research date: 2026-04-03

This document surveys existing open-source projects that bridge Telegram to Claude, Claude Code, or similar LLM tools. The goal is to identify patterns around security, convenience, resilience, and robustness that inform our own implementation.

---

## Table of Contents

1. [Individual Project Summaries](#individual-project-summaries)
   - [Claude Code-Specific Bridges](#claude-code-specific-bridges)
   - [Official Anthropic Plugin](#official-anthropic-plugin)
   - [General Claude/Anthropic Bots](#general-claudeanthropic-bots)
   - [ChatGPT/Multi-LLM Telegram Bots](#chatgptmulti-llm-telegram-bots)
2. [Cross-Cutting Analysis](#cross-cutting-analysis)
   - [Security Patterns](#security-patterns)
   - [Convenience Features](#convenience-features)
   - [Resilience and Error Handling](#resilience-and-error-handling)
   - [Robustness and Operations](#robustness-and-operations)
   - [Notable Design Decisions](#notable-design-decisions)
3. [Architecture Taxonomy](#architecture-taxonomy)
4. [Key Takeaways](#key-takeaways)

---

## Individual Project Summaries

### Claude Code-Specific Bridges

#### 1. terranc/claude-telegram-bot-bridge

- **URL**: https://github.com/terranc/claude-telegram-bot-bridge
- **Architecture**: Python daemon using the Claude Code SDK directly. Maintains per-user dedicated SDK streams (up to 3 concurrent per user). Polling-based (no HTTP exposure). Stores runtime state under `PROJECT_ROOT/.telegram_bot/`.
- **Language**: Python 3.11+
- **Key features**:
  - Real-time progressive streaming of Claude responses
  - Numbered options converted to Telegram inline keyboard buttons
  - Voice message support (Whisper or Volcengine ASR via ffmpeg)
  - Five revert modes (full restore, conversation-only, code-only, summarize, cancel)
  - Session resumption with browsable history and pagination
  - Runtime model switching (Sonnet, Opus, Haiku)
  - Priority command queue for `/stop` and `/revert` that bypass normal message limits
  - Sandbox enforcement: project-directory access auto-allowed, external access requires inline-button confirmation
  - Messages older than 20 minutes silently dropped (staleness filter)
  - Crash recovery daemon with 5-crash/60-second circuit breaker
  - 14-day log rotation
  - macOS launchd autostart support

---

#### 2. six-ddc/ccbot

- **URL**: https://github.com/six-ddc/ccbot
- **Architecture**: Thin tmux control layer. Does NOT wrap the SDK -- reads terminal output and sends keystrokes via tmux. Each Telegram Forum topic maps 1:1 to a tmux window running a Claude Code instance. JSONL file polling at 2-second intervals for session monitoring. Hook-based session tracking via Claude's SessionStart hook.
- **Language**: Python
- **Key features**:
  - Topic-based 1:1 mapping (1 topic = 1 window = 1 session)
  - Terminal remains source of truth; seamless switch between Telegram and desktop terminal
  - Interactive UI handling for AskUserQuestion, ExitPlanMode, Permission prompts
  - Directory browser with inline keyboard navigation
  - Terminal screenshot capture with ANSI color support
  - Voice transcription via OpenAI Whisper
  - Hook-based automatic session registration (no manual setup)
  - HTML fixed parse mode avoids MarkdownV2 complexity
  - Tag-aware message splitting preserves code block integrity
  - Per-user read offset tracking prevents duplicate notifications

---

#### 3. RichardAtCT/claude-code-telegram

- **URL**: https://github.com/RichardAtCT/claude-code-telegram
- **Architecture**: Event-bus architecture with dual mode: Agentic (default, natural language) and Classic (terminal-like with 13 explicit commands). Claude SDK primary, CLI fallback. SQLite with migrations for persistent storage. Optional FastAPI webhook server.
- **Language**: Python 3.11+ (Poetry)
- **Key features**:
  - 16 configurable tools with allowlist/disallowlist control
  - Webhook API server with GitHub HMAC-SHA256 and Bearer token auth
  - Cron job scheduler with persistent storage
  - Per-chat rate limiting via token bucket algorithm
  - Directory sandboxing with path traversal prevention
  - Per-user spending limits (`CLAUDE_MAX_COST_PER_USER`)
  - Complete audit logging of all user actions
  - Session export in Markdown, HTML, JSON
  - Quick actions system with context-aware buttons
  - DDD with clean architecture (Domain, Application, Infrastructure, Presentation layers)
  - 143+ unit tests
  - Docker Compose deployment with interactive deploy script

---

#### 4. avivsinai/telclaude

- **URL**: https://github.com/avivsinai/telclaude
- **Architecture**: Multi-layer security relay between Telegram and Claude Agent SDK. Messages pass through: (1) regex-based secret detection, (2) Haiku LLM observer for semantic screening, (3) rate limiting, (4) human approval for FULL_ACCESS tier, (5) permission tier enforcement, (6) SDK sandbox or Docker container firewall.
- **Language**: TypeScript (Node.js 20+, pnpm)
- **Key features**:
  - Four permission tiers: READ_ONLY, WRITE_LOCAL, SOCIAL, FULL_ACCESS
  - Credential vault sidecar: AES-256-GCM encrypted at rest, HTTP proxy injects auth headers, agents never see raw credentials
  - TOTP-gated periodic re-authentication
  - Nonce-based human approval workflow for FULL_ACCESS requests
  - Fail-closed defaults (empty allowlist = deny all, `defaultTier=FULL_ACCESS` rejected at runtime)
  - Secret redaction with CORE pattern matching + entropy detection
  - Docker Compose recommended: 6-container stack (relay, agent, social persona, google-services sidecar, TOTP daemon, vault daemon)
  - Social services integration (Twitter/X, Bluesky)
  - Private network CIDR/host allowlist for homelab services
  - Destructive bash command blocking in WRITE_LOCAL tier
  - Socket permissions enforced (0600)
  - No telemetry; audit logs only

---

#### 5. hanxiao/claudecode-telegram

- **URL**: https://github.com/hanxiao/claudecode-telegram
- **Architecture**: Python bridge using tmux send-keys for message injection. Webhook-based Telegram integration via Cloudflare Tunnel. Claude Code's Stop hook reads transcript and sends response back to Telegram.
- **Language**: Python (77.7%), Shell (22.3%)
- **Key features**:
  - Cloudflare Tunnel avoids public IP exposure for webhooks
  - Pending file flag for state management (filesystem-based)
  - Bot commands: `/status`, `/clear`, `/resume`, `/continue_`, `/loop`, `/stop`
  - Session management and resumption
  - Minimal design relying on Claude Code's native hook system

---

#### 6. andrueandersoncs/telegram-claude

- **URL**: https://github.com/andrueandersoncs/telegram-claude
- **Architecture**: TypeScript service with four components: TelegramService, ClaudeSessionService (Agent SDK lifecycle), PersistenceService (SQLite for session-to-chat mappings), NotificationService. Each Telegram chat maps to a distinct Claude Code session.
- **Language**: TypeScript with Effect library for type-safe async composition
- **Key features**:
  - SQLite persistence surviving service restarts
  - Image analysis support
  - Formatted tool call visibility
  - Automatic context handoff at token limits
  - Natural conversation flow (no slash commands needed)
  - Single-user restriction by design (private bot)
  - Directory sandboxing via `WORKSPACE_PATH`

---

#### 7. gergomiklos/heyagent

- **URL**: https://github.com/gergomiklos/heyagent
- **Architecture**: Polling-based bridge (no webhooks). One chat per running process. Messages queued and grouped during active execution, then batched into next run. Local-first with config at `~/.heyagent/config.json`.
- **Language**: JavaScript/Node.js
- **Key features**:
  - Supports both Claude Code and Codex CLI with mid-session provider switching
  - QR code-based phone setup (Cloudflare Quick Tunnel)
  - Message batching during active execution reduces API calls
  - Sleep prevention during operation
  - Configurable permission modes per provider
  - Local CLI input alongside Telegram input
  - No external server dependency

---

#### 8. NachoSEO/claudegram

- **URL**: https://github.com/NachoSEO/claudegram
- **Architecture**: TypeScript with Grammy bot framework and Claude Agent SDK. MCP (Model Context Protocol) for tool routing. Per-chat session state with model selection, streaming mode, TTS voice preferences.
- **Language**: TypeScript (Node.js 18+)
- **Key features**:
  - Full Claude Code tool access (Bash, Read, Write, Edit, Glob, Grep)
  - Three execution modes: plan, explore, loop
  - Reddit/Medium/YouTube integrations via MCP tools
  - Voice transcription (Groq Whisper) and TTS (OpenAI, 13 voices)
  - Telegraph Instant View for long responses and tables
  - Smart code-block-aware chunking
  - Terminal-style UI with tool status spinners
  - ForceReply interactive prompts
  - Forum topic sessions for parallel projects
  - Watchdog system for long-running tasks
  - Request queue for sequential processing (prevents race conditions)
  - Stale message filtering on restart
  - Message deduplication
  - macOS caffeinate utility to prevent sleep

---

#### 9. op7418/Claude-to-IM-skill & Claude-to-IM

- **URL (skill)**: https://github.com/op7418/Claude-to-IM-skill
- **URL (library)**: https://github.com/op7418/Claude-to-IM
- **Architecture**: The skill version is a local Node.js daemon bridging multiple IM platforms (Telegram, Discord, Feishu, QQ, WeChat). The library version is a dependency-injection bridge extracted from CodePilot (a desktop GUI client) with four DI interfaces: BridgeStore, LLMProvider, PermissionGateway, LifecycleHooks.
- **Language**: TypeScript
- **Key features**:
  - Multi-platform support (5 IM platforms)
  - Interactive setup wizard with token validation
  - Permission control via inline buttons (Telegram/Discord) or text commands
  - Streaming response preview with throttling
  - Token bucket rate limiting (20 msg/min per chat)
  - Exponential backoff retry for delivery
  - HTML fallback when Markdown parsing fails
  - Message deduplication
  - `doctor` diagnostic command checking Node version, config, tokens, logs, PID
  - Credentials stored with chmod 600 file permissions
  - Automatic token redaction in all log output
  - No bundled persistence (you implement ~30 BridgeStore methods)

---

#### 10. viniciustodesco/claude-telegram-bridge

- **URL**: https://github.com/viniciustodesco/claude-telegram-bridge
- **Architecture**: Node.js spawning Claude Code CLI as subprocess with `--print --output-format stream-json`. Debounced streaming. UUID-based session IDs for context maintenance.
- **Language**: JavaScript (Node.js 18+)
- **Key features**:
  - Real-time streaming with progressive message updates
  - Image analysis via Anthropic vision API
  - Voice transcription via Whisper
  - Multi-language support (English, Portuguese, Dutch) with dynamic switching
  - Chat ID-based authentication
  - Group chat support with shared sessions
  - Graceful degradation (works without OpenAI key, just no transcription)

---

#### 11. AliceLJY/telegram-cli-bridge

- **URL**: https://github.com/AliceLJY/telegram-cli-bridge
- **Architecture**: Thin Telegram frontend for task-api. Forwards tasks to task-api endpoints, polls for results. Delegates execution to openclaw-tunnel (Docker-to-host CLI bridge). Three separate bot scripts (not unified).
- **Language**: TypeScript (Bun runtime)
- **Key features**:
  - Supports Claude Code, Codex CLI, and Gemini CLI
  - Owner-only access restriction
  - Per-chat in-memory sessions (lost on restart)
  - Media forwarding (files, photos, voice)

---

#### 12. n4rly-boop/OpenClaude

- **URL**: https://github.com/n4rly-boop/OpenClaude
- **Architecture**: Hexagonal (ports & adapters) architecture. Domain objects, abstract protocols, use cases, and repositories separated from infrastructure. Supports both SDK and subprocess backends.
- **Language**: Python 3.11+
- **Key features**:
  - Streaming live updates with tool usage indicators
  - Per-user workspace isolation (`workspaces/c{chat_id}/`)
  - Three-tier memory system (workspace-wide, per-topic, daily logs)
  - LaTeX rendering via KaTeX with matplotlib fallback
  - Ouroboros watchdog daemon auto-restarting dead processes (30-second interval)
  - Safe restart mechanism waiting for active streams
  - Message batching for rapid-fire inputs
  - Graceful splitting at paragraph boundaries
  - 218 automated tests with GitHub Actions CI
  - Guard hooks (`guard.sh`, `guard-write.sh`) enforce security at subprocess level
  - Universally blocked commands: systemctl, kill, SSH/firewall/auth modifications
  - Non-admin restrictions: no host env access, no credential file reading, workspace-confined
  - Restart recovery notifications

---

#### 13. Angusstone7/claude-code-telegram

- **URL**: https://github.com/Angusstone7/claude-code-telegram
- **Architecture**: Python with DDD clean architecture. Claude Code SDK primary with CLI fallback. TypeScript MCP server for Telegram tools. SQLite persistence.
- **Language**: Python 3.11+
- **Key features**:
  - Human-in-the-loop tool approval buttons (Approve/Deny/View)
  - YOLO mode for auto-approving actions
  - Plan review before execution
  - Multi-project support with seamless switching
  - File browser navigation via Telegram UI
  - Docker container management and system monitoring (CPU, memory, disk)
  - Docker Compose with production-ready config
  - ZhipuAI compatibility for China regions

---

### Official Anthropic Plugin

#### anthropics/claude-plugins-official (Telegram)

- **URL**: https://github.com/anthropics/claude-plugins-official/tree/main/external_plugins/telegram
- **Architecture**: MCP (Model Context Protocol) server running as a Bun process. Logs into Telegram as a bot and exposes tools (`reply`, `react`, `edit_message`) to Claude Code. Inbound messages forwarded to Claude Code session. Launched with `claude --channels plugin:telegram@claude-plugins-official`.
- **Language**: TypeScript (Bun runtime)
- **Key features**:
  - Three tools exposed to Claude: `reply` (with threading and file attachments up to 50MB), `react` (emoji reactions), `edit_message` (progress updates)
  - Pairing-code authentication (6-character code via DM)
  - Two access policies: `pairing` (default, any user gets pairing code) and `allowlist` (numeric Telegram user IDs only)
  - Automatic typing indicator while Claude processes
  - Eager photo download on arrival (Telegram Bot API has no history access)
  - Auto-chunking for long responses with configurable threading mode
  - State stored in `~/.claude/channels/telegram/`
  - Multi-bot support via `TELEGRAM_STATE_DIR`
  - No message history access (Telegram Bot API limitation)
  - Image formats render as photos; others as documents

---

### General Claude/Anthropic Bots

#### llegomark/claude-anthropic-telegram-bot

- **URL**: https://github.com/llegomark/claude-anthropic-telegram-bot
- **Architecture**: Direct Anthropic API integration. Modular Python with separate bot, API, auth, scenarios, and utils modules.
- **Language**: Python 3.12+
- **Key features**:
  - 9 conversation scenarios (Mentor, Coach, Socratic Tutor, etc.)
  - Simple auth code mechanism
  - Local conversation history storage
  - Explicitly marked "not recommended for production use"

#### FlamingoFiesta/Chatgpt-Claude_telegram_bot_with_payment_system

- **URL**: https://github.com/FlamingoFiesta/Chatgpt-Claude_telegram_bot_with_payment_system
- **Architecture**: Python with Flask webhook server. Docker Compose deployment. Stripe payment integration.
- **Language**: Python
- **Key features**:
  - Dual API support (OpenAI + Anthropic simultaneously)
  - Stripe payment system with automatic balance updates via webhook
  - User role-based pricing (admin, beta tester, friend, regular, trial)
  - Configurable profit margins
  - Trial users get starting balance; auto-upgrade after first payment
  - Pre-API-call balance validation
  - 30+ specialized chat modes
  - Docker Compose with payment/no-payment variants

---

### ChatGPT/Multi-LLM Telegram Bots

These are included because they represent mature implementations with transferable patterns.

#### n3d1117/chatgpt-telegram-bot

- **URL**: https://github.com/n3d1117/chatgpt-telegram-bot
- **Architecture**: Python with python-telegram-bot. Plugin architecture supporting function calling. In-memory conversation state with automatic summarization at token limits.
- **Language**: Python
- **Key features**:
  - Admin/user role separation with different allowlists
  - Per-user budget system (daily, monthly, all-time) with token-level price tracking
  - Guest budget enforcement in group chats
  - 16 plugins (weather, Wolfram Alpha, DuckDuckGo, Spotify, crypto, etc.)
  - Streaming enabled by default
  - Conversation summarization when memory limit reached (cost-controlled)
  - Session age limit (default 180 minutes)
  - Docker Compose + Heroku deployment
  - 20+ language localizations
  - 50+ configuration parameters

#### yym68686/ChatGPT-Telegram-Bot (TeleChat)

- **URL**: https://github.com/yym68686/ChatGPT-Telegram-Bot
- **Architecture**: Python async with custom `aient` SDK submodule (not official OpenAI client). Plugin-based. File-based config storage.
- **Language**: Python
- **Key features**:
  - Supports OpenAI, Claude, Gemini models via unified interface
  - Model grouping system (`GPT:model1,model2;Claude:model3`)
  - Auto-merging of long input messages (bypasses Telegram's length limit)
  - Output splitting for oversized responses
  - Streaming/typewriter-effect rendering
  - Per-user or global configuration modes
  - Topic-based isolation in group chats
  - Persistent config storage surviving restarts
  - One-click deployment on Koyeb, Zeabur, Replit, fly.io
  - Custom Markdown-to-Telegram converter (`md2tgmd` submodule)

#### F33RNI/GPT-Telegramus

- **URL**: https://github.com/F33RNI/GPT-Telegramus
- **Architecture**: Python with queue-based request processing. Modular backend wrapper supporting multiple AI providers. JSON-based configuration.
- **Language**: Python 3.10+
- **Key features**:
  - Multi-backend: ChatGPT, Microsoft Copilot, Gemini, Groq
  - Queue system decouples request processing from user interaction
  - Admin commands: ban/unban, queue monitoring, module restart, broadcast messaging
  - Stream writing support
  - Dynamic code block splitting
  - 11 language translations
  - Systemd service templates, Raspberry Pi/ARM support
  - AGPL-3.0 license (copyleft)

---

## Cross-Cutting Analysis

### Security Patterns

#### User Authentication and Authorization

| Pattern | Prevalence | Examples |
|---------|-----------|----------|
| **Telegram user ID allowlist** | Nearly universal | All Claude bridges, n3d1117, yym68686, F33RNI |
| **Fail-closed defaults** (empty allowlist = deny all) | telclaude | Explicit rejection of permissive defaults |
| **Admin vs user role separation** | Common | n3d1117 (ADMIN_USER_IDS), OpenClaude (first user = admin), RichardAtCT |
| **Pairing code flow** | Official plugin | 6-character code via DM, then switch to allowlist |
| **TOTP re-authentication** | telclaude | Periodic identity verification for high-trust tiers |
| **Single auth code** | llegomark | Simple shared password (not production-grade) |
| **Permission tiers** | telclaude | READ_ONLY / WRITE_LOCAL / SOCIAL / FULL_ACCESS with tool-level granularity |

**Consensus**: Telegram user ID allowlist is the baseline. More security-conscious projects layer tiered permissions and periodic re-authentication on top.

#### Token and Secret Management

| Pattern | Examples |
|---------|----------|
| Environment variables via `.env` file | Universal |
| Credential vault sidecar with AES-256-GCM encryption | telclaude |
| Agents never see raw credentials (HTTP proxy injection) | telclaude |
| chmod 600 on config files | Claude-to-IM-skill |
| Token redaction in all log output | Claude-to-IM-skill, telclaude |
| `.gitignore` for credential files | Universal |

**Consensus**: `.env` files are the norm. Only telclaude goes further with encrypted vaults and proxy-based credential injection -- a pattern worth noting for multi-user or shared-host deployments.

#### Rate Limiting

| Pattern | Examples |
|---------|----------|
| Token bucket per chat | RichardAtCT, Claude-to-IM (20 msg/min) |
| Per-user spending limits | RichardAtCT (`CLAUDE_MAX_COST_PER_USER`), n3d1117 (daily/monthly/all-time budgets) |
| Telegram API flood limit compliance | python-telegram-bot's built-in AIORateLimiter (30 msg/s global, 20 msg/min per group) |
| Message staleness filter | terranc (20-minute cutoff), claudegram (stale message filtering on restart) |

**Consensus**: Rate limiting is implemented inconsistently. Spending caps and staleness filters are more common than request-rate limiters. The Telegram API's own flood limits are a hard constraint that most projects handle implicitly.

#### Input Sanitization and Prompt Injection Prevention

| Pattern | Examples |
|---------|----------|
| Regex-based secret detection | telclaude (fast-path filter) |
| LLM observer pre-screening | telclaude (Haiku observer for semantic analysis) |
| Directory sandboxing / path traversal prevention | RichardAtCT, OpenClaude, andrueandersoncs |
| Blocked dangerous commands | OpenClaude (systemctl, kill, SSH), telclaude (rm, chown, kill in WRITE_LOCAL) |
| Guard hooks at subprocess level | OpenClaude (guard.sh, guard-write.sh) |
| SSRF protection (URL validation) | claudegram |

**Consensus**: Most projects rely on directory sandboxing and command allowlists rather than prompt injection defenses. Only telclaude implements LLM-based semantic screening. This is a significant gap -- since these bots give Claude Code shell access, prompt injection through user messages could be weaponized.

#### Admin Commands vs User Commands

Most projects separate admin-only operations:
- **Admin**: ban/unban, broadcast, queue monitoring, module restart, role management, spending limit adjustment
- **User**: new chat, history, help, model selection, status, stop

---

### Convenience Features

#### Multi-turn Conversation Support

Every project supports multi-turn conversations. Approaches vary:

| Approach | Examples |
|----------|----------|
| SDK session persistence (conversation ID) | terranc, RichardAtCT, andrueandersoncs, claudegram |
| tmux session continuity | ccbot, hanxiao |
| Claude `--resume` flag | OpenClaude |
| In-memory with summarization at token limit | n3d1117 |
| File-based conversation history | llegomark, yym68686 |

#### Context and Conversation Management

| Feature | Examples |
|---------|----------|
| `/clear` or `/new` to reset | Nearly universal |
| Session resumption with browsable history | terranc (paginated), ccbot |
| Session export (Markdown, HTML, JSON) | RichardAtCT |
| Three-tier memory (workspace, topic, daily) | OpenClaude |
| Configurable history retention | yym68686 (`PASS_HISTORY`), n3d1117 (max messages + summarization) |
| Session age limits | n3d1117 (180-minute default) |

#### Model Selection

| Feature | Examples |
|---------|----------|
| Runtime model switching | terranc (Sonnet/Opus/Haiku), claudegram, Angusstone7 |
| Model grouping with provider labels | yym68686 (`GPT:model1;Claude:model2`) |
| Per-chat model preference storage | claudegram |

#### System Prompt Customization

| Feature | Examples |
|---------|----------|
| Conversation scenarios/modes | llegomark (9 scenarios), n3d1117 (chat modes), FlamingoFiesta (30+ modes) |
| Per-project context | RichardAtCT (per user/project session), OpenClaude (per-topic memory) |

#### Image/Voice/Document Handling

| Feature | Examples |
|---------|----------|
| Voice message transcription | terranc (Whisper/Volcengine), ccbot (Whisper), claudegram (Groq Whisper), viniciustodesco (Whisper) |
| Text-to-speech responses | claudegram (OpenAI TTS, 13 voices), terranc (macOS voice) |
| Image analysis | RichardAtCT, andrueandersoncs, viniciustodesco (Anthropic vision) |
| File upload handling | terranc (images, PDFs as documents), RichardAtCT (archive extraction) |
| Eager photo download | Official plugin (Telegram Bot API has no history/retrieval) |

#### Markdown Rendering

| Approach | Examples |
|----------|----------|
| MarkdownV2 with auto-escaping | claudegram |
| Fixed HTML parse mode (avoids MarkdownV2 complexity) | ccbot |
| Custom md-to-Telegram converter | yym68686 (`md2tgmd`), ccbot (`chatgpt-md-converter`) |
| HTML fallback on Markdown parse errors | Claude-to-IM |
| Telegraph Instant View for long responses | claudegram |

**Note**: MarkdownV2 is notoriously difficult in the Telegram Bot API. Many projects avoid it entirely in favor of HTML, or build custom converters. This is a common source of bugs.

#### Streaming and Progressive Updates

| Approach | Examples |
|----------|----------|
| Edit-in-place with debouncing | terranc (150 chars / 1.0s interval), viniciustodesco |
| Streaming with throttled preview edits | Claude-to-IM, claudegram |
| Typewriter-effect rendering | yym68686 |
| Tool status spinners | claudegram (terminal-style UI) |
| Persistent typing indicators | RichardAtCT |

**Consensus**: Progressive message editing is the standard approach. Debouncing/throttling is essential to avoid Telegram's rate limits on message edits.

---

### Resilience and Error Handling

#### Retry Logic for API Failures

| Pattern | Examples |
|---------|----------|
| Exponential backoff retry for delivery | Claude-to-IM |
| Docker restart policies | yym68686 (`--restart unless-stopped`) |
| Daemon auto-restart with circuit breaker | terranc (5 crashes in 60s stops restart) |
| Ouroboros watchdog daemon | OpenClaude (30-second interval, auto-restart) |
| Module restart without full bot reload | GPT-Telegramus |

#### Timeout Handling for Long Responses

| Pattern | Examples |
|---------|----------|
| Configurable process timeout | terranc (600s), RichardAtCT (300s), Angusstone7 (600s) |
| Watchdog for long-running tasks | claudegram |
| Permission request timeout with auto-denial | Claude-to-IM-skill (5 minutes) |

#### Graceful Degradation

| Pattern | Examples |
|---------|----------|
| SDK primary, CLI fallback | RichardAtCT, Angusstone7, OpenClaude |
| Works without optional API keys | viniciustodesco (no Whisper without OpenAI key) |
| HTML fallback on Markdown parse errors | Claude-to-IM |
| Missing optional integrations don't break core | claudegram |

#### Queue Management for Concurrent Requests

| Pattern | Examples |
|---------|----------|
| Sequential request queue | claudegram (prevents race conditions) |
| Per-user message queuing with rate limiting | ccbot |
| Queue-based async processing | GPT-Telegramus |
| Priority command queue (bypass normal limits) | terranc (`/stop`, `/revert`) |
| Message batching during active execution | heyagent |

#### Connection Recovery

| Pattern | Examples |
|---------|----------|
| Polling-based (inherently resilient) | terranc, heyagent, ccbot, Claude-to-IM |
| Webhook + Cloudflare Tunnel | hanxiao |
| Dedicated HTTP/1.1 polling client with proxy awareness | terranc |
| Polling and webhook fallback | yym68686 |

**Consensus**: Polling is overwhelmingly preferred over webhooks. It avoids HTTP exposure, works behind NAT/firewalls, and recovers automatically from network interruptions.

#### State Persistence (Surviving Restarts)

| Approach | Examples |
|----------|----------|
| SQLite | RichardAtCT, andrueandersoncs, Angusstone7 |
| JSON files | ccbot (atomic JSON writes), OpenClaude, Claude-to-IM-skill |
| File-based config | yym68686 (user_configs directory) |
| In-memory only (lost on restart) | telegram-cli-bridge, n3d1117 (with summarization) |
| Filesystem state (`~/.ccbot/`, `~/.heyagent/`) | ccbot, heyagent |
| Claude `--resume` flag for session continuity | OpenClaude |

---

### Robustness and Operations

#### Logging and Monitoring

| Pattern | Examples |
|---------|----------|
| 14-day log rotation | terranc |
| Audit logging of all user actions | RichardAtCT, telclaude |
| Token redaction in logs | Claude-to-IM-skill, telclaude |
| Request/response file logging | GPT-Telegramus |
| Tool use/result summarization with statistics | ccbot |

#### Health Checks and Diagnostics

| Pattern | Examples |
|---------|----------|
| `/status` command | Nearly universal |
| `doctor` diagnostic command | Claude-to-IM-skill (checks Node version, config permissions, token validity, log directory, PID) |
| System monitoring (CPU, memory, disk) | Angusstone7 |
| Admin queue monitoring | GPT-Telegramus |

#### Containerization

| Pattern | Examples |
|---------|----------|
| Docker Compose production deployment | RichardAtCT, Angusstone7, FlamingoFiesta, n3d1117 |
| 6-container Docker Compose stack | telclaude |
| No Docker (native install) | terranc, ccbot, heyagent, claudegram, OpenClaude |
| One-click cloud deployment (Koyeb, Zeabur, fly.io) | yym68686 |
| Systemd service templates | GPT-Telegramus, OpenClaude |
| macOS launchd plist | terranc |

#### Configuration Management

| Approach | Prevalence |
|----------|-----------|
| Environment variables (`.env`) | Universal |
| JSON config files | GPT-Telegramus, ccbot, Claude-to-IM-skill |
| YAML config | FlamingoFiesta |
| JSON5 config | telclaude |
| Poetry/pyproject.toml | RichardAtCT |
| pnpm workspaces | telclaude |

#### Message Chunking for Long Responses

| Approach | Examples |
|----------|----------|
| Auto-split at Telegram's 4096-char limit | Official plugin, Claude-to-IM |
| Code-block-aware splitting | ccbot, GPT-Telegramus |
| Paragraph-boundary splitting | OpenClaude |
| Telegraph Instant View for very long content | claudegram |
| Configurable chunk limit | Official plugin (`textChunkLimit`) |

**Consensus**: Naive character-limit splitting breaks code blocks and formatting. The best implementations split at paragraph or code-block boundaries. Telegraph Instant View is a creative escape hatch for truly long content.

#### Cost Tracking and Usage Limits

| Pattern | Examples |
|---------|----------|
| Per-user spending limits | RichardAtCT (`CLAUDE_MAX_COST_PER_USER`) |
| Token-level budget tracking (daily/monthly/all-time) | n3d1117 |
| Role-based pricing with profit margins | FlamingoFiesta |
| Pre-API-call balance validation | FlamingoFiesta |

---

### Notable Design Decisions

#### Architecture Spectrum

Projects fall into four architectural categories (see [Architecture Taxonomy](#architecture-taxonomy) below):

1. **SDK-direct**: Use Claude Code SDK/Agent SDK as a library (terranc, RichardAtCT, telclaude, andrueandersoncs, claudegram, OpenClaude, Angusstone7)
2. **CLI subprocess**: Spawn `claude` CLI and parse stream-json output (viniciustodesco)
3. **tmux bridge**: Read/write to tmux sessions running Claude Code (ccbot, hanxiao)
4. **Task API delegation**: Forward to external task-api service (telegram-cli-bridge)

#### Creative or Unusual Approaches

- **telclaude's LLM pre-screening**: Using a cheap/fast model (Haiku) to semantically screen incoming messages before forwarding to the main agent. This catches prompt injection and policy violations that regex alone would miss.
- **ccbot's tmux architecture**: By operating on tmux rather than the SDK, it preserves full terminal context and allows seamless desktop/mobile switching. The terminal is always the source of truth.
- **claudegram's Telegraph Instant View**: Routing long responses to Telegraph articles solves the Telegram message length problem elegantly while maintaining readability.
- **OpenClaude's guard hooks**: Shell-level security hooks (`guard.sh`, `guard-write.sh`) that enforce restrictions at the subprocess boundary, independent of the bot code.
- **terranc's numbered-option-to-inline-keyboard conversion**: When Claude presents numbered choices, they're automatically converted to tappable Telegram buttons.
- **Official plugin's pairing code flow**: A user-friendly alternative to requiring users to find their numeric Telegram ID.
- **heyagent's message batching**: Messages arriving during active execution are grouped and sent as one batch in the next run, reducing both API calls and context noise.
- **Claude-to-IM's no-bundled-persistence design**: Forces the host application to implement storage, ensuring data ownership is explicit rather than implicit.

#### Common Trade-offs

| Trade-off | Projects choosing A | Projects choosing B |
|-----------|-------------------|-------------------|
| **Polling vs webhooks** | Polling: terranc, ccbot, heyagent, Claude-to-IM (simpler, no HTTP exposure) | Webhooks: hanxiao, RichardAtCT/API (lower latency, requires public endpoint) |
| **SDK vs CLI** | SDK: most projects (richer integration, session management) | CLI: viniciustodesco, ccbot/hanxiao via tmux (simpler, fewer dependencies, preserves native auth) |
| **Single-user vs multi-user** | Single-user: andrueandersoncs, heyagent (simpler security model) | Multi-user: RichardAtCT, telclaude, n3d1117 (complex but shareable) |
| **Stateful vs stateless** | Stateful with SQLite/JSON: RichardAtCT, ccbot, andrueandersoncs | Stateless/in-memory: telegram-cli-bridge, n3d1117 (simpler but loses state on restart) |
| **MarkdownV2 vs HTML** | MarkdownV2: claudegram (richer formatting) | HTML: ccbot, Claude-to-IM (avoids escaping nightmares) |

#### Common Pitfalls (from Issues and Documentation)

- **Telegram MarkdownV2 escaping**: Multiple projects document this as a major pain point. Special characters must be escaped, and nested formatting (bold inside code blocks) is error-prone.
- **Telegram message edit rate limits**: Progressive streaming via message edits can hit Telegram's rate limits. Debouncing/throttling is essential.
- **No message history in Telegram Bot API**: Bots cannot retrieve past messages. Photos must be downloaded eagerly on arrival. The official plugin explicitly documents this limitation.
- **Claude Code session lifecycle**: Sessions can expire, hit token limits, or crash. Projects that don't handle context handoff gracefully lose conversation continuity.
- **Concurrent request ordering**: Without a queue, rapid messages can be processed out of order or create race conditions.
- **macOS sleep**: Several projects implement "caffeinate" or sleep prevention because the host machine going to sleep kills the bridge.

---

## Architecture Taxonomy

```
                    +-----------------+
                    |    Telegram     |
                    |   Bot API       |
                    +--------+--------+
                             |
              +--------------+--------------+
              |              |              |
         Polling        Webhooks       MCP Channel
              |              |              |
    +---------+-----+   +---+---+   +------+------+
    | Bot Process    |   | HTTP  |   | Claude Code |
    | (Python/TS/JS) |   | Server|   | --channels  |
    +-------+--------+   +---+---+   +------+------+
            |                 |              |
    +-------+--------+-------+------+-------+
    |                |              |
    v                v              v
+--------+    +-----------+   +--------+
| Claude |    | Claude    |   | Claude |
| Code   |    | Code CLI  |   | Agent  |
| SDK    |    | subprocess|   | SDK    |
+--------+    +-----------+   | (MCP)  |
                              +--------+
    |                |
    v                v
+--------+    +-----------+
| Direct |    | tmux      |
| API    |    | send-keys |
+--------+    +-----------+
```

### Integration Method Comparison

| Method | Pros | Cons | Used by |
|--------|------|------|---------|
| **Claude Code SDK** | Rich session management, tool visibility, streaming events | Tight coupling to SDK version, Node.js dependency | terranc, RichardAtCT, telclaude, claudegram, OpenClaude |
| **CLI subprocess** | Simple, preserves native auth, language-agnostic | Parsing JSON output, limited session control | viniciustodesco |
| **tmux bridge** | Terminal stays source of truth, zero SDK dependency, desktop/mobile seamless | Fragile screen-scraping, limited structured data, 2s polling latency | ccbot, hanxiao |
| **MCP Channel** | Official Anthropic integration, native plugin system | Locked to Claude Code's plugin lifecycle, limited customization | Official plugin |
| **Task API** | Decoupled, supports multiple CLI tools | Extra infrastructure (task-api server), added latency | telegram-cli-bridge |

---

## Key Takeaways

### What Almost Everyone Does

1. **Telegram user ID allowlist** for authentication
2. **Environment variables** for configuration
3. **Polling** for Telegram updates (not webhooks)
4. **Progressive message editing** for streaming responses
5. **Voice transcription** via Whisper (OpenAI or Groq)
6. **Session persistence** of some form (SQLite, JSON, or Claude's `--resume`)

### What Only the Best Do

1. **Tiered permission systems** with per-tool granularity (telclaude)
2. **LLM-based semantic screening** of incoming messages (telclaude)
3. **Credential vault isolation** where agents never see raw API keys (telclaude)
4. **Code-block-aware message chunking** (ccbot, claudegram)
5. **Watchdog/supervisor processes** with circuit breakers (terranc, OpenClaude)
6. **Comprehensive test suites** (OpenClaude: 218 tests, RichardAtCT: 143+ tests)
7. **Audit logging** with token redaction (telclaude, Claude-to-IM)
8. **Guard hooks** enforcing security at the subprocess boundary (OpenClaude)
9. **Telegraph Instant View** for long-form content (claudegram)
10. **Fail-closed security defaults** (telclaude)

### Gaps and Opportunities

1. **Prompt injection defense is weak across the board.** Only telclaude implements LLM pre-screening. Most projects rely solely on user allowlists, which doesn't protect against a legitimate user's compromised device sending malicious input.
2. **No project implements E2E encryption** between Telegram and the bridge. Messages transit Telegram's servers in plaintext.
3. **Cost tracking is rare** in Claude-specific bridges. The ChatGPT bots (n3d1117, FlamingoFiesta) are more mature here.
4. **Multi-device session continuity** (e.g., starting a conversation on phone, continuing on desktop Telegram) is not explicitly handled by any project.
5. **Structured logging** (JSON-format logs suitable for log aggregation) is not implemented by any project.
6. **Health check endpoints** for monitoring infrastructure integration are absent from most projects.
7. **Graceful shutdown** (draining active requests before exiting) is rarely documented.
