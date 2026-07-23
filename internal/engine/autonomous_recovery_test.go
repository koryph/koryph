// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestParkForOperatorStop pins koryph-a1x (F1a): a phase an operator stopped via
// `koryph stop` is parked terminal (not requeued, not auto-merged), and the
// sentinel is consumed so a later deliberate re-dispatch is not blocked.
func TestParkForOperatorStop(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	sl := &ledger.Slot{PhaseID: "os1", BeadID: "os1", Status: ledger.SlotRunning}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	// No stop marker → does not park; the caller proceeds with classification.
	if r.parkForOperatorStop(context.Background(), sl) {
		t.Fatal("parkForOperatorStop should not park without a stop marker")
	}

	if err := r.store.RequestStop("os1"); err != nil {
		t.Fatalf("RequestStop: %v", err)
	}
	if !r.parkForOperatorStop(context.Background(), sl) {
		t.Fatal("parkForOperatorStop should park a stopped phase")
	}
	got := r.run.Slots["os1"]
	if got.Status != ledger.SlotBlocked {
		t.Errorf("parked slot status = %v, want blocked", got.Status)
	}
	if !strings.Contains(got.Note, "operator-stopped") {
		t.Errorf("parked slot note = %q, want an operator-stopped reason", got.Note)
	}
	if r.store.StopRequested("os1") {
		t.Error("stop marker should be consumed once the phase is parked")
	}
	// koryph-84yu: the bd claim must be reconciled to blocked, never left
	// stranded in_progress with no live agent.
	if !fakeBlocked(fake, "os1") {
		t.Errorf("operator-stop did not reconcile the bd claim to blocked; SetStatus calls = %v (the strand this guards)", fake.setStatus)
	}
}

// TestParkForDrain pins koryph-z0x (F1b): a death during an operator drain parks
// instead of requeueing, so drain suppresses retries and not only fresh dispatch.
func TestParkForDrain(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake

	sl := &ledger.Slot{PhaseID: "dr1", BeadID: "dr1", Status: ledger.SlotRunning}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	// No drain → does not park.
	if r.parkForDrain(context.Background(), sl) {
		t.Fatal("parkForDrain should not park when no drain is active")
	}

	if err := r.store.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain: %v", err)
	}
	if !r.parkForDrain(context.Background(), sl) {
		t.Fatal("parkForDrain should park a death that arrives during drain")
	}
	got := r.run.Slots["dr1"]
	if got.Status != ledger.SlotBlocked {
		t.Errorf("parked slot status = %v, want blocked", got.Status)
	}
	if !strings.Contains(got.Note, "drain active") {
		t.Errorf("parked slot note = %q, want a drain reason", got.Note)
	}
	if !fakeBlocked(fake, "dr1") {
		t.Errorf("drain-park did not reconcile the bd claim to blocked; SetStatus calls = %v", fake.setStatus)
	}
}

// TestProtectedResolutionHint pins koryph-zfg (F2): an all-liftable touch names
// the --allow-protected one-command path; a touch that includes an unliftable
// governance/project path says manual review is required.
func TestProtectedResolutionHint(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	liftable := r.protectedResolutionHint([]string{"Makefile", ".github/workflows/ci.yml"}, "feat/x", "b1")
	if !strings.Contains(liftable, "--allow-protected") {
		t.Errorf("all-liftable hint = %q, want an --allow-protected command", liftable)
	}

	manual := r.protectedResolutionHint([]string{"Makefile", "CLAUDE.md"}, "feat/x", "b1")
	if !strings.Contains(manual, "manual review") {
		t.Errorf("mixed hint = %q, want a manual-review notice", manual)
	}
	if strings.Contains(manual, "--allow-protected --push") {
		t.Errorf("mixed hint = %q, must not suggest the lift command", manual)
	}
}
