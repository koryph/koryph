// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

// syncHost is a PRHost whose Info is keyed by branch selector.
type syncHost struct {
	fakePRHost
	states map[string]PRMeta
}

func (h *syncHost) Info(_ context.Context, _, selector string) (PRMeta, error) {
	return h.states[selector], nil
}

// TestSyncPROpenedReconcilesMergedAndClosed: a pr-opened bead whose PR merged
// (externally) becomes merged; one whose PR was closed becomes blocked; one
// still open is left alone; non-pr-opened slots are ignored.
func TestSyncPROpenedReconcilesMergedAndClosed(t *testing.T) {
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main", ProjectID: "proj"}
	st := ledger.NewStore(rec.Root)
	run, err := st.NewRun("proj", "bd", "v0")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	for _, s := range []*ledger.Slot{
		{PhaseID: "merged-bead", Branch: "agent/merged-bead", Status: ledger.SlotPROpened},
		{PhaseID: "closed-bead", Branch: "agent/closed-bead", Status: ledger.SlotPROpened},
		{PhaseID: "open-bead", Branch: "agent/open-bead", Status: ledger.SlotPROpened},
		{PhaseID: "done-bead", Branch: "agent/done-bead", Status: ledger.SlotMerged}, // ignored
	} {
		if err := st.SetSlot(run, s); err != nil {
			t.Fatalf("SetSlot: %v", err)
		}
	}

	host := &syncHost{states: map[string]PRMeta{
		"agent/merged-bead": {Number: 1, State: "MERGED", URL: "u1"},
		"agent/closed-bead": {Number: 2, State: "CLOSED", URL: "u2"},
		"agent/open-bead":   {Number: 3, State: "OPEN", URL: "u3"},
	}}

	var out bytes.Buffer
	outcomes, err := SyncPROpened(context.Background(), rec, host, &out)
	if err != nil {
		t.Fatalf("SyncPROpened: %v", err)
	}
	if len(outcomes) != 3 {
		t.Fatalf("outcomes=%d, want 3 (only pr-opened slots)", len(outcomes))
	}

	reloaded, err := ledger.NewStore(rec.Root).LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if got := reloaded.Slots["merged-bead"].Status; got != ledger.SlotMerged {
		t.Errorf("merged-bead status=%q, want merged", got)
	}
	if got := reloaded.Slots["closed-bead"].Status; got != ledger.SlotBlocked {
		t.Errorf("closed-bead status=%q, want blocked", got)
	}
	if got := reloaded.Slots["open-bead"].Status; got != ledger.SlotPROpened {
		t.Errorf("open-bead status=%q, want still pr-opened", got)
	}
}

// TestReviewPRDetectsTerminalPR: a review-pr action on a PR that already merged
// is a no-op that reports the state and clears any stale saved analysis.
func TestReviewPRDetectsTerminalPR(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main", ProjectID: "proj"}
	savePRState(rec, prReviewState{Number: 4, HeadSHA: "x", Findings: []review.Finding{{Summary: "old"}}})

	host := &fakePRHost{meta: PRMeta{Number: 4, State: "MERGED", URL: "u"}, viewer: "maintainer"}
	var out bytes.Buffer
	res, err := ReviewPR(context.Background(), rec, &project.Config{}, host, nil,
		ReviewPROpts{Selector: "4", Approve: true, Out: &out})
	if err != nil {
		t.Fatalf("ReviewPR: %v", err)
	}
	if res.Verdict != "merged" {
		t.Errorf("verdict=%q, want merged", res.Verdict)
	}
	if host.approved {
		t.Error("must not approve an already-merged PR")
	}
	if _, ok := loadPRState(rec, 4); ok {
		t.Error("stale saved analysis should be cleared for a terminal PR")
	}
}
