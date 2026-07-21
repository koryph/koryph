// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestApplyOperatorOverrides proves D5: a merged directive written to the run's
// override sidecar (as `koryph merge` does on a manual land) is folded into the
// in-memory ledger, so the engine adopts it instead of reverting the row — and
// it is idempotent and refuses non-terminal directives.
func TestApplyOperatorOverrides(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	// A running slot the engine still thinks it owns.
	if err := r.store.SetSlot(r.run, &ledger.Slot{PhaseID: "b1", BeadID: "b1", Status: ledger.SlotRunning}); err != nil {
		t.Fatal(err)
	}
	// The operator landed it by hand and recorded a merged override.
	if err := r.store.RecordOverride(r.run.RunID, ledger.SlotOverride{
		BeadID: "b1", Status: ledger.SlotMerged, Note: "landed manually",
	}); err != nil {
		t.Fatal(err)
	}

	r.applyOperatorOverrides()

	got := r.run.Slots["b1"]
	if got.Status != ledger.SlotMerged {
		t.Fatalf("slot status = %q, want merged (override adopted)", got.Status)
	}
	if got.Note != "landed manually" {
		t.Errorf("slot note = %q, want the override reason", got.Note)
	}

	// Idempotent: applying again leaves it merged, no churn.
	r.applyOperatorOverrides()
	if r.run.Slots["b1"].Status != ledger.SlotMerged {
		t.Error("second apply changed the terminal slot")
	}
}

// TestApplyOperatorOverridesIgnoresNonTerminal proves a non-terminal directive
// (e.g. an attempt to force a slot back to "running") is refused — only
// mark-landed/retired directives are honored.
func TestApplyOperatorOverridesIgnoresNonTerminal(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	if err := r.store.SetSlot(r.run, &ledger.Slot{PhaseID: "b1", BeadID: "b1", Status: ledger.SlotBlocked}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.RecordOverride(r.run.RunID, ledger.SlotOverride{BeadID: "b1", Status: ledger.SlotRunning}); err != nil {
		t.Fatal(err)
	}

	r.applyOperatorOverrides()

	if got := r.run.Slots["b1"].Status; got != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked unchanged (non-terminal directive refused)", got)
	}
}
