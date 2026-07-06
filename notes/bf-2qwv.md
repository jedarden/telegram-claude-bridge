# Task 9.2: Startup tmux reconciliation - COMPLETED (Pre-existing Implementation)

## Summary

The startup tmux reconciliation feature was **already fully implemented** in the codebase. The `ReconcileOrphans` method in `internal/bridge/pty_manager.go` (lines 605-709) implements the complete reconciliation logic as specified in the task.

## Implementation Details

### What the feature does:

1. **Lists tmux windows** on startup using: `tmux list-windows -t telegram-bridge -F '#{window_name}'`

2. **Queries live database records**:
   - Sessions with `status='active'` (via `ListAllSessions`)
   - Workers with `status='running'` (direct SQL query)

3. **Parses pane names** to identify type:
   - Session panes: `t{chatID}-{threadID}` (e.g., `t123456-789`)
   - Worker panes: `w-{workerID[:8]}-{timestamp}` (e.g., `w-worker_1_123-1700000000`)
   - Unknown panes: Everything else (e.g., `init-*` subtask panes)

4. **Kills orphaned panes** that have no matching live DB record via `PTYManager.KillPane()`

5. **Logs what was reaped**: `[pty_mgr] reconcile: killing orphan pane {paneTarget} (type={type})`

### Integration point:

In `cmd/bridge/main.go` (lines 94-99):
```go
// Ensure the tmux session exists and reconcile any orphaned panes from prior crashes
if err := ptyMgr.EnsureSession(); err != nil {
    checker.LogError("ensure_tmux_session_failed", "error", err)
    os.Exit(1)
}
ptyMgr.ReconcileOrphans(db)
```

This is called **exactly once on bridge startup**, right after `EnsureSession()`.

## Why this matters

This closes the gap where `PTYManager.idleTimers` (in-memory only) is lost on every restart, permanently orphaning any pane that had a pending idle-kill timer scheduled at crash time.

Without this feature:
- A pane scheduled for idle culling at crash time would remain alive forever
- Subsequent restarts would never reap it because no idle timer exists

With this feature:
- On every startup, we compare actual tmux state against authoritative DB state
- Any pane without a corresponding live DB record is identified as an orphan
- The orphan is killed and logged

## Testing

Added comprehensive tests for the `parsePaneName` function which is critical to reconciliation logic:
- `TestParsePaneName`: Tests parsing of session, worker, and unknown pane formats
- `TestReconcileOrphanBehavior`: Documents expected behavior through scenario-based tests
- `TestPaneKeyGeneration`: Tests pane key format used for DB matching

All tests pass.

## ADR Reference

See ADR 0001 in `docs/adr/` and `plan.md` Phase 9.2 for full context.

## Work Completed

While the feature was already implemented, this task added:
1. Comprehensive test coverage for `parsePaneName` function
2. Documentation of expected reconciliation scenarios
3. Verification that the implementation matches requirements

No code changes to the feature itself were needed.
