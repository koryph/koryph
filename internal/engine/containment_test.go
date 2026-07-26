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
	"time"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
)

func startContainmentProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", `while :; do sleep 1; done`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Process.Release(); err != nil {
		_ = cmd.Process.Kill()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if dispatch.Alive(pid) {
			_ = dispatch.StopForceAndReap(pid)
		}
	})
	return pid
}

func containmentRunner(t *testing.T, pid int, identity string) (*runner, *fakeSource) {
	t.Helper()
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	source := &fakeSource{}
	r.adapter = source
	r.opts.NativeCanary = true
	r.gov = govern.NewStore()
	r.processIdentityProbe = func(context.Context, int) string { return identity }
	r.run.Slots["sibling"] = &ledger.Slot{
		PhaseID:          "sibling",
		BeadID:           "sibling",
		Status:           ledger.SlotRunning,
		PID:              pid,
		ProcessIdentity:  identity,
		VerifiedIdentity: "agent@example.com",
		Branch:           "koryph/sibling",
		Worktree:         filepath.Join(f.wtRoot, "sibling"),
		Attempts:         1,
	}
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatal(err)
	}
	if err := r.gov.Hold(govern.Lease{
		Project: r.opts.ProjectID, Bead: "sibling", PID: pid,
		EnginePID: 1, Provider: r.poolKey(),
	}); err != nil {
		t.Fatal(err)
	}
	return r, source
}

func TestNativeCanaryContainmentReapsTerminalizesAndReleases(t *testing.T) {
	pid := startContainmentProcess(t)
	r, source := containmentRunner(t, pid, "process-birth")

	got := r.containNativeCanaryHardStop()
	if !got.Required || !got.Complete || got.ActiveWorkers != 0 ||
		got.NonTerminalSlots != 0 || got.RemainingLeases != 0 || got.Error != "" {
		t.Fatalf("containment = %+v", got)
	}
	if dispatch.Alive(pid) {
		t.Fatalf("worker pid %d survived containment", pid)
	}
	sl := r.run.Slots["sibling"]
	if sl.Status != ledger.SlotBlocked ||
		sl.OutcomeClass != string(OutcomeOperatorStop) ||
		sl.PID != 0 || sl.ProcessIdentity != "" ||
		sl.DeathReason != deathReasonCanaryContainment {
		t.Fatalf("contained slot = %+v", sl)
	}
	if r.run.Status != ledger.RunAborted {
		t.Fatalf("run status = %q, want %q", r.run.Status, ledger.RunAborted)
	}
	if len(source.setStatus) != 1 || source.setStatus[0] != [2]string{"sibling", "blocked"} {
		t.Fatalf("tracker status writes = %+v", source.setStatus)
	}
	manifest, err := r.store.LoadManifest(r.run.RunID, "sibling")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExecutionState != "canary-hard-stop-contained" ||
		manifest.PID != 0 || manifest.ProcessIdentity != "" {
		t.Fatalf("manifest = %+v", manifest)
	}

	// Replay after the durable terminal checkpoint is a no-op that remains
	// converged; it must not duplicate tracker mutations.
	replayed := r.containNativeCanaryHardStop()
	if !replayed.Complete || replayed.Error != "" {
		t.Fatalf("idempotent replay = %+v", replayed)
	}
	if len(source.setStatus) != 1 {
		t.Fatalf("idempotent replay rewrote tracker: %+v", source.setStatus)
	}
}

func TestNativeCanaryContainmentIdentityMismatchFailsClosed(t *testing.T) {
	pid := startContainmentProcess(t)
	r, source := containmentRunner(t, pid, "recorded-birth")
	r.processIdentityProbe = func(context.Context, int) string { return "recycled-birth" }

	got := r.containNativeCanaryHardStop()
	if got.Complete || got.ActiveWorkers != 1 || got.NonTerminalSlots != 1 ||
		got.Error == "" {
		t.Fatalf("containment = %+v, want fail-closed live mismatch", got)
	}
	if !dispatch.Alive(pid) {
		t.Fatalf("identity mismatch signalled unrelated pid %d", pid)
	}
	sl := r.run.Slots["sibling"]
	if ledger.Terminal(sl.Status) || sl.DeathReason != deathReasonCanaryContainment {
		t.Fatalf("mismatched slot = %+v, want durable pending intent", sl)
	}
	if len(source.setStatus) != 0 {
		t.Fatalf("identity mismatch mutated tracker: %+v", source.setStatus)
	}
	manifest, err := r.store.LoadManifest(r.run.RunID, "sibling")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.ExecutionState != "canary-hard-stop-containment-pending" {
		t.Fatalf("manifest state = %q", manifest.ExecutionState)
	}
}

func TestResumeReplaysDurableNativeCanaryContainment(t *testing.T) {
	pid := startContainmentProcess(t)
	r, _ := containmentRunner(t, pid, "process-birth")
	runID := r.run.RunID
	r.containmentStop = func(int, time.Duration) error {
		return context.Canceled // crash boundary after durable pending intent
	}
	first := r.containNativeCanaryHardStop()
	if first.Complete || r.run.Slots["sibling"].DeathReason != deathReasonCanaryContainment {
		t.Fatalf("pre-crash containment = %+v, slot=%+v", first, r.run.Slots["sibling"])
	}

	recovered := &runner{
		opts: Options{ProjectID: "proj", NativeCanary: true},
		rec:  r.rec, cfg: r.cfg, store: r.store, run: r.run,
		adapter: &fakeSource{}, gov: r.gov,
	}
	recovered.processIdentityProbe = func(context.Context, int) string { return "process-birth" }
	adopted, err := recovered.resume(context.Background(), runID)
	if err != nil {
		t.Fatalf("resume containment replay: %v", err)
	}
	if !adopted || !recovered.pinnedTerminal {
		t.Fatalf("resume = adopted %t pinned %t", adopted, recovered.pinnedTerminal)
	}
	if dispatch.Alive(pid) {
		t.Fatalf("replay left worker pid %d alive", pid)
	}
	if sl := recovered.run.Slots["sibling"]; sl.Status != ledger.SlotBlocked || sl.PID != 0 {
		t.Fatalf("replayed slot = %+v", sl)
	}
}

func TestResumeReplaysTerminalContainmentBeforeLeaseRelease(t *testing.T) {
	pid := startContainmentProcess(t)
	r, _ := containmentRunner(t, pid, "process-birth")
	runID := r.run.RunID
	if err := dispatch.StopForceAndReap(pid); err != nil {
		t.Fatal(err)
	}
	if err := r.store.UpdateSlot(r.run, "sibling", func(sl *ledger.Slot) {
		sl.Status = ledger.SlotBlocked
		sl.OutcomeClass = string(OutcomeOperatorStop)
		sl.DeathReason = deathReasonCanaryContainment
		sl.PID = 0
		sl.ProcessIdentity = ""
		sl.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.checkpointCanaryContainment(
		r.run.Slots["sibling"], "canary-hard-stop-contained",
	); err != nil {
		t.Fatal(err)
	}
	// Simulate the exact crash window: terminal slot and manifest are durable,
	// but the governor lease and RunAborted checkpoint are not. Rebind the
	// fixture lease to this live test process so Snapshot cannot opportunistically
	// prune it before replay exercises the explicit release transaction.
	if err := r.gov.Hold(govern.Lease{
		Project: r.opts.ProjectID, Bead: "sibling", PID: os.Getpid(),
		EnginePID: os.Getpid(), Provider: r.poolKey(),
	}); err != nil {
		t.Fatal(err)
	}
	if r.remainingContainmentLeases(map[string]struct{}{"sibling": {}}) != 1 {
		t.Fatal("fixture lost the pre-release lease")
	}

	recovered := &runner{
		opts: Options{ProjectID: "proj", NativeCanary: true},
		rec:  r.rec, cfg: r.cfg, store: r.store, run: r.run,
		adapter: &fakeSource{}, gov: r.gov,
	}
	adopted, err := recovered.resume(context.Background(), runID)
	if err != nil {
		t.Fatalf("resume terminal containment replay: %v", err)
	}
	if !adopted || !recovered.pinnedTerminal {
		t.Fatalf("resume = adopted %t pinned %t", adopted, recovered.pinnedTerminal)
	}
	if recovered.run.Status != ledger.RunAborted {
		t.Fatalf("run status = %q, want aborted", recovered.run.Status)
	}
	if recovered.remainingContainmentLeases(map[string]struct{}{"sibling": {}}) != 0 {
		t.Fatal("terminal containment replay left governor lease")
	}
}
