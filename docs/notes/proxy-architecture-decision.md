# Architecture Decision: Token-Isolating Proxy on ardenone-cluster

Date: 2026-04-03

## Problem

The Telegram bot token must be kept completely inaccessible to Claude Code agents. If the agent can read the token from env, file, or config, it can leak it — as demonstrated when a research agent copied a real bot token from a public repo into committed docs.

## Decision

Split the system into two components separated by Tailscale:

1. **Proxy pod on ardenone-cluster** — holds the Telegram bot token, handles all Telegram API communication
2. **Bridge script on Hetzner EX44** — connects to proxy over Tailscale, manages Claude Code CLI sessions, never sees the token

## Architecture

```
Telegram API  <--token-->  Proxy Pod (ardenone-cluster)  <--Tailscale-->  Bridge Script (EX44)  -->  Claude Code CLI
                           holds token, no public ingress                  no secrets
```

This follows the same pattern as kubectl-proxy: a proxy pod on the cluster exposing a token-free API over Tailscale. The bridge script on EX44 hits something like `http://telegram-proxy:8080/updates` with no credentials in the request.

## Proxy Responsibilities

- Long-poll Telegram for updates (no public ingress required)
- Expose a token-free HTTP API over Tailscale only
- Endpoints: receive messages, send responses, upload/download media
- Token sourced from OpenBao or a sealed secret — never in the manifest

## Bridge Script Responsibilities

- Connect to proxy over Tailscale
- Manage Claude Code CLI sessions (--resume, --cwd, conversation routing)
- Handle media transcription (audio via Whisper, video frame extraction)
- Map Telegram forum topics to separate Claude Code contexts

## Deployment

- Proxy pod: own namespace on ardenone-cluster, deployed via ArgoCD from declarative-config
- Long-running Deployment, not a Job
- Bridge script: systemd unit on EX44

## Security Properties

- Token never leaves ardenone-cluster
- Token never enters EX44 process tree, env, or filesystem
- Tailscale provides mutual authentication — only Tailscale peers can reach the proxy
- No public endpoints on either side
- Claude Code agent has zero access to the token

## Token Storage Options (proxy side)

| Method | Risk |
|---|---|
| OpenBao dynamic secret (fetched at startup, held in memory) | Lowest |
| Sealed secret mounted as volume | Low |
| Env var in pod spec from secret ref | Low |
| Hardcoded in manifest | Unacceptable |

Preferred: OpenBao, since it's already deployed on ardenone-cluster for secrets management.

## Precedent

This is identical in pattern to the existing kubectl-proxy setup:
- `kubectl-ardenone-cluster:8001` — proxy pod holds ServiceAccount credentials, EX44 accesses read-only API over Tailscale
- `telegram-proxy:8080` — proxy pod holds bot token, EX44 accesses token-free message API over Tailscale
