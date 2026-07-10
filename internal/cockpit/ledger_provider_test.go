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
