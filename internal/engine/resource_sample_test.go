// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/resmon"
)

// TestSampleSlotResources_WritesDerivedFields exercises the real resmon sampler
// end-to-end: with a slot whose PID is this test process, one sampling pass must
// populate the slot's resource aggregates in the ledger.
func TestSampleSlotResources_WritesDerivedFields(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	t.Setenv(envResmon, "on") // re-enable sampling that newFixture disables

	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "tb1", BeadID: "tb1", Status: ledger.SlotRunning, PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	procs := r.sampleProcTable(context.Background())
	if procs == nil {
		t.Skip("resmon.Snapshot unavailable on this platform")
	}
	r.sampleSlotResources(r.run.Slots["tb1"], procs)

	got := r.run.Slots["tb1"]
	if got.ResourceSamples != 1 {
		t.Errorf("ResourceSamples = %d, want 1", got.ResourceSamples)
	}
	if got.PeakRSSMB <= 0 {
		t.Errorf("PeakRSSMB = %d, want > 0 (the test process has resident memory)", got.PeakRSSMB)
	}
	if got.AvgRSSMB <= 0 {
		t.Errorf("AvgRSSMB = %d, want > 0", got.AvgRSSMB)
	}
	// A second pass keeps the running aggregate consistent (peak never regresses).
	peak1 := got.PeakRSSMB
	r.sampleSlotResources(r.run.Slots["tb1"], procs)
	if r.run.Slots["tb1"].ResourceSamples != 2 {
		t.Errorf("ResourceSamples after 2nd pass = %d, want 2", r.run.Slots["tb1"].ResourceSamples)
	}
	if r.run.Slots["tb1"].PeakRSSMB < peak1 {
		t.Errorf("PeakRSSMB regressed: %d < %d", r.run.Slots["tb1"].PeakRSSMB, peak1)
	}
}

// TestSampleSlotResources_RequeueResetsPerAttempt verifies that a requeue (the
// slot's PID changing) starts a fresh per-attempt accumulation rather than
// carrying the prior attempt's CPU into a new, shorter wall window (finding #5).
func TestSampleSlotResources_RequeueResetsPerAttempt(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	t.Setenv(envResmon, "on") // re-enable sampling that newFixture disables
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "tb1", Status: ledger.SlotRunning, PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	procs := r.sampleProcTable(context.Background())
	if procs == nil {
		t.Skip("resmon unavailable")
	}
	r.sampleSlotResources(r.run.Slots["tb1"], procs)
	r.sampleSlotResources(r.run.Slots["tb1"], procs)
	if got := r.resUsage["tb1"]; got == nil || got.usage.Samples != 2 {
		t.Fatalf("same-PID: expected 2 folded samples, got %+v", got)
	}

	// Simulate a requeue: the slot keeps its phase id but gets a new PID (its
	// parent, which is alive so Aggregate still finds a cohort).
	r.run.Slots["tb1"].PID = os.Getppid()
	r.sampleSlotResources(r.run.Slots["tb1"], procs)
	got := r.resUsage["tb1"]
	if got == nil || got.pid != os.Getppid() {
		t.Fatalf("requeue: expected Usage re-keyed to new PID, got %+v", got)
	}
	if got.usage.Samples != 1 {
		t.Errorf("requeue: expected fresh per-attempt accumulation (Samples=1), got %d", got.usage.Samples)
	}
}

// TestSampleSlotResources_DeadPIDNoSample verifies a slot whose process has
// exited contributes no sample (Aggregate not found) rather than erroring.
func TestSampleSlotResources_DeadPIDNoSample(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	t.Setenv(envResmon, "on") // re-enable sampling that newFixture disables
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "tb1", Status: ledger.SlotRunning, PID: 1 << 30, // implausible pid
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	procs := r.sampleProcTable(context.Background())
	if procs == nil {
		t.Skip("resmon unavailable")
	}
	r.sampleSlotResources(r.run.Slots["tb1"], procs)
	if got := r.run.Slots["tb1"]; got.ResourceSamples != 0 || got.PeakRSSMB != 0 {
		t.Errorf("dead pid produced a sample: %+v", got)
	}
}

// TestStampFinishedAt verifies FinishedAt is stamped only on a terminal slot and
// clears the in-memory Usage for a future requeue.
func TestStampFinishedAt(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "tb1", Status: ledger.SlotRunning, PID: os.Getpid(),
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	r.resUsage = map[string]*slotResUsage{"tb1": {pid: os.Getpid(), usage: resmon.Usage{Samples: 3}}}

	// Non-terminal: no stamp, Usage retained.
	r.stampFinishedAt("tb1")
	if r.run.Slots["tb1"].FinishedAt != "" {
		t.Error("FinishedAt stamped on a running slot")
	}
	if _, ok := r.resUsage["tb1"]; !ok {
		t.Error("Usage dropped for a still-running slot")
	}

	// Drive terminal, then stamp.
	_ = r.store.UpdateSlot(r.run, "tb1", func(s *ledger.Slot) { s.Status = ledger.SlotMerged })
	r.stampFinishedAt("tb1")
	if r.run.Slots["tb1"].FinishedAt == "" {
		t.Error("FinishedAt not stamped on a terminal slot")
	}
	if _, ok := r.resUsage["tb1"]; ok {
		t.Error("Usage not dropped after terminal stamp")
	}

	// Idempotent: a second call does not overwrite the stamp.
	first := r.run.Slots["tb1"].FinishedAt
	r.stampFinishedAt("tb1")
	if r.run.Slots["tb1"].FinishedAt != first {
		t.Error("FinishedAt overwritten on second stamp")
	}
}
