// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
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
	const commandPrefix = "routine CI/build paths — land with: "
	const command = "koryph merge --project proj --allow-protected --push --close-bead b1 --reason 'operator-approved protected-path landing' feat/x"
	if got := strings.TrimPrefix(liftable, commandPrefix); got != command {
		t.Errorf("all-liftable command = %q, want %q", got, command)
	}

	// The hint is meant to be pasted into a shell. Run it through sh with a
	// stand-in koryph executable so redirections, quoting, and Go flag order
	// are exercised exactly as an operator would use the displayed command.
	binDir := t.TempDir()
	stub := filepath.Join(binDir, "koryph")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
		t.Fatalf("write koryph stub: %v", err)
	}
	cmd := exec.Command("sh", "-c", command)
	cmd.Env = append(os.Environ(), "PATH="+binDir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run advertised command in shell: %v", err)
	}
	const wantArgs = "merge\n--project\nproj\n--allow-protected\n--push\n--close-bead\nb1\n--reason\noperator-approved protected-path landing\nfeat/x\n"
	if got := string(out); got != wantArgs {
		t.Errorf("advertised command arguments = %q, want %q", got, wantArgs)
	}

	manual := r.protectedResolutionHint([]string{"Makefile", "CLAUDE.md"}, "feat/x", "b1")
	if !strings.Contains(manual, "manual review") {
		t.Errorf("mixed hint = %q, want a manual-review notice", manual)
	}
	if strings.Contains(manual, "--allow-protected --push") {
		t.Errorf("mixed hint = %q, must not suggest the lift command", manual)
	}
}
