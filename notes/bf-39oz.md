# Bead bf-39oz: Proxy allowed_updates filter missing

## Task
Proxy allowed_updates filter missing (no callback_query/my_chat_member filter in getUpdates)

## Summary
This task was already completed in previous commits. The `allowed_updates` filter was already implemented in the Telegram poller, including both `callback_query` and `my_chat_member` update types.

## Work Completed

### Previous Implementation (Commits 711ce1a and 1f854f6)
1. **Commit 711ce1a** - Added `allowed_updates` filter to getUpdates request with initial types:
   - message
   - edited_message
   - callback_query

2. **Commit 1f854f6** - Added `my_chat_member` support:
   - Added MyChatMember field to Update struct with supporting types
   - Added ServiceTypeMyChatMember constant
   - Implemented normalizeMyChatMember function
   - Updated allowed_updates filter to include 'my_chat_member'
   - Added test coverage

### Final Resolution
During bead closure, a merge conflict occurred when pulling from remote. The conflict was in the allowed_updates filter:
- **Local (correct)**: `["message","edited_message","callback_query","my_chat_member"]`
- **Remote (outdated)**: `["message","edited_message","callback_query"]`

The conflict was resolved by keeping the local version which includes `my_chat_member`, ensuring the proxy receives all supported update types from Telegram.

## Current Implementation

The proxy's getUpdates request now includes the complete allowed_updates filter:
```go
params.Set("allowed_updates", `["message","edited_message","callback_query","my_chat_member"]`)
```

This prevents receiving unsupported update types and ensures the proxy only processes the update types it supports.

## Files Modified
- internal/telegram/poller.go - getUpdates function with allowed_updates filter
- internal/telegram/poller_test.go - TestPoller_AllowedUpdates test
- internal/telegram/normalize.go - normalizeMyChatMember function
- internal/contract/types.go - ServiceTypeMyChatMember constant
- internal/telegram/types.go - MyChatMember supporting types

## Test Coverage
Test `TestPoller_AllowedUpdates` verifies that the poller includes the correct allowed_updates parameter in the request.
