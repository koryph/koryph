// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
)

// TestApplyLearnedModelsLabelsFrontier proves the koryph-qf6.6 wave-boundary
// hook end-to-end against a real ledger store: escalation evidence in past
// slots produces a recommendation, the matching frontier bead is labeled in
// bd AND in the returned in-memory slice, the throttle suppresses the very
// next boundary, and the whole pass is a no-op without the config gate.
func TestApplyLearnedModelsLabelsFrontier(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.cfg.AdaptiveEscalation = &project.AdaptiveEscalation{Enabled: true}

	const desc = "a bead like the ones that kept failing on sonnet"
	size := quota.SizeOf(len(desc))
	for _, id := range []string{"e1", "e2"} {
		sl := &ledger.Slot{
			PhaseID: id, Status: ledger.SlotMerged, Model: "opus",
			ModelWhy:   "escalated from sonnet after 2 bead-fault attempts (agent died with no commits)",
			BeadLabels: []string{"area:engine"}, SizeClass: size, IssueType: "task",
		}
		if err := r.store.SetSlot(r.run, sl); err != nil {
			t.Fatalf("SetSlot(%s): %v", id, err)
		}
	}

	frontier := []beads.Issue{
		{ID: "new1", IssueType: "task", Labels: []string{"area:engine"}, Description: desc},
		{ID: "pinned", IssueType: "task", Labels: []string{"area:engine", "model:haiku"}, Description: desc},
	}
	got := r.applyLearnedModels(t.Context(), frontier)

	want := [][2]string{{"new1", "model:opus"}}
	if len(fake.addLabels) < 2 || fake.addLabels[0] != want[0] {
		t.Fatalf("addLabels = %v, want model:opus (then provenance) on new1 only", fake.addLabels)
	}
	for _, al := range fake.addLabels {
		if al[0] == "pinned" {
			t.Errorf("human-pinned bead relabeled: %v", al)
		}
	}
	var found bool
	for _, l := range got[0].Labels {
		if l == "model:opus" {
			found = true
		}
	}
	if !found {
		t.Errorf("returned frontier labels = %v, want model:opus visible for THIS wave", got[0].Labels)
	}

	// Throttle: an immediate second boundary must not rescan or relabel.
	before := len(fake.addLabels)
	r.applyLearnedModels(t.Context(), []beads.Issue{
		{ID: "new2", IssueType: "task", Labels: []string{"area:engine"}, Description: desc},
	})
	if len(fake.addLabels) != before {
		t.Errorf("throttled boundary wrote labels: %v", fake.addLabels[before:])
	}

	// Gate: without adaptive_escalation the pass is inert.
	r.cfg.AdaptiveEscalation = nil
	r.lastLearn = time.Time{}
	out := r.applyLearnedModels(t.Context(), []beads.Issue{
		{ID: "new3", IssueType: "task", Labels: []string{"area:engine"}, Description: desc},
	})
	if len(fake.addLabels) != before || len(out) != 1 {
		t.Errorf("ungated pass mutated state: labels=%v", fake.addLabels[before:])
	}
}
