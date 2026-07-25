// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// refusingBackend satisfies dispatch.Backend without launching anything: the
// requeue decision (what these tests assert) happens before dispatch, and a
// dispatch failure downgrades the slot to blocked — never back to conflict.
type refusingBackend struct{}

func (refusingBackend) Dispatch(context.Context, dispatch.Spec) (dispatch.Handle, error) {
	return dispatch.Handle{}, errors.New("test backend refuses dispatch")
}

// A rebase conflict is changed code-defect evidence. It receives one
// standard-tier targeted repair, then parks visibly as blocked if unresolved.

func conflictSlot(t *testing.T, r *runner, id string, requeues int) *ledger.Slot {
	t.Helper()
	sl := &ledger.Slot{
		PhaseID:          id,
		Status:           ledger.SlotRunning,
		Attempts:         1,
		ConflictRequeues: requeues,
	}
	r.run.Slots[id] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return sl
}

func TestMergeConflictRequeuesWithinBudget(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	r.backend = refusingBackend{}
	sl := conflictSlot(t, r, "cb1", 0)

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if !requeued {
		t.Fatal("first rebase conflict must requeue, not park the slot")
	}
	got := r.run.Slots["cb1"]
	if got.Status == ledger.SlotConflict {
		t.Errorf("slot status = %q, want a non-terminal requeue state", got.Status)
	}
	if got.ConflictRequeues != 1 {
		t.Errorf("ConflictRequeues = %d, want 1", got.ConflictRequeues)
	}
	if got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("typed counters = code %d / merge %d, want 1 / 0",
			got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb1" && ss[1] == "open" {
			t.Error("bead must NOT be reset to open while the requeue budget remains")
		}
	}
}

// capturingBackend satisfies dispatch.Backend and SUCCEEDS, so a requeue's
// dispatchBead reaches SetSlot and REPLACES the ledger slot — the seam where
// koryph-qf6.1's counter loss lived. A refusingBackend never gets that far
// (blockSlot mutates the OLD slot in place), which is why the tests above
// could pass while every successful requeue was zeroing untracked counters.
type capturingBackend struct{ specs []dispatch.Spec }

func (b *capturingBackend) Dispatch(_ context.Context, spec dispatch.Spec) (dispatch.Handle, error) {
	b.specs = append(b.specs, spec)
	return dispatch.Handle{PID: 1, SessionID: spec.SessionID}, nil
}

// koryph-qf6.1 regression: dispatchBead builds a brand-new Slot on requeue,
// so every requeue path must thread ALL five budget counters into it. Before
// the fix ConflictRequeues was never threaded at all — its budget could never
// bind, because each successful requeue reset it to zero — and a conflict
// requeue dropped the spent rate-limit/budget-kill budgets, silently
// refilling them for causes it didn't own.
func TestConflictRequeuePreservesAllBudgetCounters(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	backend := &capturingBackend{}
	r.backend = backend

	sl := conflictSlot(t, r, "cb3", 0)
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"
	sl.GateRequeues, sl.MergeRequeues = 1, 2
	sl.RateLimitRequeues, sl.BudgetKillRequeues = 3, 1
	sl.BeadLabels, sl.SizeClass, sl.IssueType = []string{"area:sched"}, "M", "task"
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if !requeued {
		t.Fatal("conflict within budget must requeue")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1 (the requeue must reach the backend so SetSlot replaces the slot)", len(backend.specs))
	}

	got := r.run.Slots["cb3"]
	if got == sl {
		t.Fatal("slot was not replaced — this regression only manifests on the fresh-Slot path")
	}
	if got.ConflictRequeues != 1 {
		t.Errorf("ConflictRequeues = %d, want 1 (incremented AND threaded through the slot replacement)", got.ConflictRequeues)
	}
	if got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("typed counters = code %d / merge %d, want 1 / 0 across slot replacement",
			got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	if got.GateRequeues != 1 || got.MergeRequeues != 2 {
		t.Errorf("Gate/MergeRequeues = %d/%d, want 1/2 (preserved across a conflict requeue)", got.GateRequeues, got.MergeRequeues)
	}
	if got.RateLimitRequeues != 3 {
		t.Errorf("RateLimitRequeues = %d, want 3 (preserved across a conflict requeue)", got.RateLimitRequeues)
	}
	if got.BudgetKillRequeues != 1 {
		t.Errorf("BudgetKillRequeues = %d, want 1 (preserved across a conflict requeue)", got.BudgetKillRequeues)
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (a conflict requeue burns an attempt)", got.Attempts)
	}
	if len(got.BeadLabels) != 1 || got.BeadLabels[0] != "area:sched" ||
		got.SizeClass != "M" || got.IssueType != "task" {
		t.Errorf("features = %v/%q/%q, want [area:sched]/M/task frozen across the replacement (koryph-qf6.3)",
			got.BeadLabels, got.SizeClass, got.IssueType)
	}
}

func TestUnresolvedMergeConflictParksBlockedAfterStandardRepair(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	sl := conflictSlot(t, r, "cb2", 1)
	sl.Retry.CodeRepairs = 1
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if requeued {
		t.Fatal("exhausted budget must not requeue again")
	}
	if got := r.run.Slots["cb2"]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeCodeDefect) {
		t.Errorf("terminal slot = status %q / outcome %q, want blocked / code-defect",
			got.Status, got.OutcomeClass)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb2" && ss[1] == "open" {
			t.Errorf("unresolved code defect must park blocked, not reopen: %v", fake.setStatus)
		}
	}
}

func TestMergeConflictAllowsExactlyOneStandardRepairThenParks(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	backend := &capturingBackend{}
	r.backend = backend
	sl := conflictSlot(t, r, "cb4", 0)
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	}); !requeued {
		t.Fatal("first unresolved conflict must receive its one typed code repair")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after first conflict = %d, want 1", len(backend.specs))
	}
	repaired := r.run.Slots["cb4"]
	if repaired.Retry.CodeRepairs != 1 || repaired.Retry.MergeRevalidations != 0 {
		t.Fatalf("typed counters after first conflict = code %d / merge %d, want 1 / 0",
			repaired.Retry.CodeRepairs, repaired.Retry.MergeRevalidations)
	}
	if backend.specs[0].Model != "sonnet" {
		t.Fatalf("conflict repair model = %q, want standard sonnet", backend.specs[0].Model)
	}

	if requeued := r.handleMergeFailure(t.Context(), repaired, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	}); requeued {
		t.Fatal("second unresolved conflict must park after the code-repair budget")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after exhausted conflict = %d, want 1", len(backend.specs))
	}
	if got := r.run.Slots["cb4"]; got.Status != ledger.SlotBlocked ||
		got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("terminal slot = status %q / code %d / merge %d, want blocked / 1 / 0",
			got.Status, got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb4" && ss[1] == "open" {
			t.Errorf("terminal code defect reopened instead of parking: %v", fake.setStatus)
		}
	}
}

func TestMergeBaseMovedRecoveryNeverDispatchesModel(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.adapter = &fakeSource{}
	backend := &capturingBackend{}
	r.backend = backend
	sl := conflictSlot(t, r, "cb5", 0)

	if requeued := r.recoverTyped(t.Context(), sl, typedRecoveryRequest{
		outcome: OutcomeMergeBaseMoved,
		reason:  "future engine rebase",
	}); requeued {
		t.Fatal("engine-owned rebase/revalidate must not report a model dispatch")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("model dispatches = %d, want 0", len(backend.specs))
	}
	if got := r.run.Slots["cb5"]; got.Status != ledger.SlotBlocked ||
		got.Retry.MergeRevalidations != 0 {
		t.Errorf("slot = status %q / revalidations %d, want blocked / 0 until engine handler exists",
			got.Status, got.Retry.MergeRevalidations)
	}
}
