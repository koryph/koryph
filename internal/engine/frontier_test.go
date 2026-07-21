// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/sched"
)

// TestCaptureFrontier proves D7/D9: a built wave's per-candidate verdict —
// dispatched Items, deferred and skipped reasons — is recorded to the run
// ledger (in full, not truncated) for `koryph status --frontier`.
func TestCaptureFrontier(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	w := sched.Wave{
		Items:    []sched.Item{{Issue: beads.Issue{ID: "d1", Title: "do thing"}}},
		Deferred: []sched.Reason{{ID: "x1", Title: "blocked work", Reason: "footprint conflict with d1 (in-flight)"}},
		Skipped:  []sched.Reason{{ID: "s1", Title: "an epic", Reason: "container bead"}},
	}
	r.captureFrontier(w)

	fr := r.run.Frontier
	if fr == nil || len(fr.Entries) != 3 {
		t.Fatalf("frontier = %+v, want 3 entries", fr)
	}
	byID := map[string]ledger.FrontierEntry{}
	for _, e := range fr.Entries {
		byID[e.BeadID] = e
	}
	if byID["d1"].Verdict != "dispatched" {
		t.Errorf("d1 verdict = %q, want dispatched", byID["d1"].Verdict)
	}
	if byID["x1"].Verdict != "deferred" || !strings.Contains(byID["x1"].Reason, "footprint conflict") {
		t.Errorf("x1 = %+v, want deferred with a footprint reason", byID["x1"])
	}
	if byID["s1"].Verdict != "skipped" {
		t.Errorf("s1 verdict = %q, want skipped", byID["s1"].Verdict)
	}

	// Persisted to the ledger, not just held in memory.
	reloaded, err := ledger.NewStore(f.repo).LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Frontier == nil || len(reloaded.Frontier.Entries) != 3 {
		t.Errorf("frontier not persisted to the ledger: %+v", reloaded.Frontier)
	}
}
