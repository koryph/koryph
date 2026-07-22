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
| `~/.koryph/governor.json` | `{ "pools": { "<provider>": { "max_global_agents": N, "min_free_memory_mb": M, ... } } }` — one independent cap/AIMD-overlay per provider pool (koryph-v8u.11, see below). Absent/missing pool ⇒ default **8**. `min_free_memory_mb` (koryph-930) is the memory admission floor: dispatch is deferred while host available memory is below it. The gate is ON by default, sized to physical memory — the raw setting is `>0` an explicit floor, `<0` disabled, `0`/unset the auto floor (`sysmem.DefaultFloorMB` ≈ 1/8 of RAM, clamped to 1–8 GB). Edited only by the machine owner (never per-run), so no single project can lift the ceiling. |
| `~/.koryph/slots/<project>-<bead>-<pid>.json` | One **lease** per running agent: `{project, bead, pid, engine_pid, model, acquired_at, provider}`. Keyed to the **agent** PID (detached), so a lease survives an engine restart/resume and frees only when the real agent dies. `provider` selects which pool the lease counts against. |
| `~/.koryph/slots/demand/<project>.json` | One **demand heartbeat** per active engine with ready work, per pool: `{project, engine_pid, updated_at, provider}`. Refreshed each wave; pruned when stale (TTL) or `engine_pid` dead. |

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

### Per-account governor pools (koryph-v8u.11 → koryph-1o2.1, L5c)

Different LLM accounts — even on the same provider (a 20x Max subscription vs. a
work seat), and across providers (Anthropic/claude, OpenAI/codex, …) — enforce
**independent** rate limits. Every mechanism above — operator cap, leases,
demand/fair-share, the AIMD overlay, settle window, circuit breaker,
dispatch-smoothing clock — is **per-pool**, keyed by an opaque string that the
engine resolves to the account (see Key semantics):

```
governor.json:
{
  "pools": {
    "anthropic": { "max_global_agents": 8, "adaptive": true, ... },
    "openai":    { "max_global_agents": 4 }
  }
}
```

- **Key semantics.** The pool key is deliberately opaque (not an enum). The
  refinement to a per-account key that koryph-v8u.11 anticipated has landed
  (koryph-1o2.1): `internal/engine` now keys every lease on the resolved
  **account** — `runner.poolKey()` returns `quotaName()` (`Record.QuotaProfile
  ?? AccountProfile`), the same identity the cost governor already uses. A
  subscription is provider-specific, so the account subsumes the provider (a
  20x Max seat and a work seat are both "anthropic" but have independent rate
  limits, and get independent pools). `DefaultPool` (`"anthropic"`) is used
  whenever a lease, demand heartbeat, or store entry point carries no account —
  every store entry point normalizes `""` to `DefaultPool` at its boundary
  (`govern.NormalizeProvider`), so on-disk state never carries an empty pool
  key and a project with no account profile keeps the pre-koryph-1o2.1
  single-pool behavior.
- **A `govern.Lease` carries `Provider`** (the pool key), resolved at the one
  place `internal/engine` constructs leases (`internal/engine/govern.go`'s
  `poolKey()`). A `nil` record — degenerate/test paths with no project — keeps
  the empty key (⇒ `DefaultPool`), matching the old hardcoded constant exactly.
- **No cross-account shared cap, by design.** Total machine concurrency is
  the sum of every pool's cap — each account's API is an independently rate
  limited resource, so one account's 429 must never throttle another account's
  (or a codex agent's) admission. (Local CPU/RAM pressure is a separate
  concern: the operator's `--max-global` choice, made per pool, is what bounds
  it, alongside the memory floor and machine resource ledger.)
- **Migration.** A `governor.json` written before koryph-v8u.11 (any shape
  from koryph-1xk through koryph-2im.11 — a flat document with no top-level
  `"pools"` key) loads transparently as the `anthropic` pool, preserving
  every field (`govern.File.UnmarshalJSON`); it round-trips thereafter in the
  new `{"pools": {...}}` shape. `internal/doctor` reads `governor.json`
  directly (not through `Store`, since it honors `Options.Home` rather than
  `KORYPH_HOME`) and shares the same migration-aware type, so its checks see
  identical pool state.
- `koryph governor set --account NAME ...` configures one account's pool
  (`--provider P` remains as the raw-key alias; both omitted ⇒ `anthropic`,
  full back-compat); `koryph governor show` and `koryph doctor` iterate every
  pool with any live state (an explicit `governor.json` entry, a lease, or a
  demand heartbeat) — see CLI below.
- **Corrupt `governor.json` fails open for reads, fails closed for writes.**
  Admission reads (`Cap`, `MinFreeMemoryMB`, `Resources`, and every dispatch
  path through them) still fall open to the package default the moment
  `governor.json` is present but fails to parse — a stuck/corrupt file must
  never block dispatch. But `koryph governor set`/`set-resource` (and their
  `--unset` counterparts) now **refuse** to write in that state instead of
  silently treating the corrupt file as empty and overwriting it wholesale —
  the previous behavior was a machine-wide config wipe (every other pool's
  cap and the resource ledger gone) plus a silent cap relaxation back to the
  default, with no error. Either way, the first time corruption is observed
  the original bytes are copied, once, to a sibling
  `governor.json.corrupt-backup` so nothing is lost; repair or remove that
  backup, then fix or delete `governor.json`, before retrying the write.

### Per-account seeded-default cap (koryph-1o2.3)

`koryph governor set --account` is a live operator override — someone has to
run it, per account, before that account's pool caps at anything other than
the package default (`DefaultMaxGlobalAgents`, 8). koryph-1o2.3 adds a
**persisted default** that seeds the pool cap automatically, without an
operator override: `koryph quota set-threads --account NAME N` writes `N` to
that account's already-per-account quota config
(`~/.koryph/quota/<account>.json`'s `max_threads` field) — the same file
`koryph quota calibrate`/`koryph quota guard` already manage — rather than
into `governor.json`, and rather than into the cross-project registry record
(an account is a label shared across projects, so a registry field would
duplicate/conflict across every project on that account).

The two are deliberately kept as **separate, distinguishable** cap sources —
never collapsed into one `governor.json` field — because a later
`quota set-threads` change must not be silently shadowed by (nor silently
overwrite) a stale `governor set --account` override left over from a
previous decision. Resolution, applied by `internal/engine` at every
admission (`Store.Cap`/`Store.EffectiveCap`, via `Store.SeedCap`) with strict
precedence:

1. An explicit `governor set --account NAME --max-global N` operator override
   for the account's pool — always wins.
2. Else the account's persisted `quota set-threads` seed (`quota.Config.MaxThreads`).
3. Else the `anthropic` default pool's own cap — migration continuity: an
   operator's pre-per-account-pools `governor set --max-global` still governs
   a newly-onboarded named account that has configured neither of the above.
4. Else `DefaultMaxGlobalAgents`.

`internal/govern` must not import `internal/quota` (layering: quota is
account/billing policy, govern is machine-wide concurrency plumbing shared by
every provider). The engine bridges the two with a plain function value —
`Store.SeedCap func(pool string) int` — that it sets once at startup to a
closure over its own already-loaded `quota.Config`
(`internal/engine/run.go`/`govern.go`'s `seedCapForPool`); `govern` itself
only ever calls the hook, never imports the package that produces its value.
A `Store` with `SeedCap` unset (`nil` — every `NewStore()` call site that
doesn't wire it, and every hand-built `Store{}` in existing tests) simply
skips level 2, which is exactly the pre-koryph-1o2.3 precedence (1 → 3 → 4).
`koryph governor show`/`set` (`cmd/koryph/governor.go`) wire the same hook
against `internal/quota` directly (a pool key IS the account name for a named
account), so displayed caps match what admission actually enforces.

### Machine resource ledger: capacity + reservation admission (koryph-4ql)

Concurrency pools bound *how many agents* run; they say nothing about what an
agent starts running on the **host** once dispatched — a kind/k8s dev
cluster, a docker compose stack, a long-lived dev server. Those consume
machine capacity outside the agent's process tree, and two agents each
starting a multi-gigabyte cluster is invisible to both the pool cap and to
footprint conflict detection. The resource ledger is a second, additive
admission dimension for exactly that (design
`docs/designs/2026-07-resource-governor.md`, user-facing concept:
[Machine: resources](../concepts/resources.md)): a bead declares external
resource kinds with `res:<kind>` labels, and `govern` tracks per-kind
capacity and cost the same way it tracks concurrency — files under
`~/.koryph`, guarded by the same flock, fail-open on error.

**Schema.** `governor.json` gains a **top-level** `resources` section,
deliberately *outside* the per-provider `pools` map — RAM, dev clusters, and
the docker daemon are machine properties shared across every provider, not
scoped to one:

```json
{
  "pools": { "anthropic": { "max_global_agents": 8 } },
  "resources": {
    "ramp_seconds": 600,
    "kinds": {
      "kind-cluster": { "capacity": 1, "mem_mb": 6144, "ramp_seconds": 900, "probe": "kind get clusters" },
      "docker":       { "capacity": 3 }
    }
  }
}
```

- `resources.ramp_seconds` — the global default ramp window (see reservation
  admission below); a per-kind `ramp_seconds` overrides it. `<=0` falls back
  to the package default of **600s** (`govern.DefaultRampSeconds`).
- `resources.kinds.<kind>.capacity` — the max number of live leases allowed
  to hold this kind at once, counted **across every provider pool and every
  project** on the host (machine resources are cross-pool). `<=0`, or the
  kind being absent from `kinds` entirely — including when the whole
  `resources` section is absent — resolves to the fail-safe-serial default
  of **1** (`govern.DefaultResourceCapacity`). That default always binds: two
  beads declaring the same unconfigured kind can never co-dispatch, even on
  a machine with no resource configuration at all. Distinct unconfigured
  kinds do not collide with each other.
- `resources.kinds.<kind>.mem_mb` — the per-holder memory reservation
  charged while the lease is ramping (below). `0`/absent means uncalibrated:
  the kind still serializes at its capacity, it just reserves no memory.
- `resources.kinds.<kind>.ramp_seconds` — per-kind override of the global
  ramp window.
- `resources.kinds.<kind>.probe` — an operator-authored shell command that
  lists live instance names for that kind, for leak detection. Never
  consumed on the admission path (no subprocess runs under the flock);
  read only by the health patrol / `koryph doctor`.
- Schema mechanics: `govern.File` has a *custom* `UnmarshalJSON` that decodes
  only the `"pools"` key; adding a struct field for `resources` alone would
  be silently dropped on every read and then stripped from disk by the next
  setter's whole-file rewrite. `UnmarshalJSON` decodes `resources` explicitly
  on the pools-shaped document path; a legacy flat (pre-koryph-v8u.11)
  document predates the resource ledger entirely and decodes it as `nil`
  (no capacity configured — declared kinds still serialize at 1). Read with
  `Store.Resources()` (fails open to the zero `ResourcesConfig{}` on any
  read error, the `MinFreeMemoryMB` precedent); write with `Store.SetResource`
  / remove with `Store.UnsetResource`, both preserve-don't-reset
  read-modify-writes of the whole file, like `SetMinFreeMemoryMB`.

**Resolution order per kind.** Capacity has no project-level equivalent — it
is purely a machine fact. Memory cost layers two config surfaces (mirroring
the `area_map`-vs-labels split): the machine ledger's `mem_mb` when set, else
`koryph.project.json`'s `resources.<kind>.mem_mb` (the checked-in, portable
planning estimate — see [Machine: resources](../concepts/resources.md)),
else `0` (no reservation).

**Lease schema.** `govern.Lease` gains `Resources []string` (the resolved
kind tokens, not the raw labels) and `MemReserveMB int`, both `omitempty` —
an old lease file decodes as resource-free. The engine resolves a bead's
declaration once at dispatch and freezes both fields on the lease *and* on
the ledger slot, so a relabel or vocabulary edit mid-run never re-prices an
already-dispatched agent.

**Admission clauses.** `Store.Acquire`/`AcquireEx` runs two machine-scoped
checks under the flock, in addition to the existing pool cap/fair-share/AIMD
checks — pure lease-file arithmetic, no subprocess, so running them inside
the lock violates nothing:

1. **Capacity.** For each kind the candidate declares, count live leases
   holding that kind across **every** pool (the existing dead-pid prune pass
   has already run). Admit iff `holders + 1 <= capacity(kind)`. A denial
   names the kind, the resolved capacity, the holder count, and a
   deterministic representative holder.
2. **Reservation-aware memory.** Only runs when the caller supplies a memory
   reading (the engine's pre-flock probe, `internal/sysmem`) and a positive
   floor — a caller with no reading, or a disabled floor, skips this clause
   and keeps only capacity. Admit iff:

   ```
   availMB − Σ(ramping leases' MemReserveMB) − candidate's MemReserveMB ≥ floorMB
   ```

   A lease is **ramping** while `now − AcquiredAt < ramp_seconds` (the max
   ramp across every kind it holds). While ramping, its reservation is
   subtracted from the reading as a stand-in for memory it hasn't finished
   consuming; once the ramp elapses the reservation retires, since the cost
   should now show up in the real `availMB` reading. `Hold` restamps
   `AcquiredAt` on every rewrite, so the ramp clock restarts per (re)bind —
   the over-reserving, and therefore safe, direction. A denial distinguishes
   a **candidate-tipped** breach (would have passed at `MemReserveMB=0` —
   per-bead, the engine skips just this candidate) from a **pure floor
   breach** (fails even at zero reservation — machine-wide, every dispatch
   attempt breaks for this boundary).

   Both clauses also run on the half-open circuit-breaker probe grant, which
   otherwise returns before the cap/fair-share section — a resource-declared
   bead admitted as the probe still has to clear capacity and reservation.

Every error path (an unreadable `governor.json`, a probe failure) fails
**open**: dispatch proceeds. Capacity exhaustion is not an error — it is an
ordinary deferral, retried at the next scheduling boundary, exactly like a
pool-cap denial.

**Observability.** `Store.ResourcesStatus()` returns the live per-kind
ledger state across all pools: resolved capacity, configured `mem_mb`,
resolved ramp window, the configured probe command, every live holder
(project, bead, its reservation, whether it is still ramping), and the
reserved-vs-materialized MB split. `koryph governor show` renders this as a
resources section alongside the pool blocks — the operator's direct answer
to "which threads are using which shared resources right now." Set a kind's
machine capacity/cost/ramp/probe with `koryph governor set-resource`, and
clear one with its `--unset` form; see the CLI reference for the full flag
set.

### CLI

- `koryph governor` — show EVERY provider pool's cap, active leases
  (project/bead/pid/age), demand heartbeats, free slots, and (when
  configured) the AIMD overlay state including settle/breaker/smoothing, so
  operators can see contention per pool. A machine using only the default
  `anthropic` pool prints exactly one pool block — a strict superset of the
  pre-koryph-v8u.11 single-pool output.
- `koryph governor set --max-global N [--provider P] [--adaptive] [--hard-max M] [--settle-sec S] [--break-sec B] [--min-dispatch-interval I]` —
  write ONE pool's entry in `governor.json` (`--provider` omitted ⇒
  `anthropic`); without `--adaptive` this clears/disables any previously
  enabled overlay ON THAT POOL ONLY (it overwrites that pool's entry
  wholesale — every other pool is untouched). The three L5b flags are
  meaningful only under `--adaptive` and default (this package's documented
  constants) when omitted or non-positive.
- `koryph governor set --min-free-memory-mb N [--provider P]` — set the memory
  admission floor (koryph-930) for one pool. Unlike `--max-global`, this
  PRESERVES the pool's cap and AIMD state (it edits only `min_free_memory_mb`),
  and may be given alone or alongside `--max-global`. The gate is ON by default:
  `N>0` is an explicit floor, `N<0` disables it, `N==0` resets to the auto floor
  (sized to physical memory). At `Acquire` time, admission is refused while the
  host's available memory (from `/proc/meminfo` on Linux, `sysctl`+`vm_stat` on
  macOS via `internal/sysmem`) is below the effective floor — the engine checks
  this BEFORE the flocked `Acquire` so the probe never runs under the
  machine-wide lock, and fails open on any read error.
  `KORYPH_MIN_FREE_MEMORY_MB` overrides the configured floor for a single run
  (same `>0`/`0`/`<0` semantics).
- `koryph governor set-resource <kind> ...` writes one kind's
  capacity/mem-mb/ramp-seconds/probe in the top-level `resources` ledger
  (`Store.SetResource`); its `--unset` form removes a kind
  (`Store.UnsetResource`). Unlike the pool `set` subcommands, resources are
  machine-wide, not per-provider. See the [CLI reference](../reference/cli.md)
  for the full flag set. `koryph governor show` renders the resources section
  described above; leak detection (a suspected leaked instance, via the
  `<kind>-<bead-id>` naming convention and an optional probe) is a health
  patrol / `koryph doctor` finding — see
  [Machine: resources](../concepts/resources.md#leak-detection).
- `board` / `status` prune stale leases and demand (across all pools) as a
  hygiene side effect.

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
- Per-provider pools (`internal/govern/pools_test.go`, koryph-v8u.11): a
  rate-limit event (halve/settle/breaker trip) in one pool leaves another
  pool's admission, dynamic cap, and event count completely untouched (the
  core assertion); operator cap and lease/Release accounting are scoped per
  pool; a legacy pre-pools `governor.json` migrates into the `anthropic` pool
  with every AIMD field preserved and round-trips in the new shape; fair
  share is computed within a pool only (a project's demand in one pool never
  affects another project's share in a different pool); `""` normalizes to
  `anthropic` at every entry point (`Acquire`/`Hold` via `Lease.Provider`,
  and every other method's explicit `provider` argument); concurrent
  `Acquire`/`Release` across two pools never breaches either pool's cap
  (`go test -race`). CLI (`cmd/koryph/governor_test.go`): `--provider`
  set/show round-trips independently for multiple pools, and a plain
  (`--provider`-omitted) `set`/`show` stays fully back-compat with the
  single-pool CLI. Doctor (`internal/doctor/doctor_test.go`): the governor,
  adaptive-cap, and circuit-breaker checks each emit one Finding per pool,
  and zombie-lease/stale-demand Findings name the pool they belong to.

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
6. **Per-provider governor pools (koryph-v8u.11, L5c)** — every piece of
   governor state (cap, leases, demand, AIMD overlay, settle/breaker/
   smoothing) becomes per-pool, keyed by an opaque provider string; a
   pre-koryph-v8u.11 `governor.json` migrates transparently into the
   `anthropic` pool; `koryph governor set --provider P` / `show` and
   `koryph doctor` become pool-aware; see the dedicated section above.
   `internal/engine`'s lease construction hardcodes `providerAnthropic` until
   the koryph-v8u.2 runtime adapters can supply the real provider.
