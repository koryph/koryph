<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Resource-aware dispatch: declared bead resources, machine capacity ledger, reservation-aware admission (2026-07-09)

Status: approved for implementation (orchestrator-built inline, refactor-core core); beads at the end of this doc.
Origin: operator direction (2026-07-09) — parallel bead threads spin up
heavyweight external dependencies (kind dev clusters, docker compose stacks,
long-running servers) that exhaust host memory; koryph must know what shared
resources each thread will use and throttle dispatch before the machine
thrashes, with the demand declared as attributes on the bead at planning time.

## 1. Problem

A dispatched agent is not just a claude subprocess plus a worktree. Beads
whose acceptance criteria require a running system routinely provision
external dependencies — a kind cluster (~4–8 GB), a docker compose stack, a
dev server, a browser test suite — and those live on the **host**, outside
the agent's process tree and outside its worktree isolation.

The memory admission gate (koryph-930) helps but is **reactive**: it reads
*current* free RAM at `acquireGlobalSlot` (`internal/engine/govern.go:120-136`)
and defers dispatch below a floor. Three gaps remain, all observed as host
thrash during self-build waves:

1. **Admission outruns materialization.** A wave of 4 beads all pass the
   floor while RAM is still free; each then spends its first minutes spinning
   up a 6 GB cluster. By the time the pressure is measurable, all four are
   already running, and invariant I5 (never interrupt a running agent) means
   the governor can only watch the machine thrash. The gate needs to see
   *declared future demand*, not just the current reading.
2. **No capacity model for external resource kinds.** Nothing knows that this
   laptop can host at most one kind cluster. Two beads that each need one are
   happily co-dispatched today; footprints don't catch it (they encode
   merge-conflict safety, not runtime capacity — `internal/sched/footprint.go:94-113`),
   and the scheduler-throughput design explicitly scoped this out: *"local
   CPU/RAM pressure is the operator's `--max-global` per pool"*
   (docs/designs/2026-07-scheduler-throughput.md, L5c). koryph-930 partially
   superseded that sentence for raw RAM; this design supersedes it for
   declared resource kinds.
3. **Check-then-act across engines.** `memoryAdmits` runs engine-side,
   outside the governor flock: two `koryph run` processes on one host can
   both read "8 GB free" and both admit a 6 GB bead. Only accounting summed
   over lease files *under* the flock closes this race.

And the planning half: nothing asks, at decomposition time, "what will this
bead need to run?" — so even a perfect enforcement layer would have no
declarations to enforce. The bead must carry the declaration the same way it
carries its footprint.

## 2. Invariants (the correctness contract)

Everything below preserves the existing contract (scheduler-throughput
I1–I5). I6 and I7 restate existing posture the new checks must inherit; I8
and I9 are new clauses:

- **I1 — footprint exclusion** is untouched. Resources are a second,
  *additive* admission dimension; they never relax a footprint conflict.
  Mantra: **footprints protect the merge; resources protect the machine.**
- **I5 — never SIGKILL / never interrupt.** Resource control gates
  *admission only*. An agent that under-declared and is thrashing the host is
  out of scope for enforcement (the floor stops the *next* dispatch); it is
  in scope for observability (L7).
- **I6 — fail open on error, defer on capacity.** A probe failure, an
  unreadable config, a stuck governor: dispatch proceeds ("a safety rail,
  not a correctness dependency", `internal/engine/govern.go:20`).
  Capacity exhaustion is not an error — it is a normal deferral with a
  reason, retried at the next boundary. No queues, no blocking waits (the
  wave loop needs boundary pacing for this to be honest — see L3).
- **I7 — no subprocess probes under the flock.** The memory probe runs
  before the flocked `Acquire` today for exactly this reason
  (`internal/engine/govern.go:142-146`); resource accounting inside the flock
  is pure lease-file arithmetic. Per-kind instance probes (L7) run in the
  health patrol, never on the admission path.
- **I8 — freeze at dispatch.** A live slot's resource claim is fixed at
  admission, persisted on the ledger slot, and carried verbatim through every
  requeue — exactly like `Slot.Footprint` and the frozen model resolution
  (koryph-2im.3, koryph-ehx). A relabel or vocabulary edit mid-run never
  changes what a running slot is charged for.
- **I9 — additive schemas; no `res:*` labels = today's behavior.** New
  fields on `govern.Lease`, `govern.File`, `ledger.Slot`, and
  `project.Config` are `omitempty`-additive; a bead with **no** `res:*`
  labels behaves byte-for-byte as it does today under any configuration, and
  old ledgers/leases/governor files all load. (A bead **with** `res:*`
  labels is new behavior by definition — see L2 for what binds on an
  unconfigured machine.) Both dispatch loops gate through the same shared
  primitives (`governorGate`, `sched.BuildWave`, `acquireGlobalSlot`), so
  they cannot drift.

## 3. Design

### L1 — Declaration: the `res:<kind>` label grammar

A bead declares each external resource kind it will provision or consume with
one label per kind:

```
res:kind-cluster        # will run a kind/k8s dev cluster
res:docker              # will run containers via the docker daemon
res:dev-server          # will hold a long-running local server
```

- `<kind>` is an opaque lowercase token (`[a-z0-9-]+`), matched exactly.
  One label = one unit of that kind. (Multi-unit demand is deliberately out
  of scope; no real bead has needed it.)
- Parsed by a new `sched.ResourcesFor(issue) []string` — a sibling of
  `FootprintFor`, same `LabelValues` mechanics
  (`internal/sched/footprint.go:40-60`), dedupe+sort.
- **Not** nested under `fp:` — the footprint parser turns any `fp:<x>` into a
  write token verbatim, so `fp:res:kind-cluster` would silently become the
  write token `res:kind-cluster` and change conflict behavior. Resources are
  a separate namespace with separate semantics: footprint tokens are binary
  RW exclusion; resource kinds are **counted capacity** (L2). Capacity 1 is
  the exclusive case; capacity N is the "up to N threads may share this kind"
  case — counting subsumes both the exclusive and shared readings of "shared
  resource".
- A bead with **no** `res:*` labels declares "agent + worktree only". This
  deliberately inverts the footprint default (`domain:unknown` serializes
  unlabeled beads): serializing every undeclared bead on a phantom resource
  would destroy throughput for the common lightweight case, and the reactive
  memory floor (koryph-930) remains the backstop for undeclared load. The
  planning guidance (L6) and the patrol (L7) carry the honesty burden
  instead. This asymmetry is a decision, not an oversight.

**Considered and rejected — bd `metadata`.** bd supports arbitrary JSON
metadata per issue and it already rides `bd ready --json`; koryph just
doesn't decode it. Labels win anyway: they are the only channel every
existing scheduler attribute uses (`area:`, `fp:`, `model:`, `rt:`,
`runtime:`, `gt:`), they are visible in `bd list`/`bd show` without flags,
repairable with `bd label add/remove` (the koryph-replan flow), supported by
`bd create --graph` at plan time (graph metadata only accepts flat strings),
and free of bd's metadata merge-not-replace and type-inference traps.
Quantities (memory estimates, capacities) do not belong on individual beads
at all — they live in the shared vocabulary (L2), where one calibration
serves every bead of that kind.

### L2 — The vocabulary and the machine capacity ledger (govern)

Two config surfaces, mirroring "portable footprint labels + per-machine cap":

**Project vocabulary** — `koryph.project.json` gains a `resources` map
(sibling of `area_map`; loaded once per run like the rest of
`project.Config`):

```json
"resources": {
  "kind-cluster": { "mem_mb": 6144 },
  "docker":       { "mem_mb": 1024 },
  "dev-server":   { "mem_mb": 512 }
}
```

This is the checked-in, portable estimate of what one unit of each kind
costs. It is a planning artifact: the same commit that introduces a
`res:<kind>` label on beads adds the kind here.

**Machine capacity** — governor.json gains a **top-level** `resources`
section, deliberately *outside* the per-provider pools:

```json
{
  "pools": { "anthropic": { ... } },
  "resources": {
    "ramp_seconds": 600,
    "kinds": {
      "kind-cluster": { "capacity": 1, "mem_mb": 6144 },
      "docker":       { "capacity": 3 }
    }
  }
}
```

RAM, clusters, and the docker daemon are machine properties shared across
every provider pool; a per-pool home would double-count or miss (the
`min_free_memory_mb`-inside-the-anthropic-pool placement is the wrinkle we
are not repeating — it stays where it is for compatibility, but the new
state is machine-scoped from day one).

**Schema mechanics (not free).** `govern.File` has a *custom*
`UnmarshalJSON` that decodes only the `"pools"` key
(`internal/govern/types.go:206-229`); adding a struct field alone would
silently drop the section on every `readFile`, and — because every setter is
a read-modify-write of the whole `File` — the first `governor set` after
`set-resource` would strip it from disk. R1 therefore extends
`File.UnmarshalJSON` to decode the top-level `resources` key on the
pools-shaped path (the legacy flat-document path genuinely yields nil), adds
a `Store.Resources()` read accessor following the fail-open
`MinFreeMemoryMB` precedent (`internal/engine/govern.go:94`) so the engine
has a defined primitive for L3/L4's resolution, and new setters follow the
`SetMinFreeMemoryMB` preserve-don't-reset precedent (`govern.go:106-144`).
The §5 round-trip test (write resources → `SetCap` → read resources) pins
the decoder, which is where the loss would occur.

**Resolution order per kind:** machine `capacity`/`mem_mb` override →
project vocabulary `mem_mb` → defaults (**capacity 1, mem_mb 0**). The
capacity default **always binds**: a kind declared on a bead but absent from
both configs — including a machine with no `resources` section at all — is
serialized against other holders of the same kind. This is the
fail-safe-serial default, matching the footprint convention, and it is the
whole point of §1.2: two `res:kind-cluster` beads must not co-dispatch on an
unconfigured laptop. (Distinct unknown kinds do not collide with each other,
unlike `domain:unknown`.) An unconfigured machine gets no *reservations*
(mem_mb 0) — the memory half of the design does need calibration (R9).

**Lease schema.** `govern.Lease` gains `Resources []string` (resolved kinds)
and `MemReserveMB int`, both `omitempty`. `Store.Acquire`
(`internal/govern/govern.go:193`) gains two checks, both pure lease-file
arithmetic under the existing flock:

1. **Capacity:** for each requested kind, count live leases holding that
   kind **across all pools** (machine resources are cross-pool; the existing
   prune pass has already dropped dead-pid leases). Admit iff
   `holders + 1 <= capacity(kind)`.
2. **Reservation-aware memory** (L5): see below.

Both clauses run on the normal path *and* on the half-open circuit-breaker
probe grant (`govern.go:213-242`), which today returns early before the
cap/fair-share section — a resource-declared bead admitted as the breaker
probe must not skip the capacity count. The clauses are pure arithmetic, so
running them in that branch violates no I7 constraint; a resource-denied
probe candidate leaves the probe slot open for the next caller.

`Hold` keeps its no-recheck 1:1 contract (`govern.go:322-333`) and the
engine threads `Resources`/`MemReserveMB` into the lease it rewrites from
the persisted ledger slot (`dispatchReq`/`Slot`, L3) — never a govern-side
read of the prior lease file, which after a prune gap does not exist.
Accounting must tolerate leases that appear via `Hold` without a prior
`Acquire` (the requeue/resume path; see §7 for the capacity-breach window
this deliberately accepts). Note `Hold` restamps `AcquiredAt` on every
rewrite — the ramp clock (L5) restarts per (re)bind, which is the
over-reserving, safe direction. `Release` and pid-liveness pruning free
capacity exactly as they free slots today — with the explicit caveat that a
freed *lease* is not a torn-down *cluster* (L7 owns that gap).

### L3 — Engine wiring: typed denials, skip-vs-break

`acquireGlobalSlot` (`internal/engine/govern.go:141-163`) today returns a
bare bool, and both dispatch loops treat any denial as "break the batch,
retry next boundary" (`internal/engine/wave.go:370-383`,
`internal/engine/rolling.go:205-215`). That is correct for machine-wide
denials (nothing else will fit either) and **wrong** for per-bead denials: a
bead deferred on `kind-cluster` capacity says nothing about the lightweight
docs bead behind it. So `acquireGlobalSlot` returns a typed verdict:

- `deny-cap` — pool cap, fair share, breaker open/half-open-with-probe, and
  dispatch smoothing all classify here: pool-wide conditions, **batch-break**
  as today (skipping through a smoothing window would spin the whole batch).
- `deny-memory` splits by whose demand tips the inequality: a **pure floor
  breach** (would deny even a zero-reserve candidate) batch-breaks as today;
  a denial only after subtracting the *candidate's own* `MemReserveMB` is a
  per-bead condition and **skips** — the 6 GB cluster bead defers, the
  0-reserve docs bead behind it still dispatches.
- `deny-resource(kind, holder)` — **skip this item, continue the batch.**
  Deferral messages name the kind and holder (`"bead X: deferred — resource
  kind-cluster at capacity (1/1, held by Y)"`) and ride the structured
  deferral events (koryph-6g2.1 plumbing).

Both loops share the change by construction. One pacing fix rides along: in
wave mode, a boundary that dispatched nothing with nothing active returns
from `pollUntilIdle` immediately and re-scans in a tight loop — tolerable
for cap denials that clear in minutes, a hot-spin for a capacity-1 kind held
for hours by another project. R3 adds a poll-tick sleep to that
zero-dispatched/zero-active/denied boundary so I6's "retried at the next
boundary" has a defined cadence in both loops.

**Resolution and persistence.** The engine resolves the bead's declaration
once at dispatch: `kinds := sched.ResourcesFor(issue)`,
`memReserve := Σ mem_mb(kind)` (machine `Store.Resources()` override →
project vocabulary), attaches both to the `govern.Lease`, and persists them
on the ledger slot as `Slot.Resources []string` + `Slot.MemReserveMB int`
(additive; the resolved values, not the labels, per I8 — a vocabulary edit
mid-run must not re-price a live slot). `dispatchReq` threads them through
every requeue exactly like `dispatchReq.footprint`
(`internal/engine/wave.go:723`), and an `activeResources()` helper mirrors
`activeFootprints()`'s persisted-first fallback chain
(`internal/engine/wave.go:647-665`) so `--resume` adopts holdings correctly.
One asymmetry to note: for footprints the terminal fallback degrades to the
maximally-conservative `domain:unknown`; for resources it degrades to the
empty set — maximally *permissive* (L1's inverted default). Persistence at
dispatch is therefore the load-bearing mechanism, not a fast path.

### L4 — Wave packing (sched)

`govern.Acquire` is the authoritative, cross-engine check, but it runs one
bead at a time at dispatch. `sched.BuildWave` should not *select* two
capacity-1 beads into the same batch in the first place — that wastes a slot
acquire and mis-orders the deferral. So `BuildWave`'s greedy loop
(`internal/sched/wave.go:130-145`) gains a resource check between the width
cap and the footprint checks:

- `sched.Opts` gains `ActiveResources map[string][]string` (in-flight
  holdings by bead id, from `activeResources()`) and
  `ResourceCapacity map[string]int` (effective per-kind capacities, resolved
  by the engine via `Store.Resources()` → default 1; sched stays pure and
  config-free, like `AreaMap` via `cfg`).
- A candidate whose kinds would exceed capacity against
  {in-flight holdings} ∪ {already-selected items} defers with
  `"resource <kind> at capacity (held by <id>)"` — per-item, so packing
  continues past it, preserving priority order among the beads that *can*
  run. `Item` carries the parsed kinds so the engine doesn't re-derive them.

This is project-local and advisory; the flocked check in L2 remains the
cross-project truth (a bead BuildWave selects can still be denied at
`Acquire` by another engine's holdings — that denial skips, per L3). Both
existing in-flight-exactness mechanisms extend unchanged:
persisted-at-dispatch claims preferred over recomputation, and deterministic
first-blocker reporting.

### L5 — Reservation-aware memory admission

The floor check becomes demand-aware. Engine-side, *outside* the flock (I7),
the probe and floor resolution are unchanged (`memStat`, `memoryFloorMB`).
The engine passes the reading into `Acquire`, which — under the flock, where
it can see every engine's leases — admits iff:

```
availMB − Σ ramping-lease MemReserveMB − candidate MemReserveMB ≥ floorMB
```

- A lease is **ramping** while `now − AcquiredAt < ramp_seconds` (default
  600; per-kind override). After the ramp the resource is assumed
  materialized — its consumption now shows in the real `availMB` reading, so
  the reservation retires to avoid double-counting. Ramp state derives from
  the lease's `AcquiredAt`, which `Hold` restamps on every rewrite, so the
  clock restarts per (re)bind — over-reserving, the safe direction. The
  converse failure is real and documented in §7: an agent that provisions
  its cluster *late* (setup → implement → e2e at the end, past the ramp)
  has already retired its reservation with nothing materialized, re-opening
  §1.1 for that kind. `ramp_seconds` must be sized to worst-case
  time-to-provision, not typical; the durable v2 fix is retiring on
  *observed* materialization via the L7 probe rather than on a timer.
- A candidate with `MemReserveMB == 0` (undeclared bead) degrades exactly to
  today's floor check — minus other beads' outstanding reservations, which is
  the point: the wave-of-four scenario in §1 now admits the first cluster
  bead, reserves 6 GB, and defers the next three *before* the host feels it.
- Callers that pass no reading (or a zero floor) skip the memory clause but
  keep the capacity clause; every error path fails open (I6). The engine's
  cheap pre-flock `memoryAdmits` stays as-is — a fast reject that saves a
  flock acquisition under real pressure.

No per-agent baseline reservation in v1: the auto floor (1/8 RAM,
koryph-930) already covers agent+worktree stacking, and a nonzero default
baseline would violate I9 (no labels = today's behavior). Revisit only with
ledger evidence (L7 makes the evidence).

### L6 — Planning-time declaration and the agent contract

**Planning guidance.** koryph-plan gains a step inside the frontier-gated
block (between footprints and dependencies — it is scheduler-correctness
work, so it belongs inside the gate, per the koryph-plan.md structure):

> **Declare external runtime resources — do not guess.** For every bead, ask
> what must be *running* for its acceptance criteria: a kind/k8s cluster, a
> docker compose stack, a dev server, a database, a browser suite. Label
> `res:<kind>` per kind (vocabulary in `koryph.project.json` `resources`;
> add new kinds there with a `mem_mb` estimate in the same change).
> Footprints protect the merge; resources protect the machine. Undeclared
> resources risk thrashing the host mid-wave; over-declared only costs
> parallelism.

The same rule, in the house one-liner-mantra style, is replicated across
every surface that carries the footprint rules today: koryph-issue.md step 3,
koryph-replan.md (repair pass: beads whose description mentions
kind/docker/compose/server but carry no `res:*`), koryph-import.md step 3,
koryph-ops.md, `agents/koryph-architect.md`, the plan-scorer's
scheduler-correctness checks, the `internal/agentsmd` template (AGENTS.md is
the only carrier for non-Claude runtimes), CLAUDE.md (one line + pointer,
cache-warmth budget), and the docs (a `docs/concepts/resources.md` sibling
of the footprints concept page, plus running-waves). Edits go to
`internal/commands/*.md` / `agents/*.md` — never `.claude/` copies. Note
that `agents/`, `AGENTS.md`, and `CLAUDE.md` are all merge-protected paths,
so those three surfaces are orchestrator-applied by construction (§6 R6/R9);
the rollout to already-onboarded projects is
`koryph project install-assets --all-projects all --force` (scaffold's
hash-aware installer skips drifted files otherwise).

**Agent contract.** `promptc.Compile` emits a `RESOURCES` block when the
bead declares kinds: you declared these kinds; provision at most what you
declared; name instances after the bead (`<kind>-<bead-id>`, e.g.
`kind-koryph-abc`) so leak detection can attribute them; tear them down
before you exit, including on a SIGTERM checkpoint; list anything you could
not tear down in SUMMARY.md. The naming convention is what makes L7's probes
attributable rather than heuristic.

### L7 — Observability, leak detection, hygiene

- **`koryph governor show`** gains a resources section: per kind —
  capacity, live holders (bead ids), reserved-vs-materialized MB, ramp
  state. This is the operator's direct answer to "which threads are using
  which shared resources right now".
- **Cockpit/TUI/IDE.** The primary live surfaces read through
  `internal/cockpit` (the IDE is contractually forbidden from reading
  govern/ledger files directly — `cmd/koryph/cockpit.go`), so `governor
  show` alone is unreachable from them. The cockpit snapshot gains per-kind
  resource state on its governor view, and the queue view gains a
  resource-deferred classification alongside the existing footprint-deferred
  one (`internal/cockpit/queue.go`), so a resource-deferred bead is
  explained, not mysterious, in the TUI and VS Code. Old snapshots simply
  omit the section.
- **Health patrol** (`internal/engine/health.go`, 10-min cadence) gains a
  finding, detected **engine-side** (govern's prune silently removes
  dead-pid lease files and records nothing, so it cannot be the signal): a
  slot whose agent died (poll's dead-agent handling) with non-empty
  `Slot.Resources` and no clean-teardown marker in its SUMMARY/status is a
  *suspected leak* — the cluster outlives the pid. Report, never
  auto-teardown.
- **Per-kind probe (opt-in).** A kind's machine config may carry a
  `probe` command (e.g. `kind get clusters`) that lists live instance names.
  The patrol and `koryph doctor` diff probe output against the
  `<kind>-<bead-id>` naming convention and live leases: an instance matching
  the convention with no live lease → leak finding with a suggested manual
  teardown command. Probes are operator-authored shell (same trust model as
  the project `gate`), run only from patrol/doctor (I7), fail-soft.
- Resource deferrals ride the existing structured-log deferral events
  (deferrals-by-token metric family gains the kind as its token).
- v2 candidates, filed-not-scheduled: auto-teardown with an explicit opt-in
  flag; shared long-lived instances (a resource *provider* that provisions
  one cluster and hands connection info to co-running readers); probe-based
  reservation retirement (L5); per-kind observed-cost calibration feeding
  `mem_mb` the way estimator bias correction feeds cost estimates.

## 4. Compatibility

| Surface | Behavior |
|---|---|
| Beads with no `res:*` labels | Byte-for-byte today's behavior under any configuration (I9): no capacity checks bind, `MemReserveMB` 0, memory clause degrades to the koryph-930 floor. |
| `res:*` beads, governor.json without `resources` | Reservations off (mem_mb 0); declared kinds **still serialize at the default capacity 1** — the fail-safe-serial default binds without configuration. |
| governor.json `resources` section | Requires the R1 `File.UnmarshalJSON` extension — a struct-field-only change is silently dropped at `readFile` and then stripped by the next setter rewrite. Legacy flat-document migration unaffected (nil section). |
| `koryph.project.json` schema | Additive `resources` map; requires `go generate ./internal/project` + committed schema regeneration; protected path → vocabulary changes are orchestrator-applied (R9). |
| Ledger schema | Additive only (`Slot.Resources`, `Slot.MemReserveMB`); old ledgers load with zero values. Coordinate with 2026-07-state-versioning if both land (ledger surface fingerprint bump). |
| govern schema | Additive `Lease.Resources`/`Lease.MemReserveMB`; old leases decode as resource-free; round-trip test (write resources → `SetCap` → read) pins the decoder+setter pair. |
| `--resume` / adopted slots | `activeResources()` persisted-first fallback; `Hold` re-attaches with the slot-threaded resources; a pre-upgrade slot resumes resource-free (accurate: it was admitted without them). |
| `--only` / `--once` (koryph-build) | Checks apply unchanged; a capacity-deferred sole bead exits `no dispatchable work` with the kind-and-holder reason — retry after the holder lands. |
| Wave vs rolling loop | Identical by construction: shared `BuildWave` opts, shared `acquireGlobalSlot` verdicts. Skip-vs-break and the wave-mode boundary pacing change both loops' shared path in one commit (R3). |
| Cockpit snapshots | Additive resources section on the governor view + a new queue deferral state; old snapshots omit it, IDE/TUI render it when present. |
| CLI | `koryph governor set-resource <kind> --capacity N [--mem-mb M] [--ramp-seconds S] [--probe CMD]` and `koryph governor set-resource <kind> --unset` (third subverb beside `show`/`set`); `governor show`/`doctor` extended. Existing flags unchanged. |
| Engine full-run test fixtures | Already disable the memory gate for hermeticity; resource checks are inert without `res:*` labels, so fixtures stay hermetic by default. |

## 5. Testing

- **govern:** capacity accounting across pools under the flock (two `Store`s
  on one dir simulating two engines — the §1.3 race test: both try to admit
  a 6 GB bead against 8 GB free; exactly one wins); default-capacity-1
  binding with no `resources` section; half-open probe grant respects the
  capacity clause; ramp restart on `Hold` rewrite; `Hold`-without-`Acquire`
  counting; prune frees capacity; the decoder/setter round-trip (pools +
  resources document → `SetCap` → resources intact); legacy-document
  migration.
- **sched:** `ResourcesFor` grammar (multi-label, dedupe, non-interference
  with `FootprintFor` — a `res:*`-only bead still gets `domain:unknown`);
  packing tests in the `internal/sched/wave_test.go` style (capacity 1
  defers the second holder with the right reason; capacity 2 admits both;
  per-item skip preserves priority order past a blocked bead; in-flight
  holdings via `Opts.ActiveResources` gate exactly like `Opts.Active`).
- **engine:** skip-vs-break loop semantics with a fake governor (resource
  denial continues, cap/smoothing denial breaks, candidate-tipped memory
  denial skips, pure floor breach breaks) in both loops; the wave-mode
  pacing sleep on a fully-deferred boundary; freeze-through-requeue
  (relabel + vocabulary edit mid-run, requeued slot keeps its original
  claim); reservation math with injected probe (`memgate_test.go`
  extension); resume adoption of holdings (model:
  `wave_footprint_test.go`).
- **Console/golden:** new deferral lines in `console_golden_test.go`;
  structured deferral events carry the kind.
- **CLI/cockpit:** governor set-resource/show round-trip; doctor leak
  finding from a synthetic probe script; cockpit snapshot round-trip with
  and without the resources section.

## 6. Sequencing (bead map)

- **R1** `govern`: lease + file schema **including the `File.UnmarshalJSON`
  extension**, `Store.Resources()` accessor, capacity check (normal +
  half-open paths), reservation check, preserve-style setters,
  `governor show` data (L2, L5 core). *(refactor-core)*
- **R2** `sched`: `ResourcesFor`, `Opts` extensions, packing + deferral
  reasons (L1, L4). *(refactor-core)* — parallel with R1.
- **R3** `engine`: typed verdicts, skip-vs-break + memory-denial split,
  wave-mode boundary pacing, ledger persistence + requeue threading +
  `activeResources`, memStat handoff into `Acquire` (L3, L5 wiring).
  *(refactor-core; depends R1+R2)*
- **R4** `promptc`: RESOURCES block + instance-naming contract (L6 agent
  half). Depends R2 for `ResourcesFor`. Loop-dispatchable — `promptc` has no
  `area_map` key today; label `fp:go:promptc` (and note the area_map gap for
  the next orchestrator-applied config change).
- **R5** `cmd/koryph`: `governor set-resource` (incl. `--unset`) + `show`
  resources section (L7 CLI). `area:cli:ops`; depends R1.
- **R6** guidance: koryph-plan/issue/replan/import/ops command sources +
  agentsmd template (L6). `area:cli` (internal/commands) — loop-dispatchable.
  The `agents/*.md` persona edits and the CLAUDE.md one-liner are
  protected-path → **orchestrator-applied**; the koryph.project.json
  vocabulary seed belongs to R9 (R6 references it, doesn't duplicate it).
- **R7** docs: `docs/concepts/resources.md`, running-waves (fix its stale
  "fp:* wins over area:*" claim while touching it), global-governor page.
  `area:docs`; depends R1–R3 for accuracy.
- **R8** `doctor` + health patrol: engine-side leak findings, probe diffing
  (L7). Patrol half is engine code → *(refactor-core)*; doctor half
  `area:doctor`. Depends R1.
- **R9** **HUMAN, no-dispatch**: calibrate this host — set `kind-cluster`
  capacity/`mem_mb`/probe on the operator machine, seed the
  koryph.project.json vocabulary (protected path), then verify with one live
  wave containing a declared bead.
- **R10** `cockpit` + IDE surfaces: resources on the governor view, queue
  resource-deferred state (L7). `area:ide` + the cockpit Go footprint;
  depends R1+R3.

R1 ∥ R2 → R3 → {R4 ∥ R5 ∥ R7 ∥ R8-doctor ∥ R10}; R6 anytime after the
grammar is fixed (R2 merged). R1–R3 and R8's patrol half are `refactor-core`
(orchestrator-authored on main, never loop-dispatched — self-hosting safety
rule). Peak loop-dispatched width ≈ 5 (R4, R5, R7, R8-doctor, R10 after R3
lands).

## 7. Risks

- **Lying or missing declarations.** An undeclared cluster bead thrashes
  exactly as today — the design's floor backstop, guidance saturation (every
  planning surface), replan repair pass, and patrol findings shrink the
  window but cannot close it. Accepted: enforcement against a lying agent
  would require interruption (violates I5) or cgroup-style isolation (docker
  VM memory is not attributable to the agent's process tree on darwin
  anyway).
- **Requeue window can breach capacity.** Requeues re-attach via `Hold`
  (no recheck, by contract) after a backoff sleep during which any engine's
  `Acquire` may prune the dead-pid lease — a competing bead can be admitted
  against the freed count, then the requeue's `Hold` re-adds its lease:
  transiently 2 holders on a capacity-1 kind, and the re-provisioned demand
  re-enters unchecked. Accepted and bounded: the window is the requeue
  backoff; the original physical instance usually still exists (so the
  memory picture is roughly honest even when the count is not); the ramp
  restart on rebind over-reserves in compensation. Re-running `Acquire` on
  requeues would trade this for stranding warm resumes — rejected for v1.
- **Late provisioning defeats the ramp.** A bead that provisions its
  declared resource after `ramp_seconds` has already retired its reservation
  with nothing materialized — §1.1 re-opens for that kind (the inverse,
  early-materialization overlap merely over-reserves, which is safe). Size
  `ramp_seconds` to worst-case time-to-provision; the v2 probe-based
  retirement is the durable fix.
- **Starvation of heavy beads.** A capacity-1 kind held by a long-running
  bead defers every other holder each boundary. Within a project, priority
  ordering in `BuildWave` means the highest-priority waiter wins when
  capacity frees; across projects/engines it is first-boundary-wins, biased
  toward rolling-mode engines (per-tick retries) over wave-mode ones. No
  aging/reservation queue (I6 forbids queues). Accepted for v1; observable
  via deferral metrics.
- **Double-count during ramp overlap.** A materializing resource is counted
  both by its reservation and (partially) by the shrinking `availMB` reading
  until the ramp expires. Direction is safe (under-admission); bound is one
  ramp window **per (re)bind** (the clock restarts on requeue). Mitigation:
  per-kind `ramp_seconds` tuning.
- **Vocabulary drift.** `mem_mb` estimates go stale as stacks evolve; a
  too-low estimate re-opens gap §1.1 for that kind. The L7 v2 calibration
  lever (observed deltas per kind) is the durable fix; until then estimates
  are operator-owned like quota ceilings.
- **Leaked instances.** Lease-freed ≠ torn-down. v1 detects (patrol/doctor +
  naming convention + probes) and reports; auto-teardown is deliberately
  deferred behind an explicit opt-in (killing the operator's own cluster is
  worse than a leak).
- **governor.json growth.** The `resources` section is a small fixed map
  (kinds × 4 scalar fields), no per-event history — within the
  bounded-state convention (`aimd.go` maxRecentEvents precedent).

## Beads (filed 2026-07-09)

| Epic | ID | Children |
|---|---|---|
| Resource-aware dispatch | koryph-4ql | .1 govern (R1, refactor-core) · .2 sched (R2, refactor-core) · .3 engine (R3, refactor-core) · .4 promptc (R4) · .5 cli (R5) · .6 guidance (R6) · .7 docs (R7) · .8 doctor+patrol (R8, refactor-core) · .9 HUMAN calibrate (R9, no-dispatch) · .10 cockpit/IDE (R10) |

Deps: .3←{.1,.2}; .4←.2; .5←.1; .6←.2; .7←.3; .8←.3; .9←.5; .10←.3.
Built inline by the orchestrating session (operator direction 2026-07-09)
rather than loop-dispatched; refactor-core marks retained for the record.
