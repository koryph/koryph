// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/schemaver"
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
	if got.TokenSemantics != CurrentTokenSemantics {
		t.Fatalf("token_semantics = %q, want %q", got.TokenSemantics, CurrentTokenSemantics)
	}
	if want := schemaver.Current(schemaver.LedgerRun); got.SchemaVersion != want {
		t.Fatalf("schema_version = %d, want %d", got.SchemaVersion, want)
	}
}

func TestNewRunSameSecondAllocationsAreUniqueAndOrdered(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	fixed := time.Date(2026, 7, 25, 15, 30, 45, 0, time.UTC)
	st.now = func() time.Time { return fixed }

	const count = 128
	runs := make(chan *Run, count)
	errs := make(chan error, count)
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			run, err := st.NewRun("proj-x", "bd", "v1")
			runs <- run
			errs <- err
		}()
	}
	wait.Wait()
	close(runs)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	seen := make(map[string]bool, count)
	var newest string
	for run := range runs {
		if run == nil {
			t.Fatal("NewRun returned nil without an error")
		}
		if seen[run.RunID] {
			t.Fatalf("duplicate RunID %q", run.RunID)
		}
		seen[run.RunID] = true
		if run.RunID > newest {
			newest = run.RunID
		}
		loaded, err := st.LoadRun(run.RunID)
		if err != nil {
			t.Fatalf("LoadRun(%s): %v", run.RunID, err)
		}
		if loaded.RunID != run.RunID || loaded.ProjectID != "proj-x" {
			t.Fatalf("hybrid ledger for %s: %+v", run.RunID, loaded)
		}
	}
	if len(seen) != count {
		t.Fatalf("unique runs = %d, want %d", len(seen), count)
	}
	ids, err := st.ListRuns()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != count || ids[0] != newest {
		t.Fatalf("ListRuns = %d entries, newest %q; want %d/%q", len(ids), ids[0], count, newest)
	}
	target, err := os.Readlink(filepath.Join(st.KoryphRoot, latestLink))
	if err != nil {
		t.Fatal(err)
	}
	if target != newest {
		t.Fatalf("latest = %q, want fully persisted newest %q", target, newest)
	}
}

func TestNewRunFailedLedgerWriteDoesNotAdvanceLatest(t *testing.T) {
	st := NewStore(t.TempDir())
	fixed := time.Date(2026, 7, 25, 15, 30, 45, 0, time.UTC)
	st.now = func() time.Time { return fixed }
	first, err := st.NewRun("proj-x", "bd", "v1")
	if err != nil {
		t.Fatal(err)
	}
	st.saveRun = func(*Run) error { return errors.New("injected ledger failure") }
	if _, err := st.NewRun("proj-x", "bd", "v1"); err == nil {
		t.Fatal("failed ledger write unexpectedly allocated a run")
	}
	target, err := os.Readlink(filepath.Join(st.KoryphRoot, latestLink))
	if err != nil {
		t.Fatal(err)
	}
	if target != first.RunID {
		t.Fatalf("latest advanced to %q after failed allocation; want %q", target, first.RunID)
	}
	if _, err := os.Stat(filepath.Join(st.KoryphRoot, fixed.Format(runIDLayout)+"-000001")); !os.IsNotExist(err) {
		t.Fatalf("failed allocation directory survived: %v", err)
	}
}

func fullSlot() *Slot {
	return &Slot{
		PhaseID:                      "cn-42",
		BeadID:                       "cn-42",
		EpicID:                       "epic-1",
		Branch:                       "koryph/cn-42",
		Worktree:                     "/wt/cn-42",
		SessionID:                    "sess-abc",
		SessionName:                  "amber-otter",
		Agent:                        "implementer",
		Model:                        "sonnet",
		ModelWhy:                     "cost/latency",
		Effort:                       "high",
		AccountProfile:               "personal",
		ClaudeConfigDir:              "/cfg/personal",
		VerifiedIdentity:             "owner@example.com",
		VerifiedAt:                   "2026-07-02T00:00:00Z",
		BillingMode:                  "subscription",
		ProxyID:                      "http://127.0.0.1:8091#v3",
		PID:                          12345,
		Stream:                       "stream-1",
		StatusPath:                   "/s/status.json",
		LogPath:                      "/s/log.txt",
		Status:                       SlotRunning,
		Attempts:                     1,
		Commits:                      2,
		LastCommit:                   "abc1234",
		ResumeSHA:                    "def5678",
		CostUSD:                      1.25,
		InputTokens:                  10000,
		OutputTokens:                 500,
		CacheReadTokens:              8000,
		CacheCreationTokens:          1200,
		ProviderTotalInputTokens:     18000,
		HasProviderTotalInput:        true,
		ReviewIters:                  1,
		LastRevalidationKey:          "candidate@base",
		FinalizationStage:            "security-review",
		FinalizationQueuedAt:         "2026-07-02T00:00:01.500Z",
		GateEvidencePath:             "/s/gate-evidence-generation.json",
		GateEvidenceDigest:           "sha256:gate",
		GeneralReviewArtifactPath:    "/s/review-general.json",
		GeneralReviewArtifactDigest:  "sha256:general",
		GeneralReviewCandidateSHA:    "candidate-general",
		GeneralReviewBaseSHA:         "base-general",
		SecurityReviewArtifactPath:   "/s/review-security.json",
		SecurityReviewArtifactDigest: "sha256:security",
		SecurityReviewCandidateSHA:   "candidate-security",
		SecurityReviewBaseSHA:        "base-security",
		DispatchedAt:                 "2026-07-02T00:00:01Z",
		MergedAt:                     "2026-07-02T00:00:02Z",
		UpdatedAt:                    "seed-value-overwritten",
		Note:                         "a note",
	}
}

func TestEvidenceDirIsPrivateAndOutsideWorkerPhase(t *testing.T) {
	store := NewStore(t.TempDir())
	phase := store.PhaseDir("run", "bead")
	evidence := store.EvidenceDir("run", "bead")
	if strings.HasPrefix(evidence, phase+string(filepath.Separator)) || evidence == phase {
		t.Fatalf("evidence directory %q is inside worker phase %q", evidence, phase)
	}
	info, err := os.Stat(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		t.Fatalf("evidence directory permissions = %o, want private", info.Mode().Perm())
	}
}

func TestLoadRunLabelsLegacyTokenSemantics(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	runID := "20260725-120000"
	dir := filepath.Join(st.KoryphRoot, runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacy := &Run{
		SchemaVersion: schemaver.Current(schemaver.LedgerRun),
		RunID:         runID,
		ProjectID:     "p",
		Slots:         map[string]*Slot{},
	}
	if err := fsx.WriteJSONAtomic(filepath.Join(dir, ledgerFile), legacy); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadRun(runID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	if got.TokenSemantics != TokenSemanticsLegacyV0 {
		t.Fatalf("TokenSemantics = %q, want explicit legacy label %q",
			got.TokenSemantics, TokenSemanticsLegacyV0)
	}
	if err := st.SaveRun(got); err != nil {
		t.Fatalf("SaveRun labeled legacy: %v", err)
	}
	var persisted Run
	if err := fsx.ReadJSON(filepath.Join(dir, ledgerFile), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.TokenSemantics != TokenSemanticsLegacyV0 {
		t.Fatalf("persisted TokenSemantics = %q, want %q",
			persisted.TokenSemantics, TokenSemanticsLegacyV0)
	}
}

func TestSetSlotArchivesAttemptsAndDistinguishesModelRelaunch(t *testing.T) {
	store := NewStore(t.TempDir())
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	run, err := store.NewRun("demo", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	first := &Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 1,
		SessionID:          "session-1",
		DispatchGeneration: "generation-1",
		DispatchedAt:       now.Format(time.RFC3339Nano),
		Status:             SlotRunning,
		InputTokens:        10,
	}
	if err := store.SetSlot(run, first); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := store.UpdateSlot(run, "b1", func(sl *Slot) {
		sl.FinalizationQueuedAt = now.Add(-100 * time.Millisecond).Format(time.RFC3339Nano)
		sl.FinalizationStage = "gate"
		sl.Status = SlotMerging
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if err := store.UpdateSlot(run, "b1", func(sl *Slot) {
		sl.FinalizationStage = "review"
		sl.Status = SlotReview
	}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	secondDispatch := now.Format(time.RFC3339Nano)
	if err := store.SetSlot(run, &Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2,
		SessionID:          "session-2",
		DispatchGeneration: "generation-2", DispatchedAt: secondDispatch,
		Status: SlotRunning, InputTokens: 25,
	}); err != nil {
		t.Fatal(err)
	}
	if len(run.AttemptHistory) != 1 {
		t.Fatalf("attempt history = %+v", run.AttemptHistory)
	}
	archived := run.AttemptHistory[0].Slot
	if archived.Attempts != 1 || archived.FinalizationTimings.Gate.CompletedAt == "" ||
		archived.FinalizationTimings.Review.CompletedAt == "" {
		t.Fatalf("archived attempt = %+v", archived)
	}

	loaded, err := store.LoadRun(run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if err := store.SetSlot(loaded, &Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2,
		SessionID:          "session-2",
		DispatchGeneration: "generation-2",
		DispatchedAt:       now.Format(time.RFC3339Nano),
		Status:             SlotRunning,
		InputTokens:        25,
	}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.AttemptHistory) != 1 {
		t.Fatalf("same-attempt resume duplicated history: %+v", loaded.AttemptHistory)
	}
	if got := loaded.Slots["b1"].DispatchedAt; got != secondDispatch {
		t.Fatalf("idempotent persistence dispatch = %s, want original %s", got, secondDispatch)
	}

	now = now.Add(time.Second)
	relaunchDispatch := now.Format(time.RFC3339Nano)
	if err := store.SetSlot(loaded, &Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2,
		SessionID:          "session-3",
		DispatchGeneration: "generation-2-relaunch",
		DispatchedAt:       relaunchDispatch,
		Status:             SlotRunning,
		InputTokens:        25,
	}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.AttemptHistory) != 2 ||
		loaded.AttemptHistory[1].Slot.DispatchGeneration != "generation-2" {
		t.Fatalf("same-number relaunch was not archived: %+v", loaded.AttemptHistory)
	}
	if got := loaded.Slots["b1"].DispatchedAt; got != relaunchDispatch {
		t.Fatalf("relaunch dispatch = %s, want %s", got, relaunchDispatch)
	}

	now = now.Add(time.Second)
	if err := store.SetSlot(loaded, &Slot{
		PhaseID: "b1", BeadID: "b1", Attempts: 2,
		SessionID:          "session-4",
		DispatchGeneration: "generation-2-relaunch",
		DispatchedAt:       now.Format(time.RFC3339Nano),
		Status:             SlotRunning,
		InputTokens:        25,
	}); err != nil {
		t.Fatal(err)
	}
	if len(loaded.AttemptHistory) != 3 ||
		loaded.AttemptHistory[2].Slot.SessionID != "session-3" {
		t.Fatalf("same-generation/new-session relaunch was lost: %+v", loaded.AttemptHistory)
	}
}

func TestSetSlotRejectsAttemptRegressionAndGap(t *testing.T) {
	store := NewStore(t.TempDir())
	run, err := store.NewRun("demo", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(run, &Slot{PhaseID: "b1", Attempts: 2}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(run, &Slot{PhaseID: "b1", Attempts: 1}); err == nil {
		t.Fatal("attempt regression was accepted")
	}
	if err := store.SetSlot(run, &Slot{PhaseID: "b1", Attempts: 4}); err == nil {
		t.Fatal("attempt gap was accepted")
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
	if m.SchemaVersion != schemaver.Current(schemaver.LedgerManifest) {
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

func TestRunAdmissionWaitsForGuardedMaintenance(t *testing.T) {
	st := NewStore(t.TempDir())
	entered := make(chan struct{})
	release := make(chan struct{})
	maintenanceDone := make(chan error, 1)
	go func() {
		_, err := st.WithRunAdmissionGuard(func() error {
			close(entered)
			<-release
			return nil
		})
		maintenanceDone <- err
	}()
	<-entered

	lockResult := make(chan error, 1)
	go func() {
		lock, err := st.RunLock("run-after-maintenance")
		if err == nil {
			err = lock.Unlock()
		}
		lockResult <- err
	}()
	select {
	case err := <-lockResult:
		t.Fatalf("RunLock bypassed maintenance guard: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-maintenanceDone; err != nil {
		t.Fatalf("maintenance: %v", err)
	}
	if err := <-lockResult; err != nil {
		t.Fatalf("RunLock after maintenance: %v", err)
	}
}

func TestLockHolderNoLockFile(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	if pid, alive, ok := st.LockHolder(); ok {
		t.Errorf("LockHolder with no lock file: pid=%d alive=%v ok=%v, want ok=false", pid, alive, ok)
	}
}

func TestLockHolderLiveAndStale(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	l, err := st.RunLock("run-1")
	if err != nil {
		t.Fatalf("RunLock: %v", err)
	}
	defer l.Unlock()

	pid, alive, ok := st.LockHolder()
	if !ok || !alive || pid != os.Getpid() {
		t.Errorf("LockHolder live = pid=%d alive=%v ok=%v, want pid=%d alive=true ok=true", pid, alive, ok, os.Getpid())
	}

	// LockHolder is read-only: it must not have removed or altered the lock
	// file a second peek sees the same state.
	if pid2, alive2, ok2 := st.LockHolder(); pid2 != pid || alive2 != alive || ok2 != ok {
		t.Errorf("LockHolder not idempotent: first (%d,%v,%v) second (%d,%v,%v)", pid, alive, ok, pid2, alive2, ok2)
	}

	if err := l.Unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, _, ok := st.LockHolder(); ok {
		t.Error("LockHolder after Unlock: ok = true, want false (lock file removed)")
	}
}

func TestLockHolderDeadPID(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	if err := os.MkdirAll(st.KoryphRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	deadPID := 2000000000
	if processAlive(deadPID) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", deadPID)
	}
	lockPath := filepath.Join(st.KoryphRoot, "koryph.lock")
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d ghost-host\n", deadPID)), 0o644); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	pid, alive, ok := st.LockHolder()
	if !ok || alive || pid != deadPID {
		t.Errorf("LockHolder dead = pid=%d alive=%v ok=%v, want pid=%d alive=false ok=true", pid, alive, ok, deadPID)
	}
	// A dead-pid peek must not have reclaimed/removed the lock file — that is
	// RunLock's job, not LockHolder's.
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("lock file removed by a read-only LockHolder peek: %v", err)
	}
}

// TestLockPID covers the injectable-probe pid reader (koryph-oixo): it returns
// the recorded pid without any liveness probe, so the caller supplies its own.
func TestLockPID(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)

	// No lock file yet → ok=false.
	if pid, ok := st.LockPID(); ok {
		t.Errorf("LockPID with no lock file: pid=%d ok=%v, want ok=false", pid, ok)
	}

	if err := os.MkdirAll(st.KoryphRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(st.KoryphRoot, "koryph.lock")
	const want = 2000000000
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d ghost-host\n", want)), 0o644); err != nil {
		t.Fatalf("seed lock: %v", err)
	}
	// Returns the pid regardless of whether it is alive (no probe) and leaves
	// the file untouched.
	if pid, ok := st.LockPID(); !ok || pid != want {
		t.Errorf("LockPID = pid=%d ok=%v, want pid=%d ok=true", pid, ok, want)
	}
	if _, err := os.Stat(lockPath); err != nil {
		t.Errorf("LockPID removed the lock file: %v", err)
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

// TestRunLockStaleReclaimIsSingleWinner exercises the TOCTOU the reclaim guard
// closes: many goroutines racing to reclaim the SAME stale lock must produce
// exactly one holder, never two. Before the flock guard, concurrent reclaimers
// could each os.Remove the stale file and the second could delete the first's
// freshly re-acquired lock, leaving two "singleton" holders.
func TestRunLockStaleReclaimIsSingleWinner(t *testing.T) {
	repo := t.TempDir()
	st := NewStore(repo)
	if err := os.MkdirAll(st.KoryphRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	lockPath := filepath.Join(st.KoryphRoot, "koryph.lock")
	deadPID := 2000000000
	if processAlive(deadPID) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", deadPID)
	}
	if err := os.WriteFile(lockPath, []byte(fmt.Sprintf("%d ghost-host\n", deadPID)), 0o644); err != nil {
		t.Fatalf("seed stale lock: %v", err)
	}

	const racers = 24
	var wg sync.WaitGroup
	var mu sync.Mutex
	var winners []*Lock
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if l, err := st.RunLock("run-x"); err == nil {
				mu.Lock()
				winners = append(winners, l)
				mu.Unlock()
			}
		}()
	}
	close(start)
	wg.Wait()

	if len(winners) != 1 {
		t.Fatalf("got %d lock winners, want exactly 1 (TOCTOU: two holders of a process-singleton lock)", len(winners))
	}
	winners[0].Unlock()
}
