# /parallel Command Test Coverage Gaps

## Analysis Date: 2026-07-27

## Overview

This document identifies test coverage gaps for the `/parallel` command across three key areas:
1. `splitParallelPrompts` utility (internal/bridge/commands.go:2073-2084)
2. `cmdParallel` handler (internal/bridge/commands.go:2024-2069)  
3. Subtask orchestrator integration (internal/bridge/subtask_orchestrator.go:44-86)

---

## Implementation Summary

### splitParallelPrompts (lines 2073-2084)
```go
func splitParallelPrompts(text string) []string {
    parts := strings.Split(text, "\n---\n")
    var prompts []string
    for _, p := range parts {
        prompt := strings.TrimSpace(p)
        if prompt != "" {
            prompts = append(prompts, prompt)
        }
    }
    return prompts
}
```

**Behavior:**
- Splits by exact string `\n---\n`
- Trims whitespace from each part
- Filters out empty prompts
- Does NOT enforce a 5-prompt limit (that's cmdParallel's job)

### cmdParallel (lines 2024-2069)
**Validation flow:**
1. Check args != "" → usage message
2. Check ThreadID != nil → topic required
3. Check group != nil → registration required
4. Check subtaskOrchestrator != nil → availability required
5. Call splitParallelPrompts(args)
6. Check 1 <= len(prompts) <= 5 → limit enforced
7. Get session from DB (can error)
8. Create SubtaskRequest and call orchestrator.Run

---

## Existing Test Coverage

### ✅ Well-Covered Areas

| Area | Tests |
|------|-------|
| **splitParallelPrompts** | Basic splitting, whitespace, multi-line, empty filtering, delimiter variations (spaces around ---), five prompts, no delimiter, only delimiter, unicode, long prompts, special chars, consecutive delimiters, delimiter at start/end, tabs/mixed whitespace, exactly five, six prompts (boundary) |
| **cmdParallel** | Single/multiple prompts, no ThreadID, nil group, nil orchestrator, empty args, max subtasks enforced, negative chatID, unicode prompts, special characters, very long prompts, exactly five boundary, six exceeds limit |
| **Orchestrator** | Single/multiple subtasks, max subtasks limit, no prompts, too many prompts, list running, cancel, with/without session, default max_subtasks, DB errors, empty prompts in list, nil session, duplicate prompts, unique IDs, very long prompts, mixed statuses, multiple topics |

---

## 🚨 Coverage Gaps

### splitParallelPrompts - Uncovered Edge Cases

#### 1. Line Ending Variations
**Gap:** No tests for Windows-style line endings (`\r\n`)
- Input: `"prompt1\r\n---\r\nprompt2"`
- Expected: Should split correctly
- **Risk:** Users on Windows or cross-platform environments

#### 2. Empty String Handling
**Gap:** No explicit test for empty string input
- Input: `""`
- Expected: Empty array (already covered by "OnlyWhitespace" test, but not explicit)
- **Risk:** Low (existing test likely covers this)

#### 3. Delimiter with Extra Spaces (Without Newlines)
**Gap:** Tests cover ` --- `, `  ---  `, but only with `\n` prefix/suffix
- Input: `"prompt1 --- prompt2"` (spaces without newlines)
- Expected: Treat as single prompt (no split)
- **Current behavior:** WOULD NOT split (correct, but untested)
- **Risk:** Medium - user confusion about delimiter format

#### 4. Mixed Whitespace Characters
**Gap:** No tests for non-space whitespace (tabs, Unicode whitespace)
- Input: `"prompt1\n\t---\n\t\nprompt2"` (tabs around delimiter)
- Input: `"prompt1\n​---\n prompt2"` (zero-width space)
- Expected: Behavior undefined
- **Risk:** Low (rare in practice)

#### 5. Extremely Long Single Prompt
**Gap:** No test for single prompt >100KB
- Input: 100KB+ string without delimiters
- Expected: Should return as single prompt
- **Risk:** Memory/performance if code assumes reasonable sizes

#### 6. Prompts with Only Newlines
**Gap:** No test for prompts that are just newlines/spaces after trimming
- Input: `"\n\n\n---\n\t\t\t---\n\n\n"`
- Expected: Empty array (all filtered)
- **Risk:** Low (covered by existing "OnlyWhitespace" test)

#### 7. Carriage Return Only Delimiters
**Gap:** No test for `\r` without `\n`
- Input: `"prompt1\r---\rprompt2"`
- Expected: Would NOT split (only `\n---\n` recognized)
- **Risk:** Low (Mac OS 9 legacy systems)

---

### cmdParallel - Uncovered Edge Cases

#### 1. Session State Variations
**Gap:** No tests for different session states (closed, error, cancelled)
- Scenario: Session exists but Status != "active"
- Expected: Should still work (no state check in code)
- **Risk:** Medium - subtasks might reference a dead session

#### 2. GetSession DB Errors
**Gap:** No test for DB errors on GetSession
- Scenario: DB closed, corrupted, or locked
- Current code: Returns `fmt.Errorf("get session: %w", err)`
- Expected: Error propagated up
- **Risk:** Medium - error handling untested

#### 3. Orchestrator.Run Error Propagation
**Gap:** No test for orchestrator.Run returning error
- Scenario: MaxSubtasks enforced, DB error on CreateSubtask
- Current code: Returns `fmt.Errorf("start parallel tasks: %w", err)`
- Expected: Error returned to user
- **Risk:** Medium - error flow untested

#### 4. Empty Prompts After Splitting
**Gap:** No test for when split returns empty array
- Scenario: User sends `"---"` or `"\n---\n"` as args
- Current code: Returns "No prompts found" message
- Expected: Already covered by splitParallelPrompts filtering
- **Risk:** Low (implicit coverage)

#### 5. Duplicate Prompts
**Gap:** No test for identical prompts in list
- Input: `"What is 2+2?\n---\nWhat is 2+2?"`
- Expected: Should create 2 subtasks with same prompt
- **Risk:** None (duplicates are allowed)

#### 6. Special-Only Prompts
**Gap:** No test for prompts that are only special characters
- Input: `"!!!\n---\n???\n---\n@@@"`
- Expected: Should create 3 subtasks
- **Risk:** Low (edge case)

#### 7. Args with Trailing Delimiter
**Gap:** No explicit cmdParallel test for trailing delimiter
- Input: `"prompt1\n---\nprompt2\n---\n"`
- Expected: 2 prompts (trailing empty filtered)
- **Risk:** Low (covered by splitParallelPrompts test)

#### 8. Mixed Line Endings in Args
**Gap:** No test for `\r\n` in cmdParalell args
- Input: `"prompt1\r\n---\r\nprompt2"`
- Expected: Would NOT split (only `\n---\n` recognized)
- **Risk:** Medium - Windows users

---

### SubtaskOrchestrator - Uncovered Edge Cases

#### 1. Context Cancellation
**Gap:** No test for context cancellation during Run
- Scenario: User cancels operation, parent context times out
- Current behavior: Goroutines already spawned
- Expected: Subtasks continue running (best-effort)
- **Risk:** Medium - undefined cleanup behavior

#### 2. Panic Recovery
**Gap:** No tests for panic scenarios in executeSubtask
- Scenario: PTY manager panics, unexpected nil pointer
- Expected: Goroutine crashes silently
- **Risk:** Low (defer wg.Done() should still execute)

#### 3. Empty Prompts in Request
**Gap:** No test for empty string in Prompts array
- Input: `[]string{"valid", "", "valid"}`
- Current code: Creates subtask with empty prompt
- Expected: Should probably reject at Run() level
- **Risk:** Medium - wastes resources on empty subtasks

#### 4. MaxSubtasks Boundary Conditions
**Gap:** Limited tests for MaxSubtasks edge cases
- Group.MaxSubtasks = 0 (should default to 5) ✅ COVERED
- Group.MaxSubtasks = 1 (single subtask only)
- Group.MaxSubtasks = 5 (exactly at default)
- Group.MaxSubtasks = 6 (higher than default)

#### 5. Concurrent Run Calls
**Gap:** No test for multiple goroutines calling Run simultaneously
- Scenario: Two users hit /parallel in same topic at same time
- Expected: Both should succeed (within limits)
- **Risk:** Low (DB handles concurrency)

---

## Priority Gaps (Recommended for Next Bead)

### High Priority (Real-World Impact)

1. **Windows line endings (`\r\n`)** - splitParallelPrompts
   - Users on Windows or mixed environments
   - Simple fix: Normalize line endings before splitting

2. **GetSession DB error handling** - cmdParallel
   - Error path currently untested
   - Should return user-friendly error message

3. **Empty prompt in Prompts array** - SubtaskOrchestrator.Run
   - Wastes resources, should validate at Run level

4. **Session state variations** - cmdParallel
   - Subtasks might reference dead sessions
   - Should verify session is active or create new one

### Medium Priority (Edge Cases)

5. **Delimiter without newlines but with spaces** - splitParallelPrompts
   - User confusion about delimiter format
   - Clarify documentation or add more flexible parsing

6. **Orchestrator.Run error propagation** - cmdParallel
   - Error flow untested
   - Verify user sees appropriate error message

7. **Context cancellation** - SubtaskOrchestrator
   - Undefined cleanup behavior
   - Should document or implement cancellation

### Low Priority (Rare Scenarios)

8. **Extremely long single prompt** - splitParallelPrompts
   - Performance concerns at scale
   - Add size limits if needed

9. **Special-only prompts** - cmdParallel
   - Edge case, unlikely in practice
   - No action needed unless issues reported

---

## Test Implementation Plan

### Phase 1: High Priority Gaps
- [ ] TestSplitParallelPrompts_WindowsLineEndings
- [ ] TestSplitParallelPrompts_DelimiterWithoutNewlines
- [ ] TestCommandHandler_cmdParallel_GetSessionDBError
- [ ] TestCommandHandler_cmdParallel_SessionClosedState
- [ ] TestSubtaskOrchestrator_Run_EmptyPromptInArray

### Phase 2: Medium Priority Gaps
- [ ] TestCommandHandler_cmdParallel_OrchestratorRunError
- [ ] TestSubtaskOrchestrator_Run_ContextCancellation
- [ ] TestSplitParallelPrompts_MixedLineEndings

### Phase 3: Low Priority Gaps
- [ ] TestSplitParallelPrompts_ExtremelyLongPrompt
- [ ] TestCommandHandler_cmdParallel_SpecialOnlyPrompts
- [ ] TestSubtaskOrchestrator_ConcurrentRunCalls

---

## Summary Statistics

| Component | Existing Tests | Identified Gaps | Coverage |
|-----------|---------------|----------------|----------|
| splitParallelPrompts | 18 | 7 | ~72% |
| cmdParallel | 14 | 8 | ~64% |
| SubtaskOrchestrator | 15 | 5 | ~75% |
| **Overall** | **47** | **20** | **~70%** |

**Note:** "Coverage" is approximate based on logical edge cases, not code metrics.

---

## Files Referenced

- `internal/bridge/commands.go` - cmdParallel (lines 2024-2069), splitParallelPrompts (lines 2073-2084)
- `internal/bridge/subtask_orchestrator.go` - SubtaskOrchestrator.Run (lines 44-86)
- `internal/bridge/integration_test.go` - Existing test coverage
