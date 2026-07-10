<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Per-bead process metrics: memory, CPU, I/O, and wall clock for orchestration calibration (2026-07-10)

Status: implemented 2026-07-10 (orchestrator-built inline). Baseline sampler
(procfs / `ps`) landed; kernel-hook backends (§5) are the roadmap.
Origin: operator direction (2026-07-09/10) — "would be useful to see avg
memory, iops, consumed by each beads execution. We also need to track clock
time for start, stop, and cpu utilization for each bead scheduled to analyze
efficiency and calibrate orchestration." Plus: "consider kernel hooks (eBPF or
similar) for linux and darwin for measuring process metrics (CPU, memory,
iops, etc.)".

## 1. Problem

koryph knows what each bead *cost* in dollars and tokens (the quota estimator,
`internal/quota`), and — since the resource governor (2026-07-resource-governor)
— what memory a bead *declared* it would reserve. It does not know what a bead
actually *consumed*: how much resident memory its agent process cohort held,
how much CPU it burned, how much it read and wrote to disk, and how long it ran
in wall-clock terms.

Without measured consumption the operator cannot:

- calibrate the resource governor's `mem_mb` declarations against reality;
- spot beads that are CPU-bound vs I/O-bound vs idle-waiting;
- attribute host thrash to a specific bead;
- reason about wall-clock efficiency (a bead that takes 3 hours of wall time
  but 4 minutes of CPU is mostly waiting on something).

## 2. What we measure

Per slot (bead attempt cohort), sampled on the engine poll tick and persisted
to the run ledger:

| Metric | Ledger field | Source |
|---|---|---|
| Peak resident memory (MB) | `peak_rss_mb` | max subtree RSS |
| Mean resident memory (MB) | `avg_rss_mb` | mean subtree RSS over samples |
| CPU time (s) | `cpu_seconds` | cumulative user+system of the cohort |
| CPU utilization (%) | derived | `cpu_seconds / wall_seconds · 100` |
| Disk read/written (MB) | `io_read_mb` / `io_write_mb` | Linux only (see §5) |
| Sample count | `resource_samples` | number of readings folded in |
| Start clock | `dispatched_at` (existing) | dispatch time |
| Stop clock | `finished_at` (new) | terminal transition time |
| Wall time | derived | `finished_at − dispatched_at`, or now − start while live |

CPU utilization is expressed so that 100 % is one core fully saturated for the
whole wall window; a four-core-bound bead reads ~400 %.

## 3. Architecture: one snapshot, many trees

`internal/resmon` is the sampler. Its contract is deliberately narrow:

```
Snapshot(ctx) (*ProcTable, error)   // one OS sweep of every process
(*ProcTable).Aggregate(pid) Sample  // sum one agent's process cohort
Usage.Add(Sample) / AvgRSSKB() / CPUUtilPct(wall)  // lifetime fold
```

A run has several agents alive at once, so the engine takes **one** process
table snapshot per poll pass (`pollPass`, `internal/engine/poll.go`) and then
aggregates the cohort under each live slot's PID from that same table. Cost is
one syscall sweep per tick (default 10 s) regardless of slot count.

### 3.1 What is a bead's "process cohort"

A dispatched agent is a `Setsid` session/process-group leader
(`internal/dispatch/cli.go`); `/bin/sh` → the model CLI → tool subprocesses all
inherit its process group. `Aggregate(rootPID)` therefore sums the **union** of:

1. every process sharing `rootPID`'s process group — catches grandchildren that
   reparented to `init` when an intermediate process exited, which a pure
   parent-tree walk would lose; and
2. the parent→child subtree rooted at `rootPID` — catches a descendant that put
   itself in its own process group but is still a child.

Membership is a single set, so overlaps count once and a (kernel-impossible but
snapshot-transient) parent cycle terminates.

### 3.2 Persistence model

The engine holds the running `resmon.Usage` per slot **in memory**
(`runner.resUsage`, keyed by phase id with the PID it is accumulating for) and
mirrors the derived aggregates onto the ledger slot every sample via
`MutateSlot`. The in-memory Usage is the accumulation source of truth; the
ledger is a derived snapshot. A resumed run starts a fresh Usage and the
last-persisted numbers survive — acceptable because these are best-effort
efficiency data, never correctness-critical state. `finished_at` is stamped once
from `pollSlot` after `completeSlot`, covering every terminal path from one
place, and drops the slot's in-memory Usage.

**Per-attempt semantics.** Metrics are scoped to one dispatch attempt. A requeue
reuses the phase id but gets a new PID and a fresh `dispatched_at`; the sampler
detects the PID change and resets that slot's Usage, so `cpu_seconds` (a
monotonic max) is never divided by a shorter post-requeue wall window to produce
a nonsense utilization. The ledger therefore reflects the current attempt, not a
blend across attempts.

**Sampling is off the critical path's failure modes.** The per-pass snapshot is
(a) throttled to at most once per poll interval — `pollPass` also fires on every
SIGCHLD wake, and a host-wide sweep per subprocess exit would be wasteful — and
(b) bounded by a `resSampleTimeout` so a hung `ps` or a pathologically slow
`/proc` scan can never stall the poll loop's liveness, `completeSlot`, and merge
work. Any probe error or timeout simply skips sampling for that pass.

### 3.3 Surfacing

- Ledger: the fields above (all `omitempty`, additive — old ledgers decode to 0).
- Cockpit view-model (`internal/cockpit`): `SlotSnapshot` and
  `BeadDetailSnapshot` carry the metrics plus derived `CPUUtilPct` and wall time.
- TUI: the **Detail** panel renders a Resources section (start/stop clock with
  date+time, wall, avg/peak memory, CPU seconds + utilization, disk I/O). The
  **Threads** tab keeps its density (retries, model, cost) and defers the full
  resource breakdown to Detail.

## 4. Baseline backends (implemented)

Build-tagged, mirroring `internal/sysmem`, cgo-free:

- **linux** (`resmon_linux.go`): scans `/proc`, reading each `<pid>/stat`
  (ppid, pgrp, utime+stime jiffies), `<pid>/statm` (resident pages), and
  `<pid>/io` (`read_bytes`/`write_bytes` — actual storage-layer bytes). No
  privileges. `USER_HZ` is taken as 100 (constant on mainstream Linux; only the
  CPU-seconds scale depends on it).
- **darwin** (`resmon_darwin.go`): one `ps -axo pid=,ppid=,pgid=,rss=,time=`
  sweep, the same shell-out convention `sysmem_darwin` uses for `vm_stat`. macOS
  exposes no cgo-free unprivileged per-process disk-I/O counter, so `io_*_mb`
  stays 0 there (`IOAvailable=false`). `ps` reports each process's OWN CPU only
  (no `cutime`/`cstime` equivalent), so CPU consumed by short-lived subprocesses
  that already exited undercounts on darwin — the `proc_pid_rusage` backend
  (§5.2) closes this. On Linux, `cutime`/`cstime` fold reaped descendants' CPU
  into the total, so the sequential-subprocess pattern is counted correctly.
- **other** (`resmon_other.go`): reports unsupported; sampling is skipped.

Failure of any sweep returns nil for the pass — sampling can never break the
poll loop's liveness/merge work.

## 5. Kernel-hook roadmap (the accuracy tier)

The `Snapshot` seam is the single extension point: a higher-accuracy backend
implements the same `ProcTable` and everything above it is unchanged. The
baseline sampler is a coarse poll (10 s); kernel hooks give exact, low-overhead,
continuously-integrated accounting — and, on macOS, per-process disk I/O the
baseline cannot get.

### 5.1 Linux — eBPF / taskstats / cgroup v2

- **eBPF** (`cilium/ebpf`, pure-Go loader): attach to `sched` tracepoints for
  precise on-CPU time and to block-I/O tracepoints (`block_rq_issue`/`complete`)
  for exact per-cohort I/O, keyed by cgroup or pgid. Needs `CAP_BPF`/root and a
  compiled CO-RE object; the sampler would load it once per run and read a map
  each tick instead of scanning `/proc`.
- **taskstats** (netlink `TASKSTATS` genl): kernel per-task accounting including
  delay accounting (time blocked on I/O, on the runqueue) — the "waiting vs
  working" signal — without eBPF, but needs a privileged netlink socket.
- **cgroup v2**: if each agent is dispatched into its own cgroup/systemd scope,
  `cpu.stat`, `memory.peak`, and `io.stat` give exact per-bead rollups by
  reading three files — no tree-walking, no PID reuse hazard. This is the
  cleanest option and composes naturally with the resource governor.

### 5.2 Darwin — proc_pid_rusage / DTrace

- **`proc_pid_rusage(pid, RUSAGE_INFO_V4, …)`** returns, per process,
  `ri_diskio_bytesread`/`ri_diskio_byteswritten` (the disk I/O the baseline
  lacks), plus CPU times and `ri_phys_footprint`. It is a libproc call, so a
  darwin backend using it takes on cgo (a deliberate departure from the
  cgo-free baseline) — hence staged behind a build tag / opt-in.
- **DTrace** (`io`/`proc` providers) is the heavier, SIP-sensitive alternative.

### 5.3 Why staged

The baseline lands now, works on both platforms koryph runs on, needs no
privileges, and is fully testable. The kernel-hook backends need a Linux target
with `CAP_BPF` (or cgroup delegation) and, on macOS, cgo + entitlements — none
of which the current dev/CI path exercises. They are drop-in replacements for
`Snapshot`, gated by capability detection, to be added when the accuracy or the
macOS-I/O gap justifies the operational cost.

## 6. Known limitations of PID-based sampling

These are inherent to sampling by PID and are eliminated by the cgroup-v2 /
eBPF backends (§5), which key on a stable cgroup rather than a recyclable PID:

- **PID reuse on resumed runs.** A resumed agent is not a koryph child, so the
  kernel (init) reaps it and its PID can be recycled. If the exit is not yet
  observed and the recycled PID belongs to a stranger, one sample can fold the
  stranger's cohort into the bead's Usage. The window is narrow (exact reuse
  within one poll interval) and the direct-child path is immune (the engine
  reaps the zombie itself, and that pass is the completion pass). Accuracy only,
  never a data race — all sampling is on the single tick goroutine.
- **`IOAvailable` is table-global on Linux.** It is true when *any* process
  exposed a readable `/proc/<pid>/io`; a specific cohort could still contain a
  process whose `io` was unreadable and undercount. In practice every agent
  process runs as the same user, so `io` is readable and the sum is complete.

## 7. Testing

- `internal/resmon`: aggregation (subtree + pgroup union, reparented-orphan and
  own-group cases, cycle safety), lifetime fold (peak/avg/monotonic CPU/IO),
  utilization math, and per-platform parsers (`ps` time formats, `/proc/*/stat`
  comm-with-parens robustness, `/proc/*/io`).
- `internal/engine`: the real sampler end-to-end against the test's own PID
  (derived ledger fields populated, dead PID contributes nothing) and
  `finished_at` stamping (terminal-only, idempotent, Usage dropped).
- `internal/cockpit` / `internal/tui`: metrics plumbed through the snapshot and
  rendered in the Detail Resources section with date+time clocks.
