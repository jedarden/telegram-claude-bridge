# Task 9.5: Worker result injection on next prompt

## Summary

This task was already fully implemented in the codebase. The feature allows completed worker results to be prepended to the next prompt sent to the orchestrator, ensuring the orchestrator always has access to worker outputs.

## Implementation verification

### Data flow components (all present):

1. **`pendingWorkerResults` map** in `SessionManager` (line 410)
   - Type: `map[topicKey][]WorkerResult`
   - Keyed by `(chatID, threadID)` via `topicKey`

2. **`WorkerResult` struct** (lines 382-387)
   ```go
   type WorkerResult struct {
       Index  int    // Worker index (1-based for display)
       Model  string // Model used by the worker
       Result string // Worker's output
       Error  string // Worker's error message (empty if successful)
   }
   ```

3. **`AddPendingWorkerResult` method** (lines 3019-3026)
   - Appends results to the map under `SessionManager.mu` lock
   - Accumulates multiple results for the same topic

4. **Worker goroutines** call `AddPendingWorkerResult` on completion:
   - `worker_pool.go:307` - After shell command execution
   - `subtask_orchestrator.go:210,217` - After subtask completion

5. **`processBatch`** drains and prepends results (lines 2206-2226)
   - Acquires lock, drains pending results for the topic
   - Formats with proper error handling
   - Prepends to prompt with `[Worker results from previous invocation]` header

### Edge cases handled:

- Worker completes while orchestrator is still running: Results are queued in `pendingWorkerResults` and injected on the next `processBatch` call
- Multiple workers complete before next message: Results accumulated via `append` in `AddPendingWorkerResult`, all prepended together
- Worker fails: Error summary included in injection format (`Worker N (model: X) FAILED: <error>`)

## Testing

- All worker-related tests pass
- Build succeeds with no errors
