// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestResumeAdoptsStillRunningOrphan pins the engine-death child contract
// (koryph-6xoe): a dispatched agent is detached into its own session (setsid,
// dispatch.Dispatch's launch), so it OUTLIVES an engine that was SIGTERM'd or
// killed while it worked. `koryph run --resume` must adopt that still-running
// orphan by *reattaching* to it — resuming the poll loop over the same live
// process — rather than requeuing it (which clears the PID and restarts the
// bead, throwing away the work in flight).
//
// The companion cases are covered elsewhere: the graceful SIGTERM->interrupted
// finalization in interrupted_test.go (TestSIGTERMInterruptedFinalizesAndEmitsRunEnd),
// and the classifier's alive->reattach rule in ledger/classify_test.go. This
// test proves the two compose end to end through resume().
func TestResumeAdoptsStillRunningOrphan(t *testing.T) {
	// A real, still-running process standing in for the orphaned agent. Setsid
	// mirrors dispatch's detached launch: the agent is in its own session, not
	// the (now dead) engine's process group.
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	livePID := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.processIdentityProbe = func(context.Context, int) string { return "agent-birth" }

	// The ledger a killed engine left behind: a running slot whose recorded
	// agent PID is still alive. Commits=0 proves adoption keys on authenticated
	// liveness — a matching process-start identity reattaches before git probes.
	sl := &ledger.Slot{PhaseID: "tb1", BeadID: "tb1", Status: ledger.SlotRunning, PID: livePID, ProcessIdentity: "agent-birth"}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	resumed, err := r.resume(context.Background())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed {
		t.Fatal("resume returned false — a still-running orphan must be adopted, not discarded for a fresh run")
	}

	got := r.run.Slots["tb1"]
	if got == nil {
		t.Fatal("slot tb1 vanished on resume")
	}
	if got.Status != ledger.SlotRunning {
		t.Errorf("slot status = %q, want %q (a live orphan is reattached, not requeued)", got.Status, ledger.SlotRunning)
	}
	if got.PID != livePID {
		t.Errorf("slot PID = %d, want %d preserved — a reattach must keep the live agent's PID, not clear it like a requeue", got.PID, livePID)
	}
	// A reattach adopts an existing process: resume() must NOT have parked it in
	// the width-gated re-dispatch backlog (that path is for dead agents).
	if q := len(r.queuedResumeIDs()); q != 0 {
		t.Errorf("queued backlog = %d, want 0 — a live orphan reattaches in place, it is not queued for re-dispatch", q)
	}
}

func TestResumeRejectsRecycledPID(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })

	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.processIdentityProbe = func(context.Context, int) string { return "unrelated-birth" }
	// The PID is live, but the birth identity says it belonged to the agent that
	// exited before this unrelated process inherited the numeric PID.
	sl := &ledger.Slot{
		PhaseID: "reused", BeadID: "reused", Status: ledger.SlotRunning, PID: pid,
		ProcessIdentity: "agent-birth",
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	resumed, err := r.resume(context.Background())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed {
		t.Fatal("resume returned false — the rejected slot must be queued for recovery")
	}
	got := r.run.Slots["reused"]
	if got.Status != ledger.SlotQueued || got.PID != 0 {
		t.Errorf("recycled PID slot = status %q pid %d, want queued with cleared PID", got.Status, got.PID)
	}
}

func TestResumeFinalizesCompletedCandidateWithoutCodingRedispatch(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	statusPath := filepath.Join(t.TempDir(), "status.json")
	if err := os.WriteFile(statusPath, []byte(`{"state":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sl := &ledger.Slot{
		PhaseID: "complete", BeadID: "complete", Status: ledger.SlotRunning,
		PID: 9999999, Attempts: ledger.MaxAttempts, Commits: 2,
		StatusPath: statusPath,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	resumed, err := r.resume(context.Background())
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !resumed {
		t.Fatal("resume returned false for completion-ready candidate")
	}
	got := r.run.Slots["complete"]
	if got.Status != ledger.SlotFinalizing || got.PID != 0 || got.ProcessIdentity != "" {
		t.Fatalf("completed slot = status %q pid %d identity %q, want finalizing with no process", got.Status, got.PID, got.ProcessIdentity)
	}
	if got.Attempts != ledger.MaxAttempts {
		t.Fatalf("attempts = %d, want unchanged %d", got.Attempts, ledger.MaxAttempts)
	}
	if r.dispatched != 0 || len(r.queuedResumeIDs()) != 0 {
		t.Fatalf("resume dispatched=%d queued=%d, want zero coding redispatch", r.dispatched, len(r.queuedResumeIDs()))
	}

	// A second engine restart remains in finalization and still cannot turn
	// the candidate back into coding work.
	resumed, err = r.resume(context.Background())
	if err != nil || !resumed {
		t.Fatalf("second resume = %v, %v; want adopted finalization", resumed, err)
	}
	if got = r.run.Slots["complete"]; got.Status != ledger.SlotFinalizing {
		t.Fatalf("second resume status = %q, want finalizing", got.Status)
	}
}

func TestResumeReviewCandidateSkipsCompletionAccounting(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	sl := &ledger.Slot{
		PhaseID: "reviewing", BeadID: "reviewing", Status: ledger.SlotReview,
		PID: 9999999, Attempts: 1, Commits: 1,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	if resumed, err := r.resume(context.Background()); err != nil || !resumed {
		t.Fatalf("resume = %v, %v; want adopted review finalization", resumed, err)
	}
	got := r.run.Slots["reviewing"]
	if got.Status != ledger.SlotFinalizing || !got.CompletionAccounted {
		t.Fatalf("review slot = status %q accounted=%v, want finalizing/accounted", got.Status, got.CompletionAccounted)
	}
	if r.dispatched != 0 {
		t.Fatalf("coding dispatches = %d, want 0", r.dispatched)
	}
}

func TestResumePartialCommitsWithoutCompletionRemainQueued(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	sl := &ledger.Slot{
		PhaseID: "partial", BeadID: "partial", Status: ledger.SlotRunning,
		PID: 9999999, Attempts: 1, Commits: 2,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	if resumed, err := r.resume(context.Background()); err != nil || !resumed {
		t.Fatalf("resume = %v, %v; want queued partial candidate", resumed, err)
	}
	if got := r.run.Slots["partial"]; got.Status != ledger.SlotQueued {
		t.Fatalf("partial status = %q, want queued for bounded coding resume", got.Status)
	}
}
