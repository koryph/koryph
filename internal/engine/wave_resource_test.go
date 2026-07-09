// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/sched"
)

// TestBuildWaveReceivesResourceOpts proves the engine feeds sched.BuildWave both
// resource opts (koryph-4ql.3, design L4): a running slot's persisted resource
// holdings (via activeResources) plus the machine per-kind capacities (via
// resourceCapacities). At capacity 1 a ready candidate declaring the same kind
// is deferred with the kind-and-holder reason; raising the machine capacity to 2
// admits it — the same opts, a different config, opposite outcomes.
func TestBuildWaveReceivesResourceOpts(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	ctx := context.Background()

	// A currently-dispatched slot holding kind-cluster (persisted at dispatch).
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "running-1", BeadID: "running-1", Status: ledger.SlotRunning,
		Resources: []string{"kind-cluster"},
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	candidate := beads.Issue{
		ID: "cand-1", Title: "candidate", Status: "open",
		Priority: 1, IssueType: "task", Labels: []string{"res:kind-cluster"},
	}

	buildWith := func() sched.Wave {
		active := r.activeIDs()
		w, err := sched.BuildWave(ctx, []beads.Issue{candidate}, r.cfg, sched.Opts{
			Max:              5,
			ActiveIDs:        active,
			ActiveResources:  r.activeResources(ctx, active),
			ResourceCapacity: r.resourceCapacities(),
		}, nil)
		if err != nil {
			t.Fatalf("BuildWave: %v", err)
		}
		return w
	}

	// Capacity 1: the candidate collides with the in-flight holder → deferred.
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 1}); err != nil {
		t.Fatal(err)
	}
	w := buildWith()
	if len(w.Items) != 0 {
		t.Fatalf("capacity 1: candidate dispatched, want deferred: %+v", w.Items)
	}
	var reason string
	for _, d := range w.Deferred {
		if d.ID == "cand-1" {
			reason = d.Reason
		}
	}
	if !strings.Contains(reason, "resource kind-cluster at capacity") || !strings.Contains(reason, "running-1") {
		t.Fatalf("cand-1 deferral reason = %q, want it to name the kind and holder running-1", reason)
	}

	// Capacity 2: room for the in-flight holder AND the candidate → dispatched.
	if err := r.gov.SetResource("kind-cluster", govern.ResourceKind{Capacity: 2}); err != nil {
		t.Fatal(err)
	}
	w = buildWith()
	if len(w.Items) != 1 || w.Items[0].Issue.ID != "cand-1" {
		t.Fatalf("capacity 2: candidate not dispatched: items=%+v deferred=%+v", w.Items, w.Deferred)
	}
	// And the item carries the parsed kinds so the engine need not re-derive them.
	if len(w.Items[0].Resources) != 1 || w.Items[0].Resources[0] != "kind-cluster" {
		t.Errorf("item Resources = %v, want [kind-cluster]", w.Items[0].Resources)
	}
}
