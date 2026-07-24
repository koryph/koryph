---
name: koryph-ops
description: Translate plain user prompts into the correct koryph CLI invocations for the full loop lifecycle (start, observe, manage, drain, resize, stop, kill, calibrate)
---
<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

You are the koryph operations guide for this session. When the user describes
what they want to do in plain language, map it to the exact koryph CLI command
shown here. **Do not invent flags** — use only what appears in this file or in
`koryph --help`.

---

## Quick reference: user says X → run Y

| User says | Run |
|-----------|-----|
| "start the loop" | `koryph run --project <id> --review --auto-merge` |
| "start the loop, one wave only" | `koryph run --project <id> --review --auto-merge --once` |
| "build just this one bead" | `koryph run --project <id> --only <bead> --review` |
| "resume after a crash" | `koryph run --project <id> --review --auto-merge --resume` |
| "what is running?" | `koryph board` then `koryph roster --project <id>` |
| "show me the bead status" | `koryph status --project <id>` |
| "watch a running agent" | `koryph tail --project <id> <phase-id> --follow` |
| "tell the agent to do X" | `koryph nudge --project <id> <phase-id> "X"` |
| "stop adding new work" | `koryph drain --project <id>` |
| "stop everything gently" | `koryph stop --all` |
| "kill an unresponsive agent" | `koryph stop --project <id> <phase-id> --force` |
| "show quota / usage" | `koryph quota` |
| "calibrate the quota" | `/koryph-calibrate` (or follow CALIBRATE below) |
| "merge a finished branch" | `koryph merge --project <id> <branch>` |
| "land a pr-opened bead" | `koryph land --project <id> <bead>` |
| "reconcile open PRs" | `koryph pr-sync --project <id>` |
| "review an external PR" | `koryph review-pr --project <id> <pr>` |
| "change how many agents run" | `koryph resize --project <id> --max <N>` |
| "set global cap" | `koryph governor set --max-global <N>` |
| "intake / triage new issues" | `koryph intake --project <id>` |

---

## START — `koryph run`

### Flag reference

| Flag | Effect | When to use |
|------|--------|-------------|
| `--project <id>` | target project (required) | always |
| `--review` | insert a reviewer (Opus) between implement and merge | **default — always include** |
| `--auto-merge` | allow auto-merge for `merge:auto` items | include unless you want manual landing |
| `--once` | run exactly one dispatch pass, then exit | smoke tests, canary, CI one-shots |
| `--dispatch-mode wave\|rolling` | override `dispatch_mode` in `koryph.project.json` | prefer `rolling` for continuous throughput (see Efficiency) |
| `--only <bead>` | narrow to one bead; exit when it closes | single-bead builds; use `/koryph-build` for this |
| `--parent <epic>` | scope the run to one epic's children | work an epic incrementally |
| `--budget <USD>` | per-run cost ceiling; pauses (not kills) on breach | cost-bounded sprints |
| `--resume` | classify latest-run slots and re-dispatch dead ones | recovering from crash/sleep/Ctrl-C |
| `--dry-run` | plan and print the wave without dispatching | auditing the queue before committing |
| `--direct` | owner override: ff-merge to default branch, skip PRs | owner bypass; needs branch-protection allowlist |
| `--max <N>` | per-project slot cap **for this run** | ⚠️ bypasses the shared global governor — warn the user |
| `--no-billing-guard` | advisory-only quota for this run | debugging without blocking on uncalibrated quota |

### Common invocations

```sh
# Continuous loop — the standard production invocation
koryph run --project <id> --review --auto-merge

# Rolling dispatch (preferred for throughput once koryph-2im.8 is the default)
koryph run --project <id> --review --auto-merge --dispatch-mode rolling

# One bead only (prefer /koryph-build for interactive use)
koryph run --project <id> --only <bead> --review

# One wave, then stop
koryph run --project <id> --review --auto-merge --once

# Resume after interruption
koryph run --project <id> --review --auto-merge --resume

# Dry run — see what would dispatch without sending anything
koryph run --project <id> --dry-run

# Scope to one epic
koryph run --project <id> --review --auto-merge --parent <epic-id>

# Cap spend at $5 for this run
koryph run --project <id> --review --auto-merge --budget 5
```

> **Never start a second loop without checking the board first** (`koryph board`).
> Two overlapping loops for the same project double-spend governor slots and
> produce race conditions in the worktree registry.

---

## OBSERVE — inspecting state

```sh
# All projects at a glance
koryph board
koryph board --json

# Per-project live slot detail (latest run)
koryph status --project <id>
koryph status --project <id> --json

# Titled roster grouped by MERGED / RUNNING / QUEUED / DEFERRED
koryph roster --project <id>
koryph roster --project <id> --json

# Tail an agent's output (last 40 lines by default)
koryph tail --project <id> <phase-id>
koryph tail --project <id> <phase-id> -n 100
koryph tail --project <id> <phase-id> --follow   # live stream; Ctrl-C to stop

# Global concurrency governor — pools, caps, live leases
koryph governor show

# Quota / billing snapshot
koryph quota
koryph quota --json

# Run-level metrics (wave counts, slot timings, cost per bead)
koryph metrics --project <id>
koryph metrics --json

# Bead frontier — what is ready
bd ready

# What would the next wave contain?
koryph run --project <id> --dry-run
```

---

## MANAGE — in-flight operations

### Nudge a running agent

Append an operator note to the agent's `INBOX.md`; the agent polls it between
steps and adjusts course:

```sh
koryph nudge --project <id> <phase-id> "prefer the interface approach from issue 38"
```

### Intake / triage new beads

Scan the queue for invalid or poorly-labeled beads; optionally apply fixes:

```sh
koryph intake --project <id>
koryph intake --project <id> --dry-run   # preview without mutating
koryph intake --project <id> --comment   # post bd comments on flagged beads
```

### Merge and land

```sh
# Land a finished agent branch (manual merge step)
koryph merge --project <id> <branch>

# Land a pr-opened bead (fast-forward-only)
koryph land --project <id> <bead>

# Reconcile all pr-opened beads against live GitHub PR state
koryph pr-sync --project <id>

# Review an external contributor's PR (analysis only; never auto-approves)
koryph review-pr --project <id> <pr>
koryph review-pr --project <id> <pr> --approve --body "Looks good"
koryph review-pr --project <id> --all    # analyze every open PR
```

### Update a PR branch — always REBASE, never the merge button

When asked to update/refresh a PR branch against its base ("update the
branch", "bring the PR up to date"), NEVER use GitHub's Update-branch
button semantics (REST `update-branch`, or the button's default): those
mint a server-side merge commit with a non-conventional, unsigned-off
subject. Always rebase via GraphQL — commit messages, DCO sign-offs, and
conventional subjects survive verbatim:

```sh
gh api graphql -f query='mutation($id: ID!) {
  updatePullRequestBranch(input: {pullRequestId: $id, updateMethod: REBASE}) {
    pullRequest { number }
  }
}' -f id=$(gh pr view <n> --repo <owner/repo> --json id -q .id)
```

(The REST endpoint only supports merge — GraphQL is the only rebase path.)

---

## DRAIN — finish in-flight work, then stop

`koryph drain` tells the engine to stop dispatching new beads at its next
scheduling boundary. Any agent **already running** is left completely alone
to finish. Once the last active slot lands, the run exits with reason
`operator-drain`. The request is one-shot: the next `koryph run` starts clean.

```sh
koryph drain --project <id>   # drain one project
koryph drain --all             # drain every registered project
```

**When `koryph drain` is not yet available** (pre-koryph-57v.1 build):
use this manual fallback — it achieves the same effect without the command:

1. `bd ready` — note every loop-dispatchable ready bead.
2. For each, set a `no-dispatch` label: `bd update <id> --label no-dispatch`
   (this causes the engine to defer those beads at the next scheduling
   boundary without touching running agents).
3. Wait: watch `koryph status --project <id>` until all slots reach a
   terminal state (merged / blocked / pr-opened).
4. On exit, remove the temporary labels:
   `bd update <id> --remove-label no-dispatch` for each bead you deferred.

> `drain` is NOT `stop`. Drain requests a LOOP exit at the next scheduling
> boundary without signalling any process. `stop` sends signals to running
> agent processes.

---

## RESIZE — change dispatch width while the loop runs

`koryph resize` re-reads the override at every scheduling boundary; it takes
effect on the very next wave or rolling-refill tick without restarting the run.

```sh
# Change project slot cap (immediately, without restart)
koryph resize --project <id> --max 5

# Force burst above the project's configured max_concurrent_slots
koryph resize --project <id> --max 8 --force

# Clear the override and revert to project config
koryph resize --project <id> --clear

# Resize every registered project to the same cap
koryph resize --all --max 3
```

### Global governor — `koryph governor set`

The global concurrency governor is a cross-project pool shared by **every**
koryph run on this machine. Changing it affects all running projects.

```sh
# Set a static global cap (replaces any prior adaptive config for this pool)
koryph governor set --max-global 12

# Enable adaptive (AIMD) cap: probes up on quiet, halves on rate-limit
koryph governor set --max-global 6 --adaptive --hard-max 12

# Adaptive with tuned settle / breaker / smoothing
koryph governor set --max-global 6 --adaptive \
  --hard-max 12 \
  --settle-sec 120 \
  --break-sec 300 \
  --min-dispatch-interval 3

# Target a specific provider pool (default: anthropic)
koryph governor set --provider anthropic --max-global 10
```

### `max_concurrent_slots` tradeoffs

| Per-project `max_concurrent_slots` | Effect |
|------------------------------------|--------|
| 1 | Fully serialized; never conflicts but no parallelism |
| 2–3 (default) | Balanced; fits most subscription plans without rate-limit storms |
| 4–6 | Aggressive; needs well-labeled footprints and a calibrated quota to avoid governor drain |
| > 6 | Only if explicit API-key billing and `--adaptive` governor are in place |

Raising `--max` per project **bypasses the shared global governor** for that
project's slot count — warn the user before doing so.

---

## STOP vs KILL

### Stop (SIGTERM — graceful)

The agent commits its open work and exits cleanly. The engine detects the
exit on the next poll tick and either requeues (no commits) or proceeds to
review and merge.

```sh
# Stop one agent
koryph stop --project <id> <phase-id>

# Stop every live agent across all projects
koryph stop --all
```

### Kill (SIGKILL — force)

**Use only when a graceful stop did not work.** SIGKILL gives the agent no
chance to commit; any uncommitted work in its worktree is lost.

```sh
# Kill one agent (confirm graceful stop was already tried first)
koryph stop --project <id> <phase-id> --force

# Kill every agent across all projects
koryph stop --all --force
```

**What is lost:** uncommitted in-progress edits in the agent's worktree. Any
commits already pushed by the agent before the kill are preserved and will be
used on re-dispatch (`--resume`) as a checkpoint.

**Decision tree:**
1. Try `koryph stop --project <id> <phase-id>` first.
2. If the agent is still running after 60 s, use `--force`.
3. After `--force`, check `koryph status` for slots in `running` state; a
   dead-PID slot with commits will be re-dispatched on `--resume`.

> **Never reach for SIGKILL first.** A graceful stop preserves committed work;
> a kill may lose the last partial step.

---

## CALIBRATE — quota governor calibration

Calibrate whenever `koryph quota` shows `calibrated=no` or the governor
blocks dispatch unexpectedly.

```sh
# 1. Read current state
koryph quota

# 2. Open https://claude.ai/usage — note the % and USD for the 5h window.
#    Or use ccusage: ccusage blocks --active

# 3. Calibrate 5h window
koryph quota calibrate \
  --account <profile>   \
  --window 5h           \
  --observed-usd <$>    \
  --observed-pct <%>    \
  --plan-tier <tier>

# 4. Calibrate weekly window (same readings from the weekly view)
koryph quota calibrate \
  --account <profile>   \
  --window weekly       \
  --observed-usd <$>    \
  --observed-pct <%>

# 5. Confirm calibrated=yes
koryph quota
```

Governor thresholds: warn 80 %, drain (no new dispatch) 90 %, hard stop 95 %.
For the full walkthrough, use `/koryph-calibrate`.

---

## EFFICIENCY — tuning for throughput

### Prefer rolling dispatch

`--dispatch-mode rolling` continuously refills free slots without waiting for
the slowest agent in a batch to finish. For projects with variable bead
complexity, rolling dispatch reduces idle time significantly.

```sh
# Set rolling as the project default (in koryph.project.json)
# "dispatch_mode": "rolling"

# Override for a single run
koryph run --project <id> --review --auto-merge --dispatch-mode rolling
```

### Use narrowest footprints

Every footprint collision serializes two beads that could otherwise run in
parallel. Before starting a long loop, audit:

```sh
koryph plan --project <id>
```

Fix any `domain:unknown` beads with real `area:*` labels; wire missing
dependency edges between conflicting unordered beads. The audit shows
achievable parallel width before and after. Keep `res:<kind>` declarations
just as honest: footprints protect the merge, resources protect the
machine, and an undeclared cluster/compose/server bead can thrash the host
mid-wave with no admission-time signal. And share a write token across beads
that add files to a directory with a checked-in derived artifact (a migrations
lockfile, a secrets baseline) — the derived file collides at merge even when the
inputs don't; a `merge_reconcilers` / `merge_prepare` entry self-heals the
residual (docs/user-guide/merge-reconcilers.md).

### Route by model tier

Use `model:<tier>` labels to route cheap beads to cheaper models and reserve
frontier tier for scheduling-correctness work:

| Bead type | Recommended label |
|-----------|-------------------|
| Routine implementation | _(none — inherits stage default)_ |
| Novel algorithm, scheduler logic | `model:opus` |
| Implement phase only (override) | `model:implement:sonnet` |
| All stages override | `model:sonnet` |

### When NOT to start the loop

| Situation | Better approach |
|-----------|-----------------|
| Only one bead to build | `koryph run --project <id> --only <bead> --review` or `/koryph-build` |
| The bead is `refactor-core` | Implement it directly on main in this session — the loop never dispatches `refactor-core` beads |
| The bead is labeled `no-dispatch` | Remove the label first, or handle it manually |
| A loop is already running | Check `koryph board` — do not start a second loop for the same project |
| A `drain` request is pending | Wait for the current run to exit, then start fresh |

---

## SAFETY RAILS

1. **Check before starting.** Run `koryph board` to confirm no loop is already
   running for the target project before invoking `koryph run`.

2. **Never SIGKILL first.** Always try `koryph stop` (SIGTERM) before `--force`.

3. **Never dispatch `refactor-core` via the loop.** Beads labeled `refactor-core`
   touch the koryph engine's own dispatch/merge/governor loop or a protected
   path. The loop silently defers them. Implement them directly on main.

4. **`--max` per project bypasses the global governor.** Warn the user before
   raising per-project parallelism above the default (3). The default exists to
   prevent rate-limit storms across all projects sharing the same Claude plan.

5. **`drain` does not stop running agents.** It prevents the loop from
   dispatching new beads. Use `koryph stop` to interrupt a running agent.

6. **`koryph merge` and `koryph land` are operator actions.** The loop handles
   merging automatically when `--auto-merge` is set; only invoke these manually
   for slots left in `merge-pending` or `pr-opened`.

7. **Uncommitted work is lost on SIGKILL.** After a `--force` stop, always
   check `koryph status` and use `--resume` on the next run so the engine can
   checkpoint-resume from any commits the agent did write before the kill.
