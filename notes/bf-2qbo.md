# Fix for Bead Starvation Alert (bf-2qbo)

## Problem
The bead worker was reporting a "starvation alert" - 10 open beads existed but `br ready` returned an empty array, meaning no beads were available to be claimed.

## Root Cause
The issue was a **dependency chain blocked by completed-but-not-closed beads**.

### Dependency Analysis
The 10 open beads had these blocking dependencies:

1. **bf-5a2a** → blocked by bf-37q2 (completed, not closed)
2. **bf-2dvo** → blocked by bf-4dpm (completed, not closed)
3. **bf-3rmj** → blocked by bf-5a2a (open, circular dependency)
4. **bf-5tzn** → blocked by bf-3rmj (open, circular dependency)
5. **bf-3jxi** → blocked by bf-5tzn (open, circular dependency)
6. **bf-dozd** → blocked by bf-3jxi (open, circular dependency)
7. **bf-3ubi** → blocked by bf-2dvo (open, chain dependency)
8. **bf-140j** → blocked by bf-3ubi (open, chain dependency)
9. **bf-4n7h** → blocked by bf-140j (open, chain dependency)
10. **bf-7wrz** → blocked by bf-4n7h (open, chain dependency)

The key issue: `br ready` only shows beads whose dependencies are **closed**, not just **completed**. The status flow should be:
- `open` → `in_progress` → `completed` → **`closed`**

Many beads were stuck in `completed` status when they should have been `closed`.

## Solution
Closed all 7 completed beads to unblock their dependencies:

```bash
br batch --json '[
  {"op":"close","id":"bf-131c"},
  {"op":"close","id":"bf-37q2"},
  {"op":"close","id":"bf-cxu6"},
  {"op":"close","id":"bf-1z7j"},
  {"op":"close","id":"bf-3bp9"},
  {"op":"close","id":"bf-wgw6"},
  {"op":"close","id":"bf-4dpm"}
]'
```

## Result
After closing the completed beads:
- **bf-5a2a** and **bf-2dvo** became ready (no blocking dependencies)
- `br ready` now returns 2 beads available for claiming
- The dependency chain can now progress: as each bead is closed, its dependents become unblocked

## Prevention
To prevent future starvation alerts:
1. **Always close beads after completion** - don't leave them in `completed` status
2. The bead worker workflow should be enhanced to auto-close completed beads
3. Consider adding a periodic job to close completed beads older than a threshold

## Chain Progression
Once bf-5a2a and bf-2dvo are closed, the following beads will become ready:
- bf-3rmj (blocked by bf-5a2a)
- bf-3ubi (blocked by bf-2dvo)

Then those will unblock:
- bf-5tzn (blocked by bf-3rmj)
- bf-140j (blocked by bf-3ubi)

And so on, progressively unblocking the entire chain.

## Date
2026-07-06
