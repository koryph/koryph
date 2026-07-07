// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

func TestNewRunCreatesDirLedgerAndSymlink(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	run, err := st.NewRun("proj-x", "bd", "v1.2.3")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	root := paths.KoryphRoot(repo)
	dir := filepath.Join(root, run.RunID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("run dir missing: fi=%v err=%v", fi, err)
	}
	if !fsx.Exists(filepath.Join(dir, "ledger.json")) {
		t.Fatal("ledger.json not written")
	}

	// latest symlink resolves to a relative bare runID.
	target, err := os.Readlink(filepath.Join(root, "latest"))
	if err != nil {
		t.Fatalf("readlink latest: %v", err)
	}
	if target != run.RunID {
		t.Fatalf("latest target = %q, want %q", target, run.RunID)
	}

	got, err := st.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if got.RunID != run.RunID || got.ProjectID != "proj-x" ||
		got.Source != "bd" || got.EngineVersion != "v1.2.3" {
		t.Fatalf("loaded run mismatch: %+v", got)
	}
	if got.Status != RunRunning {
		t.Fatalf("status = %q, want %q", got.Status, RunRunning)
	}
	if got.SchemaVersion != schemaVersion {
		t.Fatalf("schema_version = %d, want %d", got.SchemaVersion, schemaVersion)
	}
}

func fullSlot() *Slot {
	return &Slot{
		PhaseID:             "cn-42",
		BeadID:              "cn-42",
		EpicID:              "epic-1",
		Branch:              "koryph/cn-42",
		Worktree:            "/wt/cn-42",
		SessionID:           "sess-abc",
		SessionName:         "amber-otter",
		Agent:               "implementer",
		Model:               "sonnet",
		ModelWhy:            "cost/latency",
		Effort:              "high",
		AccountProfile:      "personal",
		ClaudeConfigDir:     "/cfg/personal",
		VerifiedIdentity:    "owner@example.com",
		VerifiedAt:          "2026-07-02T00:00:00Z",
		BillingMode:         "subscription",
		ProxyID:             "http://127.0.0.1:8091#v3",
		PID:                 12345,
		Stream:              "stream-1",
		StatusPath:          "/s/status.json",
		LogPath:             "/s/log.txt",
		Status:              SlotRunning,
		Attempts:            1,
		Commits:             2,
		LastCommit:          "abc1234",
		ResumeSHA:           "def5678",
		CostUSD:             1.25,
		InputTokens:         10000,
		OutputTokens:        500,
		CacheReadTokens:     8000,
		CacheCreationTokens: 1200,
		ReviewIters:         1,
		DispatchedAt:        "2026-07-02T00:00:01Z",
		MergedAt:            "2026-07-02T00:00:02Z",
		UpdatedAt:           "seed-value-overwritten",
		Note:                "a note",
	}
}

func TestSaveRunLoadRunRoundtrip(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	run, err := st.NewRun("p", "markdown", "v0")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}

	slot := fullSlot()
	if err := st.SetSlot(run, slot); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	// SetSlot stamps UpdatedAt in place; the in-memory slot is now the
	// canonical value we expect to read back.
	if slot.UpdatedAt == "seed-value-overwritten" || slot.UpdatedAt == "" {
		t.Fatalf("SetSlot did not stamp UpdatedAt: %q", slot.UpdatedAt)
	}

	got, err := st.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	gotSlot, ok := got.Slots[slot.PhaseID]
	if !ok {
		t.Fatalf("slot %q missing after reload", slot.PhaseID)
	}
	if !reflect.DeepEqual(slot, gotSlot) {
		t.Fatalf("slot roundtrip mismatch:\n in: %+v\nout: %+v", slot, gotSlot)
	}
}

func TestUpdateSlotStampsUpdatedAt(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, err := st.NewRun("p", "bd", "v")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	// Seed a slot with a stale UpdatedAt directly.
	run.Slots["a"] = &Slot{PhaseID: "a", Status: SlotQueued, UpdatedAt: "2000-01-01T00:00:00Z"}

	if err := st.UpdateSlot(run, "a", func(s *Slot) { s.Status = SlotRunning }); err != nil {
		t.Fatalf("UpdateSlot: %v", err)
	}

	got, err := st.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	sl := got.Slots["a"]
	if sl.Status != SlotRunning {
		t.Fatalf("status = %q, want %q", sl.Status, SlotRunning)
	}
	if sl.UpdatedAt == "2000-01-01T00:00:00Z" || sl.UpdatedAt == "" {
		t.Fatalf("UpdatedAt not stamped: %q", sl.UpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339, sl.UpdatedAt); err != nil {
		t.Fatalf("UpdatedAt not RFC3339: %v", err)
	}
}

// TestMutateSlotDefersWrite proves the batching primitive: MutateSlot changes
// the in-memory run but does NOT persist; the next SaveRun flushes it. This is
// what lets the poll tick coalesce N per-slot progress writes into one.
func TestMutateSlotDefersWrite(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, err := st.NewRun("p", "bd", "v")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if err := st.SetSlot(run, &Slot{PhaseID: "a", Status: SlotQueued}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	// Two in-memory mutations, no write.
	st.MutateSlot(run, "a", func(s *Slot) { s.Status = SlotRunning })
	st.MutateSlot(run, "a", func(s *Slot) { s.Commits = 3 })

	// On disk the slot is still queued with 0 commits.
	onDisk, err := st.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if d := onDisk.Slots["a"]; d.Status != SlotQueued || d.Commits != 0 {
		t.Fatalf("MutateSlot persisted early: status=%q commits=%d", d.Status, d.Commits)
	}

	// One SaveRun flushes both mutations.
	if err := st.SaveRun(run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	flushed, err := st.LoadRun(run.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if f := flushed.Slots["a"]; f.Status != SlotRunning || f.Commits != 3 {
		t.Fatalf("after SaveRun status=%q commits=%d, want running/3", f.Status, f.Commits)
	}
}

func TestUpdateSlotCreatesMissing(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, _ := st.NewRun("p", "bd", "v")

	if err := st.UpdateSlot(run, "new", func(s *Slot) { s.Status = SlotDispatching }); err != nil {
		t.Fatalf("UpdateSlot: %v", err)
	}
	got, _ := st.LoadRun(run.RunID)
	if sl, ok := got.Slots["new"]; !ok || sl.Status != SlotDispatching || sl.PhaseID != "new" {
		t.Fatalf("missing slot not created correctly: %+v ok=%v", got.Slots["new"], ok)
	}
}

func TestFinalizeRunEmptyMarksDone(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, err := st.NewRun("p", "bd", "v")
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	if run.Status != RunRunning {
		t.Fatalf("precondition: status = %q", run.Status)
	}

	if err := st.FinalizeRun(run); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}
	if run.Status != RunDone {
		t.Fatalf("empty run not finalized: %q", run.Status)
	}
	got, _ := st.LoadRun(run.RunID)
	if got.Status != RunDone {
		t.Fatalf("persisted status = %q, want %q", got.Status, RunDone)
	}
}

func TestFinalizeRunAllTerminalMarksDone(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, _ := st.NewRun("p", "bd", "v")
	run.Slots["a"] = &Slot{PhaseID: "a", Status: SlotMerged}
	run.Slots["b"] = &Slot{PhaseID: "b", Status: SlotFailed}

	if err := st.FinalizeRun(run); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}
	if run.Status != RunDone {
		t.Fatalf("status = %q, want %q", run.Status, RunDone)
	}
}

func TestFinalizeRunActiveStaysRunning(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, _ := st.NewRun("p", "bd", "v")
	run.Slots["a"] = &Slot{PhaseID: "a", Status: SlotMerged}
	run.Slots["b"] = &Slot{PhaseID: "b", Status: SlotRunning}

	if err := st.FinalizeRun(run); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}
	if run.Status != RunRunning {
		t.Fatalf("active run finalized prematurely: %q", run.Status)
	}
}

func TestFinalizeRunDrainedStaysDrained(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	run, _ := st.NewRun("p", "bd", "v")
	run.Status = RunDrained
	run.Slots["a"] = &Slot{PhaseID: "a", Status: SlotDone}

	if err := st.FinalizeRun(run); err != nil {
		t.Fatalf("FinalizeRun: %v", err)
	}
	if run.Status != RunDrained {
		t.Fatalf("status = %q, want %q", run.Status, RunDrained)
	}
}

func TestListRunsNewestFirst(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	root := paths.KoryphRoot(repo)

	// Fabricate three runs with known lexical/chronological order.
	ids := []string{"20260101-000000", "20260102-000000", "20260103-000000"}
	for _, id := range ids {
		if err := fsx.WriteJSONAtomic(filepath.Join(root, id, "ledger.json"), &Run{RunID: id}); err != nil {
			t.Fatalf("seed %s: %v", id, err)
		}
	}
	// Noise that must be ignored.
	_ = os.Symlink(ids[2], filepath.Join(root, "latest"))
	_ = os.WriteFile(filepath.Join(root, "koryph.lock"), []byte("1 host\n"), 0o644)

	got, err := st.ListRuns()
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	want := []string{"20260103-000000", "20260102-000000", "20260101-000000"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ListRuns = %v, want %v", got, want)
	}
}

func fullManifest() *Manifest {
	return &Manifest{
		ProjectID:       "p",
		BeadID:          "cn-9",
		EpicID:          "epic-1",
		AccountProfile:  "personal",
		ClaudeConfigDir: "/cfg",
		SessionID:       "sess",
		SessionName:     "name",
		Model:           "opus",
		ModelWhy:        "architecture",
		WorktreePath:    "/wt",
		Branch:          "b",
		BaseCommit:      "base111",
		HeadCommit:      "head222",
		Attempt:         2,
		ExecutionState:  "running",
		LeaseOwner:      "owner",
		LeaseExpiresAt:  "2026-07-02T01:00:00Z",
		Plan: PlanState{
			CurrentStep:      "s2",
			CompletedSteps:   []string{"s1"},
			InvalidatedSteps: []string{"s0"},
		},
		ChangedFiles:  []string{"a.go", "b.go"},
		PatchFiles:    []string{"p.patch"},
		WIPCommit:     "wip333",
		CommandsRun:   []string{"go build"},
		TestsRun:      []string{"go test"},
		LatestTest:    "pass",
		ReviewStatus:  "approved",
		OpenQuestions: []string{"q1"},
		NextAction:    "merge",
		QuotaSnapshot: "snap",
		PromptCache:   "aggressive",
		BatchAllowed:  true,
		RecoveryConf:  "high",
		RecoveryTier:  2,
		MergePolicy:   "auto",
		AutoMerge:     true,
		BillingMode:   "subscription",
		ProxyID:       "http://127.0.0.1:8091#v3",
		BootstrapCmds: []string{"nix develop"},
	}
}

func TestManifestRoundtrip(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	m := fullManifest()
	if err := st.SaveManifest("20260101-010101", "cn-9", m); err != nil {
		t.Fatalf("SaveManifest: %v", err)
	}
	if m.SchemaVersion != schemaVersion {
		t.Fatalf("SchemaVersion not stamped: %d", m.SchemaVersion)
	}
	if m.UpdatedAt == "" {
		t.Fatal("UpdatedAt not stamped")
	}

	got, err := st.LoadManifest("20260101-010101", "cn-9")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if !reflect.DeepEqual(m, got) {
		t.Fatalf("manifest roundtrip mismatch:\n in: %+v\nout: %+v", m, got)
	}
}

func TestRunLockSecondAcquireFails(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	l1, err := st.RunLock("run-1")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	// Second acquire, same live process → must fail (holder alive).
	if l2, err := st.RunLock("run-1"); err == nil {
		_ = l2.Unlock()
		t.Fatal("second acquire should fail while lock held by live process")
	}
	if err := l1.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// After unlock a fresh acquire should succeed.
	l3, err := st.RunLock("run-1")
	if err != nil {
		t.Fatalf("acquire after unlock: %v", err)
	}
	if err := l3.Unlock(); err != nil {
		t.Fatalf("final unlock: %v", err)
	}
}

func TestRunLockStaleRecovered(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	if err := os.MkdirAll(st.KoryphRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(st.KoryphRoot, "koryph.lock")
	// A PID far above any pid_max: kill(pid,0) → ESRCH → treated as dead.
	deadPID := 2000000000
	if processAlive(deadPID) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", deadPID)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d ghost-host\n", deadPID)), 0o644); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	l, err := st.RunLock("run-x")
	if err != nil {
		t.Fatalf("stale lock not recovered: %v", err)
	}
	defer l.Unlock()

	pid, ok := readLockPID(lockPath)
	if !ok || pid != os.Getpid() {
		t.Fatalf("lock not re-taken by this process: pid=%d ok=%v", pid, ok)
	}
}
