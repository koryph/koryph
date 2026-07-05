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

// koryph-3as regression: a rebase conflict at merge time must requeue the
// agent for in-worktree resolution (budgeted), and on budget exhaustion must
// reset the bead to open — run 20260704-225403 drained with four beads
// stranded in_progress behind terminal SlotConflict slots that no future run
// could adopt (in_progress is invisible to bd ready).

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
	r.rt = runtimetest.Stub{}
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
	for _, ss := range fake.setStatus {
		if ss[0] == "cb1" && ss[1] == "open" {
			t.Error("bead must NOT be reset to open while the requeue budget remains")
		}
	}
}

func TestMergeConflictBudgetExhaustedResetsBeadOpen(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	sl := conflictSlot(t, r, "cb2", conflictRequeueBudget)

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if requeued {
		t.Fatal("exhausted budget must not requeue again")
	}
	if got := r.run.Slots["cb2"].Status; got != ledger.SlotConflict {
		t.Errorf("slot status = %q, want %q", got, ledger.SlotConflict)
	}
	found := false
	for _, ss := range fake.setStatus {
		if ss[0] == "cb2" && ss[1] == "open" {
			found = true
		}
	}
	if !found {
		t.Errorf("bead not reset to open on terminal conflict; SetStatus calls = %v (the strand this test exists to prevent)", fake.setStatus)
	}
}
