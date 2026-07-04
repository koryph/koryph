<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Self-hosting: how koryph builds koryph

koryph is developed by its own loop: the engine's features are planned as
beads, dispatched to agents, reviewed, gated, and merged by the very
machinery being built. This chapter documents what that buys — not as
philosophy, but through a real optimization performed on 2026-07-04, using
nothing but the data koryph captures about every run.

## What koryph records while it works

Every run leaves a complete operational record, with no extra
instrumentation:

- **Refill lines** — each scheduling pass logs how much ready work existed,
  how much actually dispatched, the estimated cost, and the quota-window
  burn: `refill 6: 25 ready, dispatched 1 (est $1.78 / window 3%)`.
- **Deferral reasons** — every bead that *could not* dispatch says exactly
  why, naming the blocking bead:
  `deferred: koryph-9jn.1 (footprint conflict with koryph-fr3.6)`.
- **Per-bead lifecycle events** — dispatch (with model, persona, tier,
  attempt, pid), requeues with their cause (`gate failed after rebase`,
  `agent died with no commits`, `blocking review findings`), merges with
  SHAs, and parkings with exhausted budgets.
- **Governor state** — the per-provider concurrency pool as JSON: the
  adaptive cap, when it last probed up, settle windows, breaker state,
  dispatch-smoothing timestamps.
- **Quota and audit** — per-account window burn, and an append-only
  `audit.jsonl` of every account/dispatch decision.

Individually these are log lines. Together they make a question like *"why
isn't my fleet parallel?"* answerable with `grep` instead of intuition.

## The worked example: finding a parallelism ceiling

**The observation.** During a heavy self-build day, the operator asked
whether dynamic throttling was limiting the fleet. The governor said no:
`dynamic_cap: 16` (probed up from 8, zero rate-limit events, breaker
closed). Yet the refill lines told a different story — across 16
consecutive refills, **15 dispatched exactly one bead**; the average
concurrency was ~1.06 against a permitted 16.

**The trace.** The deferral reasons localized the ceiling in one command:

```
$ grep -oE "footprint conflict with [a-z0-9.-]+" run.log | sort | uniq -c
 180 footprint conflict with koryph-0vf.9
 100 footprint conflict with koryph-0vf.10
```

280 deferrals, all naming beads that held the `area:cli` footprint token.
Nearly every feature bead carried that token — and it was *honest*: every
new CLI command edits two central registration files (`cmd/koryph/main.go`,
451 lines; `completion.go`, 479 lines). Earlier the same day, three beads
touching those hubs had produced two real rebase conflicts — ground truth
that the label reflected genuine coupling, not over-labeling.

**The diagnosis.** The data separated three causes that would otherwise
blur together:

1. **Hub coupling (architectural, fixable)** — two shared registration
   files force every command-adding bead into mutual exclusion, even
   though the repo is otherwise well-partitioned (34 per-command files,
   per-package areas that never appeared as conflict sources).
2. **Token coarseness (convention, fixable)** — one `area:cli` covers all
   34 files, so unrelated commands conflict on the label wherever they
   truly overlap only on the hub lines.
3. **Dependency chains (intrinsic, not a defect)** — much of the day's
   work was sequential by design (`contract → extraction → compiler`).
   Chains serialize regardless of architecture; no refactor changes that.

**The fix, filed as beads with measurable acceptance.** Self-registering
commands (each file `init()`s into a registry; the hubs shrink to a
framework), then per-family footprint tokens (`cli:bot`, `cli:posture`, …)
once the hubs no longer force sharing. The acceptance criterion for the
convention bead is itself a log line: a wave over three disjoint-family
CLI beads must show `dispatched >1` in a refill — the same telemetry that
found the problem verifies the cure.

## The method, generalized

This loop works for any project koryph manages, not just koryph:

1. Compare the **permitted** concurrency (governor state) with the
   **achieved** concurrency (refill lines). A gap means scheduling, not
   throttling.
2. `grep` the **deferral reasons** and cluster them by blocking token.
3. For each hot token, decide which cause you have: honest coupling (fix
   the architecture), coarse labels (split the token), or a dependency
   chain (accept it — or re-plan the decomposition).
4. Re-measure with the same log lines after the fix.

The scheduler's footprint model turns architectural coupling into an
*observable operational cost* — hub files literally show up as deferral
counts. That inversion is the point: instead of a coupling opinion, you
get a coupling measurement.

## What else self-hosting caught (same day)

- **A prompt-delivery bug**: operator notes appended to not-yet-dispatched
  beads never reached agent prompts. Found because two scope addenda were
  silently lost mid-build; fixed by the loop itself the same afternoon,
  with the reliable channel (bead notes → compiled prompt) now regression-
  tested.
- **A requeue bug**: killing an agent whose bead had just been closed made
  the engine redispatch the closed bead until its attempt budget parked
  it. The lifecycle events made the misbehavior obvious; the fix (re-check
  bead state before any requeue) merged within the hour — built by the
  same loop that exhibited the bug.
- **A phantom gate failure**: a shared lint cache served stale results
  across worktrees, burning a bead's whole requeue budget on errors citing
  files in *deleted* worktrees. The per-attempt gate logs made the pattern
  identifiable, and the fix (per-checkout caches) ended the class.

Each of these is the same story: the machinery records enough about its
own operation that its failures are diagnosable from the record — and the
fleet that exhibited a bug in the morning ships its fix by evening.
