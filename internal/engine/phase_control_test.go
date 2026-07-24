// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/phasecontrol"
)

func phaseControlRunner(t *testing.T) (*runner, *fakeSource, *ledger.Slot, string) {
	t.Helper()
	store := &ledger.Store{KoryphRoot: t.TempDir()}
	run := &ledger.Run{RunID: "run-1", ProjectID: "demo", Slots: map[string]*ledger.Slot{}}
	source := &fakeSource{}
	sl := &ledger.Slot{PhaseID: "bead-1", BeadID: "bead-1"}
	r := &runner{
		opts:    Options{ProjectID: "demo"},
		adapter: source,
		store:   store,
		run:     run,
		issues:  map[string]beads.Issue{"bead-1": {ID: "bead-1"}},
	}
	return r, source, sl, store.PhaseDir(run.RunID, sl.PhaseID)
}

func TestPhaseControlAddsCurrentBeadLabelOnce(t *testing.T) {
	r, source, sl, phaseDir := phaseControlRunner(t)
	req, err := phasecontrol.NewRequest(sl.PhaseID, phasecontrol.OperationLabelAdd)
	if err != nil {
		t.Fatal(err)
	}
	req.Label = "area:docs"
	if err := phasecontrol.Submit(phaseDir, req); err != nil {
		t.Fatal(err)
	}
	r.processPhaseRequests(context.Background(), sl)
	r.processPhaseRequests(context.Background(), sl)
	if len(source.addLabels) != 1 || source.addLabels[0] != [2]string{"bead-1", "area:docs"} {
		t.Fatalf("AddLabel calls = %v", source.addLabels)
	}
	resp, err := phasecontrol.WaitResponse(context.Background(), phaseDir, req)
	if err != nil || resp.State != phasecontrol.ResponseApplied {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
}

func TestPhaseControlRejectsRoutingLabelAndPhaseMismatch(t *testing.T) {
	r, source, sl, phaseDir := phaseControlRunner(t)
	for _, tc := range []struct {
		phase string
		label string
	}{
		{phase: sl.PhaseID, label: "model:frontier"},
		{phase: "another-bead", label: "area:docs"},
	} {
		req, err := phasecontrol.NewRequest(tc.phase, phasecontrol.OperationLabelAdd)
		if err != nil {
			t.Fatal(err)
		}
		req.Label = tc.label
		if err := phasecontrol.Submit(phaseDir, req); err != nil {
			t.Fatal(err)
		}
		r.processPhaseRequests(context.Background(), sl)
		resp, err := phasecontrol.WaitResponse(context.Background(), phaseDir, req)
		if err != nil || resp.State != phasecontrol.ResponseRejected {
			t.Fatalf("response=%+v err=%v", resp, err)
		}
	}
	if len(source.addLabels) != 0 {
		t.Fatalf("unexpected AddLabel calls: %v", source.addLabels)
	}
}
