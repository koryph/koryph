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
| `~/.koryph/governor.json` | `{ "max_global_agents": N }` — the machine-wide cap. Absent ⇒ default **4**. Edited only by the machine owner (never per-run), so no single project can lift the ceiling. |
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

### CLI

- `koryph governor` — show the cap, active leases (project/bead/pid/age), demand
  heartbeats, and free slots, so operators can see contention.
- `koryph governor set --max-global N` — write `governor.json`.
- `board` / `status` prune stale leases and demand as a hygiene side effect.

## Failure & edge handling

- **Stale leases**: reclaimed by PID liveness (`dispatch.Alive`) plus a TTL
  backstop, so a crash never permanently consumes a slot.
- **`n > cap`**: some `fairShare` values are 0 for a round; rotation guarantees
  each project periodically gets a turn.
- **Lock contention**: the flock is held for microseconds; acquisition races
  resolve on the next re-check.
- **Absent `governor.json`**: default cap 4 — chosen so a single project's
  typical width (3) is unaffected; the cap only bites when projects contend.

## Testing

- N-goroutine `Acquire`/`Release` against a temp `KORYPH_HOME`:
  - the cap is **never** exceeded under contention;
  - fair share is respected across demanders;
  - dead-PID leases and stale demand are reclaimed;
  - rotation prevents starvation when `n > cap`;
  - work-conserving top-up uses idle capacity when others are satisfied.

## Implementation phases (each gated green)

1. **`internal/govern`** — lease + demand store, flock, `paths.SlotsDir()`,
   `Acquire` / `Release` / `Prune` / `FairShare`, `governor.json` load/save.
   Full unit tests.
2. **Engine integration** — demand heartbeat + acquire/release in the wave and
   poll paths; the fair-share width warning.
3. **CLI** — `koryph governor` show/set; prune on `board`/`status`.
