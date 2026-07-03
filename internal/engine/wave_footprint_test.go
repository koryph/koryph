// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/sched"
)

// TestActiveFootprintsGatesConflictingCandidate proves the koryph-2im.1 L2
// wiring end to end at the engine layer: a bead that is currently dispatched
// (a non-terminal ledger slot) excludes a ready candidate whose footprint
// conflicts with it — not just by id (that was already true), but by the
// bead's REAL footprint, recomputed from its labels via r.activeFootprints.
// This is the "--resume adopted-slot gap" the design doc calls out: before
// L2, a freshly built wave only excluded already-active ids, so a candidate
// with a genuinely conflicting footprint could still be dispatched alongside
// an adopted slot it collides with.
func TestActiveFootprintsGatesConflictingCandidate(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// A ready candidate that writes go:engine.
	candidate := beads.Issue{
		ID: "cand-1", Title: "candidate", Status: "open",
		Priority: 1, IssueType: "task", Labels: []string{"fp:go:engine"},
	}
	r.adapter = &fakeSource{readyIssues: []beads.Issue{candidate}}

	// Simulate a currently-dispatched slot for a DIFFERENT bead that also
	// writes go:engine — r.issues is checked before adapter.Show (see
	// issueFor), so this stands in for "the bead behind a live slot".
	r.issues["running-1"] = beads.Issue{ID: "running-1", Labels: []string{"fp:go:engine"}}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "running-1", BeadID: "running-1", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	active := r.activeIDs()
	if !active["running-1"] {
		t.Fatalf("expected running-1 among active ids, got %v", active)
	}
	activeFP := r.activeFootprints(ctx, active)
	if got := activeFP["running-1"]; len(got.Writes) != 1 || got.Writes[0] != "go:engine" {
		t.Fatalf("activeFootprints[running-1] = %+v, want Writes=[go:engine]", got)
	}

	w, err := sched.BuildWave(ctx, []beads.Issue{candidate}, r.cfg, sched.Opts{
		Max:       5,
		ActiveIDs: active,
		Active:    activeFP,
	}, r.childLister(ctx))
	if err != nil {
		t.Fatalf("BuildWave: %v", err)
	}
	if len(w.Items) != 0 {
		t.Fatalf("expected the conflicting candidate deferred, got dispatched: %+v", w.Items)
	}
	var reason string
	for _, d := range w.Deferred {
		if d.ID == "cand-1" {
			reason = d.Reason
		}
	}
	if want := "footprint conflict with running-1 (in-flight)"; reason != want {
		t.Fatalf("cand-1 deferral reason = %q, want %q (deferred=%+v)", reason, want, w.Deferred)
	}
}

// TestActiveFootprintsFallsBackToUnknownOnShowFailure proves the
// maximally-conservative fallback: when a non-terminal slot's bead is not in
// r.issues and adapter.Show fails to find it, activeFootprints must still
// produce a (write-only) footprint — TokenUnknown — rather than silently
// dropping the slot from gating.
func TestActiveFootprintsFallsBackToUnknownOnShowFailure(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// No r.issues entry, and Show errors (fakeBDScript style below would
	// succeed; here we simulate the failure by giving a WorkSource whose
	// Show always fails).
	r.adapter = &failingShowSource{}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "ghost-1", BeadID: "ghost-1", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	active := r.activeIDs()
	activeFP := r.activeFootprints(ctx, active)
	got := activeFP["ghost-1"]
	if len(got.Writes) != 1 || got.Writes[0] != sched.TokenUnknown || len(got.Reads) != 0 {
		t.Fatalf("activeFootprints[ghost-1] = %+v, want write-only TokenUnknown", got)
	}
}

// failingShowSource is a WorkSource whose Show always errors — it stands in
// for a bd hiccup so activeFootprints' fallback chain reaches its last resort.
type failingShowSource struct{ fakeSource }

func (failingShowSource) Show(context.Context, string) (beads.Issue, error) {
	return beads.Issue{}, context.DeadlineExceeded
}
