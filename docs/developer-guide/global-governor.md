<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Global concurrency governor — design

Status: **implemented** (phases 1–3 landed) · Bead: `koryph-1xk` · Owner: orchestrator (refactor-core)

## Problem

`koryph run` is **one process per project**. Each caps only its *own* wave
width (`--max` / `max_concurrent_slots`, default 3), optionally scaled *down* by
the per-account **cost** governor (`internal/quota`, which gates by dollars).
Nothing bounds the **sum** of concurrently-running agents across projects and
processes: running loops on _K_ projects launches up to _3K_ headless `claude`
sessions, which breaches the Claude API concurrency / rate limits (429s,
throttling). The cost governor never bounds concurrency or request rate — a
burst of cheap agents still exceeds the limit.

## Goals

- A **machine-global cap** on concurrently-running agents that every
  `koryph run` process respects before launching an agent.
- **Fair-share** allocation across active projects (round-robin) so no project
  starves. Agents are never preempted, so idle capacity is reclaimed not by
  lending but when a project drains its frontier and drops its demand — that
  shrinks the denominator and raises every remaining project's share.
- A **per-project override** that is allowed but **warned** — a project may
  request more width, the global cap still binds, and the override is logged as
  a fairness risk.
- **No daemon**: coordination uses shared state under `~/.koryph`, consistent
  with the existing registry and ledger file-lock patterns.
- **Orthogonal to** and **composable with** the cost governor: a dispatch
  proceeds only when *both* governors allow it (concurrency = rate-limit safety;
  cost = spend safety).

## Non-goals (v1)

- Token- or request-per-minute rate windows. v1 governs **concurrency**, the
  first-order cause of 429s. TPM/RPM bursts are a documented follow-up; the cost
  governor already samples usage and can grow a rate window later.
- A central scheduler/daemon or cross-machine coordination. Scope is one host's
  `~/.koryph`.

## Design

### Shared state (under `~/.koryph`)

| Path | Contents |
|---|---|
| `~/.koryph/governor.json` | `{ "max_global_agents": N }` — the machine-wide cap. Absent ⇒ default **8**. Edited only by the machine owner (never per-run), so no single project can lift the ceiling. |
| `~/.koryph/slots/<project>-<bead>-<pid>.json` | One **lease** per running agent: `{project, bead, pid, engine_pid, model, acquired_at}`. Keyed to the **agent** PID (detached), so a lease survives an engine restart/resume and frees only when the real agent dies. |
| `~/.koryph/slots/demand/<project>.json` | One **demand heartbeat** per active engine with ready work: `{project, engine_pid, updated_at}`. Refreshed each wave; pruned when stale (TTL) or `engine_pid` dead. |

All mutations happen under a short-lived flock on the slots directory (the
`internal/ledger.Lock` pattern), held only for the prune + count + write
(milliseconds).

### Fair-share allocation (cross-process, no daemon)

Let `cap = max_global_agents` and `demanders = sort(active demand heartbeats)`
with `n = len(demanders)`.

```
fairShare(p):
    base = cap / n            # integer division
    rem  = cap % n
    # the first `rem` demanders (in ROTATING order) get one extra slot
    order = (indexOf(p) + epochBucket) mod n     # epochBucket = unixMinutes / window
    return base + (1 if order < rem else 0)
```

Rotation of the remainder (and therefore of any zero-share turns when `n > cap`)
is what prevents permanent starvation: every project periodically rises into a
`base+1` (or, when `n > cap`, a `1`) slot.

**Acquire** (under the flock), before a dispatch:

1. Prune dead-PID leases and stale/dead demand heartbeats.
2. Compute `globalActive` (all leases), `myActive` (self leases),
   `fairShare(self)`.
3. Grant iff `globalActive < cap` **and** `myActive < fairShare(self)` (strict
   fair share). Because the per-project shares always sum to exactly `cap`, a
   project never takes a slot reserved for another that still demands; a slow
   project's reserved slots wait for it rather than being lent out (no
   preemption). Idle capacity returns when a project drops its demand.
4. On grant, write the lease file; on deny, the caller defers the item (it stays
   in `bd ready` and is retried next wave).

**Release**: remove the lease when the slot reaches a terminal state. Crashed
agents are reclaimed by prune (PID liveness + a TTL backstop). A project's demand
heartbeat is dropped when its frontier drains or the run ends.

### Engine integration

- In the wave loop (`internal/engine/wave.go`), refresh this project's demand
  heartbeat, then dispatch is bounded by `min(perProjectWidth, cap − globalActive)`.
  Acquire a lease per item as it dispatches; a lost race → defer that item.
- Release in `completeSlot` / `blockSlot` / `mergeSlot`
  (`internal/engine/poll.go`).
- The cost governor still runs first; the concurrency governor is an additional
  gate. Dispatch needs both.

### Per-project override + warning

- `--max N` / `max_concurrent_slots` still sets the per-project **width**, but
  the global cap always binds — a project can never exceed `cap`.
- When a project's width exceeds `fairShare(self)` at wave start, the engine logs:
  `project X width N exceeds its fair share M (cap C across D projects); extra
  slots are used only when others are idle`.

### AIMD overlay: adaptive concurrency (koryph-2im.4)

`governor.json`'s cap is static by default — the operator sets it once and it
never moves. `--adaptive` turns it into a congestion controller (classic AIMD:
additive increase, multiplicative decrease) so the cap **floats** between a
floor of 1 and an operator-set ceiling instead:

```
effectiveCap = adaptive ? clamp(dynamicCap, 1, hardMax) : maxGlobalAgents
koryph governor set --max-global N [--adaptive] [--hard-max M]
```

- `--max-global N` seeds `dynamicCap` (and, with `--adaptive` off, is the
  fixed cap — exactly the pre-koryph-2im.4 behavior).
- `--adaptive` enables the overlay.
- `--hard-max M` bounds upward probing (default `2×max-global`) — the ceiling
  probing is allowed to *discover*, never an unbounded runaway.

Additive fields on `Config` (`internal/govern/types.go`): `Adaptive`,
`HardMax`, `DynamicCap`, `LastDecreaseAt`, `LastRateLimitAt`, `LastProbeAt`,
`RateLimitEvents`, plus koryph-2im.11's settle/breaker/smoothing fields (next
section). A `governor.json` written before these existed unmarshals them all
to zero, so `Adaptive=false` reproduces the old static-cap behavior
byte-for-byte (`Config.EffectiveCap`, `internal/govern/aimd.go`).

**Signal.** A dead agent's stream is scanned for an API rate-limit/overload
marker (`429`, `rate_limit_error`, `overloaded_error`) in an error-flagged
event — `dispatch.ParseRateLimited` (`internal/dispatch/cli.go`), deliberately
liberal about the exact event shape. `internal/engine/poll.go`'s
`completeSlot` checks this *before* the commits/finishCandidate check (a
rate-limited death is never a completed candidate) and, when it fires, calls
`requeueRateLimited` instead of the normal requeue path.

**Multiplicative decrease** (`Store.ReportRateLimit`): `dynamicCap =
max(1, effectiveCap/factor)`, at most once per settle window (see below) —
events inside settle still increment `RateLimitEvents` (observability) but do
not re-apply, so a burst of near-simultaneous rate-limited deaths across
engines on the host halves the shared cap once, not once each. `factor` is 2
normally, 4 on a detected burst (koryph-2im.11, next section).

**Additive increase / probe** (`Store.EffectiveCap`, evaluated lazily on every
`Acquire`): `dynamicCap += 1` per full 5 minutes elapsed since the settle
window last expired, up to `hardMax`. This is what lets the cap climb **past**
the operator's starting width to find the real sustainable concurrency —
`applyProbe` anchors on `max(SettleUntil, LastProbeAt)` (koryph-2im.11
replaced the old `LastDecreaseAt` anchor with the settle deadline; see below),
so steady state is a classic AIMD sawtooth just under the true API ceiling. No
daemon: the probe advances (and persists) inside whichever engine happens to
call `Acquire` next.

**Slot handling** (`internal/ledger.Slot.RateLimitRequeues`, additive field):
a rate-limited death requeues WITHOUT incrementing `Attempts` — the failure is
environmental, not the bead's — bounded instead by its own budget (5,
`internal/engine/poll.go`'s `rateLimitedRequeueBudget`), using the existing
linear backoff. Exhausting it blocks with a `rate-limited requeues exhausted`
note. I5 (never interrupt a running agent) holds unconditionally: the cap only
gates the *next* `Acquire`, never a live process.

### Settle windows, circuit breaker, dispatch smoothing (koryph-2im.11)

AIMD alone can thrash: agents dispatched under the *old* cap keep triggering
rate-limit events for minutes after a decrease, an instantaneous burst can
need more than one halving, and several projects' refill loops reacting to
the same cap raise can dispatch simultaneously (thundering herd). Three
mechanisms, all Adaptive-gated (zero effect when the overlay is off) and all
coordinated through the same flocked store:

- **Settle window** (`Config.SettleSeconds`/`SettleUntil`, default 120s,
  CLI `--settle-sec`). After ANY `DynamicCap` change — a decrease *or* an
  additive-increase step — further changes in **either** direction are frozen
  until `SettleUntil`. Events arriving during settle are still counted
  (`RateLimitEvents`, and fed into the burst history below) but apply no
  further change: decisions are only made against a population that reflects
  the *current* cap. This subsumes and replaces the pre-L5b 60s decrease
  cooldown. The additive probe's quiet-clock anchors on `SettleUntil`, not the
  change's own timestamp — `applyProbe` in `internal/govern/aimd.go`.
- **Burst-scaled decrease** (`Config.RecentRateLimitEvents`, a small
  window-pruned history keyed by project+bead). A decrease that fires with
  **>=3 distinct (project,bead) slots** reported within the last 30s applies
  factor **4** instead of 2 (floored at 1) — one settle-window's worth of
  extra lowering up front instead of two full halve-and-wait cycles. The same
  slot reporting repeatedly is NOT a burst (distinct-identity count, not raw
  event count).
- **Circuit breaker** (`Config.BreakerState`: `""` closed / `"open"` /
  `"half-open"`, plus `BreakerOpenAt`, `BreakerBreakSeconds`,
  `BreakerReopenCount`, `ProbeProject`/`ProbeBead`/`ProbeAdmittedAt`).
  Opens when a rate-limit event arrives with `DynamicCap` already at the
  floor, or on **3 decreases within 10 minutes** (`Config.RecentDecreases`).
  While open, `Acquire` denies EVERY lease machine-wide (I5 holds — running
  agents are never touched, only new admission is refused) for
  `Config.BreakSeconds` (default 300s, CLI `--break-sec`), doubling per
  consecutive re-open up to a 3600s ceiling. Once elapsed the breaker goes
  half-open: the next `Acquire` (any project) is admitted as the sole probe
  (the flock makes this race-free — exactly one caller wins); every other
  `Acquire` is denied until it resolves. A clean `Release` of the probe's own
  lease (no rate-limit ever reported for it) **closes** the breaker and
  resumes AIMD from `DynamicCap=1`, resetting `BreakerReopenCount`. A
  `ReportRateLimit` for the probe (or, when the reporting call site cannot
  supply bead identity, any report from the probe's own project — see the
  KNOWN GAP note in `internal/engine/govern.go`'s `reportRateLimit`; treating
  an ambiguous report as a probe failure is the conservative, safe direction)
  **re-opens** with a doubled break. A probe whose lease disappears without
  either signal ever arriving (a crashed agent, or its owning engine dying)
  is resolved by `pruneCrashedProbe`: after `Store.ProbeTimeout` (default 30
  minutes) with no live lease and no resolution, it conservatively re-opens —
  a crashed probe cannot wedge the breaker half-open forever.
- **Dispatch smoothing** (`Config.MinDispatchIntervalSeconds`, default 3s,
  CLI `--min-dispatch-interval`). A machine-wide minimum inter-dispatch
  spacing, jittered ±50% per admission (`Store.Jitter`, deterministic-
  seedable for tests), enforced at `Acquire` in the closed state only (the
  breaker's own half-open probe is a single deliberate dispatch, exempt). A
  denial for spacing behaves exactly like a cap denial — the engine's
  existing refill-batch deferral handles it with **no engine-loop change** —
  and, critically, does **not** itself advance the spacing clock.

`koryph governor show` prints the operator cap, adaptive on/off, dynamic cap,
hard max, last decrease timestamp, rate-limit event count
(`Store.AIMDStatus`), whether settle is currently active (and its deadline),
the breaker's state (closed / open-until / half-open+probe identity), and the
smoothing interval. `koryph doctor` warns when the dynamic cap has sat pinned
at its floor for a long time after a real decrease, and separately when the
circuit breaker is open/half-open (calling out "flapping" once
`BreakerReopenCount` reaches 2 without an intervening clean close) — either
is a signal the account is being persistently rate-limited, or `--hard-max`/
`--break-sec` need attention.

### CLI

- `koryph governor` — show the cap, active leases (project/bead/pid/age), demand
  heartbeats, free slots, and (when configured) the AIMD overlay state
  including settle/breaker/smoothing, so operators can see contention.
- `koryph governor set --max-global N [--adaptive] [--hard-max M] [--settle-sec S] [--break-sec B] [--min-dispatch-interval I]` —
  write `governor.json`; without `--adaptive` this clears/disables any
  previously enabled overlay (it overwrites the file wholesale). The three
  L5b flags are meaningful only under `--adaptive` and default (this
  package's documented constants) when omitted or non-positive.
- `board` / `status` prune stale leases and demand as a hygiene side effect.

## Failure & edge handling

- **Stale leases**: reclaimed by PID liveness (`dispatch.Alive`) plus a TTL
  backstop, so a crash never permanently consumes a slot.
- **`n > cap`**: some `fairShare` values are 0 for a round; rotation guarantees
  each project periodically gets a turn.
- **Lock contention**: the flock is held for microseconds; acquisition races
  resolve on the next re-check.
- **Absent `governor.json`**: default cap 8 — raised to let a single
  self-hosting project run a wider wave; under watch for Claude API rate
  limiting (drop to 6 if beads get throttled). The cap only bites when total
  demand across projects exceeds it.

## Testing

- N-goroutine `Acquire`/`Release` against a temp `KORYPH_HOME`:
  - the cap is **never** exceeded under contention;
  - fair share is respected across demanders;
  - dead-PID leases and stale demand are reclaimed;
  - rotation prevents starvation when `n > cap`;
  - work-conserving top-up uses idle capacity when others are satisfied.
- AIMD overlay (`internal/govern/aimd_test.go`): halve from various caps,
  floor at 1, settle suppresses a double halve, additive probe climbs past
  the starting cap up to `hardMax`, adaptive-off is byte-for-byte compatible
  with a pre-koryph-2im.4 store.
- Settle/breaker/smoothing (`internal/govern/settle_breaker_test.go`,
  koryph-2im.11): settle freezes both directions and the probe clock anchors
  on settle expiry; burst-scaled decrease (distinct-slot /4 vs same-slot /2,
  window pruning and bounded history); breaker opens on an at-floor event and
  on 3 decreases in 10 minutes, re-open duration doubles and caps at 3600s,
  half-open admits exactly one lease under concurrent-goroutine contention,
  closes on a clean probe release, re-opens on a probe rate-limit report, a
  crashed probe resolves via the `ProbeTimeout` fallback; smoothing denies a
  second acquire inside the jittered interval without advancing the spacing
  clock on denial; two independent `*Store` handles over one `KORYPH_HOME`
  (simulating two engines) keep the shared dynamic cap consistent under
  concurrent `Acquire`; adaptive-off ignores every L5b field even when
  hand-set to values that would otherwise gate admission.
- Rate-limit stream classification fixtures
  (`internal/dispatch/cli_test.go`): positive (`429`, `rate_limit_error`,
  `overloaded_error` shapes) and negative (ordinary errors, clean results,
  the marker appearing only in non-error-flagged text).
- Engine (`internal/engine/ratelimit_test.go`): a rate-limited death requeues
  without incrementing `Attempts`, capping at `RateLimitRequeues` and blocking
  once exhausted; an ordinary death is unaffected and still burns
  `ledger.MaxAttempts`.
- Doctor (`internal/doctor/doctor_test.go`): the circuit-breaker check is OK
  when unconfigured/adaptive-off/closed, warns when open or half-open, and
  calls out flapping once `BreakerReopenCount` reaches 2.

## Implementation phases (each gated green)

1. **`internal/govern`** — lease + demand store, flock, `paths.SlotsDir()`,
   `Acquire` / `Release` / `Prune` / `FairShare`, `governor.json` load/save.
   Full unit tests.
2. **Engine integration** — demand heartbeat + acquire/release in the wave and
   poll paths; the fair-share width warning.
3. **CLI** — `koryph governor` show/set; prune on `board`/`status`.
4. **AIMD overlay (koryph-2im.4)** — adaptive cap, rate-limit stream
   classification, attempt-free requeue budget; see the dedicated section
   above.
5. **Settle windows, circuit breaker, dispatch smoothing (koryph-2im.11)** —
   hardens the AIMD overlay against thrashing and thundering-herd restarts;
   see the dedicated section above. Known gap: `internal/engine/govern.go`'s
   `reportRateLimit` cannot yet thread the dying slot's bead id through from
   `internal/engine/poll.go` (out of bounds for that change — see the KNOWN
   GAP comment there), so burst-distinct-slot counting is project-level
   rather than per-bead until a one-line follow-up lands.
