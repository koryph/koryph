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

// TestWriteBackEscalatedMergeLabelsBead proves a bead that merged only after
// its final attempt escalated gets a model-observed:<tier> label — and that
// an ordinary (non-escalated) merge writes nothing.
func TestWriteBackEscalatedMergeLabelsBead(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	r.run.Slots["wb1"] = &ledger.Slot{
		PhaseID: "wb1", Model: "opus",
		ModelWhy: "escalated from sonnet after 2 bead-fault attempts (agent died with no commits)",
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

// TestBlockedAttemptsExhaustedCommentsBead proves the attempts-exhausted
// block leaves a bd comment carrying model, attempt count, and death summary.
func TestBlockedAttemptsExhaustedCommentsBead(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}

	sl := &ledger.Slot{
		PhaseID:  "wb3",
		Status:   ledger.SlotRunning,
		Attempts: ledger.MaxAttempts,
		Model:    "opus",
		ModelWhy: "escalated from sonnet after 2 bead-fault attempts (agent died with no commits)",
	}
	r.run.Slots["wb3"] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	r.completeSlot(t.Context(), sl)

	if got := r.run.Slots["wb3"].Status; got != ledger.SlotBlocked {
		t.Fatalf("status = %q, want blocked", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("Comment calls = %v, want exactly one", fake.comments)
	}
	text := fake.comments[0][1]
	if fake.comments[0][0] != "wb3" ||
		!strings.Contains(text, "blocked after 3 attempts") ||
		!strings.Contains(text, "opus") ||
		!strings.Contains(text, "escalated from sonnet") {
		t.Errorf("blocked comment = %q, want attempts + model + escalation rationale", text)
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
	}
	r.run.Slots["wb4"] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	r.requeueBudgetKilled(t.Context(), sl, 1, dispatch.TokenUsage{})

	if got := r.run.Slots["wb4"].Status; got != ledger.SlotBlocked {
		t.Fatalf("status = %q, want blocked (parked)", got)
	}
	if len(fake.comments) != 1 {
		t.Fatalf("Comment calls = %v, want exactly one", fake.comments)
	}
	text := fake.comments[0][1]
	if !strings.Contains(text, "needs-attention") || !strings.Contains(text, "$7.50") {
		t.Errorf("park comment = %q, want needs-attention + accumulated cost", text)
	}
}
