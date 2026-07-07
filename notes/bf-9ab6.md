# Fix for Bead Starvation Alert (bf-9ab6)

## Problem
Needle's Pluck strand was failing to find any beads despite 11 open beads existing in the workspace.

**Root Cause:** Needle workspace configuration pointed to wrong directory.

## Investigation
- **Configured workspace:** `/home/coding/claude-governor` (empty)
- **Actual workspace:** `/home/coding/telegram-claude-bridge` (148 beads, 11 open)
- **Bead counts:**
  - `/home/coding/.beads/beads.db`: 0 beads (empty database)
  - `/home/coding/telegram-claude-bridge/.beads/beads.db`: 148 beads (11 open)

## Solution
Updated needle configuration to point default workspace to correct location:

**File:** `~/.config/needle/config.yaml`
**Change:** `workspace.default` from `/home/coding/claude-governor` to `/home/coding/telegram-claude-bridge`

## Verification
After fix, confirmed beads are accessible:
```bash
cd /home/coding/telegram-claude-bridge && br list --status open --count
# Should return: 11
```

## Related Beads
- bf-6dki: Validate needle worker workspace path configuration
- bf-wpl5: Identify configuration issue causing workspace mismatch
- bf-1n82: Compare configured workspace vs actual beads.db locations

## Date
2026-07-06
