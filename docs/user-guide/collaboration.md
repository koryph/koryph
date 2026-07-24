<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Collaboration

Koryph is designed so several engineers can share one repository while each
runs agents under their **own** Claude account, quota, and identity. This page
explains what lives where and how to onboard a new collaborator.

---

## What lives in the repo

Check these into git; every collaborator picks them up automatically after a
normal `git pull`.

| Artefact | Path | Purpose |
|---|---|---|
| Project config | `koryph.project.json` | Engine version pin, gate commands, merge policy, footprint rules. |
| Beads database | `.beads/` | Issue graph, synced peer-to-peer over `refs/dolt/data`. |
| Agent personas | `agents/*.md` | Canonical shared role definitions; Claude links its native directory here and Codex reads the same source. |
| Hook scripts | `hooks/` | Boundary guards and lifecycle hooks enforced on every worktree. |
| Gate commands | `koryph.project.json → gate` | Green-gate checks every agent must pass before merge. |

### Engine version pinning

Set `engine_version` in `koryph.project.json` to require a minimum engine
on every machine:

```json
{
  "engine_version": "0.2+"
}
```

Koryph refuses to dispatch if the local binary is older than the pinned
version. If a colleague's `koryph version` is too old, they need to
`go install github.com/koryph/koryph/cmd/koryph@latest` before
launching a wave.

---

## What lives per user

Each collaborator keeps these on their own machine; they are **never** checked
into the shared repo.

| Artefact | Path | Purpose |
|---|---|---|
| Registry record | `~/.koryph/registry.d/<project-id>.json` | Local path, account triple, model tiers, quota profile. |
| Account mapping | `expected_identity` field in the record | The Claude email this machine dispatches under. |
| Quota snapshots | `~/.koryph/quota/` | Per-account rate-limit and spend state. |
| Audit trail | `~/.koryph/audit.jsonl` | Local account-change log. |

These artefacts are project-specific but machine-private: two engineers working
on the same repo bill entirely separately and see independent quota governor
state.

---

## Onboarding a new collaborator

Each new team member runs this sequence on their own machine.

### 1 — Prerequisites

Install or verify the tools from [Installation](installation.md):
`koryph`, `claude` (authenticated), and `bd`.

### 2 — Clone the repo

```sh
git clone <repo-url>
cd <repo>
```

The shared artefacts (`agents/`, `commands/`, `hooks/`, `koryph.project.json`,
`.beads/`) arrive automatically.

### 3 — Register the project

```sh
koryph project add <repo-root> \
  --account personal \
  --identity you@example.com
```

`koryph project add` inspects the repo, writes a registry record under
`~/.koryph/registry.d/`, and installs the koryph scaffolding: canonical
personas in `agents/`, canonical workflows in `commands/`, and runtime-native
links/configuration for every enabled runtime. Claude uses `.claude/agents`,
`.claude/commands`, and `.claude/settings.json`; Codex uses `.codex/agents`,
`.agents/skills`, and `.codex/hooks.json`. It prints the record as JSON on
success.

Required flags:
- **`--account`**: account profile to dispatch under (`personal` or `work`).
- **`--identity`**: the email address printed by `claude auth login`. Koryph
  fails closed — if the logged-in identity at dispatch time does not match
  this field, no agents are launched.

> **Note:** `koryph onboard <root>` is a separate, read-only inspection tool
> that reports the koryph state of an existing project directory. It does
> **not** register anything or prompt for credentials. Use `koryph project add`
> to register.

### 4 — Validate

```sh
koryph validate <project-id>
```

`koryph validate <project-id>` runs the pre-dispatch gate checks (hooks,
adapter, beads initialisation, and identity match). On a first green pass it
promotes the record from `registered` → `migrated` and prints `OK`. Fix any
reported issues and re-run until you see `OK`.

To reach `validated` (required for production dispatch without
`--allow-unvalidated`), you must first run a canary wave, then re-validate:

```sh
koryph run --project <project-id> --once --allow-unvalidated
koryph validate <project-id>
```

The operator runs the canary; `koryph validate` does not launch agents itself.
The record is promoted to `validated` only when the latest run has at least
one merged slot and no failures.

### 5 — Pull beads

```sh
bd dolt pull
```

Fetches the shared issue graph from `refs/dolt/data` on the git remote so the
new machine sees all open issues.

---

## Beads sync across collaborators

Beads issues live in a Dolt database stored under `.beads/` and synced as a
parallel git ref (`refs/dolt/data`) on the same remote. No separate server is
required.

```sh
bd dolt pull   # pull latest issues before starting work
bd dolt push   # push your updates after creating or closing issues
```

Merges are automatic for non-conflicting changes (different issues). If two
people close the same issue simultaneously, the later push wins and the earlier
is a no-op — Dolt handles conflict resolution at the row level.

---

## Account isolation at a glance

```
repo (shared)                per-machine (~/.koryph)
─────────────────────────    ──────────────────────────────────────────
koryph.project.json   →   registry record (root, account, quota)
.beads/  (Dolt ref)      ↔   bd dolt pull / push
agents/*.md               →   canonical personas (Claude links; Codex injects)
commands/*.md             →   canonical workflows (runtime-native links)
hooks/                   →   same files, enforced on every worktree
                             expected_identity: alice@example.com (Alice)
                             expected_identity: bob@example.com   (Bob)
```

Alice and Bob share tasks and history through the repo; they bill and quota
independently through their own registry records.
