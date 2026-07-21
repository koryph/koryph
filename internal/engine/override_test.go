// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/beads"
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

// TestApplyInjections proves D10: a ready operator-injected bead outside the
// run's --parent scope is merged into the frontier, while a not-ready injection
// is left out (it cannot be force-dispatched).
func TestApplyInjections(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	// The unscoped bd frontier: inj-ready is ready; inj-blocked is not present.
	r.adapter = &fakeSource{readyIssues: []beads.Issue{{ID: "inj-ready"}}}

	if err := r.store.RecordInjection(r.run.RunID, "inj-ready"); err != nil {
		t.Fatal(err)
	}
	if err := r.store.RecordInjection(r.run.RunID, "inj-blocked"); err != nil {
		t.Fatal(err)
	}

	out := r.applyInjections(context.Background(), []beads.Issue{{ID: "s1"}})

	ids := map[string]bool{}
	for _, iss := range out {
		ids[iss.ID] = true
	}
	if !ids["s1"] {
		t.Error("scoped frontier bead dropped")
	}
	if !ids["inj-ready"] {
		t.Error("ready injection was not added to the frontier")
	}
	if ids["inj-blocked"] {
		t.Error("not-ready injection must not be force-dispatched")
	}
}

// TestApplyInjectionsSkipsAlreadyDispatched proves an injection whose bead
// already has a slot is not re-added (fulfilled, no churn).
func TestApplyInjectionsSkipsAlreadyDispatched(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.adapter = &fakeSource{readyIssues: []beads.Issue{{ID: "inj1"}}}
	if err := r.store.SetSlot(r.run, &ledger.Slot{PhaseID: "inj1", BeadID: "inj1", Status: ledger.SlotRunning}); err != nil {
		t.Fatal(err)
	}
	if err := r.store.RecordInjection(r.run.RunID, "inj1"); err != nil {
		t.Fatal(err)
	}

	for _, iss := range r.applyInjections(context.Background(), nil) {
		if iss.ID == "inj1" {
			t.Error("already-dispatched injection must not be re-added")
		}
	}
}
