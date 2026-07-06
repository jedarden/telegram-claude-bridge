# WorkerPool Edge Cases - Test Coverage Analysis

## Overview
Analysis of `internal/bridge/worker_pool.go` and `internal/bridge/integration_test.go` to identify edge cases not covered by current tests.

## Summary
**Total edge cases identified: 9**
- DB state transitions: 3
- Validation logic gaps: 3
- Error handling: 3

---

## Category: DB State Transitions

### 1. Worker Status Transition: "running" → "done" with FinishedAt Timestamp
**Why it needs testing:** The `finishWorker` method updates the worker status from "running" to "done", but existing tests only verify the status field. The `FinishedAt` timestamp is set by the database via `datetime('now')`, but tests don't verify it's actually set.

**Test scenario:**
```go
func TestWorkerPool_FinishedAtSet_OnSuccess(t *testing.T) {
    // Create a running worker
    // Call finishWorker with success
    // Verify worker.Status == "done"
    // Verify worker.FinishedAt != nil
    // Verify FinishedAt > StartedAt
}
```

**Risk:** If `FinishedAt` is not set correctly, queries filtering by completion time will fail.

### 2. Worker Status Transition: "running" → "failed" with FinishedAt
**Why it needs testing:** Similar to above, but for the error case. Tests verify status changes to "failed" but don't check that `FinishedAt` is set for failed workers.

**Test scenario:**
```go
func TestWorkerPool_FinishedAtSet_OnFailure(t *testing.T) {
    // Create a running worker
    // Call finishWorker with error
    // Verify worker.Status == "failed"
    // Verify worker.FinishedAt != nil
}
```

**Risk:** Failed workers without `FinishedAt` could appear as "stuck" in monitoring queries.

### 3. Worker Result Field NULL vs Empty String Handling
**Why it needs testing:** The `scanWorker` function uses `sql.NullString` for the Result field. When a worker is created, Result is NULL (not set). When finishWorker succeeds, Result is set (possibly empty string). Tests don't verify the NULL vs empty string distinction.

**Test scenario:**
```go
func TestWorkerPool_ResultNULL_vs_EmptyString(t *testing.T) {
    // Create worker - verify Result is NULL
    // finishWorker with empty result - verify Result is "" (empty string, not NULL)
    // finishWorker with actual result - verify Result is set
}
```

**Risk:** NULL vs empty string queries could behave differently in SQL (`WHERE result IS NULL` vs `WHERE result = ''`).

---

## Category: Validation Logic Gaps

### 4. Model Resolution Priority Chain Incomplete Coverage
**Why it needs testing:** The model resolution has three levels: input model → group.DefaultModel → hardcoded default. Tests cover empty group.DefaultModel and explicit models, but don't test the full priority chain with various combinations.

**Test scenarios missing:**
```go
// Test when input model is provided but group.DefaultModel is empty
func TestWorkerPool_ModelPriority_InputProvided_GroupEmpty(t *testing.T)

// Test when all three levels are set
func TestWorkerPool_ModelPriority_AllLevelsSet(t *testing.T)

// Test when input model is empty, group default is set
func TestWorkerPool_ModelPriority_InputEmpty_GroupSet(t *testing.T)
```

**Risk:** Model resolution could use wrong priority, causing workers to run with incorrect model settings.

### 5. MaxWorkers Negative Value Handling
**Why it needs testing:** The code checks `if maxWorkers <= 0 { maxWorkers = 5 }`. Tests cover `MaxWorkers = 0` but don't test negative values, which could occur if DB is manually modified or migrated incorrectly.

**Test scenario:**
```go
func TestWorkerPool_MaxWorkersNegative(t *testing.T) {
    group.MaxWorkers = -1 // Invalid value
    // Should default to 5, not reject workers
    // Verify can spawn up to 5 workers
}
```

**Risk:** Negative values could be treated as "unlimited" or cause other unexpected behavior.

### 6. Concurrent SpawnWorker Race Condition on Index Assignment
**Why it needs testing:** The code has a mutex-protected index increment: `wp.mu.Lock(); wp.nextIndex[key]++; index = wp.nextIndex[key]; wp.mu.Unlock()`. Tests verify indices are unique with 10 concurrent workers, but don't test higher concurrency or verify the exact sequence.

**Test scenarios missing:**
```go
// Test with higher concurrency (50+ workers)
func TestWorkerPool_ConcurrentIndexing_HighConcurrency(t *testing.T)

// Verify no gaps in index sequence under concurrency
func TestWorkerPool_ConcurrentIndexing_NoGaps(t *testing.T) {
    // Spawn 100 workers concurrently
    // Verify indices are 1..100 exactly (no missing numbers)
}
```

**Risk:** Under high concurrency, the mutex might not guarantee sequential indices, causing gaps or duplicates.

---

## Category: Error Handling

### 7. Sender.SendResponse Timeout Handling in finishWorker
**Why it needs testing:** The `finishWorker` method calls `wp.sender.SendResponse` with a 10-second timeout. Tests use a mock server that always succeeds, so timeout/error paths are not tested.

**Test scenario:**
```go
func TestWorkerPool_finishWorker_SenderTimeout(t *testing.T) {
    // Use a slow/hanging mock server
    // Verify finishWorker logs error but doesn't panic
    // Verify DB is still updated (should not be blocked by sender failure)
}
```

**Risk:** If sender hangs, finishWorker might block indefinitely, preventing worker completion.

### 8. CountRunningWorkers DB Query Error Handling
**Why it needs testing:** Tests cover the case where the DB is closed, but don't test other DB errors like constraint violations, corruption, or context cancellation.

**Test scenarios missing:**
```go
func TestWorkerPool_CountRunningWorkers_ContextCanceled(t *testing.T) {
    // Cancel context before counting
    // Verify error is propagated correctly
}

func TestWorkerPool_CountRunningWorkers_DatabaseLocked(t *testing.T) {
    // Lock DB with exclusive transaction
    // Try to spawn worker
    // Verify proper error handling
}
```

**Risk:** Certain DB errors might cause panics or incorrect worker counts.

### 9. WorkerID Collision on Rapid Sequential Spawns
**Why it needs testing:** Worker IDs are generated as `worker_{threadID}_{timestamp}` where timestamp is `time.Now().UnixNano()`. If two workers are spawned in rapid succession within the same nanosecond, they could get the same ID. Tests use `time.Sleep` or are slow enough that this doesn't occur.

**Test scenario:**
```go
func TestWorkerPool_WorkerIDUniqueness_RapidSpawns(t *testing.T) {
    // Spawn workers in a tight loop without delays
    // Verify all worker IDs are unique
    // If collision occurs, detect second worker creation failure
}
```

**Risk:** ID collisions would cause the second worker to fail with a DB constraint violation (PRIMARY KEY).

---

## Additional Notes

### Coverage Gaps by Method

**SpawnWorker:**
- ✅ Input validation (empty prompt, malformed JSON)
- ✅ Concurrency limit enforcement
- ✅ Index incrementing
- ✅ Model resolution (basic cases)
- ❌ Model resolution (priority chain edge cases)
- ❌ MaxWorkers negative values

**runWorker:**
- ✅ Basic flow (via integration tests)
- ❌ PTY spawn failures (not tested without real tmux)
- ❌ Startup timeout handling
- ❌ Prompt injection failures

**finishWorker:**
- ✅ Success/failure status updates
- ✅ Result truncation
- ✅ Error message truncation
- ❌ FinishedAt timestamp verification
- ❌ NULL vs empty string for Result field
- ❌ Sender timeout handling

**DB State:**
- ✅ CreateWorker, UpdateWorker, GetWorker
- ✅ CountRunningWorkers
- ❌ FinishedAt transition verification
- ❌ NULL vs empty string handling

### Test Infrastructure Limitations

Current tests use:
- Mock HTTP server for sender (always succeeds)
- Test doubles for SessionManager and PTYManager
- In-memory SQLite DB

Missing:
- Failure injection for Sender
- Real PTY/spawn simulation
- Context cancellation scenarios
- Database corruption/lock scenarios
- High-concurrency stress testing (100+ workers)

### Priority Ranking

**High Priority (affects correctness):**
1. WorkerID collision on rapid spawns (#9)
2. FinishedAt timestamp verification (#1, #2)
3. Model resolution priority chain (#4)

**Medium Priority (edge cases likely in production):**
4. MaxWorkers negative values (#5)
5. Concurrent index gaps (#6)
6. Result NULL vs empty string (#3)

**Low Priority (unlikely failure modes):**
7. Sender timeout handling (#7)
8. DB error scenarios (#8)
9. PTY failure scenarios (runWorker)

## Implementation Recommendation

Add tests in order of priority, starting with high-impact edge cases. Each test should:
1. Set up the specific edge case condition
2. Exercise the code path
3. Verify DB state changes
4. Verify return values/errors
5. Clean up properly

---

**Generated:** 2026-07-06
**Bead:** bf-1j7a
