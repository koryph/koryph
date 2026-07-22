// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/procx"
)

// deadTestPID is a PID far above any pid_max (kill(pid,0) → ESRCH → dead) —
// the same sentinel internal/ledger's and internal/procx's own tests use.
const deadTestPID = 2000000000

func TestOpsReconcileNoRuns(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "norun")

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "no runs found") {
		t.Errorf("stdout = %q, want 'no runs found'", out)
	}
}

func TestOpsReconcileLiveEngineIsNoOp(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "liveengine")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID},
	})

	// A live engine holds the project's koryph.lock (RunLock is held for the
	// engine's whole lifetime) — reconcile must refuse to touch anything.
	lstore := ledger.NewStore(rec.Root)
	lock, err := lstore.RunLock(run.RunID)
	if err != nil {
		t.Fatalf("RunLock: %v", err)
	}
	defer lock.Unlock()

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "live engine") || !strings.Contains(out, "nothing to reconcile") {
		t.Errorf("stdout = %q, want a live-engine no-op notice", out)
	}

	reloaded, err := lstore.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if got := reloaded.Slots["b1"].Status; got != ledger.SlotRunning {
		t.Errorf("slot status = %q, want unchanged %q", got, ledger.SlotRunning)
	}
}

func TestOpsReconcileDeadSlotBlockedAndFinalizes(t *testing.T) {
	isolate(t)
	if procx.Alive(deadTestPID) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", deadTestPID)
	}
	rec := registerMinimalProject(t, "deadslot")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID, Commits: 3},
	})

	// Seed a govern lease keyed to a LIVE pid (this test process) so govern's
	// own pruning does not remove it out from under the assertion — only
	// `ops reconcile`'s explicit Release call should.
	gs := govern.NewStore()
	if err := gs.Hold(govern.Lease{Project: rec.ProjectID, Bead: "b1", PID: os.Getpid(), EnginePID: os.Getpid(), Provider: "personal"}); err != nil {
		t.Fatalf("seed lease: %v", err)
	}

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	for _, want := range []string{
		"b1: running -> blocked",
		"3 commits preserved on agent/b1",
		"run " + run.RunID + " finalized",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}

	lstore := ledger.NewStore(rec.Root)
	reloaded, err := lstore.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	sl := reloaded.Slots["b1"]
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want %q", sl.Status, ledger.SlotBlocked)
	}
	if sl.PID != 0 {
		t.Errorf("slot pid = %d, want 0 (cleared)", sl.PID)
	}
	if !strings.Contains(sl.Note, "reconciled: agent dead, loop gone; 3 commits preserved on agent/b1") {
		t.Errorf("slot note = %q, want the reconcile note", sl.Note)
	}
	if reloaded.Status != ledger.RunDone {
		t.Errorf("run status = %q, want %q", reloaded.Status, ledger.RunDone)
	}

	// The lease must be gone: released explicitly by reconcile (not merely
	// pruned — its pid is alive, so govern's own prune would have kept it).
	_, leases, _, err := gs.Snapshot("personal")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, l := range leases {
		if l.Project == rec.ProjectID && l.Bead == "b1" {
			t.Errorf("lease for %s/b1 still held after reconcile: %+v", rec.ProjectID, l)
		}
	}
}

func TestOpsReconcileAlivePidLeftAlone(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "aliveslot")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: os.Getpid()},
	})

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "pid") || !strings.Contains(out, "alive") || !strings.Contains(out, "left alone") {
		t.Errorf("stdout = %q, want an alive-pid left-alone notice", out)
	}
	if !strings.Contains(out, "still has 1 live slot") {
		t.Errorf("stdout = %q, want a not-finalized notice", out)
	}

	lstore := ledger.NewStore(rec.Root)
	reloaded, err := lstore.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if got := reloaded.Slots["b1"].Status; got != ledger.SlotRunning {
		t.Errorf("slot status = %q, want unchanged %q", got, ledger.SlotRunning)
	}
	if reloaded.Status == ledger.RunDone {
		t.Errorf("run finalized while a live slot remained")
	}
}

func TestOpsReconcileDryRunMutatesNothing(t *testing.T) {
	isolate(t)
	if procx.Alive(deadTestPID) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", deadTestPID)
	}
	rec := registerMinimalProject(t, "dryrun")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID, Commits: 1},
	})

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID, "--dry-run")
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "would reconcile") || !strings.Contains(out, "dry-run") {
		t.Errorf("stdout = %q, want dry-run wording", out)
	}

	lstore := ledger.NewStore(rec.Root)
	reloaded, err := lstore.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if got := reloaded.Slots["b1"].Status; got != ledger.SlotRunning {
		t.Errorf("dry-run mutated slot status to %q, want unchanged %q", got, ledger.SlotRunning)
	}
	if reloaded.Status == ledger.RunDone {
		t.Errorf("dry-run finalized the run")
	}
}

// TestOpsReconcilePidReuseWarning proves the koryph-1es pid-reuse cross-check:
// a pid that is alive but whose process start time is well after the slot's
// recorded DispatchedAt is flagged as a suspected recycled pid, even though
// it is still left alone (the contract only changes the report, not the
// action).
func TestOpsReconcilePidReuseWarning(t *testing.T) {
	isolate(t)
	if _, ok := procx.StartTime(os.Getpid()); !ok {
		t.Skip("procx.StartTime unavailable on this platform (no `ps -o etime=`); skipping")
	}
	rec := registerMinimalProject(t, "pidreuse")
	seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: os.Getpid(), DispatchedAt: "2000-01-01T00:00:00Z"},
	})

	code, out, errb := runCmd("ops", "reconcile", "--project", rec.ProjectID)
	if code != 0 {
		t.Fatalf("code = %d, stderr = %q", code, errb)
	}
	if !strings.Contains(out, "possible pid reuse") {
		t.Errorf("stdout = %q, want a pid-reuse warning", out)
	}
}

func TestOpsReconcileUnknownSubcommand(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("ops", "bogus")
	if code == 0 {
		t.Error("unknown ops subcommand: code = 0, want non-zero")
	}
	if !strings.Contains(errb, "bogus") {
		t.Errorf("stderr = %q, want it to name the unknown subcommand", errb)
	}
}

func TestOpsBareShowsHelp(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("ops")
	if code != 0 {
		t.Errorf("code = %d, want 0", code)
	}
	if !strings.Contains(out, "reconcile") {
		t.Errorf("stdout = %q, want the reconcile subcommand listed", out)
	}
}
