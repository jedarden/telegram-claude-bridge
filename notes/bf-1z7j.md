# Bead bf-1z7j: CI update-manifests step fixes

**Status:** Already completed (commit `a10c7e6` in declarative-config)

This bead's work was already completed on 2026-07-02 in the `jedarden/declarative-config` repository.

## Changes Made (in declarative-config)

### 1. Fixed find pattern in update-declarative-config step
- Changed from: `find . -name "*.yaml"`
- Changed to: `find . \( -name "*.yml" -o -name "*.yaml" \)`
- This ensures manifests with both `.yml` and `.yaml` extensions are found

### 2. Pinned proxy image in deployment.yml
- Changed from: `image: ronaldraygun/telegram-claude-bridge:latest`
- Changed to: `image: ronaldraygun/telegram-claude-bridge:0.3.0`
- Aligns with repo-wide rule to never use `:latest` tags

### 3. Bumped go-test step image
- Changed from: `image: golang:1.23-alpine`
- Changed to: `image: golang:1.25-alpine`
- Matches go.mod requirement for Go 1.25.0

### 4. Pinned kaniko executor images
- Changed from: `image: gcr.io/kaniko-project/executor:latest`
- Changed to: `image: gcr.io/kaniko-project/executor:v1.23.2`
- Applied to both docker-build and docker-build-dashboard templates

## Verification

All changes are present on the remote in commit `a10c7e69893bebba89677570048c810b1664b2ab`.

## Acceptance Criteria Met

- ✅ deployment.yml references semver tag `0.3.0`, not `latest`
- ✅ find command matches both `.yml` and `.yaml` files
- ✅ Next workflow run will auto-bump the image tag
- ✅ go-test uses correct Go version
- ✅ kaniko executor is pinned to versioned tag
