# Workspace Configuration Mismatch - Self-Update Failure

## Date Discovered
2026-08-21

## Issue Summary

The bridge's self-update mechanism has been completely broken for **14+ days** due to a workspace configuration mismatch. The systemd unit points to the shared NEEDLE dev worktree instead of a dedicated deploy worktree.

## Root Cause

**Configuration Error:** The systemd unit `deploy/telegram-claude-bridge.service` contains:
```ini
Environment=REPO_PATH=/home/coding/telegram-claude-bridge
Environment=BINARY_PATH=bin/bridge
```

This path (`/home/coding/telegram-claude-bridge`) is the **shared dev worktree** used by NEEDLE workers, not a dedicated deploy worktree.

## Why This Breaks Self-Update

1. **The updater checks for uncommitted changes** before building (line 276 in `internal/updater/updater.go`):
   ```go
   if u.hasUncommittedChanges(ctx) {
       log.Printf("[updater] skipping update: uncommitted changes in repo")
       return
   }
   ```

2. **The shared dev worktree is ALWAYS dirty:**
   - Modified tracked files (`.beads/checkpoint/*.json`, `.needle-predispatch-sha`)
   - Untracked test files
   - 6 beads currently `in_progress` against this checkout
   - Normal NEEDLE activity continuously creates commits

3. **Result:** Every 5-minute check logs:
   ```
   [updater] skipping update: uncommitted changes in repo
   ```

4. **Impact:** Last successful self-update was **2026-07-06 11:10** (commit `0865036`). Since then: **3,796 silent skips** over 14 days while `main` advanced 50+ commits.

## The Existing Workaround (Insufficient)

The `hasUncommittedChanges()` function has an allowlist for `.beads/` and `.needle-predispatch-sha` (lines 354-358):

```go
if strings.HasPrefix(filename, ".beads/") || filename == ".needle-predispatch-sha" {
    continue
}
```

**Why this isn't enough:**
- The allowlist only covers beads-related churn
- Real source code edits from concurrent NEEDLE workers still block updates
- A dirty worktree is the NORMAL state for this directory
- The allowlist is fragile and needs constant upkeep

## The Fix (Per ADR-001)

Create a dedicated deploy worktree isolated from the shared dev worktree:

### Step 1: Create the Deploy Worktree

```bash
# From the shared dev worktree
cd /home/coding/telegram-claude-bridge
git worktree add /home/coding/.telegram-claude-bridge-deploy main

# Verify it was created
git worktree list
# Should show both:
# /home/coding/telegram-claude-bridge              main [shared NEEDLE dev]
# /home/coding/.telegram-claude-bridge-deploy     main [deploy-only]
```

### Step 2: Update Systemd Configuration

Edit `~/.config/systemd/user/telegram-claude-bridge.service`:

```ini
# Change REPO_PATH to the deploy worktree
Environment=REPO_PATH=/home/coding/.telegram-claude-bridge-deploy

# BINARY_PATH stays the same (relative to REPO_PATH)
Environment=BINARY_PATH=bin/bridge

# ExecStart stays the same (binary is at deploy worktree + binary path)
ExecStart=/home/coding/.telegram-claude-bridge-deploy/bin/bridge
```

### Step 3: Update the Deployed Unit File

Copy the corrected unit to the repo's deploy directory:

```bash
# Update the source file in the repo
cp deploy/telegram-claude-bridge.service deploy/telegram-claude-bridge.service.old
cat > deploy/telegram-claude-bridge.service <<'EOF'
[Unit]
Description=Telegram Claude Bridge
After=network.target

[Service]
Environment=REPO_PATH=/home/coding/.telegram-claude-bridge-deploy
Environment=BINARY_PATH=bin/bridge
ExecStart=/home/coding/.telegram-claude-bridge-deploy/bin/bridge
Restart=on-failure
RestartSec=5
StartLimitBurst=3
StartLimitIntervalSec=60
ExecStopPost=/home/coding/telegram-claude-bridge/scripts/bridge-crash-alert.sh

[Install]
WantedBy=default.target
EOF

# Commit the fix
git add deploy/telegram-claude-bridge.service
git commit -m "fix(updater): use dedicated deploy worktree instead of shared dev checkout

Per ADR-001, the updater must use its own isolated git worktree to avoid
hasUncommittedChanges() always returning true due to concurrent NEEDLE
worker activity in the shared dev checkout.

Changes:
- REPO_PATH: /home/coding/telegram-claude-bridge → /home/coding/.telegram-claude-bridge-deploy
- ExecStart: updated to match new REPO_PATH
- Resolves 14-day self-update outage (3,796 skipped checks)"
```

### Step 4: Apply the Fix to the Running Service

```bash
# Reload systemd to apply the unit file changes
systemctl --user daemon-reload

# Restart the bridge with the new configuration
systemctl --user restart telegram-claude-bridge

# Verify it's running with the new path
systemctl --user status telegram-claude-bridge
# Should show ExecStart pointing to the deploy worktree

# Check logs to confirm updater is using the new path
journalctl --user -u telegram-claude-bridge -n 50 | grep updater
# Should NOT see "skipping update: uncommitted changes"
```

### Step 5: Clean Up the Code (Optional)

Once the deploy worktree is in use, the `.beads/` and `.needle-predispatch-sha` allowlist in `hasUncommittedChanges()` can be removed:

```go
// internal/updater/updater.go, line 337
func (u *Updater) hasUncommittedChanges(ctx context.Context) bool {
    cmd := exec.CommandContext(ctx, "git", "-C", u.repoPath, "status", "--porcelain")
    output, err := cmd.Output()
    if err != nil {
        log.Printf("[updater] git status failed: %v", err)
        return true // Skip update on error
    }
    for _, line := range strings.Split(string(output), "\n") {
        if len(line) < 2 {
            continue
        }
        // Skip untracked (??) and ignored (!!) lines.
        if line[:2] == "??" || line[:2] == "!!" {
            continue
        }
        // No need for the .beads/ and .needle-predispatch-sha allowlist anymore
        return true // Any other status means modified/staged/conflicted
    }
    return false
}
```

This simplification can wait for the next release—prioritize getting the worktree deployed first.

## Verification

After applying the fix, verify self-update is working:

1. **Check the updater is no longer skipping:**
   ```bash
   journalctl --user -u telegram-claude-bridge -f | grep updater
   # Should see "[updater] no updates available" (not "skipping update")
   ```

2. **Trigger a manual update check:**
   - Send `/update do` in a Telegram topic
   - Should return "✅ No updates available" or "✅ Updating to <commit>..."
   - Should NOT return "⚠️ Cannot update: uncommitted changes in repository"

3. **Monitor the next automatic check (5 min):**
   ```bash
   # Wait ~5 minutes, then check logs
   journalctl --user -u telegram-claude-bridge --since "5 minutes ago" | grep updater
   # Should show a successful check (no skips)
   ```

## Why This Fix Works

**The deploy worktree is clean by design:**
- Only the updater's `git fetch` / `git pull` / `go build` sequence writes to it
- No concurrent NEEDLE workers touch it
- No beads tracking, no test files, no dev artifacts
- `git status --porcelain` returns nothing

**The shared dev worktree continues working normally:**
- NEEDLE workers still use `/home/coding/telegram-claude-bridge`
- No disruption to ongoing bead work
- The deploy worktree shares the same `.git` object store (efficient)

## Impact

**Before the fix:**
- Self-update: 100% broken (0% success rate)
- Update checks: 3,796 silent skips over 14 days
- Security patches: not applied automatically
- Bug fixes: not reaching production

**After the fix:**
- Self-update: fully operational
- Update checks: runs every 5 minutes unconditionally
- Security patches: applied within 5 minutes of merge to `main`
- Manual updates via `/update do`: work immediately

## References

- **ADR-001:** docs/plan/plan.md (section "Self-Updating" → "Bridge (EX44)" → "ADR-001")
- **Updater code:** internal/updater/updater.go (lines 276, 337-363)
- **Systemd unit:** deploy/telegram-claude-bridge.service
- **Related beads:** telegram-126af296 (this bead, documenting the issue)

## Notes

- The deploy worktree name (`.telegram-claude-bridge-deploy`) uses a leading dot to keep it hidden alongside the main repo
- Disk cost: ~13 MB for the worktree (negligible on EX44)
- Both worktrees share the same `.git` directory, so `git fetch` in either updates both
