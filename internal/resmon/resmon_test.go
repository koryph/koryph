// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package resmon

import (
	"testing"
	"time"
)

// tree builds a ProcTable from (pid, ppid, pgid, rssKB, cpuSec) tuples for tests.
func tree(ioAvail bool, rows ...[5]int) *ProcTable {
	procs := make([]procInfo, len(rows))
	for i, r := range rows {
		procs[i] = procInfo{pid: r[0], ppid: r[1], pgid: r[2], rssKB: int64(r[3]), cpuSec: float64(r[4])}
	}
	return newProcTable(procs, ioAvail)
}

func TestAggregate_SubtreeSum(t *testing.T) {
	// pgid 100 for the whole cohort. 100 → {200, 201}; 201 → {300}.
	tbl := tree(false,
		[5]int{100, 1, 100, 10, 1},
		[5]int{200, 100, 100, 20, 2},
		[5]int{201, 100, 100, 30, 3},
		[5]int{300, 201, 100, 40, 4},
		[5]int{999, 1, 999, 500, 50}, // unrelated group, must be excluded
	)
	s, ok := tbl.Aggregate(100)
	if !ok {
		t.Fatal("expected root 100 found")
	}
	if s.RSSKB != 100 { // 10+20+30+40
		t.Errorf("RSSKB = %d, want 100", s.RSSKB)
	}
	if s.CPUSeconds != 10 { // 1+2+3+4
		t.Errorf("CPUSeconds = %v, want 10", s.CPUSeconds)
	}
	if s.PIDs != 4 {
		t.Errorf("PIDs = %d, want 4", s.PIDs)
	}
}

// TestAggregate_ReparentedOrphanViaPgroup verifies the process-group union
// catches a grandchild that reparented to init (ppid=1) when its intermediate
// parent exited — the case a pure parent-tree walk from rootPID would miss. The
// grandchild keeps the agent's process group (Setsid leader = rootPID).
func TestAggregate_ReparentedOrphanViaPgroup(t *testing.T) {
	tbl := tree(false,
		[5]int{500, 1, 500, 10, 1}, // agent session leader, pgid 500
		[5]int{700, 1, 500, 40, 4}, // reparented to init (ppid 1) but still pgid 500
	)
	s, ok := tbl.Aggregate(500)
	if !ok {
		t.Fatal("expected root 500 found")
	}
	if s.PIDs != 2 || s.RSSKB != 50 || s.CPUSeconds != 5 {
		t.Errorf("aggregate = %+v, want 2 pids / 50 KB / 5s (orphan included via pgroup)", s)
	}
}

// TestAggregate_ChildInOwnGroupViaTree verifies the parent-tree arm catches a
// descendant that started its own process group but is still a child.
func TestAggregate_ChildInOwnGroupViaTree(t *testing.T) {
	tbl := tree(false,
		[5]int{500, 1, 500, 10, 1},   // leader
		[5]int{800, 500, 800, 25, 2}, // child that setpgid'd into its own group
	)
	s, ok := tbl.Aggregate(500)
	if !ok {
		t.Fatal("expected root 500 found")
	}
	if s.PIDs != 2 || s.RSSKB != 35 {
		t.Errorf("aggregate = %+v, want 2 pids / 35 KB (child included via tree)", s)
	}
}

func TestHasCohortPeerIncludesDescendantsAndReparentedTools(t *testing.T) {
	tbl := tree(false,
		[5]int{500, 1, 500, 10, 1},
		[5]int{700, 1, 500, 40, 4}, // same process group, but reparented
		[5]int{800, 500, 800, 25, 2},
		[5]int{900, 1, 900, 25, 2}, // unrelated singleton process group
	)
	if has, found := tbl.HasCohortPeer(500); !found || !has {
		t.Errorf("HasCohortPeer(500) = (%v, %v), want (true, true)", has, found)
	}
	if has, found := tbl.HasCohortPeer(900); !found || has {
		t.Errorf("HasCohortPeer(900) = (%v, %v), want (false, true)", has, found)
	}
	if has, found := tbl.HasCohortPeer(999); found || has {
		t.Errorf("HasCohortPeer(999) = (%v, %v), want (false, false)", has, found)
	}
}

func TestProcessIdentityRequiresExactBirth(t *testing.T) {
	tbl := newProcTable([]procInfo{{pid: 500, ppid: 1, pgid: 500, birthID: "linux:42"}}, false)
	if got, ok := tbl.ProcessIdentity(500); !ok || got != "linux:42" {
		t.Errorf("ProcessIdentity(500) = (%q, %v), want (linux:42, true)", got, ok)
	}
	if !tbl.MatchesProcess(500, "linux:42") {
		t.Error("matching process identity was rejected")
	}
	if tbl.MatchesProcess(500, "linux:43") {
		t.Error("recycled PID identity was accepted")
	}
	if tbl.MatchesProcess(500, "") {
		t.Error("empty recorded identity was accepted")
	}
}

func TestAggregate_MissingRoot(t *testing.T) {
	tbl := tree(false, [5]int{1, 0, 0, 1, 1})
	if _, ok := tbl.Aggregate(4242); ok {
		t.Error("expected not-found for exited root pid")
	}
}

func TestAggregate_CycleSafe(t *testing.T) {
	// A pathological parent cycle (PID reuse in a stale snapshot): 10↔11.
	// Aggregate must terminate and not double-count.
	tbl := tree(false,
		[5]int{10, 11, 10, 5, 1},
		[5]int{11, 10, 10, 5, 1},
	)
	s, ok := tbl.Aggregate(10)
	if !ok {
		t.Fatal("expected found")
	}
	if s.PIDs != 2 || s.RSSKB != 10 {
		t.Errorf("cycle aggregate = %+v, want 2 pids / 10 KB", s)
	}
}

func TestUsage_PeakAvgAndMonotonic(t *testing.T) {
	var u Usage
	u.Add(Sample{RSSKB: 100, CPUSeconds: 2, IOReadKB: 50, IOWriteKB: 10, IOAvailable: true})
	u.Add(Sample{RSSKB: 300, CPUSeconds: 5, IOReadKB: 80, IOWriteKB: 20, IOAvailable: true})
	// A transient dip must not regress the monotonic cumulative counters.
	u.Add(Sample{RSSKB: 200, CPUSeconds: 4, IOReadKB: 70, IOWriteKB: 15, IOAvailable: true})

	if u.Samples != 3 {
		t.Errorf("Samples = %d, want 3", u.Samples)
	}
	if u.PeakRSSKB != 300 {
		t.Errorf("PeakRSSKB = %d, want 300", u.PeakRSSKB)
	}
	if got := u.AvgRSSKB(); got != 200 { // (100+300+200)/3
		t.Errorf("AvgRSSKB = %d, want 200", got)
	}
	if u.CPUSeconds != 5 { // max, not the later 4
		t.Errorf("CPUSeconds = %v, want 5 (monotonic max)", u.CPUSeconds)
	}
	if u.IOReadKB != 80 || u.IOWriteKB != 20 {
		t.Errorf("IO = %d/%d, want 80/20", u.IOReadKB, u.IOWriteKB)
	}
	if !u.IOAvailable {
		t.Error("IOAvailable should be true")
	}
}

func TestUsage_CPUUtilPct(t *testing.T) {
	u := Usage{CPUSeconds: 30}
	// 30 CPU-seconds over a 60s wall window = 50% of one core.
	if got := u.CPUUtilPct(60 * time.Second); got != 50 {
		t.Errorf("CPUUtilPct(60s) = %v, want 50", got)
	}
	// Multi-core saturation reads above 100.
	u2 := Usage{CPUSeconds: 120}
	if got := u2.CPUUtilPct(60 * time.Second); got != 200 {
		t.Errorf("CPUUtilPct = %v, want 200", got)
	}
	// Non-positive window is safe.
	if got := u.CPUUtilPct(0); got != 0 {
		t.Errorf("CPUUtilPct(0) = %v, want 0", got)
	}
}

func TestUsage_EmptyAvg(t *testing.T) {
	var u Usage
	if got := u.AvgRSSKB(); got != 0 {
		t.Errorf("empty AvgRSSKB = %d, want 0", got)
	}
}
