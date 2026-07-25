<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Data retention & gc

`koryph gc` applies koryph's data lifecycle policy: it compresses and
eventually deletes old run phase-directories, bounds project-scoped Go
caches, and size-rotates append-only audit logs. The binary-native autonomous
loop runs this same policy at terminal and idle boundaries. Manual engine
health patrols remain opt-in (see [Automatic gc](#automatic-gc-gc_auto)
below).

```sh
koryph gc [--dry-run] [--project ID] [--json]
```

- **`--dry-run`** — scan and report what would be compressed/deleted, change
  nothing. Run this first.
- **`--project ID`** — also process the project's run phase-directories
  (`<repo>/.plan-logs/koryph/<run-id>/`). Without it, only the global
  artifact classes (`~/.koryph/audit.jsonl`, `~/.koryph/runs.jsonl`) are
  processed.
- **`--json`** — emit the result as JSON instead of a table.

Exit codes: `0` clean, `1` when any artifact class reported a non-fatal
error (the table lists each one as a `gc warning`).

## What gc manages — and what it refuses to touch

gc covers four artifact classes:

| Class | Policy | Default |
|---|---|---|
| Run phase-dirs (`<repo>/.plan-logs/koryph/<run-id>/`) | compress whole run dir to `.tar.gz` after N days; delete after M days | compress at 7 days, delete at 90 days |
| `~/.koryph/audit.jsonl` | size-based rotation to `audit-<date>.jsonl.gz` | rotate at 10 MiB, retain rotated files **forever** |
| `~/.koryph/runs.jsonl` | size-based rotation to `runs-<date>.jsonl.gz` | rotate at 10 MiB, retain rotated files **forever** |
| Project artifacts (shared Go caches, eligible terminal transcripts and filed planning snapshots) | age-based retention plus soft/hard byte budget | warn at 2 GiB, reclaim toward 2 GiB above a 5 GiB hard limit |
| Telemetry (`~/.koryph/telemetry/`) | managed by the observability layer, not by `koryph gc` | see [Observability](observability.md) |

As soon as an individual slot becomes terminal, gc removes only its recognized
mutable roots: current `cache`, `go-cache`, `go-mod-cache`,
`go-build<digits>`, `go-tmp`, and `go-telemetry` directories; known legacy
`gocache`, `gomodcache`, `go-build-cache`, and `runtime-cache` layouts; and
engine-private reviewer `.runtime-scratch`. A live sibling slot is untouched.
Ledgers, manifests, status, streams, logs, summaries, unknown directories,
`latest`, and nonterminal slot trees are preserved. The `latest` symlink
protects durable evidence from archival, but does not retain disposable
compiler state. `--dry-run` reports exact known cache bytes but conservatively
reports zero for unknown future compression savings.

Safety exemptions, always in force:

- **Active runs are never archived or deleted** — gc reads `ledger.json` and
  requires a terminal run plus terminal slots. Only already-terminal slot
  scratch is eligible for immediate cleanup.
- **Shared caches are admission-coordinated** — engine startup and cache
  pruning use one kernel-backed guard, so a new run cannot enter while gc is
  deleting a project cache.
- **Durable evidence is authenticated before deletion** — a compact archive
  must be a regular, readable gzip/tar containing the expected terminal
  ledger. Symlinks, empty files, corrupt archives, and wrong-run ledgers fail
  closed. Selected transcript tails are created before source compression.
- **Filed planning evidence is authenticated** — only snapshots under
  `.plan-logs/koryph-plan/` with exact snapshot/post-file digests, a committed
  design blob, and the matching digest in the epic's Beads notes age out.
- **Posture snapshots are exempt by design** — they are your rollback
  evidence and are never auto-deleted.
- When a run dir is compressed, a companion `<run-id>.manifest.json` is
  written beside the archive (the per-phase manifests plus the ledger), so
  history queries can introspect archived runs without decompressing them.
- Rotation retention defaults to *forever* for both audit logs — they are
  audit trails; you must explicitly configure `retain_days` to prune them.
  Appenders and rotation take the same inode lock, so copy/truncate cannot
  lose a concurrent audit record.

## The retention policy: `retention.json`

The single config surface is `~/.koryph/retention.json` (global), with an
optional per-project overlay at `<repo>/.koryph/retention.json`. Non-zero
fields in the project file win over the global file. Missing files simply
mean "defaults". All fields are optional.

```json
{
  "run_dirs": {
    "compress_after_days": 7,
    "delete_after_days": 90
  },
  "audit_log": {
    "rotate_size_mb": 10,
    "retain_days": "never"
  },
  "runs_index": {
    "rotate_size_mb": 10,
    "retain_days": "never"
  },
  "project_budget": {
    "soft_mb": 2048,
    "hard_mb": 5120,
    "transcript_retain_days": 14,
    "failure_retain_days": 30,
    "log_tail_kb": 64
  },
  "footprint_warn_gb": 1.0,
  "gc_auto": false
}
```

Field reference:

| Field | Meaning | Default |
|---|---|---|
| `run_dirs.compress_after_days` | age (days) after which a completed run dir is compressed to `.tar.gz`; `"never"` disables compression | 7 |
| `run_dirs.delete_after_days` | age (days) after which the run dir (or its archive + companion manifest) is deleted; `"never"` disables deletion | 90 |
| `audit_log.rotate_size_mb` | size (MiB) at which `audit.jsonl` is rotated to `audit-<date>.jsonl.gz` | 10 |
| `audit_log.retain_days` | days to keep rotated `audit-*.jsonl.gz` files; `0` or `"never"` means keep forever | never |
| `runs_index.rotate_size_mb` | size (MiB) at which `runs.jsonl` is rotated | 10 |
| `runs_index.retain_days` | days to keep rotated `runs-*.jsonl.gz` files; `0` or `"never"` means keep forever | never |
| `project_budget.soft_mb` | project artifact target and warning threshold | 2048 |
| `project_budget.hard_mb` | threshold that makes eligible successful transcripts, full archives with compact evidence, and idle shared caches immediately reclaimable | 5120 |
| `project_budget.transcript_retain_days` | minimum age for successful terminal transcripts and filed planning snapshots | 14 |
| `project_budget.failure_retain_days` | minimum age for failed terminal transcripts, even above the hard budget | 30 |
| `project_budget.log_tail_kb` | tail retained when a full transcript is pruned | 64 |
| `footprint_warn_gb` | pending-gc footprint (GiB) above which the health patrol and `koryph doctor` warn | 1.0 |
| `gc_auto` | opt-in: let the health patrol run a live gc pass automatically | `false` |

### The `"never"` sentinel

Every retention value accepts the string `"never"`:

```json
{ "run_dirs": { "compress_after_days": "never", "delete_after_days": "never" } }
```

`"never"` in the project overlay always overrides a numeric value in the
global config — you can globally delete runs at 90 days while pinning one
project's history forever.

## Footprint monitoring

`koryph doctor` includes a `gc-footprint` check: it performs a dry-run scan,
reports per-class reclaimable sizes and the active policy, and warns when the
pending-gc footprint exceeds `footprint_warn_gb`. The engine's in-run health
patrol performs the same check on its patrol tick, so a long-running loop
tells you when it is time to run `koryph gc` — it does not delete anything on
its own.

## Automatic gc (`gc_auto`)

The binary-native `koryph loop` always performs bounded maintenance at idle
and terminal boundaries; this is part of its autonomous disk-safety contract.
It preserves live runs, retained failures, compact evidence, audit records,
posture snapshots, and active/final canary reports even when that means
reporting an unreclaimable hard-budget overage.

Setting `"gc_auto": true` in `retention.json` separately opts the **in-run
health patrol** into
running a live (non-dry-run) gc pass whenever the reclaimable footprint
exceeds `footprint_warn_gb` during a run. The patrol finding then reports
what was reclaimed instead of warning.

This is deliberately opt-in and off by default: it authorizes unattended
deletion under the retention policy above. Before enabling it, confirm your
policy with:

```sh
koryph gc --dry-run --project <ID>
```

All the safety exemptions still apply — auto-gc can never touch the active
run, a run with non-terminal slots, or posture snapshots.

`gc_auto` also gates a second, independent mechanism: on every patrol tick
(regardless of whether the run-dir footprint has crossed `footprint_warn_gb`)
the patrol runs the same retention pass as `koryph obs prune` against
`~/.koryph/telemetry/`, so telemetry volume no longer grows unbounded on a
long-lived project between manual prunes. Its outcome is appended to the
`gc-footprint` finding, e.g. `... [telemetry: pruned 3 stale file(s)]`.
