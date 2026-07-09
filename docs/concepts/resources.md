<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Machine: resources

*This page expands the [Concepts overview](index.md). See
[Running waves](../user-guide/running-waves.md) and
[Global governor](../developer-guide/global-governor.md) for the commands
that operate it.*

## The idea

A dispatched agent is more than a `claude` subprocess in a worktree. Beads
whose acceptance criteria require a running system routinely provision
external dependencies — a kind/k8s dev cluster, a docker compose stack, a
long-running dev server, a browser test suite — and those live on the
**host**, outside the agent's process tree and outside its worktree
isolation. [Footprints](footprints.md) know nothing about this: a footprint
protects the *merge* (two agents editing the same file), not the *machine*
(two agents each starting a 6 GB cluster). koryph needs a second, separate
admission dimension for that: **resources**.

**Mantra: footprints protect the merge; resources protect the machine.**
The two systems are additive, not substitutes — a resource declaration never
relaxes a footprint conflict, and a footprint-clean bead can still be
deferred on resource capacity.

## In koryph

### Declaring a resource: `res:<kind>`

A bead declares each external resource kind it will provision or consume
with one label per kind:

```
res:kind-cluster        # will run a kind/k8s dev cluster
res:docker              # will run containers via the docker daemon
res:dev-server          # will hold a long-running local server
```

- `<kind>` is an opaque, lowercase token (`[a-z0-9-]+`), matched exactly.
  One label is one unit of that kind — multi-unit demand on a single bead is
  not supported.
- Parsed by `sched.ResourcesFor(issue)`, a sibling of `FootprintFor` with the
  same label-scanning mechanics, deduplicated and sorted.
- **Never** nest a resource under `fp:` — `fp:res:kind-cluster` would be
  parsed by the footprint grammar as the plain write token `res:kind-cluster`
  and silently become a merge-conflict lock instead of a capacity
  declaration. Resources are a separate label namespace with separate
  semantics: footprint tokens are binary reader/writer exclusion; resource
  kinds are **counted capacity**.

### When *not* to declare

A bead with **no** `res:*` labels declares "agent + worktree only" — the
common, lightweight case. This deliberately *inverts* the footprint default:
an unlabeled footprint serializes against every other unlabeled bead
(`domain:unknown`), but an undeclared resource set imposes no capacity
check at all. Serializing every undeclared bead against a phantom resource
would destroy throughput for the ordinary case where nothing external is
provisioned. The [memory admission floor](../user-guide/running-waves.md#memory-admission-floor)
remains the reactive backstop for undeclared load; honest declaration at
planning time is what makes the *proactive* half of this system work.

### Counted capacity semantics

Each kind resolves to a **capacity**: the maximum number of live leases
allowed to hold it at once, counted across every project and every provider
pool on the host (machine resources are not scoped to one provider). Capacity
**1** is the exclusive case (only one holder at a time — the common case for
a single dev cluster); capacity **N** is "up to N threads may share this
kind" — one counted mechanism covers both readings of "shared resource".

A kind declared on a bead but never configured anywhere still resolves to
capacity **1** — the default always binds, including on a machine with no
resource configuration at all. This is the fail-safe-serial default,
mirroring the footprint convention: two beads declaring the same
unconfigured kind can never co-dispatch. Unlike `domain:unknown`, though,
*distinct* unconfigured kinds do not collide with each other.

### Two config surfaces, one resolution order

Declaration (the label) and configuration (what a kind costs and how many
units the host can run) are deliberately separate, mirroring the
footprint's "portable label + per-machine `area_map`" split:

**Project vocabulary** — `koryph.project.json`'s `resources` map is the
checked-in, portable estimate of what one unit of a kind costs. It travels
with the repo, so the same commit that adds a `res:<kind>` label to a bead
adds the kind here:

```json
"resources": {
  "kind-cluster": { "mem_mb": 6144 },
  "docker":       { "mem_mb": 1024 }
}
```

**Machine capacity** — `~/.koryph/governor.json`'s top-level `resources`
section is the per-host ledger: how many units of a kind *this machine* can
run, plus any per-kind cost/ramp/probe overrides. It lives outside the
per-provider pools because RAM, dev clusters, and the docker daemon are
machine properties shared across every provider, not scoped to one:

```json
{
  "pools": { "anthropic": { "max_global_agents": 8 } },
  "resources": {
    "ramp_seconds": 600,
    "kinds": {
      "kind-cluster": { "capacity": 1, "mem_mb": 6144, "ramp_seconds": 900 },
      "docker":       { "capacity": 3 }
    }
  }
}
```

Set it with `koryph governor set-resource` — see
[Global governor](../developer-guide/global-governor.md) for the schema and
admission mechanics, and the CLI reference for the full flag set.

**Resolution order**, per kind:

- **Capacity** — the machine ledger's `capacity` when configured (`>0`),
  else the default of **1**. Capacity has no project-vocabulary
  equivalent — it is a machine-only concept, because "how many clusters can
  this host run" is a fact about the host, not the codebase.
- **Memory reservation (`mem_mb`)** — the machine ledger's `mem_mb` when set,
  else the project vocabulary's `mem_mb`, else **0** (no reservation). A
  machine with no `resources` section at all still serializes declared
  kinds at capacity 1 — it just reserves no memory for them until
  calibrated.

### Reservation-aware memory admission and the ramp window

The plain [memory admission floor](../user-guide/running-waves.md#memory-admission-floor)
is *reactive* — it reads current free RAM and defers when it is low. That
outruns materialization: a wave of cluster beads can all pass the floor
while RAM is still free, then each spend its first minutes provisioning a
multi-gigabyte stack before the pressure is even measurable. Resources make
the floor check *demand-aware*: admission also subtracts every other live
lease's outstanding memory reservation before checking the floor —

```
availMB − Σ(ramping leases' MemReserveMB) − candidate's own MemReserveMB ≥ floorMB
```

A lease is **ramping** while `now − AcquiredAt < ramp_seconds` (default 600s,
overridable globally or per kind). While ramping, its declared `mem_mb` is
subtracted from the reading as a stand-in for memory it hasn't finished
consuming yet. Once the ramp elapses the reservation retires — the resource
is assumed materialized, so its cost now shows up in the real `availMB`
reading, and keeping the reservation would double-count it.

**Late-provisioning caveat.** The ramp is a *timer*, not an observation: if
an agent provisions its declared resource late in its run (setup →
implement → e2e at the very end), the reservation may already have retired
with nothing actually materialized — reopening exactly the admission-outruns-
materialization gap this mechanism exists to close, for that one kind, for
that window. Size `ramp_seconds` to the worst case, not the typical case.
Retiring on *observed* materialization (via the leak-detection probe below)
rather than on a timer is the durable fix, not yet built.

### The agent contract

An agent whose bead declares `res:<kind>` labels receives a `RESOURCES`
block in its prompt naming the declared kinds, with four rules:

- Provision **at most** what you declared — no other resource kinds.
- Name every instance `<kind>-<bead-id>` (e.g. `kind-cluster-koryph-abc`) so
  leak detection can attribute it back to this task.
- Tear everything down before you exit, including when checkpointing on a
  SIGTERM.
- List anything you could not tear down in `SUMMARY.md`.

The naming convention is what turns leak detection from heuristic guessing
into an attributable diff.

### Leak detection

A freed **lease** is not the same thing as a torn-down **instance** —
koryph's admission accounting only ever knows about the lease. Leak
detection closes that gap at the observability layer, never the enforcement
layer: the health patrol and `koryph doctor` compare live, resource-holding
leases against each kind's `<kind>-<bead-id>` naming convention and an
optional operator-authored probe command (e.g. `kind get clusters`) that
lists live instance names for that kind. A dead agent whose slot still
carries resource kinds with no clean-teardown marker, or a probed instance
matching the naming convention with no live lease behind it, is reported as
a **suspected leak** with a suggested manual teardown command — never
auto-torn-down. Killing the operator's own cluster automatically would be
worse than leaving a leak to be cleaned up by hand.

## The failure mode it prevents

Without declared resources, nothing stops two beads that each need a kind
dev cluster from co-dispatching: footprints don't see it (no shared code
token), and the reactive memory floor only notices once both are already
mid-provision and the host is thrashing — by which point koryph's own
"never interrupt a running agent" invariant means the governor can only
watch. Declaring `res:kind-cluster` on both beads makes the second one defer
*before* dispatch, with a clear reason naming the kind and the holder,
instead of after the host has already felt it.

## Operate it

- [Running waves](../user-guide/running-waves.md) — the resource-deferral
  message shapes, alongside footprint deferrals.
- [Global governor](../developer-guide/global-governor.md) — the
  `governor.json` resources schema, admission clauses, `set-resource` CLI,
  and `governor show` observability.
- Mechanics live in `internal/sched/resources.go` (`ResourcesFor` — the
  label grammar) and `internal/sched/wave.go` (`resourceBlocker` — wave
  packing), `internal/govern/govern.go` (`checkResourcesLocked` — the
  flocked capacity + reservation admission clauses) and
  `internal/govern/types.go` (`ResourcesConfig`, `leaseRamping`), and
  `internal/engine/govern.go` (`resolveMemReserveMB`, `resourceCapacities`,
  `classifyAdmit`).
- Resources feed the same admission path as
  [rolling dispatch](rolling-dispatch.md) and the wave loop: both loops
  share `acquireGlobalSlot` and `sched.BuildWave`, so a resource-declared
  bead behaves identically under either dispatch mode.
