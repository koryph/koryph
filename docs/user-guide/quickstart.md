<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Quickstart

This walkthrough takes you from zero to a first dry-run wave in about ten
minutes. The fastest path installs koryph and runs the adoption wizard end to
end; an alternative section further down shows the equivalent flag-driven
`project add` path for scripting or skipping the prompts. Either way you'll
pass the pre-dispatch gate, fire a single wave in dry-run mode, and read the
board and slot status.

## Prerequisites

- A git repository you want to manage (we call it `~/src/myproject` below)
- `git` on your `PATH`
- A Claude account profile name and the email address tied to it — the
  adoption wizard usually derives both from `~/.claude.json`, so you may not
  need to know them up front

---

## Step 1 — Install koryph and adopt the project

```sh
brew install koryph/tap/koryph
koryph adopt ~/src/myproject
```

`koryph adopt` is the wizard front door: it detects what your repo already
has, prints a plan naming each step and why it's needed, asks for your
consent in three scopes (the repo-wide plan, each system-level install, and
the derived account/gate/forge values), then executes — installing missing
prerequisites (`claude`, `bd`, `gh`), initializing `~/.koryph` and the beads
issue DB, registering the project under a derived account, writing
`koryph.project.json` with confirmed `gate`/`forge`/`area_map`, installing
the koryph scaffolding, offering signing/posture, offering one signed
`chore: adopt koryph` commit, and finally running `koryph validate`
in-process. See [koryph adopt](adopt.md) for the full phase-by-phase
walkthrough.

Confirm the project was registered:

```sh
koryph project list
```

You should see one row with `STATUS` of `migrated` — `adopt` already ran
`koryph validate` for you on the first green pass. If a beads database
existed before you ran `adopt`, `bd stats` remains a quick way to sanity-check
its health at any time.

Skip ahead to [Step 2 — Fire a first dry-run wave](#step-2-fire-a-first-dry-run-wave).

---

## Alternative: register manually with `project add`

Prefer to drive every step yourself, or scripting onboarding without
prompts? `koryph project add` is the lower-level primitive `adopt` sequences:
it registers the project and installs the agent scaffolding, but does
**not** initialize or harden beads, install missing tools, or infer
`gate`/`forge`/`area_map` for you — those are yours to handle first or by
hand.

```sh
koryph project add ~/src/myproject \
  --account personal \
  --identity you@example.com
```

`project add` inspects the repo, writes a registry record, scaffolds the
koryph adapter if one is missing, and installs the koryph scaffolding. Exactly
what it installs depends on the project's configured runtime (koryph-v8u.9):

| Asset | Where | When |
|---|---|---|
| **`AGENTS.md`** | repo root | **always** — canonical cross-runtime operating contract |
| **fallback personas** | `.claude/agents/` | **always** (rendered for the configured runtime) |
| `koryph-*` slash commands | `.claude/commands/` | Claude Code only (`Capabilities.Personas`) |
| hook scripts + `settings.json` | `hooks/` + `.claude/settings.json` | Claude Code only (`Capabilities.Hooks`) |

`AGENTS.md` is the runtime-neutral instruction file read natively by Codex, Cursor, Grok,
Copilot, opencode, and amp — it documents the koryph operating contract so every runtime
follows the same rules. Runtimes without hook support rely on **worktree isolation** and
**merge-time protected-path refusal** for containment instead of in-editor lifecycle guards.

The rules are what make koryph's boundaries hold in-editor (Claude Code only): the
`agent-boundary-guard` and `worktree-guard` hooks and the `bd prime`
session-start hook, plus a baseline permission allow/deny list. Unlike the
whole-file agents and commands, `.claude/settings.json` is **merged
additively** — koryph's hooks and permissions are added and your own
settings are never clobbered (re-run `koryph rules install <root>` any time; it
is idempotent).

Both the agents and the commands are namespaced with the `koryph-` prefix
(`koryph-implementer`, `koryph-issue`, …) so they never collide with a
project's own — or another tool's — `.claude` entries. A project overrides any
stage's persona by name via the `stages`/`tiers` maps in
`koryph.project.json`.

Installing is idempotent and never clobbers your edits: a file that already
exists with **identical** content is a silent no-op, and one whose content
**differs** is left untouched with a warning. To refresh or repair the assets
later, the canonical grouped verb installs all four at once:

```sh
koryph project install-assets <root> [agentsmd|agents|commands|rules|all]  # default: all
```

Pass a single kind to narrow it, `--force` to overwrite differing files, or
`--all-projects` to refresh every registered project. The per-kind verbs
(`koryph agents install <root> --force`, `koryph commands install <root> --force`,
`koryph rules install <root> --force`) remain as working aliases. The point of
shipping these commands is that koryph semantics are enforced whether you run
`koryph` yourself or an in-editor prompt drives it for you.

The `koryph-*` slash commands (grouped under the `koryph-` prefix in the
editor's command list):

| Command | Does |
|---|---|
| `/koryph-design "<ask>"` | Turn a described ask into a reviewed design doc, then decompose it into beads |
| `/koryph-issue "<desc>"` | File a well-formed beads issue (no work started) |
| `/koryph-build [bead]` | Build one issue — picks from `bd ready` if none named |
| `/koryph-import [path]` | Convert existing markdown plans/TODOs into a bead corpus (onboarding) |
| `/koryph-plan <doc>` | Decompose a design doc into a filed, conflict-aware bead graph |
| `/koryph-loop [max= budget= auto-merge=]` | Start the wave loop (joins the shared cross-project governor) |
| `/koryph-stop [--all]` | Graceful stop (SIGTERM) for this project or all |
| `/koryph-kill [--all]` | Forceful stop (SIGKILL) — last resort |

| Flag | Purpose |
|------|---------|
| `--account` | Account profile: `personal` or `work` |
| `--identity` | Login email verified at dispatch time |
| `--id` | Override the slug (default: repo directory name) |
| `--name` | Human display name (default: same as `--id`) |
| `--branch` | Override the detected default branch |

Confirm the project was registered:

```sh
koryph project list
```

You should see one row with `STATUS` of `registered`.

This path leaves beads uninitialised and `koryph.project.json` unwritten —
initialise beads yourself (`bd init`, confirm with `bd stats`) and write
`gate`/`merge_policy` (at minimum) into `koryph.project.json` before
continuing. Then run the pre-dispatch gate:

```sh
koryph validate myproject
```

`validate` checks that the repo has the required koryph hooks, a valid
adapter, beads initialised, and a matching account identity. On the first
green pass it promotes the record from `registered` → `migrated` and prints:

```
promoted migration_status: registered -> migrated
OK
```

Fix any reported issues and re-run until you see `OK`. A `FAILED` exit
means at least one check did not pass — the output names the failing check.

---

## Step 2 — Fire a first dry-run wave

```sh
koryph run --project myproject --once --dry-run
```

`--once` limits the engine to a single wave so you see exactly what would be
dispatched without committing to a full run. `--dry-run` plans and prints the
wave without launching any agent processes.

The output lists each bead the engine would dispatch, the model it would use,
and the worktree branch it would create.

Other useful flags for early exploration:

| Flag | Effect |
|------|--------|
| `--max N` | Cap the wave width at N slots |
| `--parent EPIC` | Scope the bead frontier to a parent epic |
| `--allow-unvalidated` | Run even if `validate` has not passed yet |
| `--default-model M` | Model for beads with no explicit label |

---

## Step 3 — Read the board

```sh
koryph board
```

`board` prints one row per registered project:

```
PROJECT     MIGRATION   ACCOUNT    RUN          RUN-STATUS  SLOTS  LIVE
myproject   migrated    personal   run-abc123   dry-run     -      0
```

Columns: project slug, migration status, account profile, latest run ID,
run status, slot status counts (e.g. `merged:2 running:1`), and live PID
count. Add `--json` for the machine-readable payload.

---

## Step 4 — Read slot status

```sh
koryph status --project myproject
```

`status` shows per-slot detail for the latest run:

```
project myproject  run run-abc123  status dry-run  wave 1

PHASE        STATUS   MODEL     COST   ATTEMPTS  BRANCH  WORKTREE
beads-a1b    planned  sonnet    $0.00  0         -       -
```

Add `--json` to get the full ledger entry for scripting or post-processing.

Add `--frontier` to see the **last wave's dispatch verdict** instead of the slot
table — every ready bead the scheduler considered and why, with per-verdict
counts and full reasons (no truncation):

```sh
koryph status --project myproject --frontier
```

```
project myproject  run run-abc123  wave 4  frontier @ 2026-07-21T15:00:00Z
  2 dispatched · 3 deferred · 1 skipped

BEAD       VERDICT     REASON                                    TITLE
beads-a1b  dispatched  -                                         add widget
beads-c3d  deferred    footprint conflict with beads-a1b (in-flight)  widget tests
beads-e5f  skipped     container bead                            widget epic
```

`deferred` = a ready bead the scheduler held back this wave (footprint/resource/
wave-full); `skipped` = structurally non-dispatchable (wrong issue_type, gate
bead). Beads that are not *ready* at all (blocked by an open bd dependency) are
upstream of the wave and do not appear here — use `bd dep tree <id>` for those.

---

## What's next

- **If your project's work lives in markdown** (design docs, `ROADMAP.md`,
  `TODO.md`, inline TODO/FIXME clusters): run `/koryph-import` in the editor
  to convert that corpus into a filed bead graph before starting waves. This
  creates conflict-aware, footprint-labelled beads the loop can dispatch in
  parallel — without it, `bd ready` is empty and the loop has nothing to
  build.
- **Remove `--dry-run`** to fire a live wave:
  `koryph run --project myproject --once`
- After a successful wave, re-run `koryph validate myproject` — once a run
  has at least one merged slot and no failures, the record is promoted to
  `validated`.
- Read [Running waves](running-waves.md) for multi-wave runs, `--resume`,
  `--review`, and auto-merge.
- Read [Billing and quota](billing-and-quota.md) before enabling
  `--allow-api-spend`.
