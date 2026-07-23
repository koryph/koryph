// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os/exec"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

// fakeSource is an in-memory WorkSource so loop logic can be unit-tested with no
// `bd` binary — the seam koryph-8iu.2 opened. Unused methods return zero values.
type fakeSource struct {
	setStatus   [][2]string   // (id, status) calls, in order
	comments    [][2]string   // (id, text) calls, in order (koryph-qf6.5 write-back)
	addLabels   [][2]string   // (id, label) calls, in order (koryph-qf6.5 write-back)
	readyIssues []beads.Issue // Ready() frontier; nil (default) reproduces the old "no frontier" fake
}

func (f *fakeSource) Ready(context.Context, beads.ReadyOpts) ([]beads.Issue, error) {
	return f.readyIssues, nil
}
func (f *fakeSource) Show(_ context.Context, id string) (beads.Issue, error) {
	return beads.Issue{ID: id}, nil
}
func (f *fakeSource) ListChildren(context.Context, string) ([]beads.Issue, error) { return nil, nil }
func (f *fakeSource) ListChildrenAll(context.Context, string) ([]beads.Issue, error) {
	return nil, nil
}
func (f *fakeSource) Claim(context.Context, string) error         { return nil }
func (f *fakeSource) Close(context.Context, string, string) error { return nil }
func (f *fakeSource) Comment(_ context.Context, id, text string) error {
	f.comments = append(f.comments, [2]string{id, text})
	return nil
}
func (f *fakeSource) AddLabel(_ context.Context, id, label string) error {
	f.addLabels = append(f.addLabels, [2]string{id, label})
	return nil
}
func (f *fakeSource) SetStatus(_ context.Context, id, status string) error {
	f.setStatus = append(f.setStatus, [2]string{id, status})
	return nil
}
func (f *fakeSource) MergeSlotAcquire(context.Context, string, string, int) error { return nil }
func (f *fakeSource) MergeSlotRelease(context.Context, string) error              { return nil }

// fakeBlocked reports whether the fake WorkSource was asked to set id's status
// to "blocked" — the koryph-84yu reconcile that keeps a terminally-blocked slot
// from stranding its bd claim in_progress.
func fakeBlocked(f *fakeSource, id string) bool {
	for _, c := range f.setStatus {
		if c[0] == id && c[1] == "blocked" {
			return true
		}
	}
	return false
}

// deadPID returns the PID of a process that has already exited and been reaped,
// so slotAlive reports it dead on every platform. A hardcoded constant cannot:
// low PIDs like 2 are live system processes on Linux (kthreadd) and probe as
// alive (EPERM), which silently defeats orphan reconciliation there.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dead-pid helper: %v", err)
	}
	pid := cmd.Process.Pid
	_ = cmd.Wait() // reap: pid is now definitively dead
	return pid
}

// TestReconcileOrphansWithFakeSource drives a loop path (orphan reconciliation)
// against a fake WorkSource — impossible before the interface, which required a
// real bd binary on PATH.
func TestReconcileOrphansWithFakeSource(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	// Seed an orphan: a non-terminal slot whose agent PID is dead and which has
	// no worktree (so it reopens rather than being kept for --resume). Use a
	// reaped process's PID — a hardcoded low PID is not portable: PID 2 is
	// kthreadd on Linux and slotAlive reports it alive (EPERM), so the orphan
	// would never reconcile there (green on macOS, red in CI).
	orphan := &ledger.Slot{
		PhaseID: "tb1",
		BeadID:  "tb1",
		Status:  ledger.SlotRunning,
		PID:     deadPID(t),
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
