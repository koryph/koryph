<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Scheduler throughput: continuous dispatch, RW footprints, adaptive concurrency (2026-07-03)

Status: approved for implementation (orchestrator-authored, refactor-core).
Beads: filed under the epic listed at the end of this doc.

## 1. Problem

Three independent throughput losses, measured on the 2026-07-03 self-build run
(`20260703-221730`, `max_concurrent_slots: 8`):

1. **Wave barrier.** `engine.loop` dispatches a batch, then `pollUntilIdle`
   blocks until *every* slot is terminal before the next frontier scan
   (`internal/engine/wave.go`, `internal/engine/poll.go:53-77`). A wave of 8
   where 7 finish early leaves 7 slots idle until the slowest bead lands.
   Observed: waves of 1–2 while 28 beads sat ready.
2. **Coarse pessimistic footprints.** Any shared token is a conflict
   (`internal/sched/footprint.go:57-69`). `area:cli` serialized four ready CLI
   beads into four consecutive waves ("wave 3: 28 ready, dispatching 1"). No
   read/write distinction: a docs bead that merely *reads* engine code
   excludes an engine writer. `domain:unknown` serializes all unlabeled beads.
3. **Slow completion detection.** The 45 s poll tick adds up to 45 s of dead
   air per slot completion — pure latency under any refill scheme.

A fourth, operator-reported requirement: the machine-wide agent cap
(`koryph governor set --max-global N`) is static. When the Claude API rate
limits us we keep dispatching into the wall; when it recovers we stay
conservatively low. The cap should adapt: back off on rate-limit signals,
recover when they stop, and the scheduler must refill slots as the cap moves.

## 2. Invariants (the correctness contract)

Every lever below must preserve:

- **I1 — footprint exclusion.** No two *simultaneously in-flight* beads may
  hold conflicting footprints. (Today this is guaranteed by greedy coloring
  *within* a wave plus the barrier; rolling dispatch must re-establish it
  against the live in-flight set.)
- **I2 — linearized merges.** One merge at a time (bd merge-slot mutex),
  always rebase-onto-current-base → re-gate → ff-only → push
  (`internal/merge/merge.go`). Untouched by this design.
- **I3 — fail-closed safety paths.** Review-degraded blocks, signing verifies,
  protected paths reject, conventional-commit preflight — all unchanged.
- **I4 — governor semantics.** warn 80 / drain 90 / stop 95, per-run budget,
  wave preflight, never interrupting a running agent. Rolling dispatch
  re-checks these per *refill* instead of per wave.
- **I5 — never SIGKILL on scale-down.** Lowering any cap only stops *new*
  dispatch; running agents always finish their current attempt.

## 3. Design

### L1 — Continuous (rolling) dispatch

Replace the batch→drain→repeat structure with a refill loop:

```
for {
  governor checks (level, budget, preflight)      // per refill, same rules
  capacity := width - activeCount()               // width = effectiveWidth × ScaleSlots
  if capacity > 0 && allowDispatch {
    frontier := adapter.Ready()
    batch := sched.BuildWave(frontier, cfg, Opts{
        Max: capacity,
        ActiveIDs:  activeIDs(),
        Active:     activeFootprints(),           // NEW — see L2
    })
    dispatch batch (global-governor Acquire per bead, stagger)
  }
  if activeCount() == 0 && nothing eligible && nothing dispatched {
    exit (drained / no-dispatchable — same outcomes as today)
  }
  wait one poll tick OR SIGCHLD wake (L3); poll slots
}
```

Decisions:

- **Mode switch.** `dispatch_mode: "rolling" | "wave"` in
  `koryph.project.json` + `--dispatch-mode` run flag (flag wins). Default
  stays `wave` until rolling has survived a self-build burn-in; a follow-up
  bead flips the default. `--once` keeps exactly today's semantics in both
  modes (one dispatch pass, poll to idle, exit) — the validate canary depends
  on it.
- **Frontier scan cost.** `bd ready` is a subprocess; scan only when
  `capacity > 0`, at most once per tick.
- **Wave counter.** `run.Wave` increments per refill *that dispatches ≥ 1
  bead* (keeps `status`/`roster`/IDE observability meaningful).
- **Requeues** (review bounce, gate-failed, merge-error, died-no-commits)
  re-enter through the same slot as today and are unaffected: the slot never
  went terminal, its footprint never left the in-flight set — I1 holds across
  attempts by construction.
- **Governor stop/drain mid-run:** identical to today, evaluated per refill:
  stop/drain → no new dispatch; active slots finish; run exits paused-quota
  when the last slot lands.
- **Per-refill preflight.** `quota.Preflight(usage, estimate(batch), cfg)`
  runs against the *refill batch* estimate. Refill batches are smaller than
  waves, so preflight granularity improves (a 1-bead refill can proceed where
  an 8-bead wave was refused).

### L2 — In-flight footprint gating (prerequisite for L1)

`sched.Opts` gains the in-flight footprint set:

```go
type Opts struct {
    ...
    ActiveIDs        map[string]bool      // existing: exclusion by id
    Active           map[string]Footprint // NEW: in-flight footprints, keyed by bead id
}
```

`BuildWave` defers any candidate whose footprint conflicts with an in-flight
footprint (reason `"footprint conflict with <id> (in-flight)"`), in addition
to the existing intra-batch greedy coloring.

**Footprint persistence.** `ledger.Slot` gains
`Footprint *sched.FootprintRecord` (reads/writes token lists), written at
dispatch. The engine derives the in-flight set from live slots' persisted
footprints. Fallbacks, in order: persisted record → recompute from the bead's
current labels (`adapter.Show`) → `domain:unknown` (maximally conservative).
This also closes a **latent pre-existing gap**: on `--resume`, adopted
running slots were only excluded by ID — a freshly built wave could conflict
with an adopted slot's footprint. With L2 the resume path is correct by
construction.

### L3 — Fast completion detection

- Default `PollSec` 45 → **10** (`KORYPH_POLL_SEC` override unchanged; new
  optional `poll_seconds` project-config field, flag > config > default).
- **SIGCHLD wake.** Dispatched agents are direct children (that is why
  `slotAlive`'s `Wait4(WNOHANG)` works — `internal/engine/poll.go:116-131`).
  `signal.Notify(ch, syscall.SIGCHLD)` and select on
  `{ctx, tick, sigchld}`: a child exit triggers an immediate poll pass.
  SIGCHLD also fires for short-lived `git`/`bd` children, so it is a *wake
  hint only* — the poll pass decides what actually changed; wakes coalesce
  (buffered chan of 1). Completion latency drops from ≤45 s to ~instant, with
  the 10 s tick as backstop.
- **Split probe cost.** Liveness (`Wait4`/`kill 0`) every tick; the git
  progress probe (`rev-list --count` per slot) every 3rd tick (~30 s) — same
  freshness as today at a fraction of the subprocess churn, so the faster
  tick does not multiply cost.

### L4 — Finer footprints: read/write modes + sub-area tokens

**Model.** A footprint becomes two token sets:

```go
type Footprint struct {
    Reads  []string `json:"reads,omitempty"`
    Writes []string `json:"writes"`
}
```

`Conflicts(a, b)` = ∃ shared token t where at least one side holds t in
`Writes`. Two readers of the same token co-run (RWMutex semantics — two
readers cannot collide on a merge, so I1 is preserved by construction).
`domain:unknown` is always a write token.

**Label grammar** (backward compatible):

- `fp:<token>` — write token (existing labels keep their meaning).
- `fp:read:<token>` — **NEW**, read token.
- `area:<name>` — write tokens via `area_map` (unchanged).
- Mixed labels compose: a docs bead may carry `area:docs` +
  `fp:read:go:engine`.

**Sub-area tokens** (pure config + labeling, no code): extend
`koryph.project.json` `area_map` with per-package areas so `area:engine` no
longer means "the whole engine": `sched → go:sched`, `quota → go:quota`,
`dispatch → go:dispatch`, `ledger → go:ledger`, `govern → go:govern`,
`merge → go:merge`, `review → go:review`, `worktree → go:worktree`,
`beads → go:beads`, `registry → go:registry`. Existing labels stay valid;
new beads should use the narrowest area(s) they touch. Labeling guidance
lands in CLAUDE.md's conventions row and the issue-authoring command.

**Path-level footprints (`fp:path:<prefix>`) are deferred** to a follow-up
bead: they need area↔path comparability (an `area_map` schema extension
declaring each area's path prefixes) to avoid silently under-approximating
blast radius. The merge pipeline (I2) remains the backstop for mis-declared
footprints either way. Filed, not scheduled.

### L5 — Adaptive global concurrency cap (AIMD on rate-limit signals)

The machine-wide governor (`~/.koryph/governor.json`, `internal/govern`)
becomes a congestion controller. The cap **floats**, probing *upward* when
the API is quiet — including *above* the configured starting width, until a
rate-limit event reveals the real ceiling — and halving when rate-limited:

```
effectiveCap = clamp(dynamicCap, minCap=1, hardMax)
koryph governor set --max-global N [--adaptive] [--hard-max M]
```

- `--max-global N` — the starting cap (dynamicCap initializes here). With
  `--adaptive` off (default), it is the fixed cap — exactly today's behavior.
- `--adaptive` — enables the AIMD overlay: probe up on quiet, halve on
  rate-limit.
- `--hard-max M` — absolute safety bound for upward probing (default
  `2 × max-global`). Probing discovers the throughput ceiling *within* an
  operator-set blast radius; it never runs away unbounded.

- **Signal.** A slot (or reviewer) that dies with a rate-limit/overload
  marker in its stream (`429`, `rate_limit_error`, `overloaded_error` in the
  stream-json error/result events) is classified `rate-limited` by a new
  `dispatch.ParseStreamOutcome` (extension of the existing
  `ParseResultCost` scan).
- **Multiplicative decrease.** On a rate-limit event:
  `dynamicCap = max(1, effectiveCap/2)`, at most one decrease per 60 s
  (events inside the cooldown window are counted, not re-applied). Recorded
  in the govern store with a timestamp — the cap is machine-wide, so all
  engines on the host back off together (the store already has flock
  semantics).
- **Additive increase (probe).** `dynamicCap += 1` per 5 min with no
  rate-limit events, up to `hardMax` — this both *recovers* after a decrease
  and *probes past* the starting cap to find the actual sustainable
  concurrency. Recovery is lazy (evaluated on each Acquire/refill) — no
  daemon needed. Steady state oscillates just under the true API ceiling
  (classic AIMD sawtooth), which is maximal safe throughput.
- **Slot handling.** A rate-limited slot **requeues without burning an
  attempt** (the failure is environmental, not the bead's): new
  `Slot.RateLimitRequeues` counter, capped (5) to bound pathology, with the
  existing linear backoff. I5 holds: running agents are never interrupted;
  the cap gates *admission* only.
- **Slots stay filled as caps move.** In rolling mode this is emergent: every
  tick recomputes `capacity` from the *current* effective cap and refills or
  holds accordingly. Cap up → next tick dispatches more; cap down → next
  ticks dispatch nothing until attrition brings the in-flight count under the
  new cap. No slot is ever left idle while capacity and eligible work exist.
- `koryph governor show` displays operator cap, dynamic cap, last decrease,
  and event counts. `koryph doctor` flags a dynamic cap pinned low.

#### L5b — Settle windows, circuit breaker, dispatch smoothing

AIMD alone can thrash: agents dispatched under the *old* cap keep triggering
rate-limit events for minutes after a decrease, an instantaneous burst can
need more than one halving, and several projects' refill loops reacting to
the same cap raise can dispatch simultaneously (thundering herd). Three
mechanisms, all in the flocked govern store so every engine on the machine
coordinates:

- **Settle window.** After *any* dynamicCap change (either direction), the
  cap is frozen for `settle` seconds (default 120, subsumes the 60 s
  decrease cooldown). Events arriving during settle are counted for
  observability and burst detection but apply no further change — decisions
  are only made against a population that reflects the current cap. The
  additive-increase clock starts at settle expiry.
- **Burst-scaled decrease.** ≥3 distinct-slot rate-limit events within 30 s
  is a burst: the decrease applies factor 4 instead of 2 (floor 1) — one
  settle-window's worth of "additional lowering" up front instead of two
  full cycles of halve-and-wait.
- **Circuit breaker** (closed → open → half-open). Opens when a rate-limit
  event arrives with dynamicCap already at 1, or on 3 decreases within
  10 min. Open: admission is 0 machine-wide (running agents always finish —
  I5) for `break` seconds (default 300, doubling per consecutive re-open,
  cap 3600). Half-open: exactly one probe dispatch is admitted; if it
  completes without a rate-limit classification the breaker closes and AIMD
  resumes from dynamicCap=1; if it rate-limits, re-open with doubled break.
- **Dispatch smoothing.** A machine-wide minimum inter-dispatch spacing
  (default 3 s, jittered ±50 %) enforced at admission: a refill denied for
  spacing defers the rest of its batch to the next tick exactly like a cap
  denial (rolling mode retries next tick; no spin, no queue). This bounds
  the start-burst after a cap raise or breaker close regardless of how many
  projects refill at once.

`governor show` and `doctor` surface breaker state, settle deadline, and
smoothing config. Worst case (breaker misconfigured/flapping) degrades to
serialized dispatch, never to a stampede.

#### L5c — Per-provider governor pools

The service providers behind agent runtimes (Anthropic/claude,
OpenAI/codex, Google/gemini, xAI/grok build, …) enforce **independent**
rate limits, so a single machine-wide pool is wrong in both directions: an
Anthropic 429 must not throttle codex agents, and codex load must not
consume claude admission slots. The entire governor state — cap, leases,
demand, fair share, AIMD overlay, settle window, breaker, smoothing clock —
becomes **per-pool**, keyed by provider:

- A lease carries `Provider`, resolved from the runtime adapter that
  dispatches the agent (constant `anthropic` until the koryph-v8u adapters
  land — behavior identical to today). The pool key is an opaque string so
  it can later refine to `provider:account` (rate limits are really
  per-account within a provider) without another schema change.
- `koryph governor set --provider P …` / `show` lists every pool /
  `doctor` iterates pools. `--provider` omitted = `anthropic`
  (back-compat). An existing single-pool `governor.json` migrates on load
  into the `anthropic` pool.
- Cross-provider there is NO shared cap: total machine concurrency is the
  sum of pool caps by design (each API is the resource being protected;
  local CPU/RAM pressure is the operator's `--max-global` per pool).

### L6 — Requeue budgets

Replace the single-shot Note-marker dedup (`gateRequeueNote`,
`mergeErrorRequeueNote`) with small counters on the slot:
`GateRequeues`, `MergeRequeues` — budget **2 each** (was 1), still bounded by
`ledger.MaxAttempts`, using the existing `attempts × 15 s` backoff. A rare
real race (base moved twice) self-heals instead of stranding the bead.
Commit-style stays at 1 (a reword bounce either fixes it or won't).

### L7 — Speculative gate outside the merge mutex (filed, deferred)

Only worth it if merges queue. Sketch: rebase+gate the candidate against
current main *without* the lock; acquire the mutex only for
fetch/ff-check/merge/push; if the base moved since the speculative gate,
quick re-rebase (and re-gate only when the rebase was not clean). Needs merge
wait-time metrics first (`koryph metrics` gains merge-mutex wait). Blocked on
evidence of contention.

### L8 — Governor calibration (operational)

`quota.ScaleSlots` cannot throttle an *uncalibrated* account (uncalibrated ⇒
advisory), so calibration does not unlock width today — but it is what makes
warn/drain/stop and preflight *enforceable* instead of advisory, which the
adaptive levers above assume. Run `/koryph-calibrate` with a fresh `/usage`
reading (`koryph quota calibrate --account personal --window 5h
--observed-usd X --observed-pct Y`). Human-in-the-loop; filed `no-dispatch`.

## 4. Compatibility

| Surface | Behavior |
|---|---|
| `--once` | Unchanged in both modes: one dispatch pass, poll to idle, exit. |
| `--only`, `--parent` | Orthogonal (frontier narrowing precedes batching). |
| `--resume` | Improved: adopted slots now footprint-gate new dispatch (L2). |
| `--dry-run` | Prints the *first* refill plan and exits (same as today's wave plan). |
| `--manual` | Quota-exempt as today; rolling mode irrelevant (single bead). |
| Wave mode | Fully preserved behind `dispatch_mode: "wave"` until the default flips. |
| Ledger schema | Additive only (`Footprint`, requeue counters); old ledgers load (missing fields zero). |
| Govern store | Additive fields; old stores load; absent dynamicCap ⇒ operatorCap. |

## 5. Testing

- `sched`: RW conflict table tests (r/r, r/w, w/w, unknown), in-flight
  deferral, legacy `fp:*` labels parse as writes.
- `engine`: rolling refill with a fake WorkSource + fake backend — slot
  frees mid-run → refill within one tick; footprint-conflicting candidate
  held while its blocker runs, dispatched after it lands; governor stop/drain
  per refill; `--once` parity; drained-exit parity; budget cap mid-refill.
- SIGCHLD: completion detected without waiting a full tick (existing
  fake-claude harness; see `test(dispatch): reap detached fake claude`).
- govern: AIMD unit tests (halve, cooldown, additive recovery, operator-cap
  clamp); rate-limit stream classification fixtures; requeue-without-attempt.
- e2e self-build canary: `koryph run --project koryph --once --dispatch-mode
  rolling --auto-merge --review` before flipping any default.

## 6. Sequencing (bead map)

1. **S1** `sched`: RW footprint model + in-flight gating (L2+L4 core).
2. **S2** `engine`: poll 10 s + SIGCHLD wake + split probes (L3).
3. **S3** `engine`: rolling dispatch behind flag/config (L1; needs S1).
4. **S4** `govern`+`dispatch`+`engine`: rate-limit classification + AIMD cap +
   attempt-free requeue (L5; benefits from S2's wake loop, works without).
5. **S5** config/labels: `area_map` sub-areas + relabel open beads (L4 data;
   orchestrator-applied — `koryph.project.json` is protected).
6. **S6** requeue budgets (L6; small, independent).
7. **S7** docs: architecture.md + user-guide loop chapter + packages.md
   (folded per-bead for user-facing deltas; this bead is the cross-cutting
   refresh).
8. **S8** flip `dispatch_mode` default to rolling (after burn-in; needs S3).
9. **S9** speculative gate (L7) — filed, blocked on contention evidence.
10. **S10** calibration (L8) — HUMAN, no-dispatch.

S1–S4, S6, S8, S9 are `refactor-core` (orchestrator-authored on main, never
loop-dispatched — self-hosting safety rule). S5/S7/S10 are data/docs/ops.

## 7. Risks

- **Rolling + shared bd frontier:** `bd ready` excludes claimed beads, and
  the engine claims at dispatch — two engines on one project are already
  excluded by the run lock; no new exposure.
- **SIGCHLD portability:** darwin+linux fine (Go runtime coexists with
  `signal.Notify(SIGCHLD)`); treated as a hint, so a missed/spurious signal
  degrades to the 10 s tick, never to incorrectness.
- **Footprint under-declaration** remains possible (it is today): I2's
  serialized rebase+re-gate is the unchanged backstop; a real overlap
  surfaces as `StatusConflict`/gate failure, never as silent corruption.
- **AIMD flapping:** cooldown + additive-only recovery bound oscillation;
  worst case degenerates to today's static cap at `operatorCap`.
