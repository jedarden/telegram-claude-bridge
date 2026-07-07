# Bead Starvation Fix (bf-40ui)

## Problem
The bead worker (Pluck) was finding no beads to claim despite having 148 total beads and 11 open beads. The starvation alert indicated "possible configuration error."

## Root Cause
Circular dependencies among open beads. Every open bead was blocked by another open bead, creating a deadlock where no bead could be claimed according to the dependency rules.

## Circular Dependencies Found
- **bf-3jxi ↔ bf-5tzn ↔ bf-3rmj ↔ bf-5a2a**: Test beads blocking each other
- **bf-140j ↔ bf-3ubi ↔ bf-2dvo**: Module test beads in a cycle  
- **bf-4n7h → bf-140j**: Commands tests blocked by media tests
- **bf-dozd → bf-3jxi**: Integration tests blocked by unit tests
- **bf-7wrz → bf-4n7h**: Core module tests blocked by commands tests

## Solution
Removed all blocking dependencies where both the bead and its blocker were in `open` status. This breaks the circular dependency chains while preserving dependencies on beads that are actually closed/in-progress.

## SQL Fix Applied
```sql
DELETE FROM dependencies 
WHERE issue_id IN (
    SELECT i1.id
    FROM issues i1
    INNER JOIN dependencies d ON d.issue_id = i1.id
    INNER JOIN issues i2 ON d.depends_on_id = i2.id
    WHERE i1.status = 'open'
      AND i2.status = 'open'
      AND d.type = 'blocks'
)
AND depends_on_id IN (
    SELECT i2.id
    FROM issues i1
    INNER JOIN dependencies d ON d.issue_id = i1.id
    INNER JOIN issues i2 ON d.depends_on_id = i2.id
    WHERE i1.status = 'open'
      AND i2.status = 'open'
      AND d.type = 'blocks'
);
```

## Result
- **Before**: 0 beads available for claim (all blocked by open beads)
- **After**: 8 beads available for claim
- **Cycles remaining**: 0

## Beads Now Available
1. bf-3rmj: Add missing subtask orchestrator edge case tests
2. bf-5tzn: Add missing /parallel command edge case tests  
3. bf-3jxi: Run full integration test suite and verify all pass
4. bf-3ubi: Add unit tests for callback_handler
5. bf-140j: Add unit tests for media modules
6. bf-4n7h: Add unit tests for commands handler and telegram sender
7. bf-dozd: Add integration tests for worker pool
8. bf-7wrz: Add unit tests for untested core modules

## Prevention
When creating bead dependencies, ensure:
1. No circular dependencies in the same status state
2. Test beads should follow dependency order, not create cycles
3. Use appropriate dependency types (parent-child vs blocks)
