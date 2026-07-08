// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"
	"time"

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
