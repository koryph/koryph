// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

// seedRunningRun writes a run ledger with two running slots under repo and
// points `latest` at it. Returns the store's repo root.
func seedRunningRun(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	st := ledger.NewStore(repo)
	run, err := st.NewRun("proj", "test", "v0")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	run.Slots["bead-1"] = &ledger.Slot{
		PhaseID: "bead-1", BeadID: "bead-1", Status: ledger.SlotRunning,
		Model: "sonnet", DispatchedAt: now,
	}
	run.Slots["bead-2"] = &ledger.Slot{
		PhaseID: "bead-2", BeadID: "bead-2", Status: ledger.SlotReview,
		Model: "sonnet", DispatchedAt: now,
	}
	if err := st.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return repo
}

// TestRefreshDecouplesSlotsFromDerived is the regression guard for the TUI
// "no threads show" bug: the cheap ledger slot data (threads) must be assembled
// and returned synchronously, while the expensive beads-sourced derived
// sections (burndown, efficiency, graph, queue) are recomputed in the
// background. If a future change re-couples them — computing the derived
// sections inline again — the first Refresh would block on bd (~15 s on a large
// project) and stamp the derived ComputedAt fields synchronously, failing this
// test.
func TestRefreshDecouplesSlotsFromDerived(t *testing.T) {
	repo := seedRunningRun(t)
	p := NewLedgerProvider("proj", repo, "")

	// First refresh: slots present immediately.
	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(snap.Slots) != 2 {
		t.Fatalf("first Refresh slots = %d, want 2 (threads must load synchronously)", len(snap.Slots))
	}
	if snap.RunStatus != ledger.RunRunning {
		t.Errorf("RunStatus = %q, want %q", snap.RunStatus, ledger.RunRunning)
	}

	// Derived sections must NOT be computed synchronously on the first tick:
	// their ComputedAt stamps are still zero because the background job that
	// fills them has not published yet. (Under the old synchronous code these
	// were stamped inline, which is exactly the behaviour that stalled the
	// Threads tab.)
	if !snap.Burndown.ComputedAt.IsZero() {
		t.Errorf("Burndown computed synchronously on first Refresh (ComputedAt=%v); must be async", snap.Burndown.ComputedAt)
	}
	if !snap.Queue.ComputedAt.IsZero() {
		t.Errorf("Queue computed synchronously on first Refresh (ComputedAt=%v); must be async", snap.Queue.ComputedAt)
	}
	if !snap.Graph.ComputedAt.IsZero() {
		t.Errorf("Graph computed synchronously on first Refresh (ComputedAt=%v); must be async", snap.Graph.ComputedAt)
	}

	// The background job publishes shortly after; a later Refresh sees it.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s, err := p.Refresh()
		if err != nil {
			t.Fatalf("Refresh (poll): %v", err)
		}
		if !s.Queue.ComputedAt.IsZero() {
			// Slots must still be present after the derived sections fill in.
			if len(s.Slots) != 2 {
				t.Fatalf("slots dropped after derived refresh: got %d, want 2", len(s.Slots))
			}
			return // background refresh published — success
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("background derived refresh never published within 20s")
}

// hangingBD writes a fake bd binary that sleeps far longer than the derived
// timeout, simulating a dolt-locked bd that previously wedged refreshDerived
// forever (koryph-b01).
func hangingBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	// exec replaces the shell so the ctx-cancel kill hits the sleeping process
	// itself (real bd is a single binary; a lingering child holding the stdout
	// pipe would stall cmd.Wait and test the wrong thing).
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	return bin
}

// TestDerivedRefreshTimeoutResetsLatch is the koryph-b01 regression guard: a
// wedged bd must not latch derivedRefreshing forever. The pass is bounded by
// derivedTimeout, the latch resets, Refresh stays fast throughout, and a
// non-empty queue cache survives the failed pass instead of being clobbered.
func TestDerivedRefreshTimeoutResetsLatch(t *testing.T) {
	repo := seedRunningRun(t)
	p := NewLedgerProvider("proj", repo, "")
	p.bd = beads.New(repo)
	p.bd.Bin = hangingBD(t)
	p.derivedTimeout = 500 * time.Millisecond

	// Seed a good queue cache to prove the failed pass cannot blank it.
	seeded := QueueSnapshot{
		Roots:      []QueueNode{{Issue: beads.Issue{ID: "root"}, State: QueueStateReady}},
		NodeCount:  1,
		ComputedAt: time.Now().Add(-time.Minute),
	}
	p.queueCache = seeded

	start := time.Now()
	if _, err := p.Refresh(); err != nil { // kicks the background pass
		t.Fatalf("Refresh: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Refresh blocked %v on a hanging bd; must return immediately", elapsed)
	}

	// The pass must abort at the timeout and reset the latch.
	deadline := time.Now().Add(10 * time.Second)
	for {
		p.mu.Lock()
		refreshing := p.derivedRefreshing
		p.mu.Unlock()
		if !refreshing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("derivedRefreshing still latched 10s after a 500ms-bounded pass (the pre-koryph-b01 wedge)")
		}
		time.Sleep(50 * time.Millisecond)
	}

	p.mu.Lock()
	got := p.queueCache
	p.mu.Unlock()
	if got.NodeCount != 1 || got.ComputedAt != seeded.ComputedAt {
		t.Errorf("queue cache clobbered by the failed pass: %+v (want the seeded tree kept, stale ComputedAt intact)", got)
	}
}

// TestSlotToSnapshot_Zombie is the koryph-k6o regression guard for the
// zombie-slot check itself: a RUNNING slot with a recorded pid the probe
// reports dead → Zombie=true; a terminal slot, an unset pid, or a live pid
// must all leave it false, and a nil alive func must never panic (best-effort,
// skip silently — same rendering as before this change). Crucially, review/
// stuck/dispatching slots legitimately have a dead AGENT pid while the engine
// drives post-build stages and must NOT be flagged (else every reviewed bead
// falsely reads as a zombie — the blocking review finding this guards).
func TestSlotToSnapshot_Zombie(t *testing.T) {
	dead := func(int) bool { return false }
	live := func(int) bool { return true }
	now := time.Now()

	cases := []struct {
		name  string
		sl    *ledger.Slot
		alive func(int) bool
		want  bool
	}{
		{"running, dead pid → zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 4242}, dead, true},
		{"running, live pid → not zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 4242}, live, false},
		{"terminal status, dead pid → not zombie (finished, not a zombie)", &ledger.Slot{Status: ledger.SlotDone, PID: 4242}, dead, false},
		{"no pid recorded → not zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 0}, dead, false},
		{"nil alive func → not zombie (best-effort skip)", &ledger.Slot{Status: ledger.SlotRunning, PID: 4242}, nil, false},
		{"review status, dead agent pid → not zombie (engine drives review)", &ledger.Slot{Status: ledger.SlotReview, PID: 4242}, dead, false},
		{"stuck status, dead agent pid → not zombie (not the running stage)", &ledger.Slot{Status: ledger.SlotStuck, PID: 4242}, dead, false},
		{"dispatching status, dead agent pid → not zombie (agent not up yet)", &ledger.Slot{Status: ledger.SlotDispatching, PID: 4242}, dead, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := slotToSnapshot(c.sl, now, c.alive)
			if got.Zombie != c.want {
				t.Errorf("Zombie = %v, want %v", got.Zombie, c.want)
			}
		})
	}
}

// TestRefresh_ZombieSlot verifies the end-to-end wiring: a LedgerProvider with
// its alive probe overridden to report every pid dead surfaces a non-terminal
// slot's dead pid as Zombie=true on the Snapshot returned by Refresh — the
// path the TUI/cockpit actually reads from (koryph-k6o).
func TestRefresh_ZombieSlot(t *testing.T) {
	repo := t.TempDir()
	st := ledger.NewStore(repo)
	run, err := st.NewRun("proj", "test", "v0")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	run.Slots["bead-1"] = &ledger.Slot{
		PhaseID: "bead-1", BeadID: "bead-1", Status: ledger.SlotRunning, PID: 99999,
	}
	if err := st.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	p := NewLedgerProvider("proj", repo, "")
	p.alive = func(int) bool { return false } // every pid reported dead

	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(snap.Slots) != 1 || !snap.Slots[0].Zombie {
		t.Fatalf("Slots = %+v, want exactly one slot with Zombie=true", snap.Slots)
	}
}

// seedLock writes a koryph.lock recording pid under repo's KoryphRoot so
// LockPID has something to read (koryph-oixo run-level liveness).
func seedLock(t *testing.T, repo, pid string) {
	t.Helper()
	root := ledger.NewStore(repo).KoryphRoot
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir KoryphRoot: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "koryph.lock"), []byte(pid+" host\n"), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
}

// TestRefresh_RunDead: a status=running run whose engine pid is dead surfaces
// as RunDead in the snapshot the TUI header reads (koryph-oixo).
func TestRefresh_RunDead(t *testing.T) {
	repo := seedRunningRun(t) // NewRun leaves Status=running
	seedLock(t, repo, "2000000000")

	p := NewLedgerProvider("proj", repo, "")
	p.alive = func(int) bool { return false } // engine pid reported dead

	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if snap.RunStatus != ledger.RunRunning {
		t.Fatalf("RunStatus = %q, want %q", snap.RunStatus, ledger.RunRunning)
	}
	if !snap.RunDead {
		t.Errorf("RunDead = false, want true (running run, dead engine pid)")
	}
}

// TestRefresh_RunLive: the same run with a live engine pid is NOT flagged dead.
func TestRefresh_RunLive(t *testing.T) {
	repo := seedRunningRun(t)
	seedLock(t, repo, "4242")

	p := NewLedgerProvider("proj", repo, "")
	p.alive = func(int) bool { return true } // engine pid reported alive

	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if snap.RunDead {
		t.Errorf("RunDead = true, want false (running run, live engine pid)")
	}
}

// TestRefresh_RunDeadNoLock: a running run with no lock file at all (engine
// exited/crashed without a lock) is also a phantom — no live engine owns it.
func TestRefresh_RunDeadNoLock(t *testing.T) {
	repo := seedRunningRun(t)

	p := NewLedgerProvider("proj", repo, "")
	p.alive = func(int) bool { return true } // irrelevant: no lock → no pid

	snap, err := p.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if !snap.RunDead {
		t.Errorf("RunDead = false, want true (running run, no lock holder)")
	}
}
