# Bead bf-5z0x: Add Dashboard Build to Argo CI Workflow

## Summary

The dashboard build was missing from the Argo CI workflow template - only the proxy image was being built and pushed.

## Changes Made

Modified `jedarden/declarative-config` repository:

**File:** `k8s/iad-ci/argo-workflows/telegram-claude-bridge-workflowtemplate.yml`

### Changes:
1. **Added `docker-build-dashboard` step** (lines 184-222)
   - Builds dashboard image using `Dockerfile.dashboard`
   - Runs in parallel with the main proxy build
   - Pushes to `ronaldraygun/telegram-claude-bridge-dashboard:{version}` and `:latest`
   - Uses Kaniko executor with caching enabled

2. **Updated `update-declarative-config` step**
   - Added sed pattern to also update dashboard image tags in deployment manifests
   - Ensures both proxy and dashboard images are tagged with the same version

## Result

The CI workflow now builds both images:
- `ronaldraygun/telegram-claude-bridge:{version}` (proxy)
- `ronaldraygun/telegram-claude-bridge-dashboard:{version}` (dashboard)

Both images are versioned together and pushed to Docker Hub on each build.

## Commit

**Repository:** `jedarden/declarative-config`
**Commit:** `7ca6310`
**Message:** "feat: add dashboard build to telegram-claude-bridge CI workflow"
