# Proxy Offset Persistence Fix (bf-4vgm)

## Issue
The proxy was losing its Telegram polling offset on pod restart because:
- `readOnlyRootFilesystem: true` was set for security
- No writable volume was mounted for offset persistence
- OFFSET_FILE_PATH pointed to a read-only location

## Solution Applied
The fix was already implemented in `/home/coding/declarative-config/k8s/ardenone-cluster/telegram-bridge/deployment.yml`:

1. Added `emptyDir` volume named `offset-data`
2. Set `OFFSET_FILE_PATH` to `/data/offset.json` 
3. Mounted the volume at `/data`

## emptyDir Characteristics
- Survives container crashes/restarts within the same pod
- Does NOT survive pod reschedules to a different node
- For full resilience across pod reschedules, a PVC would be needed, but emptyDir is sufficient for this use case given:
  - Single replica deployment
  - Offset reprocessing is idempotent (duplicates are filtered by bridge)
  - 24h window is acceptable for transient scenarios

## Changes Committed
- Commit `c44afd2`: "fix: add emptyDir volume for proxy offset persistence"
- Pushed to origin/main on 2026-06-24
