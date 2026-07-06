# WorkerPool Edge Cases Analysis

## Overview
Analysis of `internal/bridge/worker_pool.go` edge cases not covered by existing integration tests in `internal/bridge/integration_test.go`.

## DB State Transition Edge Cases

### 1. **Worker Status Race Condition**
**Category:** DB State / Concurrency  
**Why:** When a worker finishes and updates to "done"/"failed", a concurrent SpawnWorker call might count it as "running" and incorrectly enforce the limit.

**Scenario:** 
1. SpawnWorker checks running count (sees N workers)
2. Worker finishes (updates DB to "done")
3. SpawnWorker creates N+1 worker
4. Actual running count should be N (one just finished)

**Missing Test:** Verify that `CountRunningWorkers` excludes non-"running" statuses and handle race between counting and worker completion.

---

### 2. **Index Persistence After Bridge Restart**
**Category:** DB State / Architecture  
**Why:** The `nextIndex` map is in-memory only. After a bridge restart, indices restart at 1, causing potential confusion in orchestrator injection (multiple workers with index 1).

**Scenario:**
1. Spawn workers 1, 2, 3 (indices 1, 2, 3)
2. Bridge restarts (nextIndex map cleared)
3. Spawn new worker → gets index 1 again
4. Orchestrator injection logic may misinterpret which worker is which

**Missing Test:** Document this behavior limitation; test verifies index resets after WorkerPool recreation.

---

### 3. **Worker ID Collision with Rapid Spawning**
**Category:** DB State / Validation  
**Why:** Worker IDs use `worker_{threadID}_{timestamp}`. With nanosecond precision and rapid spawning, collisions are theoretically possible (though unlikely).

**Scenario:**
1. Two workers spawned in same nanosecond for same thread
2. Generate identical worker IDs
3. DB CreateWorker fails on UNIQUE constraint

**Missing Test:** Verify duplicate worker ID handling (should error gracefully, not corrupt state).

---

### 4. **Stale "running" Workers After Crash**
**Category:** DB State / Cleanup  
**Why:** If the bridge crashes while workers are "running", they never transition to "done"/"failed". These accumulate and permanently reduce capacity.

**Scenario:**
1. Spawn 5 workers (status: "running")
2. Bridge crashes (SIGKILL)
3. On restart, CountRunningWorkers still sees 5
4. MaxWorkers limit incorrectly enforced (0 capacity available)

**Missing Test:** Verify behavior on startup with stale "running" workers; need cleanup mechanism or advisory note.

---

### 5. **FinishedAt Timestamp Not Verified**
**Category:** DB State / Validation  
**Why:** The `finishWorker` method updates status to "done"/"failed" but tests don't verify that `FinishedAt` timestamp is actually set by the DB.

**Scenario:**
1. Worker completes successfully
2. finishWorker calls UpdateWorker with status "done"
3. DB should set FinishedAt via datetime('now')
4. Tests don't verify FinishedAt is set correctly

**Missing Test:** Verify `FinishedAt` is set and is greater than `StartedAt` for both success and failure cases.

---

### 6. **Result Field NULL vs Empty String Handling**
**Category:** DB State / Validation  
**Why:** The `scanWorker` function uses `sql.NullString` for Result. When created, Result is NULL. Tests don't verify the NULL vs empty string distinction.

**Scenario:**
1. Create worker - Result should be NULL
2. finishWorker with empty result - Result should be "" (empty string, not NULL)
3. Queries using `WHERE result IS NULL` vs `WHERE result = ''` behave differently

**Missing Test:** Verify NULL vs empty string handling throughout worker lifecycle.

---

## Validation Logic Edge Cases

### 7. **Tool Restrictions with spawn_worker Edge Case**
**Category:** Validation / Tool Restrictions  
**Why:** The code always adds "spawn_worker" to disallowed tools (line 136), but there's no test verifying this actually prevents recursive worker spawning.

**Scenario:**
1. Worker spawned with certain allowed/disallowed tools
2. Worker Claude instance tries to use spawn_worker
3. Should be blocked by tool restriction
4. Current code disallows spawn_worker but doesn't test enforcement

**Missing Test:** Verify spawn_worker is always in disallowed list and prevents recursive spawning.

---

### 8. **Permission Resolution Edge Cases**
**Category:** Validation / Permissions  
**Why:** `resolvePermissionArgs(group)` is called in runWorker (line 123) but there are no tests for permission edge cases (nil Group, empty permissions).

**Scenario:**
1. Group with nil or empty permission fields
2. resolvePermissionArgs returns unexpected results
3. Claude CLI fails on invalid permission args

**Missing Test:** Test with various Group permission configurations (nil, empty, invalid format).

---

### 9. **Model Fallback Chain Edge Cases**
**Category:** Validation / Configuration  
**Why:** Three-tier model fallback (input → group → defaultSessionModel). Only basic case is tested; edge cases in the chain are missing.

**Scenario:**
1. Input model explicitly empty string `""` (vs omitted)
2. Group DefaultModel is whitespace-only `"  "`
3. Both input and group models are invalid model names
4. defaultSessionModel constant is empty

**Missing Test:** Verify fallback behavior with malformed/empty model values at each tier, including full priority chain testing.

---

### 10. **MaxWorkers Negative Value Handling**
**Category:** Validation / Configuration  
**Why:** The code checks `if maxWorkers <= 0 { maxWorkers = 5 }`. Tests cover `MaxWorkers = 0` but don't test negative values.

**Scenario:**
1. Group.MaxWorkers = -1 (from manual DB modification or migration bug)
2. Should default to 5, not reject workers
3. Negative values could be treated as "unlimited" in some contexts

**Missing Test:** Verify negative MaxWorkers values default to 5.

---

### 11. **ConcurrentIndexing Under High Concurrency**
**Category:** Validation / Concurrency  
**Why:** Existing test uses 10 concurrent workers, but production might see higher concurrency. No test verifies no gaps in index sequence.

**Scenario:**
1. Spawn 100+ workers concurrently
2. Verify indices are 1..N exactly (no missing numbers)
3. Verify mutex guarantees sequential assignment under load

**Missing Test:** High concurrency test (50+ workers) with sequence verification.

---

## Error Handling Edge Cases

### 12. **PTY Manager Failures During Execution**
**Category:** Error Handling / PTY Integration  
**Why:** runWorker calls multiple PTY methods. Only some failure modes are tested; no comprehensive PTY failure coverage.

**Scenario:**
1. SpawnPane fails (pty unavailable)
2. WaitForStartup fails (Claude doesn't start)
3. InjectPrompt fails (pty write error)
4. WaitForResponse fails (timeout, pty dies)

**Missing Test:** Test each PTY failure point verifies proper error propagation to finishWorker with appropriate error messages.

---

### 13. **Telegram Sender Timeout in finishWorker**
**Category:** Error Handling / Network  
**Why:** finishWorker uses 10-second timeout for Telegram send (line 204). No test verifies timeout behavior or error handling.

**Scenario:**
1. Worker completes successfully
2. Telegram send times out after 10s
3. Should log error but not panic
4. Worker result should still be injected into SessionManager

**Missing Test:** Verify timeout handling, error logging, and pending result injection despite send failure.

---

### 14. **Context Cancellation During Worker Execution**
**Category:** Error Handling / Concurrency  
**Why:** Workers run in goroutines with background context. No test verifies behavior when parent context is cancelled.

**Scenario:**
1. Worker spawned with context
2. Parent context cancelled (e.g., session closed)
3. Worker goroutine continues execution (no cancellation propagation)
4. finishWorker may operate on closed/deleted resources

**Missing Test:** Verify worker cleanup when parent context cancelled; should kill worker goroutine or prevent finishWorker side effects.

---

### 15. **Database Errors in finishWorker Are Logged But Swallowed**
**Category:** Error Handling / DB  
**Why:** Line 181 logs DB errors but doesn't propagate them. This could mask DB corruption or connectivity issues.

**Scenario:**
1. Worker completes
2. DB UpdateWorker fails (connection lost, disk full)
3. Error logged but worker result still injected
4. DB state inconsistent (worker still "running")

**Missing Test:** Verify DB error handling - should either retry, propagate error, or have clear recovery mechanism.

---

### 16. **SessionManager Nil Reference in finishWorker**
**Category:** Error Handling / Null Safety  
**Why:** finishWorker accesses `wp.sessionMgr` (line 211) without nil check. If WorkerPool created with nil SessionManager, this would panic.

**Scenario:**
1. WorkerPool created with nil SessionManager (shouldn't happen but defensive programming)
2. finishWorker called
3. Panics on nil pointer dereference

**Missing Test:** Verify nil SessionManager handling (add defensive nil check or document invariant).

---

### 17. **CountRunningWorkers Context Cancellation**
**Category:** Error Handling / Context  
**Why:** Tests cover DB closed but don't test context cancellation during counting.

**Scenario:**
1. SpawnWorker starts, calls CountRunningWorkers
2. Context cancelled during DB query
3. Should return error, not panic or return stale count

**Missing Test:** Verify context cancellation during CountRunningWorkers is handled correctly.

---

### 18. **Database Lock Contention**
**Category:** Error Handling / Concurrency  
**Why:** No tests for DB lock scenarios during concurrent operations.

**Scenario:**
1. Multiple workers finishing simultaneously
2. DB locked by exclusive transaction
3. finishWorker UpdateWorker calls fail with "database is locked"

**Missing Test:** Verify DB lock errors are handled gracefully (retry or log error).

---

## Summary

### Critical (High Priority)
1. **Worker Status Race Condition** - Can cause incorrect limit enforcement
2. **Stale "running" Workers After Crash** - Capacity leak
3. **PTY Manager Failures During Execution** - Incomplete error coverage
4. **Worker ID Collision with Rapid Spawning** - Can cause worker spawn failure

### Important (Medium Priority)
5. **Index Persistence After Bridge Restart** - Confusion in injection
6. **Tool Restrictions with spawn_worker** - Recursive spawning risk
7. **Telegram Sender Timeout** - Silent failure in notification
8. **FinishedAt Timestamp Not Verified** - Monitoring query failures
9. **Result NULL vs Empty String** - Query inconsistency

### Nice to Have (Low Priority)
10. **Permission Resolution Edge Cases** - Defensive programming
11. **Model Fallback Chain Edge Cases** - Defensive programming
12. **MaxWorkers Negative Value** - Configuration edge case
13. **Context Cancellation During Worker Execution** - Graceful shutdown
14. **Database Errors in finishWorker** - Observability
15. **SessionManager Nil Reference** - Defensive programming
16. **ConcurrentIndexing Under High Concurrency** - Stress testing
17. **CountRunningWorkers Context Cancellation** - Context handling
18. **Database Lock Contention** - Concurrency handling

## Testing Recommendations

1. **Add race detector tests** for concurrent SpawnWorker + finishWorker
2. **Mock PTY Manager** to test all failure modes without requiring tmux
3. **Add database fault injection** to test error handling at each DB call
4. **Document architectural limitations** (in-memory index, no stale worker cleanup)
5. **Consider adding health check** for stale "running" workers on startup

## Test Infrastructure Gaps

Current tests use:
- Mock HTTP server for sender (always succeeds)
- Test doubles for SessionManager and PTYManager
- In-memory SQLite DB

Missing:
- Failure injection for Sender (timeouts, errors)
- Real PTY/spawn simulation
- Context cancellation scenarios
- Database corruption/lock scenarios
- High-concurrency stress testing (100+ workers)
- Database fault injection (connection lost, disk full)

---

**Generated:** 2026-07-06  
**Bead:** bf-1j7a  
**Component:** WorkerPool (internal/bridge/worker_pool.go)
