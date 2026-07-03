// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

// fakeSource is an in-memory WorkSource so loop logic can be unit-tested with no
// `bd` binary — the seam koryph-8iu.2 opened. Unused methods return zero values.
type fakeSource struct {
	setStatus [][2]string // (id, status) calls, in order
}

func (f *fakeSource) Ready(context.Context, beads.ReadyOpts) ([]beads.Issue, error) {
	return nil, nil
}
func (f *fakeSource) Show(_ context.Context, id string) (beads.Issue, error) {
	return beads.Issue{ID: id}, nil
}
func (f *fakeSource) ListChildren(context.Context, string) ([]beads.Issue, error) { return nil, nil }
func (f *fakeSource) Claim(context.Context, string) error                         { return nil }
func (f *fakeSource) Close(context.Context, string, string) error                 { return nil }
func (f *fakeSource) Comment(context.Context, string, string) error               { return nil }
func (f *fakeSource) SetStatus(_ context.Context, id, status string) error {
	f.setStatus = append(f.setStatus, [2]string{id, status})
	return nil
}
func (f *fakeSource) MergeSlotAcquire(context.Context, string, string, int) error { return nil }
func (f *fakeSource) MergeSlotRelease(context.Context, string) error              { return nil }

// TestReconcileOrphansWithFakeSource drives a loop path (orphan reconciliation)
// against a fake WorkSource — impossible before the interface, which required a
// real bd binary on PATH.
func TestReconcileOrphansWithFakeSource(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	// Seed an orphan: a non-terminal slot whose agent PID is long dead and which
	// has no worktree (so it reopens rather than being kept for --resume).
	orphan := &ledger.Slot{
		PhaseID: "tb1",
		BeadID:  "tb1",
		Status:  ledger.SlotRunning,
		PID:     2, // never a live agent
	}
	if err := r.store.SetSlot(r.run, orphan); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	r.reconcileOrphans(context.Background())

	// The fake tracker was asked to reopen the orphan — no bd binary involved.
	var reopened bool
	for _, c := range fake.setStatus {
		if c[0] == "tb1" && c[1] == "open" {
			reopened = true
		}
	}
	if !reopened {
		t.Fatalf("reconcileOrphans did not reopen the orphan via the WorkSource; calls=%v", fake.setStatus)
	}
}
