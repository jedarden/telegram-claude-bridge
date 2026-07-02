# Bead bf-3bp9: Update Offset Persistence and Idempotency

## Problem

The proxy pod stores the Telegram polling offset in an `emptyDir` volume at `/data/offset.json`. When the pod is rescheduled (ArgoCD sync, node drain, deletion), the `emptyDir` is wiped and the offset is lost. Telegram then re-delivers all unacknowledged updates, and the bridge has no idempotency to skip replayed updates, causing duplicate Claude prompt processing.

## Decision: Option 2 (Bridge-side Idempotency)

**Chosen approach:** Add bridge-side update deduplication by tracking processed `update_id` values in `bridge.db`.

### Why Option 2 over Option 1?

| Aspect | Option 1 (PVC) | Option 2 (Dedup) |
|--------|----------------|-------------------|
| **Root cause** | Addresses symptom (offset loss) | Addresses symptom (replay) |
| **Protection scope** | Only proxy pod restart | Any replay scenario |
| **Infrastructure change** | Requires k8s manifest change | Pure application code |
| **Defense in depth** | Single point of failure | Redundant protection |
| **Long-term value** | Limited | Higher |

**Key advantages of Option 2:**

1. **Defense regardless of proxy storage**: Even if the proxy offset is lost due to any reason (not just pod rescheduling), the bridge won't process duplicates.

2. **Infrastructure-independent**: No k8s changes required; pure application-level fix that works regardless of deployment configuration.

3. **Future-proof**: If we later add multiple proxy instances, load balancer changes, or other infrastructure modifications, the idempotency protection remains.

4. **Defense in depth**: Even if we implement Option 1 later (PVC for offset persistence), having bridge-side deduplication provides an additional safety layer.

### Why Option 2 is "preferable long-term"

Option 2 represents a more robust architectural pattern: **idempotent message processing**. This is a best practice for any system that consumes messages from an external source, as it protects against not just storage failures but also:

- Network duplicates
- Retry storms
- Load balancer replays
- Manual reprocessing
- Infrastructure migrations

## Implementation

### Database Changes

Added `processed_updates` table (migration version 24):

```sql
CREATE TABLE IF NOT EXISTS processed_updates (
    update_id INTEGER PRIMARY KEY,
    processed_at TEXT NOT NULL DEFAULT (datetime('now'))
);
```

### Code Changes

1. **`internal/bridge/state.go`**:
   - Added `IsUpdateProcessed(ctx, updateID) (bool, error)`
   - Added `MarkUpdateProcessed(ctx, updateID) error`

2. **`internal/bridge/poller.go`**:
   - Added `db *DB` field to `Poller` struct
   - Updated `NewPoller()` to accept `db *DB` parameter
   - Modified `pollLoop()` to:
     1. Check if update was already processed
     2. Skip if duplicate (with log message)
     3. Forward to channel if new
     4. Mark as processed after successful send

3. **`cmd/bridge/main.go`**:
   - Updated `NewPoller()` call to pass `db` parameter

4. **`internal/bridge/poller_test.go`**:
   - Updated existing tests to pass `nil` for DB parameter
   - Added `TestPoller_DeduplicationFiltersDuplicateUpdateIDs()` test

### Test Coverage

Unit test `TestPoller_DeduplicationFiltersDuplicateUpdateIDs` verifies:
- First batch of updates is processed and marked
- Second batch of same updates (simulating replay) is filtered out
- Channel receives only one copy of each unique update

### Performance Considerations

- **Database writes**: One `INSERT` per update (negligible; updates are already rate-limited to 30/min per user)
- **Database reads**: One `SELECT` per update (indexed by primary key; very fast)
- **Storage**: `update_id` is monotonically increasing; we may add periodic cleanup of old entries (e.g., >7 days) in a future bead

## Acceptance Criteria Met

✅ The bridge provably ignores replayed update_ids (unit test: feed duplicate update batch to poller, assert single dispatch)

✅ No duplicate Claude prompt processing after a simulated proxy restart with lost offset

## Future Considerations

1. **Periodic cleanup**: The `processed_updates` table will grow indefinitely. Consider adding a cleanup job to remove entries older than N days (since Telegram's update queue is relatively short-lived).

2. **Option 1 still viable**: We may still implement Option 1 (PVC) in the future as a defense-in-depth measure, but it's no longer critical since Option 2 provides the core protection.

3. **Monitoring**: Consider adding a metric for "duplicate updates skipped" to monitor replay frequency.

## Related Files

- `internal/bridge/state.go` - Database schema and methods
- `internal/bridge/poller.go` - Update filtering logic
- `internal/bridge/poller_test.go` - Deduplication tests
- `cmd/bridge/main.go` - Wire DB to poller
- `docs/plan/plan.md` - Risk Register row 1
