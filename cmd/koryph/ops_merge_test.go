// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/ledger"
)

// TestMergeFailsFastOnLiveEngineLock covers koryph-2zc: a manual `koryph
// merge` issued while a `koryph run` engine is live on the same project must
// name the holder and fail fast immediately, instead of racing the engine's
// own git activity and hanging with no indication of what it is waiting on.
func TestMergeFailsFastOnLiveEngineLock(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "livemerge")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID},
	})

	// A live engine holds the project's koryph.lock for its whole lifetime
	// (ledger.Store.RunLock) — merge must refuse to proceed underneath it.
	lstore := ledger.NewStore(rec.Root)
	lock, err := lstore.RunLock(run.RunID)
	if err != nil {
		t.Fatalf("RunLock: %v", err)
	}
	defer lock.Unlock()

	code, _, errb := runCmd("merge", "--project", rec.ProjectID, "some-branch")
	if code != engine.ExitFatal {
		t.Fatalf("code = %d, want %d; stderr = %q", code, engine.ExitFatal, errb)
	}
	if !strings.Contains(errb, "live engine") || !strings.Contains(errb, "--wait") {
		t.Errorf("stderr = %q, want a live-engine holder notice mentioning --wait", errb)
	}
	if !strings.Contains(errb, run.RunID) {
		t.Errorf("stderr = %q, want it to name the holding run %s", errb, run.RunID)
	}
}

// TestMergeWaitPollsUntilLockReleases covers the --wait opt-in: instead of
// failing fast, merge polls with periodic progress until the live engine's
// lock clears, then proceeds (past the lock gate — it still fails afterward
// here since "some-branch" has no worktree, which is fine: the point is it
// got past the contended gate instead of failing fast or hanging forever).
func TestMergeWaitPollsUntilLockReleases(t *testing.T) {
	isolate(t)
	orig := mergeLockPollInterval
	mergeLockPollInterval = 20 * time.Millisecond
	t.Cleanup(func() { mergeLockPollInterval = orig })

	rec := registerMinimalProject(t, "waitmerge")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID},
	})

	lstore := ledger.NewStore(rec.Root)
	lock, err := lstore.RunLock(run.RunID)
	if err != nil {
		t.Fatalf("RunLock: %v", err)
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(60 * time.Millisecond)
		_ = lock.Unlock()
		close(released)
	}()
	t.Cleanup(func() { <-released })

	code, out, errb := runCmd("merge", "--project", rec.ProjectID, "--wait", "some-branch")
	// Past the lock gate, merge fails for lack of a worktree carrying
	// "some-branch" — that failure is expected and not what this test checks.
	if code == 0 {
		t.Fatalf("code = 0, want a non-zero exit past the lock gate (no worktree for the branch); stdout=%q stderr=%q", out, errb)
	}
	if !strings.Contains(out, "waiting on") {
		t.Errorf("stdout = %q, want progress while waiting on the live engine", out)
	}
	if !strings.Contains(out, "released") {
		t.Errorf("stdout = %q, want a notice that the lock released", out)
	}
	if strings.Contains(errb, "--wait") {
		t.Errorf("stderr = %q, should not fail fast when --wait was passed", errb)
	}
}

// TestLandFailsFastOnLiveEngineLock is TestMergeFailsFastOnLiveEngineLock's
// counterpart for `koryph land`, which shares the same awaitProjectLock gate.
func TestLandFailsFastOnLiveEngineLock(t *testing.T) {
	isolate(t)
	rec := registerMinimalProject(t, "liveland")
	run := seedTestRun(t, rec, []*ledger.Slot{
		{PhaseID: "b1", BeadID: "b1", Branch: "agent/b1", Status: ledger.SlotRunning, PID: deadTestPID},
	})

	lstore := ledger.NewStore(rec.Root)
	lock, err := lstore.RunLock(run.RunID)
	if err != nil {
		t.Fatalf("RunLock: %v", err)
	}
	defer lock.Unlock()

	code, _, errb := runCmd("land", "--project", rec.ProjectID, "b1")
	if code != engine.ExitFatal {
		t.Fatalf("code = %d, want %d; stderr = %q", code, engine.ExitFatal, errb)
	}
	if !strings.Contains(errb, "koryph land") || !strings.Contains(errb, "live engine") || !strings.Contains(errb, "--wait") {
		t.Errorf("stderr = %q, want a koryph-land live-engine holder notice mentioning --wait", errb)
	}
}
