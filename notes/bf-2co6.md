# Task 6.1: Bridge Event Stream — Unix Socket NDJSON Publisher

## Status: Already Implemented

The Unix socket NDJSON publisher for the bridge event stream was already fully implemented in the codebase.

## Implementation Summary

### Publisher (`internal/events/publisher.go`)
- **Socket path**: `/tmp/telegram-bridge-events.sock` (configurable via `EVENT_SOCKET_PATH`)
- **Protocol**: NDJSON (newline-delimited JSON)
- **Non-blocking**: Events are dropped silently when no listener is connected
- **Connection management**: Single listener supported, with automatic connection health monitoring

### Event Types (Phase 6 specification)

| Event Type | Schema | Emitted By |
|------------|--------|------------|
| `message_in` | `chat_id`, `thread_id`, `topic`, `user`, `preview` | Router |
| `message_out` (streaming) | `chat_id`, `thread_id`, `topic`, `status="streaming"`, `tokens`, `elapsed_ms` | SessionManager |
| `message_out` (complete) | `chat_id`, `thread_id`, `topic`, `status="complete"`, `tokens`, `cost_usd`, `elapsed_ms` | SessionManager |
| `command` | `chat_id`, `command`, `args`, `topic`, `user`, `result` | Router |
| `session_update` | `chat_id`, `thread_id`, `topic`, `status`, `model` | SessionManager |
| `health` | `proxy_ok`, `proxy_latency_ms`, `db_ok`, `db_latency_ms` | HealthChecker |

### Configuration

| Environment Variable | Default | Description |
|---------------------|---------|-------------|
| `EVENT_PUBLISHING_ENABLED` | (unset) | Set to `true`/`1` to enable event publishing |
| `EVENT_SOCKET_PATH` | `/tmp/telegram-bridge-events.sock` | Unix socket path for event streaming |

### Integration Points

1. **cmd/bridge/main.go**: Publisher created, started, and passed to components
2. **internal/bridge/router.go**: Publishes `PublishCommand`, `PublishMessageIn`
3. **internal/bridge/session_manager.go**: Publishes `PublishMessageOutComplete`, `PublishMessageOutStreaming`, `PublishSessionUpdate`
4. **internal/health/health.go**: Publishes `PublishHealth`

### Dashboard

The TUI dashboard (`cmd/dashboard/main.go`) is already implemented to consume the NDJSON event stream and display:
- Active sessions panel
- System health panel
- Messages in flight log
- Command log
- Cost tracker

### Tests

All tests pass (`internal/events/publisher_test.go`):
- Flat JSON structure verification
- Unix socket integration tests
- Non-blocking drop behavior
- Helper function tests
