// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package modellearn

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

const escWhy = "escalated from sonnet after 2 bead-fault attempts (agent died with no commits)"

func esc(id string, areas ...string) Evidence {
	return Evidence{BeadID: id, Escalated: true, Tier: "opus", FromTier: "sonnet", Areas: areas, Size: "M"}
}

func clean(id string, areas ...string) Evidence {
	return Evidence{BeadID: id, Tier: "sonnet", Areas: areas, Size: "M"}
}

func TestRecommendThreshold(t *testing.T) {
	t.Run("two escalations beat one clean merge", func(t *testing.T) {
		recs := Recommend([]Evidence{
			esc("b1", "area:engine"), esc("b2", "area:engine"), clean("b3", "area:engine"),
		}, 0)
		if len(recs) != 1 {
			t.Fatalf("recs = %+v, want exactly one", recs)
		}
		r := recs[0]
		if r.Area != "area:engine" || r.Size != "M" || r.Tier != "opus" || r.Evidence != 2 || r.CleanMerges != 1 {
			t.Errorf("rec = %+v, want area:engine/M/opus with 2 evidence, 1 clean", r)
		}
	})

	t.Run("one escalation is an incident, not a pattern", func(t *testing.T) {
		if recs := Recommend([]Evidence{esc("b1", "area:engine")}, 0); len(recs) != 0 {
			t.Errorf("recs = %+v, want none below min evidence", recs)
		}
	})

	t.Run("clean majority suppresses the recommendation", func(t *testing.T) {
		if recs := Recommend([]Evidence{
			esc("b1", "area:engine"), esc("b2", "area:engine"),
			clean("b3", "area:engine"), clean("b4", "area:engine"), clean("b5", "area:engine"),
		}, 0); len(recs) != 0 {
			t.Errorf("recs = %+v, want none when the area usually merges clean", recs)
		}
	})

	t.Run("size buckets do not cross-contaminate", func(t *testing.T) {
		small := esc("b1", "area:engine")
		small.Size = "S"
		if recs := Recommend([]Evidence{small, esc("b2", "area:engine")}, 0); len(recs) != 0 {
			t.Errorf("recs = %+v, want none (one S + one M is no pattern in either bucket)", recs)
		}
	})
}

type fakeWriter struct {
	labels [][2]string
	fail   bool
}

func (f *fakeWriter) AddLabel(_ context.Context, id, label string) error {
	if f.fail {
		return context.DeadlineExceeded
	}
	f.labels = append(f.labels, [2]string{id, label})
	return nil
}

func TestApplyMatchingAndPrecedence(t *testing.T) {
	recs := []Recommendation{{Area: "area:engine", Size: "S", Tier: "opus", Evidence: 2}}
	issues := []beads.Issue{
		{ID: "match", IssueType: "task", Labels: []string{"area:engine"}, Description: "short"},
		{ID: "human", IssueType: "task", Labels: []string{"area:engine", "model:haiku"}, Description: "short"},
		{ID: "epic", IssueType: "epic", Labels: []string{"area:engine"}, Description: "short"},
		{ID: "otherarea", IssueType: "task", Labels: []string{"area:cli"}, Description: "short"},
	}
	w := &fakeWriter{}

	applied, updated, failed := Apply(t.Context(), w, issues, recs, "2026-07-10")

	if failed != 0 || len(applied) != 1 || applied[0].BeadID != "match" {
		t.Fatalf("applied = %+v (failed %d), want exactly the matching task", applied, failed)
	}
	if len(w.labels) != 2 ||
		w.labels[0] != [2]string{"match", "model:opus"} ||
		w.labels[1] != [2]string{"match", "model-learned:2026-07-10"} {
		t.Errorf("labels written = %v, want routing + provenance on 'match' only", w.labels)
	}
	if !hasLabel(updated[0].Labels, "model:opus") {
		t.Errorf("updated frontier copy = %v, want the new labels visible in-memory", updated[0].Labels)
	}
	if hasLabel(updated[1].Labels, "model:opus") {
		t.Errorf("human-labeled bead mutated: %v — a pre-existing model:* label must win", updated[1].Labels)
	}

	// Idempotency: re-applying over the updated frontier writes nothing new.
	w2 := &fakeWriter{}
	applied2, _, _ := Apply(t.Context(), w2, updated, recs, "2026-07-11")
	if len(applied2) != 0 || len(w2.labels) != 0 {
		t.Errorf("second apply wrote %v — must be a no-op (model:* already present)", w2.labels)
	}
}

func TestApplyFailureSkipsBead(t *testing.T) {
	recs := []Recommendation{{Area: "area:engine", Size: "S", Tier: "opus"}}
	issues := []beads.Issue{{ID: "b", IssueType: "task", Labels: []string{"area:engine"}, Description: "x"}}
	w := &fakeWriter{fail: true}

	applied, updated, failed := Apply(t.Context(), w, issues, recs, "2026-07-10")
	if len(applied) != 0 || failed != 1 {
		t.Errorf("applied/failed = %v/%d, want 0 applied, 1 failed", applied, failed)
	}
	if hasModelLabel(updated[0].Labels) {
		t.Errorf("frontier copy gained labels despite the write failing: %v", updated[0].Labels)
	}
}

// TestCollectFromLedger drives Collect against a real on-disk store: an
// escalated-merged slot with frozen features becomes evidence; a blocked slot
// and a featureless legacy slot do not.
func TestCollectFromLedger(t *testing.T) {
	store := ledger.NewStore(t.TempDir())
	run, err := store.NewRun("proj", "bd", "test")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	slots := []*ledger.Slot{
		{PhaseID: "e1", Status: ledger.SlotMerged, Model: "opus", ModelWhy: escWhy,
			BeadLabels: []string{"area:engine", "fp:go:engine"}, SizeClass: "M", IssueType: "task"},
		{PhaseID: "c1", Status: ledger.SlotMerged, Model: "sonnet", ModelWhy: "stage default (implement)",
			BeadLabels: []string{"area:engine"}, SizeClass: "M", IssueType: "task"},
		{PhaseID: "blocked", Status: ledger.SlotBlocked, Model: "sonnet",
			BeadLabels: []string{"area:engine"}, SizeClass: "M"},
		{PhaseID: "legacy", Status: ledger.SlotMerged, Model: "sonnet"},
	}
	for _, sl := range slots {
		if err := store.SetSlot(run, sl); err != nil {
			t.Fatalf("SetSlot(%s): %v", sl.PhaseID, err)
		}
	}

	evs, err := Collect(store)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("evidence = %+v, want 2 rows (escalated + clean merge only)", evs)
	}
	byID := map[string]Evidence{}
	for _, ev := range evs {
		byID[ev.BeadID] = ev
	}
	e1 := byID["e1"]
	if !e1.Escalated || e1.Tier != "opus" || e1.FromTier != "sonnet" ||
		len(e1.Areas) != 1 || e1.Areas[0] != "area:engine" || e1.Size != "M" {
		t.Errorf("e1 evidence = %+v, want escalated opus-from-sonnet in area:engine/M", e1)
	}
	if c1 := byID["c1"]; c1.Escalated || c1.Tier != "sonnet" {
		t.Errorf("c1 evidence = %+v, want a clean sonnet merge", c1)
	}
}

func TestFromTierOf(t *testing.T) {
	if got := fromTierOf(escWhy); got != "sonnet" {
		t.Errorf("fromTierOf = %q, want sonnet", got)
	}
	if got := fromTierOf("stage default (implement)"); got != "" {
		t.Errorf("fromTierOf on non-escalated rationale = %q, want empty", got)
	}
}

func TestRecommendIsDeterministic(t *testing.T) {
	evs := []Evidence{
		esc("b1", "area:b", "area:a"), esc("b2", "area:b", "area:a"),
	}
	recs := Recommend(evs, 0)
	if len(recs) != 2 || recs[0].Area != "area:a" || recs[1].Area != "area:b" {
		t.Errorf("recs = %+v, want sorted area:a then area:b", recs)
	}
	if strings.Join(recs[0].Beads, ",") != "b1,b2" {
		t.Errorf("beads = %v, want sorted b1,b2", recs[0].Beads)
	}
}
