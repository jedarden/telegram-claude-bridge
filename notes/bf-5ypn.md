# Bead bf-5ypn: Per-user message attribution in /sessions and /status output

## Task Summary

The task was to implement per-user message attribution in `/sessions` and `/status` output.

## Current State

**The per-user attribution feature is already fully implemented!**

### What Already Works

1. **Database Schema**:
   - `sessions` table has `last_from_user_id` column (added in migration v23)
   - `cost_events` table has `from_user_id` column (added in migration v18)

2. **Query Functions in `state.go`**:
   - `GetSessionParticipants(ctx, chatID, threadID)` - Returns user IDs who participated
   - `GetCostsByUser(ctx, chatID)` - Returns cost breakdown by user for a group
   - `GetSessionUserCosts(ctx, chatID, threadID)` - Returns cost breakdown by user for a session
   - `UpdateSessionLastUser(ctx, chatID, threadID, userID)` - Updates last user attribution

3. **Command Output in `commands.go`**:
   - `/status` command (lines 440-490): Shows "Last message from: user X" and "Other participants: X, Y, Z"
   - `/sessions` command (lines 492-532): Shows "Last message from: user X" and "Other participants: X, Y, Z"
   - `/cost` command (lines 1258-1361): Shows per-user cost breakdowns

## Work Completed

While the feature was already implemented, I found and fixed critical SQL syntax bugs:

### Fixed SQL Syntax Errors in `state.go`

1. **Line 562 & 574**: Fixed missing closing quote in `COALESCE(topic_name,)` → `COALESCE(topic_name,'')`
2. **Line 737**: Fixed missing closing parenthesis in `ListAllSessions` query
3. **Lines 647-649**: Fixed duplicate `topic_name` parameter and missing `dispatcher_mode` in `UpdateSession` function

These bugs would have caused runtime errors when querying the database.

## Example Output

### /status command:
```
Active sessions (2):
  • thread 123 — 15 messages, last active 5m ago
    Last message from: user 987654321
    Other participants: 123456789
  • thread 456 — 8 messages, last active 2h ago
    Last message from: user 123456789
```

### /sessions command:
```
All sessions (2):
  • chat -1001234567890 / thread 123 [active] — 15 messages, last active 5m ago
    Last message from: user 987654321
    Other participants: 123456789
  • chat -1001234567890 / thread 456 [active] — 8 messages, last active 2h ago
    Last message from: user 123456789
```

### /cost command (General topic):
```
Cost by user:
  • User 987654321: $1.2345 (12 events)
  • User 123456789: $0.5678 (5 events)
```

## Status

**COMPLETE** - Per-user attribution is fully functional in `/status`, `/sessions`, and `/cost` commands. The SQL bugs have been fixed and the code compiles successfully.
