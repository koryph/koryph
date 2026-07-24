// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package resmon samples the OS resource usage (CPU time, resident memory, and
// — where the platform exposes it — disk I/O) of an agent process tree, so the
// koryph engine can record per-bead efficiency metrics in the run ledger and
// the cockpit can surface avg/peak memory, CPU utilization, and I/O per bead.
//
// # Design: one snapshot, many trees
//
// A koryph run has several agent processes alive at once. Rather than probe
// each PID independently, callers take ONE process-table Snapshot per tick and
// then Aggregate the subtree rooted at each slot's PID. That keeps the cost at
// one syscall sweep (Linux: a /proc scan; macOS: a single `ps` invocation) per
// tick regardless of how many slots are running.
//
// A bead's agent typically spawns children (the runtime wrapper, the model CLI,
// tool subprocesses), so usage is always aggregated over the whole subtree
// rooted at the slot PID, not just that one process.
//
// # Platform backends and the kernel-hook roadmap
//
// Snapshot has a per-platform implementation (build-tagged, mirroring
// internal/sysmem):
//
//   - linux   — reads /proc/<pid>/stat (CPU jiffies, RSS pages) and
//     /proc/<pid>/io (disk read/write bytes). No privileges, no cgo.
//   - darwin  — shells out to `ps -axo pid=,ppid=,rss=,time=` (cgo-free, the
//     same shell-out convention sysmem_darwin uses for vm_stat). macOS exposes
//     no unprivileged cgo-free per-process disk-I/O counter, so IOAvailable is
//     false there.
//   - other   — a stub that reports nothing is available.
//
// These are the always-on, portable baselines. Higher-accuracy KERNEL-HOOK
// backends are the intended evolution and are documented in
// docs/designs/2026-07-process-metrics.md:
//   - Linux:  an eBPF backend (CPU scheduling + block-I/O tracepoints, or
//     taskstats netlink / cgroup-v2 io.stat) for low-overhead, exact I/O and
//     scheduling accounting.
//   - Darwin: proc_pid_rusage(RUSAGE_INFO_V4) — which DOES expose per-process
//     ri_diskio_bytes{read,written} plus CPU and phys_footprint — or DTrace.
//
// The Snapshot seam is the single extension point: a kernel-hook backend
// implements the same ProcTable and everything above it is unchanged.
package resmon

import "time"

// procInfo is one process's contribution to the table. All fields are absolute
// (cumulative-since-start for CPU and I/O; instantaneous for RSS).
type procInfo struct {
	pid       int
	ppid      int
	pgid      int     // process-group id (== the Setsid leader's pid for an agent)
	birthID   string  // opaque, stable process-start identity; empty when unavailable
	rssKB     int64   // resident set size, kilobytes
	cpuSec    float64 // cumulative CPU time (user+system), seconds
	ioReadKB  int64   // cumulative disk bytes read, kilobytes (0 when unavailable)
	ioWriteKB int64   // cumulative disk bytes written, kilobytes (0 when unavailable)
}

// ProcTable is a point-in-time snapshot of the process tree, used to aggregate
// the subtree under one or more slot PIDs. Build it once per tick via Snapshot.
type ProcTable struct {
	byPID       map[int]procInfo
	children    map[int][]int // ppid → child pids
	pgroup      map[int][]int // pgid → member pids
	ioAvailable bool
}

// newProcTable builds a ProcTable from a flat process list. Platform Snapshot
// implementations collect the procInfo slice and call this to index it.
func newProcTable(procs []procInfo, ioAvailable bool) *ProcTable {
	t := &ProcTable{
		byPID:       make(map[int]procInfo, len(procs)),
		children:    make(map[int][]int, len(procs)),
		pgroup:      make(map[int][]int, len(procs)),
		ioAvailable: ioAvailable,
	}
	for _, p := range procs {
		t.byPID[p.pid] = p
		t.children[p.ppid] = append(t.children[p.ppid], p.pid)
		t.pgroup[p.pgid] = append(t.pgroup[p.pgid], p.pid)
	}
	return t
}

// Sample is the aggregate resource reading of one process subtree.
type Sample struct {
	// RSSKB is the summed resident memory of every process in the subtree, KB.
	RSSKB int64
	// CPUSeconds is the summed cumulative CPU time of the subtree, seconds.
	CPUSeconds float64
	// IOReadKB / IOWriteKB are the summed cumulative disk bytes of the subtree,
	// KB. Only meaningful when IOAvailable is true.
	IOReadKB  int64
	IOWriteKB int64
	// IOAvailable reports whether the platform supplied disk-I/O counters.
	IOAvailable bool
	// PIDs is the number of processes that contributed to this sample.
	PIDs int
}

// Aggregate sums the resource usage of one agent's whole process cohort rooted
// at rootPID. found is false when rootPID is not in the table (the process has
// already exited), in which case Sample is the zero value.
//
// The cohort is the UNION of two sets, because a koryph agent is dispatched as a
// Setsid session/process-group leader (internal/dispatch/cli.go) and its tools
// fork freely:
//
//  1. every process sharing rootPID's process group — this catches grandchildren
//     that reparented to init when an intermediate process exited, which a pure
//     parent-tree walk would lose; and
//  2. the parent→child subtree rooted at rootPID — this catches a descendant
//     that put itself in a different process group but is still a child.
//
// Membership is tracked in a single set, so overlaps are counted once and a
// cycle in the reported parent links (which the kernel never produces, but a
// stale snapshot might momentarily imply after PID reuse) cannot loop forever.
func (t *ProcTable) Aggregate(rootPID int) (Sample, bool) {
	if t == nil {
		return Sample{}, false
	}
	root, ok := t.byPID[rootPID]
	if !ok {
		return Sample{IOAvailable: t.ioAvailable}, false
	}

	included := make(map[int]bool, 8)
	queue := make([]int, 0, 8)
	enqueue := func(pid int) {
		if _, known := t.byPID[pid]; known && !included[pid] {
			included[pid] = true
			queue = append(queue, pid)
		}
	}
	// Seed with the process group and the root, then BFS the parent tree.
	for _, pid := range t.pgroup[root.pgid] {
		enqueue(pid)
	}
	enqueue(rootPID)
	for i := 0; i < len(queue); i++ {
		for _, child := range t.children[queue[i]] {
			enqueue(child)
		}
	}

	s := Sample{IOAvailable: t.ioAvailable}
	for pid := range included {
		p := t.byPID[pid]
		s.RSSKB += p.rssKB
		s.CPUSeconds += p.cpuSec
		s.IOReadKB += p.ioReadKB
		s.IOWriteKB += p.ioWriteKB
		s.PIDs++
	}
	return s, true
}

// HasCohortPeer reports whether rootPID has another live process in its agent
// cohort. found is false when the root is absent, so callers can fail closed
// when a process table races an agent exit. Aggregate includes both ordinary
// descendants and a reparented process that remains in the agent's Setsid
// process group; that latter case is still live tool work and must veto a
// recovery SIGTERM.
func (t *ProcTable) HasCohortPeer(rootPID int) (has, found bool) {
	s, found := t.Aggregate(rootPID)
	return found && s.PIDs > 1, found
}

// ProcessIdentity returns the opaque process-start identity for pid. A present
// process without an identity is deliberately reported unavailable: callers
// that may reattach or signal a previously recorded PID must fail closed rather
// than mistake a recycled numeric PID for the agent they dispatched.
func (t *ProcTable) ProcessIdentity(pid int) (string, bool) {
	if t == nil {
		return "", false
	}
	p, ok := t.byPID[pid]
	if !ok || p.birthID == "" {
		return "", false
	}
	return p.birthID, true
}

// MatchesProcess reports whether pid still identifies the process that was
// recorded with want. Empty identities never match, so legacy ledgers and
// unavailable platform readings cannot authorize a reattach or SIGTERM.
func (t *ProcTable) MatchesProcess(pid int, want string) bool {
	got, ok := t.ProcessIdentity(pid)
	return ok && want != "" && got == want
}

// Usage is the lifetime aggregate of a slot's resource usage, updated
// incrementally as samples arrive over the slot's run. It is the shape the
// engine persists to the ledger and the cockpit renders.
type Usage struct {
	// Samples is how many readings have been folded in.
	Samples int
	// PeakRSSKB is the maximum subtree RSS observed, KB.
	PeakRSSKB int64
	// SumRSSKB is the running sum of subtree RSS, KB, used to derive the mean.
	SumRSSKB int64
	// CPUSeconds is the cumulative CPU time of the whole cohort, seconds — taken
	// as the max reading seen so it never regresses. On Linux each process's
	// contribution includes its reaped children (cutime/cstime), so CPU consumed
	// by short-lived subprocesses that exited between samples IS counted, and the
	// cohort sum grows monotonically toward the true total. On darwin `ps` only
	// reports a process's OWN CPU, so a burst of already-exited sequential
	// subprocesses undercounts until the proc_pid_rusage kernel-hook backend
	// (docs/designs/2026-07-process-metrics.md §5.2) lands.
	CPUSeconds float64
	// IOReadKB / IOWriteKB are the latest cumulative disk totals, KB (max seen).
	IOReadKB  int64
	IOWriteKB int64
	// IOAvailable is true once any sample reported platform I/O counters.
	IOAvailable bool
}

// Add folds one Sample into the lifetime Usage.
func (u *Usage) Add(s Sample) {
	u.Samples++
	u.SumRSSKB += s.RSSKB
	if s.RSSKB > u.PeakRSSKB {
		u.PeakRSSKB = s.RSSKB
	}
	if s.CPUSeconds > u.CPUSeconds {
		u.CPUSeconds = s.CPUSeconds
	}
	if s.IOReadKB > u.IOReadKB {
		u.IOReadKB = s.IOReadKB
	}
	if s.IOWriteKB > u.IOWriteKB {
		u.IOWriteKB = s.IOWriteKB
	}
	if s.IOAvailable {
		u.IOAvailable = true
	}
}

// AvgRSSKB is the mean subtree RSS across all folded samples, KB (0 with none).
func (u *Usage) AvgRSSKB() int64 {
	if u.Samples == 0 {
		return 0
	}
	return u.SumRSSKB / int64(u.Samples)
}

// CPUUtilPct is the average CPU utilization over wall-clock window wall, as a
// percentage: 100 means one core fully saturated for the whole window; a tree
// using four cores flat out reads ~400. Returns 0 for a non-positive window.
func (u *Usage) CPUUtilPct(wall time.Duration) float64 {
	if wall <= 0 {
		return 0
	}
	return u.CPUSeconds / wall.Seconds() * 100
}
