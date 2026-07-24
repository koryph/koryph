<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Recovery & escalation

A fleet that runs for hours unattended will see agents stall, gates go red,
rebases collide, rate limits bite, and budgets expire. koryph treats every
one of those as an *input* with a defined next step — detect, classify,
retry, escalate, and only then park for a human — so a failure at 2 a.m.
costs you a retry, not a morning of forensics.

This page collects the whole story in one place. The mechanics live in
[Running waves](running-waves.md#recovery-and-resume); the architecture
notes are in [Architecture](../architecture.md).

## Detect

- **Structured status, not output scraping.** Every dispatched slot reports
  its stage through a status file heartbeat. A running agent whose heartbeat
  goes silent for 15+ minutes is flagged **stalled** — visible in the
  [TUI](tui.md) Threads tab (`⚠ stalled <age>`) and the status bar tallies.
  If its transcript, commits, and cohort CPU are also quiet *and* its current
  process snapshot has no child or other process in its cohort, koryph
  gracefully interrupts it for recovery; a live gate/test child is deliberately
  left alone.
- **The health patrol.** On a fixed cadence the engine sweeps for dead
  agents on running slots, stuck claims, stale worktrees, and suspected
  resource leaks. Warn-level findings surface in the Events feed; what can
  be repaired safely is repaired automatically and marked `(auto-fixed)`.
- **Death classification.** When a slot dies, the engine classifies *why* —
  gate failure, review bounce, rebase conflict, crash, rate limit, budget
  kill, operator stop — and the classification, not a guess, decides what
  happens next.

## Retry

Retries are bounded, cause-coded, and visible (the TUI Threads tab shows
`×N g/m/c/rl/bk` — gate, merge, conflict, rate-limit, budget-kill):

- **Bead faults** (gate failure, review bounce, rebase conflict, crash) are
  requeued against a retry budget. A post-rebase gate failure is retried
  once before it counts as a fault at all — a flaky test doesn't burn an
  attempt.
- **Environment noise is not a fault.** Rate limits and transient merge
  errors requeue without consuming the bead's attempts — the governor's
  circuit breakers and settle windows handle the backoff instead.
- **Host blocks are structured.** A sandbox or host denial — such as an
  unavailable `ssh-agent`, credential, filesystem, network, tool, or host
  resource — is reported with `koryph phase block`, which parks the bead
  without a coding-agent retry or model escalation. A legacy generic worker
  self-block gets a same-tier classification-correction retry; it never
  spends the final frontier escalation.
- **Inert live-PID recovery is not a fault.** A stale, childless agent is
  SIGTERMed and resumed on its frozen tier/session without consuming an
  attempt; its no-commit worktree is retained so the session can resume.
  `engine.slot.stale_heartbeat_recovery` records the action.
- **Budget-killed agents warm-resume.** An agent stopped by a per-bead
  budget cap resumes its *own session* — context, plan, and partial work
  intact — rather than starting over; see
  [Budget-killed agents](running-waves.md#budget-killed-agents).
- **Turn-exhausted agents restart *fresh*.** An agent that runs past the
  per-bead turn ceiling (`per_agent_max_turns`, default 150) is gracefully
  interrupted and requeued with a *new* session — the opposite of a warm
  resume — so it sheds the accreted context that was driving the runaway
  cache-read cost. Committed work carries forward (the branch is rebased, not
  discarded); see [Per-agent turn ceiling](billing-and-quota.md#per-agent-turn-ceiling).
- **Stage timeouts degrade, not park.** A timed-out stage records a
  degraded result and moves on where that's safe, instead of freezing the
  bead.
- **Frozen model on retry.** Retries re-run the model the bead was first
  dispatched with — a mid-run relabel never switches a live retry — with
  exactly one exception, below.

## Escalate { #escalation }

- **Final-attempt escalation.** When a bead-fault requeue is about to burn
  the **final** attempt on a cheap tier (`haiku`/`sonnet`), that last
  attempt runs on the frontier tier (`opus`) instead — provided the
  project's `allowed_models` permits it. The slot's model rationale records
  `escalated from <tier>`, the TUI marks the row `↑`, and a bead that merges
  this way gains a durable `model-observed:<tier>` label.
- **Faults only.** Escalation counts genuine faults — never dispatch
  counts, environment no-ops, or rate limits — so frontier-tier spend goes
  to problems a stronger model can actually solve.
- **Tier policy is yours.** A bead's `rt:<n>` label (or the project's
  `risk_tier_default`) sets the recovery tier; reviewer timeouts have their
  own per-project adaptive escalation with a hard cap.

## Learn { #learned-model-labels }

Escalations are a training signal, not just a save:

```console
$ koryph models            # dry run: recommendations + evidence
$ koryph models --apply    # pre-label matching ready beads
```

`koryph models` (the two-word `models learn` still works as an alias)
aggregates escalated-then-merged beads by area and size
bucket; once a bucket has enough evidence, similar ready beads are labelled
to *start* on the stronger tier — skipping the doomed cheap attempts
entirely. A human-set `model:*` label always wins. Projects can run the
pass automatically at every wave boundary:

```json
"adaptive_escalation": { "enabled": true, "min_evidence": 2 }
```

## The operator's hand { #the-operators-hand }

When you do step in, the engine gets out of the way — and stays out:

| You want to… | Do this |
|---|---|
| See why beads did or didn't dispatch | `koryph status --project <id> --frontier` — every ready bead's verdict (`dispatched` / `deferred` / `skipped`) with full reasons |
| Add a bead to a running loop | `koryph inject <bead>` — no restart, even outside the run's `--parent` scope |
| Redirect a live agent | `koryph nudge <bead> "<note>"` — lands in the agent's `INBOX.md` |
| Take over a bead by hand | `koryph stop <phase>` — graceful SIGTERM; the bead is **parked**, never auto-retried into a race with your hand-work |
| Merge or close something yourself, mid-run | `koryph merge --close-bead <id>` — recorded in the run's **override sidecar** (`overrides.json`), which the engine folds in each cycle instead of clobbering your manual state |
| Wind a run down | `koryph drain` — in-flight slots finish; no new dispatch, and no retries either |
| Find what needs attention offline | `koryph doctor --project <id>` — stalled runs, parked beads, degraded validations, stranded epics, each with its recovery command |
| Pick a run back up | `koryph run --resume` |

A blocked protected-path merge names its exact resolution command in the
block note, so the fix is a paste, not an investigation.

**A terminally-blocked bead is never a silent strand.** Whenever the engine
gives up on a slot without merging it — attempts exhausted, an agent that
died, an operator stop, a drain, a budget cap, a merge the gate refused — it
reconciles the bead's tracker status to `blocked` (with a note naming the run,
attempt count, and any uncommitted worktree it preserved). Previously such a
bead stayed `in_progress` with no live agent, and because `bd ready` excludes
`in_progress`, it fell out of every future frontier until an operator reset it
by hand. Now it is visible — `bd list --status blocked` shows exactly what
needs a decision — and the health patrol WARNs on any residual `in_progress`
claim with no live agent (a bead a hard crash left before it could reconcile)
as a backstop. Reopen a resolved one with `bd update <id> --status open`.

The branch and worktree of a terminally blocked bead are retained even when the
tree is clean. Health patrol reports that retained manual-review state and any
ledger/Bead mismatch with an inspection command. Inspect first with
`git -C <worktree> status`; recover or commit the intended work, then reopen the
bead with `bd update <id> --status open`. Koryph never deletes or force-merges
that worktree during reconciliation.

## When the engine itself dies { #engine-death }

The recovery paths above are for a dead *agent* under a live engine. The
opposite case — the **engine** dies while an agent is mid-work — has its own
contract, and it is deliberate: **a dispatched agent is detached from the
engine and outlives it.** Every agent is launched into its own session
(`setsid`), never the engine's process group, so a signal aimed at the engine
does not reach the agents it started. The engine's job on the way down is to
leave a clean, resumable ledger — not to take its children with it.

- **Catchable death is graceful.** A `SIGTERM` or `SIGINT` to the engine
  (a loop-harness stop, `Ctrl-C`, a plain `kill`) is converted to a
  cancellation that runs the engine's `interrupted` path: it checkpoints every
  active slot (leaving it non-terminal and recoverable), emits an
  `engine.run.end` record with reason `interrupted`, releases `koryph.lock`,
  and exits. The run is left `running` on purpose so `--resume` can pick it up.
  The in-flight agent is **never signalled** — it keeps working in its own
  session.
- **Uncatchable death is backstopped.** A `SIGKILL` (or power loss) cannot be
  handled, so the engine writes no `run.end` and the run is stranded at
  status `running` with no live engine owning `koryph.lock`. The read-side
  liveness derivation flags this as a **dead run** (`koryph board` shows
  `run_dead`, `koryph doctor` lists it), and `koryph ops reconcile` finalizes
  it. The detached agents still survive this too.
- **`--resume` adopts an authenticated still-running orphan.** On resume the
  engine probes each non-terminal slot's recorded PID *and process-start
  identity*. A matching live agent is *reattached* — the poll loop resumes over
  the same process, with no restart and no lost work — while a dead, legacy, or
  PID-recycled slot is requeued or re-dispatched per its death classification.
  An agent that outlived its engine is therefore picked up exactly where it
  was, not started over; an unrelated process is never adopted or signalled.

> **Operator caution.** Because the engine is catchable-but-graceful, stop it
> with a single `SIGTERM` to the engine process (or wind the run down with
> `koryph drain`) — never a `TaskStop`/`kill` aimed at the whole *process
> tree* that escalates `TERM → KILL`. A tree-wide `SIGKILL` bypasses the
> graceful path entirely (no `run.end`, no checkpoint) and can catch a
> not-yet-fully-detached child in the same sweep. The loop wrapper's shutdown
> uses a stop-sentinel for exactly this reason.

## The hard lines

Some things never happen automatically, no matter how clean the recovery
path looks:

- **Never SIGKILL.** Graceful stops only — uncommitted worktree work
  survives.
- **Never delete a dirty worktree** without explicit approval.
- **Operator actions are terminal.** A bead you stopped stays parked until
  you say otherwise.
- **Identity is fail-closed.** A recovery path never re-dispatches under an
  unverified account.

## See also

- [Running waves — Recovery and resume](running-waves.md#recovery-and-resume)
  — ledger mechanics, resume semantics, and the full retry tables.
- [Epic validation — degraded outcomes](epic-validation.md#degraded-outcomes)
  — recovery at the epic level.
- [Billing & quota](billing-and-quota.md) — the budget caps and governor
  ladder that trigger budget kills and throttles.
- [Terminal cockpit](tui.md) — watching all of the above live.
