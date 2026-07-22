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
- **Budget-killed agents warm-resume.** An agent stopped by a per-bead
  budget cap resumes its *own session* — context, plan, and partial work
  intact — rather than starting over; see
  [Budget-killed agents](running-waves.md#budget-killed-agents).
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
