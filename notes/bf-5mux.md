# OpenBao Bot Token Path Reconciliation (bf-5mux)

## Issue
The plan.md and the deployed ExternalSecret specified different OpenBao paths for the Telegram bot token.

## Resolution

### 1. Plan.md Update
Updated `/home/coding/telegram-claude-bridge/docs/plan/plan.md` line 152:

**Before:**
```
OpenBao path: `secret/data/telegram-claude-bridge/bot-token`
```

**After:**
```
OpenBao path: `ardenone-cluster/telegram/ardenone_bot` (key in ExternalSecret: `remoteRef.key`)
```

### 2. ExternalSecret Verification
Verified the actual deployed path in `/home/coding/declarative-config/k8s/ardenone-cluster/telegram-bridge/external-secret.yml`:
```yaml
remoteRef:
  key: ardenone-cluster/telegram/ardenone_bot
  property: token
```

This path is the source of truth as it's what's actually deployed in the cluster.

### 3. Bead bf-cxu6 Update
Added comment to bead bf-cxu6 documenting the correct OpenBao path reference for future implementation work.

## Correct Path
The canonical OpenBao path for the Telegram bot token is:
```
ardenone-cluster/telegram/ardenone_bot
```

When implementing direct OpenBao fetching (as planned in bead bf-cxu6), this is the path that should be used.
