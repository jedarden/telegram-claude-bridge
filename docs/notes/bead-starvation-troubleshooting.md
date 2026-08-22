# Bead starvation troubleshooting — root cause and resolution

Consolidated record of the 2026-08-14/16 "beads invisible to worker" starvation
incident in this workspace, and the runbook for triaging the next one. Sources:
investigation beads `telegram-9adb09a9`, `telegram-1680d67a`, `telegram-2c51e1d2`,
`telegram-99cd5790`, `telegram-64902f03`, `telegram-126af296`, `telegram-bc40ca7b`;
alert beads `telegram-fd5fc508`, `telegram-0acc2877`, `telegram-481450e8`,
`telegram-104df728`. Deep mechanics live in
[`notes/needle-workspace-resolution.md`](../../notes/needle-workspace-resolution.md);
this doc is the operational summary and triage guide.

## TL;DR

A starvation alert means "the worker's Pluck found nothing while open beads
exist **somewhere**" — not "in the database this worker is bound to". Two
things were wrong at once:

1. **Wrong store.** A worker launched without an explicit `--workspace`
   resolves its workspace to `workspace.default` from
   `~/.config/needle/config.yaml` — which on ex44 points at
   `/home/coding/claude-governor`, not this repo. Beads "existed in the
   database" (this repo's `beads.db`) but the worker was querying a different
   one. On 2026-08-14 a 0-bead store at `/home/coding/.beads/beads.db` let any
   process resolving to `/home/coding` silently bind an empty ancestor store.
2. **Legitimate exclusions misread as starvation.** Beads labeled `deferred`
   are excluded by the active Pluck config, and beads with unfinished
   blockers are never on the ready frontier. Both look like "open but not
   picked" to the alert heuristic.

Fixed by: always launching this workspace's workers with an explicit
`--workspace /home/coding/telegram-claude-bridge`, deleting the 0-bead
ancestor store, and (upstream, bead-rs R030) making discovery fail loudly
instead of climbing past a foreign `.beads` directory. Verified working:
38 successful claims in the 7 days to 2026-08-22, 0 starvation events for
this workspace in that window.

## The incident

The alert beads all carry the same shape:

```
## Starvation Alert

Open beads exist but Pluck found none — possible configuration error.

**Workspace:** default
```

`**Workspace:** default` is itself a tell: it reports the *label* "default",
not a resolved path, so it cannot distinguish "correctly bound, frontier
legitimately empty" from "bound to the wrong store entirely". Timeline:

| Date | Event |
|---|---|
| 2026-08-14 | Three alert beads created (`0acc2877`, `481450e8`, `104df728`). Store state at the time: this repo 138 beads / 11 open; `/home/coding/.beads/beads.db` existed with **0 beads** |
| 2026-08-16 | Fourth alert (`fd5fc508`, "Open: 10, in-progress: 0") — this one remained open as the tracking bead for the investigation |
| 2026-08-21 | Investigation beads close the loop: workspace-resolution mechanics documented, bead-db census taken, live worker verified bound correctly, ancestor trap confirmed defused |
| 2026-08-22 | Post-fix verification: discovery/claim fully functional; remaining "empty frontier" observations are legitimate dependency chains |

## Root cause

### Mechanism 1 — workspace mis-resolution binds the wrong `beads.db` (the actual starvation)

How a needle worker decides which store to query:

```
--workspace arg  >  workspace.default (~/.config/needle/config.yaml)  >  process cwd
        ↓
needle runs every `bead` CLI op with current_dir = <resolved workspace>
        ↓
bead-rs walks UP from that cwd to the first .beads/ → <root>/.beads/beads.db
```

On ex44, `workspace.default: /home/coding/claude-governor` (a busy, unrelated
live store with 1000+ beads). A worker started without `--workspace` therefore
never sees this repo's beads — it plucks claude-governor's. Separately, any
needle process whose workspace resolved to `/home/coding` bound the 0-bead
ancestor store that existed there on 2026-08-14: pluck always returns nothing,
starvation alert fires, and the alert's own advice ("check workspace path")
points at configs that look fine.

Two properties made this silent:

- **The asymmetry.** Needle's own sanity checks (`has_valid_bead_store`,
  `needle doctor`) check an *exact* `workspace/.beads` join, and Explore only
  looks one level *down* from its workspace root — while the actual store
  binding walks *up*. A store at `$HOME` is invisible to the checks yet is
  exactly what the binding selects.
- **HOOP side-effects re-arm the trap.** Heartbeats/events are written to
  `<workspace>/.beads/`, so a process resolving to `/home/coding` *creates*
  `.beads/` there. As of 2026-08-22 that directory contains a recreated
  0-byte `beads.db` plus `heartbeats.jsonl` — fingerprints show needle
  processes still occasionally resolve workspace to `/home/coding`.

Mitigations now in place:

- The live worker runs with an explicit `--workspace
  /home/coding/telegram-claude-bridge` (verified via `/proc/<pid>/cmdline`) —
  this flag is **load-bearing**; removing it re-points the worker at
  claude-governor.
- bead-rs R030 (2026-08-21) changed discovery to **stop at the first `.beads`
  directory and fail** if it lacks `config.json`, instead of silently
  climbing past it. A mis-resolved process now gets a loud
  "not a bead-rs workspace" error rather than an empty result set. The
  recreated `/home/coding/.beads/beads.db` is inert (no `config.json`) — but
  never run `bead init` from `/home/coding`, which would re-arm the trap.

### Mechanism 2 — Pluck label exclusion (legitimate invisibility)

The active exclusion list is in `~/.config/needle/config.yaml`:

```yaml
strands:
  pluck:
    exclude_labels: [deferred, human, blocked, starvation-alert]
```

- This list **replaces** the source default (`[deferred, human, blocked]`,
  `NEEDLE/src/strand/pluck.rs` `PluckConfig::new`) — it does not merge with
  it. A non-empty configured list that omits a default silently un-excludes
  that label.
- On 2026-08-21, three of the open beads carried the `deferred` label and were
  excluded solely by this rule — correct behavior, but indistinguishable from
  starvation if you only count `bead list --status open`.
- `starvation-alert` is itself excluded, deliberately: alert beads exist to
  attract a human/worker investigation, not to be plucked as routine work.

**Dead config, do not chase:** `mitosis.skip_labels: [no-mitosis, atomic,
mitosis-parent]` and `mitosis.skip_types: [bug, hotfix]` in the legacy
`/home/coding/.needle/config.yaml` are unread — current `MitosisConfig` has
neither field. They are old auto-split controls, never Pluck controls.
Adding `deferred` there does nothing.

**Labels that look like filters but are not:** `failure-count:N` is a sort
key within priority tiers (and triggers split-at-3), and `split-child` /
`umbrella` are not filtered by Pluck or Explore at all.

Also note the label `deferred` and the bead *status* `deferred` are distinct
mechanisms; both remove a bead from the frontier, via different layers.

### Mechanism 3 — dependency blocking (legitimate empty frontier)

The bead-rs ready frontier is `open + unassigned + no unfinished blockers +
not in_progress`. A dependency chain of open beads yields an empty `--ready`
with nothing wrong. In fact the tail of this very incident had that shape:
`fd5fc508` (the last alert) was blocked by the documentation bead
`telegram-01787e6d`, which was blocked by the verification bead
`telegram-bc40ca7b` — `bead list --ready` correctly returned nothing until the
chain closed.

Related trap (fleet-wide, 2026-08-16): a bead that is `open` *with* an
assignee is invisible to the ready frontier, and `bead reopen` **retains** the
assignee — after reopening, clear it (`bead update <id> --clear-assignee`) or
no worker can ever claim it. `bead show` reports this state as healthy.

## Workspace resolution logic (condensed)

Full detail with source references:
[`notes/needle-workspace-resolution.md`](../../notes/needle-workspace-resolution.md).
The rules that matter operationally:

1. `needle run --workspace W` wins over everything; it is canonicalized.
2. Without it: `workspace.default` from `~/.config/needle/config.yaml`.
3. Without that file: the needle process's cwd — so where `needle run` was
   invoked silently decides the store.
4. A workspace-level `.needle.yaml` **cannot** override `workspace.default`
   (non-overridable; warns). Env override: `NEEDLE_WORKSPACE__DEFAULT`
   (double underscore → dot). `NEEDLE_WORKSPACE` is not a config input.
5. The bead CLI resolves `.beads` by walking up from its cwd (needle pins
   that cwd to the workspace); since R030 it stops and errors at the first
   foreign `.beads` instead of climbing past it.
6. The dispatched *agent's* cwd is `config.workspace.default` for bead-rs
   beads (bead-rs `create` never sets `source_repo`), so the agent's own
   `bead` commands resolve from that cwd — same trap, one level down.

## Resolution and verification

| Fix | When | Effect |
|---|---|---|
| Worker launched with explicit `--workspace /home/coding/telegram-claude-bridge` | in place since the incident | Store binding deterministic; verified live 2026-08-21/22 |
| 0-bead `/home/coding/.beads/beads.db` deleted | 2026-08-21 | Walk-up from `/home/coding` no longer binds an empty store |
| bead-rs R030: discovery stops at first `.beads`, fails on foreign | 2026-08-21 19:06 | Mis-resolution now errors loudly instead of returning empty |
| `needle doctor --workspace /home/coding/telegram-claude-bridge` | 2026-08-21 | Passed workspace, SQLite integrity, backend, store checks |

Post-fix verification (bead `telegram-bc40ca7b`, 2026-08-22):

- 38 successful `telegram`-prefix claims in the prior 7 days across 3 workers.
- 0 starvation events for this workspace in the prior 7 days.
- Ready frontier live-tested with a throwaway bead: appears in `--ready`
  immediately; dependency blocking and `in_progress` exclusion behave
  correctly.
- Backend resolution: `bead 0.1.3`, `atomic_claim=true`, bead-rs per
  `.needle.yaml` + `.beads/config.json`.

## Correct workspace configuration pattern

The invariants that keep this workspace's workers fed:

1. **Always pass `--workspace /home/coding/telegram-claude-bridge` when
   launching a worker for this repo.** The global `workspace.default` points
   at claude-governor and will silently win otherwise.
2. **Repo `.needle.yaml` declares the backend explicitly**: `bead_cli.backend:
   bead-rs`. `auto` is rejected by needle's `open_configured` — executable
   discovery is never store ownership.
3. **Never initialize a bead store at or above `$HOME`-adjacent shared
   directories** (`/home/coding/.beads` especially). HOOP keeps writing
   heartbeats there; a `bead init` would turn that directory into a
   0-bead trap that walk-up can bind.
4. **Changes to Pluck exclusions go in `~/.config/needle/config.yaml` under
   `strands.pluck.exclude_labels`**, and the full intended list must be
   spelled out (replacement, not merge).
5. **Treat the shared dev checkout as shared.** The sibling incident in
   [`notes/workspace-config-issue.md`](../../notes/workspace-config-issue.md)
   — the bridge's self-updater silently dead for 14 days — came from the same
   root misconfiguration pattern: pointing a single-purpose consumer at the
   shared NEEDLE dev checkout. Isolate consumers (deploy worktree) instead of
   allowlisting the churn.

## Troubleshooting runbook: "open beads exist but Pluck found none"

Work top to bottom; each step is a confirmed-possible cause from this
incident.

1. **Which store is the worker actually bound to?**
   ```bash
   # What the worker was told:
   pgrep -af 'needle.*run' | grep -- --workspace
   tr '\0' ' ' < /proc/<pid>/cmdline        # if not in pgrep output
   # What that directory binds (walk-up, same algorithm as the CLI):
   cd <resolved-workspace> && bead doctor
   ```
   No `--workspace` in the cmdline → the global default won; check
   `~/.config/needle/config.yaml` → `workspace.default`.
2. **Is the frontier legitimately empty?**
   ```bash
   cd /home/coding/telegram-claude-bridge
   bead list --ready
   bead list --status open --json    # compare against --ready
   bead why --id <open-bead-id>      # Ready: No → see the blocker
   ```
   Dependency-blocked, `in_progress`, or assigned-but-open are all legitimate
   exclusions, not starvation.
3. **Are the open beads label-excluded?**
   ```bash
   bead list --status open --json | jq '.labels'
   ```
   Compare against `strands.pluck.exclude_labels` in
   `~/.config/needle/config.yaml`. Remember the configured list *replaces*
   the default set.
4. **Check for ancestor/foreign `.beads` hazards.**
   ```bash
   ls -la /home/coding/.beads/        # heartbeats + stray files here are fingerprints
   ```
   Any `heartbeats.jsonl` in a directory means some needle process resolved
   its workspace there. A `.beads` without `config.json` is inert but must
   never gain one.
5. **Distrust the alert's own workspace line.** `**Workspace:** default` is a
   label, not a path — it cannot tell you which store was queried.

If all five check out and the frontier is still unexpectedly empty, capture
`bead list --ready --json`, the worker cmdline, and `bead doctor` output on
the alert bead before closing it — that trio is what made this incident
diagnosable after the fact.

## Common configuration pitfalls

| Pitfall | Symptom | Reality |
|---|---|---|
| Launching a worker without `--workspace` | Worker plucks another repo's beads, or nothing | Global `workspace.default` (claude-governor) wins |
| Editing `mitosis.skip_labels`/`skip_types` in `~/.needle/config.yaml` to change plucking | No effect | Dead keys; current `MitosisConfig` reads neither |
| Expecting `exclude_labels` to merge with defaults | A default-excluded label becomes pluckable | Non-empty list replaces the default set wholesale |
| `bead reopen <id>` and moving on | Bead sits open forever, never claimed | Reopen retains assignee; clear it or the frontier ignores the bead |
| Treating `bead show` health as claimability | "Healthy" bead never picked | Assigned-open beads are invisible to `--ready` and to doctor |
| Running `bead init` from `/home/coding` | Later: silent wrong-store binding | Creates `config.json` in the fingerprint-magnet `.beads` dir |
| Trusting the starvation alert's workspace field | Misdirected investigation | It prints a label ("default"), not a resolved path |
| One shared checkout for workers + deploy + updater | See `notes/workspace-config-issue.md`: 3,796 silent update skips | Isolate each consumer in its own worktree |

## References

- Investigation beads: `telegram-9adb09a9` (db census), `telegram-1680d67a`
  (worker cwd), `telegram-2c51e1d2` (filter/exclusion analysis),
  `telegram-99cd5790` + `telegram-64902f03` (workspace resolution),
  `telegram-126af296` (config fix), `telegram-bc40ca7b` (post-fix
  verification)
- [`notes/needle-workspace-resolution.md`](../../notes/needle-workspace-resolution.md)
  — full resolution mechanics with NEEDLE/bead-rs source references
- [`notes/workspace-config-issue.md`](../../notes/workspace-config-issue.md) —
  sibling incident: updater starvation from the shared-checkout pattern
- NEEDLE upstream: `docs/notes/worker-starvation-lessons.md` (legacy-system
  false-positive analysis — the "verify before alerting" lessons that shaped
  the current strand design), ADR-014 (explicit workspace bead-backend
  binding), ADR-015 (concurrent same-repo worker isolation)
- bead-rs R030 (commit `2440b90`, 2026-08-21): discovery stops at the first
  `.beads` directory and fails closed on a foreign one
