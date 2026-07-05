<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph doctor

`koryph doctor` checks the health of the koryph installation.
Without `--project` it runs global checks against `~/.koryph`.
With `--project <id>` it runs project-scoped checks against the named project.

## Usage

```
koryph doctor [--json] [--fix]
koryph doctor --project <project-id> [--json]
```

| Flag | Description |
|------|-------------|
| `--project <id>` | Run project-scoped checks for the named registered project |
| `--json` | Emit the report as a JSON object instead of a text table |
| `--fix` | Remove zombie slot lease files and stale demand heartbeats (global mode) |

## Exit codes

| Code | Meaning |
|------|---------|
| `0` | All checks passed |
| `1` | One or more warnings |
| `2` | One or more errors |

## Checks

### layout
Verifies that `~/.koryph` exists with the required subdirectory skeleton
(`registry.d/`, `quota/`, `slots/`). A missing home directory or subdirectory
is an **error** — run `koryph init` to repair.

### binaries
Checks that `git`, `claude`, and `bd` are on `PATH`. Missing binaries are
**warnings** (the CLI can still run for some commands).

### registry
Parses every `*.json` file in `registry.d/`. A file that is not valid JSON
is an **error**.

### governor
Validates `governor.json` when present. An absent file is fine — koryph
falls back to the default cap. A corrupt or zero-value file is an
**error**/**warning** respectively.

### zombie-leases
Scans `slots/*.json` for lease files whose tracked PID is no longer alive.
Each zombie is a **warning**; pass `--fix` to remove them immediately.

```
koryph doctor --fix
```

### stale-demand
Scans `slots/demand/*.json` for demand heartbeats whose engine PID is dead or
whose `updated_at` timestamp is older than 10 minutes. Each stale entry is a
**warning**; pass `--fix` to remove them.

### quota-calibration
Reads each `quota/<account>.json` and checks whether both `window_ceiling_usd`
and `weekly_ceiling_usd` are greater than zero. An uncalibrated account is a
**warning** — the governor runs in advisory-only mode until calibrated:

```
koryph quota calibrate --account personal --window 5h \
    --observed-usd 4.20 --observed-pct 21
```

### vault-providers
When `~/.koryph/vault.json` is present, checks that the first binary in
each provider's `fetch` template (e.g. `pass-cli` for ProtonPass, `op` for
1Password) is on `PATH`. A missing binary is a **warning**.

## Project-mode checks

When `--project <id>` is given, koryph doctor runs against the project's
repository root instead of `~/.koryph`. These checks mirror what
`koryph onboard` validates structurally (no network calls, no subprocess
invocations):

### project-config
Loads and validates `koryph.project.json`. A missing or corrupt file is an
**error**.

### git-repo
Verifies that `.git` exists at the project root. A missing `.git` is an
**error**.

### hooks-wiring
Checks `.claude/settings.json` for the three koryph hook markers:
`bd prime` (SessionStart), `agent-boundary-guard.sh` (PreToolUse Bash), and
`worktree-guard.sh` (PreToolUse Bash|Edit|Write). Each missing marker is a
**warning** — run `koryph rules install` to repair.

### signing
Inspects the project's `signing` block in `koryph.project.json`:
- Absent block → **ok** (signing not configured).
- Provider set but `public_key` not captured → **warning** (run
  `koryph signing setup`).
- Invalid config shape (caught by project-config parse) → **error**.

### protected-paths
Validates the `protected_paths` list for empty entries (**error**) and
duplicate entries (**warning**).

### stalled-runs
Scans the project ledger (`.plan-logs/koryph/`) for runs in `running` status
where any non-terminal slot has not been updated for more than 30 minutes.
Each stalled slot is a **warning**. Investigate manually — stalled agents may
need to be stopped with `koryph stop`.

```
koryph doctor --project koryph
```

### orphan-worktrees
Lists git worktrees registered under the project's worktree root
(default `<parent>/<repo>-worktrees/`) whose branch starts with `agent/` but
have no corresponding active slot in any currently-running ledger run. Each
orphan is a **warning**. Koryph never removes a dirty worktree automatically;
review and remove manually:

```bash
git worktree remove --force path/to/orphan
```

### ci-assets
Checks whether the koryph gate pipeline CI workflow is installed and up to
date with the current template rendering.

The check is **skipped** (ok) when the project has no recognisable forge
remote (not a GitHub repository or no git remote configured).

When the gate pipeline is **absent** or its content **drifted** from what
`koryph ci setup` would render, the finding is a **warning** with the exact
remediation command:

```
koryph ci setup --project <id>
```

When the installed file matches the current template the finding is **ok**.

The gate pipeline path is `.github/workflows/koryph-gate.yml` for GitHub
projects. Install or update it with:

```
koryph ci setup --project <id>
koryph ci setup --project <id> --force   # overwrite a locally modified file
```

## JSON output

```json
{
  "at": "2026-07-02T12:00:00Z",
  "home": "/Users/you/.koryph",
  "findings": [
    { "check": "layout", "level": "ok", "message": "layout ok" },
    { "check": "zombie-leases", "level": "ok", "message": "fixed",
      "fixed": true }
  ],
  "fixed_count": 1
}
```

In project mode the response includes a `"project"` field and `"home"` is the
project's repo root:

```json
{
  "at": "2026-07-02T12:00:00Z",
  "home": "/Users/you/src/myproject",
  "project": "myproject",
  "findings": [
    { "check": "project-config", "level": "ok", "message": "project_id=myproject work_source=bd" },
    { "check": "stalled-runs",   "level": "warn", "message": "stalled slot: run=20260702-100000 phase=bead-x status=running age=1h30m0s" }
  ]
}
```

The top-level `fixed_count` is non-zero only when `--fix` was passed and
files were actually removed (global mode only).
