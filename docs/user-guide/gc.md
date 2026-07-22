<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Data retention & gc

`koryph gc` applies koryph's data lifecycle policy: it compresses and
eventually deletes old run phase-directories, and size-rotates the
append-only audit logs. It is the **only** koryph command that deletes data,
and it never runs automatically unless you explicitly opt in (see
[Automatic gc](#automatic-gc-gc_auto) below).

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
| Telemetry (`~/.koryph/telemetry/`) | managed by the observability layer, not by `koryph gc` | see [Observability](observability.md) |

Safety exemptions, always in force:

- **Active runs are never touched** — the current run and the target of the
  `latest` symlink are skipped.
- **Runs with live slots are never touched** — gc reads each run's
  `ledger.json` and skips any run whose slots are not all terminal.
- **Posture snapshots are exempt by design** — they are your rollback
  evidence and are never auto-deleted.
- When a run dir is compressed, a companion `<run-id>.manifest.json` is
  written beside the archive (the per-phase manifests plus the ledger), so
  history queries can introspect archived runs without decompressing them.
- Rotation retention defaults to *forever* for both audit logs — they are
  audit trails; you must explicitly configure `retain_days` to prune them.

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

Setting `"gc_auto": true` in `retention.json` opts the **health patrol** into
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
