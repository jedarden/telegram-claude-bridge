# Fix for bf-2mn1: Starvation Alert - Beads Invisible to Worker

## Problem
The needle worker (Pluck strand) was unable to find any open beads despite the workspace containing 11 open beads. The error message was:

```
Starvation Alert: beads invisible to worker
Workspace: default
Total beads: 146
Open: 11
In-progress: 0
Pluck found none
```

## Root Cause
The needle configuration at `~/.needle/config.yaml` had the `workspace.default` set to `/home/coding/claude-governor`, which contained 1003 beads but 0 open beads. The current workspace `/home/coding/telegram-claude-bridge` had 11 open beads but was not being checked.

## Solution
Updated the needle configuration to point to the correct workspace:

```yaml
workspace:
  default: /home/coding/telegram-claude-bridge
```

## Files Changed
- `~/.needle/config.yaml` - Updated workspace.default from `/home/coding/claude-governor` to `/home/coding/telegram-claude-bridge`

## Impact
The needle worker (Pluck strand) will now correctly discover and process open beads from the telegram-claude-bridge workspace.

## Date
2026-07-06
