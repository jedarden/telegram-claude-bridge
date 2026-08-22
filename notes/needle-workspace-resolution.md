# How NEEDLE resolves the workspace path and locates `.beads`

Reference for the bead-starvation investigation (beads `telegram-fd5fc508`,
`telegram-9adb09a9`, `telegram-01787e6d`). All source references are to the
checkouts on ex44 as of 2026-08-21: `/home/coding/NEEDLE` (needle) and
`/home/coding/bead-rs` (bead CLI). Paths below like `src/...` are relative to
those roots.

## TL;DR

Needle never opens `beads.db` itself and never computes a `.beads` path in the
execution path. It resolves a **workspace path** (a directory), then runs the
bead CLI with `current_dir = <that directory>`, and the **bead CLI** finds
`.beads` by walking **up** from its cwd. So the real relationship is:

```
resolved workspace path
  → pinned cwd of every `bead` subprocess        (needle side)
    → nearest ancestor (incl. cwd) with .beads/config.json   (bead-rs side)
      → <that ancestor>/.beads/beads.db
```

`workspace/.beads` as a plain join is only what needle's *own sanity checks*
assume (`has_valid_bead_store`, `needle doctor`). Whenever
`<workspace>/.beads/config.json` does not exist but an ancestor's does, the
store actually used silently differs from the workspace needle thinks it
configured. That divergence — plus a `workspace.default` pointing somewhere
other than the intended repo — is the mismatch class behind the starvation.

## Layer 1 — Resolving the workspace path (needle)

For `needle run`, `cmd_run` (`src/cli/mod.rs:501-507`):

1. `--workspace W` given → `W.canonicalize()` (raw `W` if canonicalize fails).
2. Otherwise → `ConfigLoader::load_global()` → `workspace.default` from
   `~/.config/needle/config.yaml`.
3. If that file does not exist → `Config::default()` →
   `WorkspaceConfig::default_workspace()` = **`std::env::current_dir()` of the
   needle process** (`src/config/mod.rs:462-465`).

The full config hierarchy (`load_resolved`, `src/config/mod.rs:6819-6860`):

```
defaults → ~/.config/needle/config.yaml → <workspace>/.needle.yaml
        → NEEDLE_* env vars → CLI args
```

Key details:

- A workspace-level `.needle.yaml` (`load_workspace`, `src/config/mod.rs:6397`)
  **cannot** set `workspace.default`/`workspace.home` — only `labels` and a few
  other sections are overridable there; path fields are global-only and
  non-overridable keys produce warnings (`WorkspaceLabelsOverride`,
  `src/config/mod.rs:431-446`).
- `needle config` resolves its workspace as plain cwd (`src/cli/mod.rs:3225`);
  `needle doctor` uses `--workspace` or the global default
  (`src/cli/mod.rs:4116`).
- tmux launch (`launch_in_tmux`, `src/cli/mod.rs:1232-1300`) forwards
  `--workspace` to the inner `NEEDLE_INNER=1` worker **only if it was given**.
  `tmux new-session` is invoked without `-c`, so an un-flagged inner worker
  inherits the launching process's cwd and re-resolves per rules 2-3 above.
  Net effect: with no `--workspace` and no global `workspace.default`, the
  store binding depends on *where `needle run` happened to be invoked*.

## Layer 2 — Workspace → bead store binding (needle)

`open_configured` (`src/bead_store/mod.rs:44-88`) is the production entry:

1. Requires an explicit `bead_cli.backend` in `<workspace>/.needle.yaml`
   (`backend: bead-rs` or `bead-forge`). `auto` **bails** with an error naming
   the expected file — executable discovery alone is never store ownership.
2. Resolves the CLI binary (`resolve_bead_cli`, `src/config/mod.rs:595`):
   explicit `bead_cli.path` → `PATH` → `~/.local/bin/bead` →
   `/usr/local/cargo/bin/bead`.
3. Verifies identity (`--version`) and, for bead-rs, capabilities
   (`capabilities --profile native-v1`, `src/bead_store/mod.rs:242-250`) —
   both probes also run with `current_dir(workspace)`.
4. Constructs `CliBeadStore { workspace, binary, .. }`. **Every** subsequent
   backend operation — list, claim, show, update, close — is spawned with
   `.current_dir(&workspace)` (`src/bead_store/cli_store.rs:200-212`).

## Layer 3 — cwd → `.beads` (bead-rs)

`WorkspaceConfig::probe` (`bead-rs/src/store/mod.rs:63-92`):

- Start at cwd; if `<dir>/.beads/config.json` exists, that dir is the workspace
  root; otherwise move to the **parent** and repeat until found or exhausted.
- DB path = `<root>/.beads/beads.db` (`store/mod.rs:113`).
- `config.json` present but DB schema missing is a distinct *recoverable*
  `Uninitialized` state (fresh clone: identity is git-tracked, db is not).

Because needle pins cwd to the workspace, the walk normally terminates
immediately at the workspace itself. **But if `<workspace>/.beads/config.json`
is absent, bead-rs silently climbs** — e.g. a workspace resolved to
`/home/coding` (or any directory under it without its own store) binds
`/home/coding/.beads` whenever that store exists.

> **Update 2026-08-22 — the silent climb is gone (bead-rs R030).** The
> behavior described above was accurate when this doc was written (verified
> against source the morning of 2026-08-21); commit `2440b90` (R030, later
> that day) changed `WorkspaceConfig::probe` to **stop at the first `.beads`
> directory on the walk and fail** if it lacks `config.json`
> (`foreign_workspace_message`, "discovery does not continue past the first
> .beads directory"), rather than climbing past it. A mis-resolved process now
> gets a loud "not a bead-rs workspace" error instead of silently binding an
> ancestor store. The trap this doc describes is therefore historical; the
> operational summary and triage runbook now live in
> `docs/notes/bead-starvation-troubleshooting.md`.

## The asymmetry — where mismatches come from

Needle's own `.beads` logic points **down / exact**, bead-rs's points **up**:

| Consumer | Algorithm | Code |
|---|---|---|
| Store binding (execution) | bead-rs walk-**up** from pinned cwd | `bead-rs/src/store/mod.rs:63-92` |
| `has_valid_bead_store` | exact dir check `workspace/.beads` (no walk-up) | `src/bead_store/mod.rs:1046-1048` |
| `needle doctor` | exact join `workspace_root/.beads` | `src/cli/mod.rs:4125` |
| Explore discovery | immediate **children** of `strands.explore.workspace_root` containing `.beads/` — one level, downward only | `src/strand/explore.rs:357-393` |

So a `.beads` directory at `$HOME` is invisible to Explore (it's the root, not
a child) yet is exactly what the walk-up binds whenever a needle process ends
up with cwd=`$HOME`-ish. HOOP side-effects reinforce the trap: heartbeats and
events are written to `<workspace>/.beads/heartbeats.jsonl` /
`<workspace>/.beads/events.jsonl` (`src/hoop_hooks.rs:58-62, 101-105`), so a
worker whose workspace resolved to `/home/coding` *creates* `.beads/` there
even if no store was intended.

## Which cwd does the dispatched agent get?

The agent process's cwd determines where the agent's **own** `bead` commands
resolve — the same walk-up applies to them.

- Invoke templates are `cd {workspace} && <agent> …` (`src/dispatch/mod.rs:627`
  and siblings).
- `{workspace}` = `dispatch_ws` (`src/worker/mod.rs:2514-2519`):
  `bead.workspace` if set, else `config.workspace.default`.
- `Bead.workspace` is deserialized from the `source_repo` JSON field
  (`src/types/mod.rs:905-907`); unset means `""` or `"."`
  (`is_workspace_unset`, `src/worker/mod.rs:5262-5265`).
- **bead-rs `create` never sets `source_repo`** (the INSERT at
  `bead-rs/src/service/issues.rs:67` omits the column, and the JSON output
  skips nulls — verified live: `bead list --json` in this repo emits no
  `source_repo` key). So for bead-rs stores the agent cwd is effectively
  **always `config.workspace.default`**, except for Explore-roamed beads where
  the pre-claim workspace is backfilled after claim
  (`src/worker/mod.rs:2008-2018`).
- The worker's own `current_workspace` starts as `config.workspace.default`
  (`src/worker/mod.rs:798, 854`).

## Environment variables and config overrides

| Knob | Effect | Where |
|---|---|---|
| `needle run --workspace W` | highest precedence for the run; canonicalized | `src/cli/mod.rs:501-503` |
| `workspace.default` in `~/.config/needle/config.yaml` | global default workspace | `src/config/mod.rs:427-435` |
| `workspace.default` in `<workspace>/.needle.yaml` | **ignored** (non-overridable; warns) | `src/config/mod.rs:6420-6460` |
| `NEEDLE_WORKSPACE__DEFAULT` | env override of `workspace.default` (`__` → `.`) | `apply_env_overrides`, `src/config/mod.rs:6606+` |
| `NEEDLE_WORKSPACE__HOME` | env override of `workspace.home` | same |
| `NEEDLE_WORKSPACE` | **not a config input** — it's what needle *exports* to validation child processes (`src/validation/mod.rs:371-373`); as a config key it falls into the ignored catch-all | `src/config/mod.rs:6790+` |
| `NEEDLE_HOME` | needle home for state/logs in the `needle_home()` helpers (`src/build_metadata.rs:183-190`); note `workspace.home` itself defaults to `$HOME/.needle` via `dirs_or_home` without consulting `NEEDLE_HOME` | `src/build_metadata.rs:196-202` |
| `NEEDLE_EVENTS` / `NEEDLE_HEARTBEATS` | override HOOP paths (default `<workspace>/.beads/events.jsonl` / `heartbeats.jsonl`) | `src/hoop_hooks.rs:58-62, 101-105` |
| `bead_cli.backend` / `bead_cli.path` in `<workspace>/.needle.yaml` | selects + pins the backend CLI for that workspace | `src/bead_store/mod.rs:50-56` |

## Live state on ex44 (observed 2026-08-21)

- `~/.config/needle/config.yaml` sets `workspace.default:
  /home/coding/claude-governor` — **not this repo** — and
  `strands.explore.workspace_root: /home/coding/`.
- `/home/coding/claude-governor/.beads/` is a full, live bead-rs store.
- This repo and `/home/coding` both carry a `.needle.yaml` binding
  `bead_cli.backend: bead-rs`.
- `/home/coding/.beads/` currently contains **only** `heartbeats.jsonl`
  (worker `claudego-e00231e1-repro`, 2026-08-21T01:10Z) — proof a needle
  process recently resolved its workspace to `/home/coding`. Per bead
  `telegram-9adb09a9`, a 0-bead `beads.db` existed there on 2026-08-14 while
  this repo's store held 138 beads / 11 open.

Implication for the starvation report (`telegram-fd5fc508`): any worker
launched without an explicit `--workspace /home/coding/telegram-claude-bridge`
resolves to `/home/coding/claude-governor` (global default) — its store
binding, agent cwds, and HOOP side-effect files all land on the
claude-governor/home stores, and this repo's beads are invisible to it.

## Diagnostic checklist

1. What store would a directory bind? Walk up from it looking for
   `.beads/config.json` — that ancestor's `.beads/beads.db` is the store.
2. What workspace did a live worker resolve? Check its cmdline for
   `--workspace` (`/proc/<pid>/cmdline`; needle's own orphan scan does this at
   `src/cli/mod.rs:4821-4830`), else `tmux display-message -p -t <session>
   '#{pane_current_path}'` for the inherited cwd, then apply rules 2-3 of
   Layer 1.
3. `needle doctor` prints the `beads_dir` it *assumes* (exact join) — compare
   with step 1's walk-up result for the same directory.
4. Stray `<dir>/.beads/heartbeats.jsonl` or `events.jsonl` files fingerprint
   past needle processes that resolved `<dir>` as their workspace.
