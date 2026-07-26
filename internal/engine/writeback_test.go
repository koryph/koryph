// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

// koryph-qf6.5: terminal states write history back to the bead itself —
// labels and comments sync through the beads DB, unlike the gitignored
// machine-local run ledger where this data was previously stranded.

// TestWriteBackEscalatedMergeLabelsBead preserves read-only provenance for a
// historical pre-typed-policy ledger that already records an escalation. New
// runs cannot create this attempt-driven rationale; an ordinary typed-policy
// merge writes nothing.
func TestWriteBackEscalatedMergeLabelsBead(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	r.run.Slots["wb1"] = &ledger.Slot{
		PhaseID: "wb1", Model: "opus",
		ModelWhy: "historical escalation imported from a pre-typed-policy ledger",
	}
	r.run.Slots["wb2"] = &ledger.Slot{
		PhaseID: "wb2", Model: "sonnet", ModelWhy: "stage default (implement)",
	}

	r.writeBackEscalatedMerge(t.Context(), "wb1")
	r.writeBackEscalatedMerge(t.Context(), "wb2")

	if len(fake.addLabels) != 1 {
		t.Fatalf("AddLabel calls = %v, want exactly one (the escalated merge)", fake.addLabels)
	}
	if fake.addLabels[0] != [2]string{"wb1", "model-observed:opus"} {
		t.Errorf("AddLabel = %v, want [wb1 model-observed:opus]", fake.addLabels[0])
	}
}

// TestTypedBlockedCandidateCommentsBead proves a typed terminal block leaves
// a bd comment carrying the outcome, model, and attempt count without
// inventing an attempt-number model escalation.
func TestTypedBlockedCandidateCommentsBead(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}

	sl := &ledger.Slot{
		PhaseID:  "wb3",
		Status:   ledger.SlotRunning,
		Attempts: 1,
		Model:    "sonnet",
		ModelWhy: "persona koryph-implementer standard implementation tier",
	}
	r.run.Slots["wb3"] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	// Exercise writeback from an already-classified worker defect. Candidate
	// assessment is covered separately and now correctly treats this test's
	// intentionally minimal slot (no trusted dispatch manifest) as an engine
	// invariant.
	r.parkTypedRecovery(t.Context(), sl, OutcomeCodeDefect, "worker result is malformed")

	if got := r.run.Slots["wb3"].Status; got != ledger.SlotBlocked {
		t.Fatalf("status = %q, want blocked", got)
	}
	// koryph-84yu: the bd claim is reconciled to blocked — never left stranded
	// in_progress with no live agent — and the comment still carries model,
	// attempt count, and death summary.
	if !fakeBlocked(fake, "wb3") {
		t.Fatalf("attempts-exhausted did not reconcile the bd claim to blocked; SetStatus = %v (the strand this guards)", fake.setStatus)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("Comment calls = %v, want exactly one", fake.comments)
	}
	text := fake.comments[0][1]
	if fake.comments[0][0] != "wb3" ||
		!strings.Contains(text, "code-defect") ||
		!strings.Contains(text, "1 attempt(s)") ||
		!strings.Contains(text, "sonnet") ||
		strings.Contains(strings.ToLower(text), "escalat") {
		t.Errorf("blocked comment = %q, want typed outcome + attempt + standard model without escalation", text)
	}
}

// TestBudgetKillParkCommentsBead proves the needs-attention park leaves a bd
// comment with the why and accumulated cost.
func TestBudgetKillParkCommentsBead(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	sl := &ledger.Slot{
		PhaseID:            "wb4",
		Status:             ledger.SlotRunning,
		Attempts:           2,
		Model:              "sonnet",
		CostUSD:            7.5,
		BudgetKillRequeues: 1, // at budget: park, don't warm-resume again
		Retry: ledger.RetryCounters{
			BudgetContinuations: 1,
		},
	}
	r.run.Slots["wb4"] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	r.requeueBudgetKilled(t.Context(), sl, 1, dispatch.TokenUsage{})

	if got := r.run.Slots["wb4"].Status; got != ledger.SlotBlocked {
		t.Fatalf("status = %q, want blocked (parked)", got)
	}
	if !fakeBlocked(fake, "wb4") {
		t.Fatalf("budget-kill park did not reconcile the bd claim to blocked; SetStatus = %v", fake.setStatus)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("Comment calls = %v, want exactly one", fake.comments)
	}
	text := fake.comments[0][1]
	if !strings.Contains(text, "needs-attention") || !strings.Contains(text, "$7.50") {
		t.Errorf("park comment = %q, want needs-attention + accumulated cost", text)
	}
}
