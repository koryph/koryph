// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestProjectedRunCostCountsInFlight is the core koryph-u7q fix: settled cost
// alone reads $0 for a wave still in flight, so budget admission must add each
// running slot's dispatch-time estimate.
func TestProjectedRunCostCountsInFlight(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots = map[string]*ledger.Slot{
		"done":    {PhaseID: "done", Status: ledger.SlotMerged, CostUSD: 3.0, EstimateUSD: 2.0},
		"running": {PhaseID: "running", Status: ledger.SlotRunning, CostUSD: 0, EstimateUSD: 4.0},
	}
	if got := r.runCostUSD(); got != 3.0 {
		t.Errorf("runCostUSD = %v, want 3.0 (settled only — the in-flight slot reads $0)", got)
	}
	if got := r.projectedRunCostUSD(); got != 7.0 {
		t.Errorf("projectedRunCostUSD = %v, want 7.0 (3 settled + 4 in-flight estimate)", got)
	}
}

// TestProjectedRunCostCountsStuck is the koryph audit finding #29 fix: a slot
// that goes quiet flips from SlotRunning to SlotStuck (poll.go's pollSlot),
// but its process — and its spend — is still alive; koryph never interrupts a
// running agent on its own. Before the fix, projectedRunCostUSD only added
// EstimateUSD for sl.Status == SlotRunning, so the moment a slot was flagged
// stuck its in-flight estimate silently dropped out of the projection and the
// --budget cap could be breached without tripping.
func TestProjectedRunCostCountsStuck(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots = map[string]*ledger.Slot{
		"done":    {PhaseID: "done", Status: ledger.SlotMerged, CostUSD: 3.0, EstimateUSD: 2.0},
		"stuck":   {PhaseID: "stuck", Status: ledger.SlotStuck, CostUSD: 1.0, EstimateUSD: 4.0},
		"running": {PhaseID: "running", Status: ledger.SlotRunning, CostUSD: 0, EstimateUSD: 5.0},
	}
	// settled: 3 (done) + 1 (stuck's prior accrued cost) = 4
	// in-flight: 4 (stuck's estimate, still alive) + 5 (running's estimate) = 9
	if got := r.projectedRunCostUSD(); got != 13.0 {
		t.Errorf("projectedRunCostUSD = %v, want 13.0 (4 settled + 9 in-flight, stuck slot counted)", got)
	}
}

// TestBudgetExhaustedUsesProjected proves the cap is measured against projected
// spend, so an in-flight wave can trip it before any agent settles.
func TestBudgetExhaustedUsesProjected(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots = map[string]*ledger.Slot{
		"running": {PhaseID: "running", Status: ledger.SlotRunning, EstimateUSD: 9.0},
	}

	r.opts.BudgetUSD = 0
	if r.budgetExhausted() {
		t.Error("no cap set → never exhausted")
	}
	r.opts.BudgetUSD = 10
	if r.budgetExhausted() {
		t.Error("projected 9 < cap 10 → not exhausted")
	}
	r.opts.BudgetUSD = 8
	if !r.budgetExhausted() {
		t.Error("projected 9 >= cap 8 → exhausted (in-flight estimate counted)")
	}
}

// TestParkForRunBudget proves a requeue is refused (slot parked terminal) once
// the run --budget is exhausted, and proceeds normally while budget remains.
func TestParkForRunBudget(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	over := &ledger.Slot{PhaseID: "tb1", BeadID: "tb1", Status: ledger.SlotRunning, CostUSD: 12.0}
	if err := r.store.SetSlot(r.run, over); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	r.opts.BudgetUSD = 10 // 12 projected >= 10

	if !r.parkForRunBudget(over) {
		t.Fatal("parkForRunBudget should park when over budget")
	}
	got := r.run.Slots["tb1"]
	if got.Status != ledger.SlotBlocked {
		t.Errorf("parked slot status = %v, want blocked", got.Status)
	}
	if !strings.Contains(got.Note, "run --budget cap reached") {
		t.Errorf("parked slot note = %q, want a budget-cap reason", got.Note)
	}

	// Well under budget → does not park; the caller proceeds with the requeue.
	r.opts.BudgetUSD = 1000
	under := &ledger.Slot{PhaseID: "tb2", BeadID: "tb2", Status: ledger.SlotRunning, CostUSD: 1.0}
	if err := r.store.SetSlot(r.run, under); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	if r.parkForRunBudget(under) {
		t.Error("parkForRunBudget should not park while budget remains")
	}
}
