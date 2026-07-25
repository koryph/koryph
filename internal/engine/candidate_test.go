// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
	"golang.org/x/sys/unix"
)

func candidateFixture(t *testing.T) (*runner, *ledger.Slot, string) {
	t.Helper()
	f := newFixture(t, fixOpts{})
	wt := filepath.Join(f.wtRoot, "candidate")
	runGit(t, f.repo, "worktree", "add", "-b", "agent/candidate", wt, "main")
	store := ledger.NewStore(f.repo)
	run, err := store.NewRun("proj", "bd", "test")
	if err != nil {
		t.Fatal(err)
	}
	sl := &ledger.Slot{
		PhaseID: "candidate", BeadID: "candidate", Branch: "agent/candidate",
		Worktree: wt, StatusPath: filepath.Join(t.TempDir(), "status.json"),
		Status: ledger.SlotRunning, Attempts: 1, SessionID: "candidate-session",
	}
	base := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))
	dispatch := phasecontrol.DispatchContext{
		RunID: run.RunID, PhaseID: sl.PhaseID, Attempt: sl.Attempts,
		SessionID: sl.SessionID, BaseSHA: base,
	}
	sl.DispatchBaseSHA = base
	sl.DispatchGeneration = phasecontrol.DispatchGeneration(dispatch)
	if err := store.SetSlot(run, sl); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveManifest(run.RunID, sl.PhaseID, &ledger.Manifest{
		ProjectID: "proj", BeadID: sl.PhaseID, SessionID: sl.SessionID,
		WorktreePath: wt, Branch: sl.Branch, BaseCommit: base,
		DispatchGeneration: sl.DispatchGeneration, Attempt: sl.Attempts,
	}); err != nil {
		t.Fatal(err)
	}
	r := &runner{
		rec:   &registry.Record{Root: f.repo, DefaultBranch: "main"},
		run:   run,
		store: store,
		issues: map[string]beads.Issue{
			sl.PhaseID: {
				ID: sl.PhaseID, Title: "candidate",
				AcceptanceCriteria: "AC1: candidate work is committed",
			},
		},
	}
	return r, sl, wt
}

func completeCandidate(t *testing.T, r *runner, sl *ledger.Slot) phasecontrol.ResultManifest {
	t.Helper()
	manifest, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if err != nil {
		t.Fatal(err)
	}
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	summary := filepath.Join(phaseDir, "SUMMARY.md")
	writeFile(t, summary, "completed\n", 0o644)
	logPath := filepath.Join(phaseDir, "focused.log")
	writeFile(t, logPath, "ok\n", 0o644)
	evidencePath := filepath.Join(phaseDir, "evidence.json")
	writeFile(t, evidencePath,
		`{"focused_tests":[{"command":"go test ./internal/example","exit_status":0,"log_path":"`+logPath+
			`"}],"acceptance":[{"criterion_id":"AC1","references":[{"kind":"focused-test","command":"go test ./internal/example"}]}]}`,
		0o644,
	)
	result, err := phasecontrol.Complete(context.Background(), phasecontrol.CompleteOptions{
		PhaseDir: phaseDir, Worktree: sl.Worktree, SummaryPath: summary, EvidencePath: evidencePath,
		Dispatch: phasecontrol.DispatchContext{
			RunID: r.run.RunID, PhaseID: sl.PhaseID, Attempt: manifest.Attempt,
			SessionID: manifest.SessionID, BaseSHA: manifest.BaseCommit,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCandidateEligibleRejectsBlockedZeroCommitRuntimeNeutrally(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"blocked"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ok, reason := r.candidateEligible(context.Background(), sl)
	if ok {
		t.Fatal("candidateEligible = true, want false")
	}
	for _, want := range []string{"agent reported completion state blocked", "no commits beyond"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q missing %q", reason, want)
		}
	}
	if sl.LastCommit != "" {
		t.Errorf("LastCommit=%q, want empty for unchanged base", sl.LastCommit)
	}
}

func TestCandidateEligibleRejectsDirtyCommittedCandidate(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "committed.txt"), "committed\n", 0o644)
	runGit(t, wt, "add", "committed.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): committed")
	writeFile(t, filepath.Join(wt, "staged.txt"), "staged\n", 0o644)
	runGit(t, wt, "add", "staged.txt")

	ok, reason := r.candidateEligible(context.Background(), sl)
	if ok {
		t.Fatal("candidateEligible = true, want false")
	}
	if !strings.Contains(reason, "staged, unstaged, or untracked") {
		t.Errorf("reason=%q", reason)
	}
	if sl.Commits != 1 || sl.LastCommit == "" {
		t.Errorf("progress commits=%d last=%q, want 1/non-empty", sl.Commits, sl.LastCommit)
	}
}

func TestCandidateEligibleAcceptsOnlyMatchingTerminalResult(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"blocked","runtime_specific_field":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	result := completeCandidate(t, r, sl)

	ok, reason := r.candidateEligible(context.Background(), sl)
	if !ok {
		t.Fatalf("candidateEligible = false: %s", reason)
	}
	if sl.CandidateGeneration != result.Generation || sl.CandidateResultPath == "" {
		t.Fatalf("typed lifecycle was not stamped on slot: %+v", sl)
	}
}

func TestCompleteSlotCurrentResultOutranksStaleHeartbeatMarker(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	completeCandidate(t, r, sl)
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"testing"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	sl.DeathReason = deathReasonStaleHeartbeat
	if err := r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.DeathReason = deathReasonStaleHeartbeat
	}); err != nil {
		t.Fatal(err)
	}
	r.cfg = &project.Config{MergePolicy: project.PolicyManual}
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()

	r.completeSlot(t.Context(), sl)

	if sl.Status != ledger.SlotMergePending {
		t.Fatalf("status = %q, want merge-pending from valid current result", sl.Status)
	}
	if sl.DeathReason != "" {
		t.Fatalf("stale death marker was not cleared: %q", sl.DeathReason)
	}
	if sl.Retry.TransientRetries != 0 || sl.Attempts != 1 {
		t.Fatalf("valid result was requeued: attempts=%d retry=%+v", sl.Attempts, sl.Retry)
	}
}

func TestCompleteSlotCurrentResultOutranksAutomaticRecoveryMarkers(t *testing.T) {
	tests := []struct {
		name  string
		stamp func(*testing.T, *ledger.Slot)
	}{
		{
			name: "budget exhausted",
			stamp: func(t *testing.T, sl *ledger.Slot) {
				t.Helper()
				sl.Stream = filepath.Join(t.TempDir(), "stream.jsonl")
				writeFile(t, sl.Stream,
					`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"errors":["Reached maximum budget"]}`+"\n",
					0o644,
				)
			},
		},
		{
			name: "turn exhausted",
			stamp: func(t *testing.T, sl *ledger.Slot) {
				t.Helper()
				sl.DeathReason = deathReasonTurnExhausted
			},
		},
		{
			name: "rate limited",
			stamp: func(t *testing.T, sl *ledger.Slot) {
				t.Helper()
				sl.Stream = filepath.Join(t.TempDir(), "stream.jsonl")
				writeFile(t, sl.Stream,
					`{"type":"error","message":"rate_limit_error: 429 during final unwind"}`+"\n",
					0o644,
				)
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, sl, wt := candidateFixture(t)
			writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
			runGit(t, wt, "add", "work.txt")
			runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
			completeCandidate(t, r, sl)
			tc.stamp(t, sl)
			if err := r.store.SetSlot(r.run, sl); err != nil {
				t.Fatal(err)
			}
			r.cfg = &project.Config{MergePolicy: project.PolicyManual}
			r.adapter = &fakeSource{}
			r.reg = registry.NewStore()

			r.completeSlot(t.Context(), sl)

			if sl.Status != ledger.SlotMergePending {
				t.Fatalf("status = %q, want merge-pending from valid current result", sl.Status)
			}
			if sl.Retry.BudgetContinuations != 0 || sl.Retry.TurnContinuations != 0 ||
				sl.Retry.TransientRetries != 0 || sl.BudgetKillRequeues != 0 ||
				sl.TurnExhaustedRequeues != 0 || sl.RateLimitRequeues != 0 {
				t.Fatalf("valid result entered resource retry: retry=%+v mirrors budget/turn/rate=%d/%d/%d",
					sl.Retry, sl.BudgetKillRequeues, sl.TurnExhaustedRequeues, sl.RateLimitRequeues)
			}
		})
	}
}

func TestAssessCandidateRepairsOnlyMissingCompletionContractWithinBudget(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	writeFile(t, filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "SUMMARY.md"), "done\n", 0o644)
	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || !a.retryableBlock || a.outcome != OutcomeCompletionContractMissing {
		t.Fatalf("assessment = %+v, want one completion repair", a)
	}

	sl.Attempts = ledger.MaxAttempts
	manifest, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Attempt = sl.Attempts
	sl.DispatchGeneration = phasecontrol.DispatchGeneration(phasecontrol.DispatchContext{
		RunID: r.run.RunID, PhaseID: sl.PhaseID, Attempt: sl.Attempts,
		SessionID: sl.SessionID, BaseSHA: sl.DispatchBaseSHA,
	})
	manifest.DispatchGeneration = sl.DispatchGeneration
	if err := r.store.SaveManifest(r.run.RunID, sl.PhaseID, manifest); err != nil {
		t.Fatal(err)
	}
	a = r.assessCandidate(context.Background(), sl)
	if !a.retryableBlock {
		t.Fatalf("assessment at max attempts = %+v, completion budget must be independent", a)
	}

	sl.Attempts = 2
	manifest.Attempt = sl.Attempts
	sl.DispatchGeneration = phasecontrol.DispatchGeneration(phasecontrol.DispatchContext{
		RunID: r.run.RunID, PhaseID: sl.PhaseID, Attempt: sl.Attempts,
		SessionID: sl.SessionID, BaseSHA: sl.DispatchBaseSHA,
	})
	manifest.DispatchGeneration = sl.DispatchGeneration
	if err := r.store.SaveManifest(r.run.RunID, sl.PhaseID, manifest); err != nil {
		t.Fatal(err)
	}
	sl.Retry.CompletionRepairs = 1
	a = r.assessCandidate(context.Background(), sl)
	if a.retryableBlock {
		t.Fatalf("assessment after one completion repair = %+v, want terminal", a)
	}
}

func TestHeartbeatAndSummaryCannotEnterFinalization(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"done"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "SUMMARY.md"), "advisory only\n", 0o644)

	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || a.outcome != OutcomeCompletionContractMissing {
		t.Fatalf("advisory artifacts entered finalization: %+v", a)
	}
}

func TestCandidateAssessmentDoesNotBlockOnFIFOStatus(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	if err := unix.Mkfifo(sl.StatusPath, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan candidateAssessment, 1)
	go func() { done <- r.assessCandidate(context.Background(), sl) }()
	select {
	case a := <-done:
		if a.eligible || a.retryableBlock || !strings.Contains(a.reason, "bounded regular file") {
			t.Fatalf("FIFO status assessment = %+v", a)
		}
	case <-time.After(time.Second):
		t.Fatal("candidate assessment blocked reading FIFO status")
	}
}

func TestCandidateRejectsResultAfterCandidateSHAChanges(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "first.txt"), "first\n", 0o644)
	runGit(t, wt, "add", "first.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): first")
	completeCandidate(t, r, sl)
	writeFile(t, filepath.Join(wt, "second.txt"), "second\n", 0o644)
	runGit(t, wt, "add", "second.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): second")

	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || a.retryableBlock || !strings.Contains(a.reason, "candidate SHA") {
		t.Fatalf("stale result assessment = %+v", a)
	}
}

func TestCandidateRequiresSlotOwnedDispatchIdentity(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	completeCandidate(t, r, sl)

	generation := sl.DispatchGeneration
	sl.DispatchGeneration = ""
	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || !strings.Contains(a.reason, "slot is missing trusted dispatch identity") {
		t.Fatalf("empty slot generation assessment = %+v", a)
	}

	sl.DispatchGeneration = generation
	manifest, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if err != nil {
		t.Fatal(err)
	}
	manifest.DispatchGeneration = "stale"
	if err := r.store.SaveManifest(r.run.RunID, sl.PhaseID, manifest); err != nil {
		t.Fatal(err)
	}
	a = r.assessCandidate(context.Background(), sl)
	if a.eligible || !strings.Contains(a.reason, "generation does not match slot") {
		t.Fatalf("mismatched manifest generation assessment = %+v", a)
	}
}

func TestCandidateRejectsSymlinkedDispatchManifest(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	phaseDir := r.store.PhaseDir(r.run.RunID, sl.PhaseID)
	manifestPath := filepath.Join(phaseDir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	realPath := filepath.Join(phaseDir, "worker-manifest.json")
	if err := os.WriteFile(realPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realPath, manifestPath); err != nil {
		t.Fatal(err)
	}

	a := r.assessCandidate(t.Context(), sl)
	if a.eligible || !strings.Contains(a.reason, "dispatch manifest is missing or unreadable") {
		t.Fatalf("symlinked manifest assessment = %+v", a)
	}
}

func TestCandidateRequiresExactIssueAcceptanceMatrix(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	completeCandidate(t, r, sl)
	issue := r.issues[sl.PhaseID]
	issue.AcceptanceCriteria = "AC1: candidate work is committed\nAC2: second outcome is verified"
	r.issues[sl.PhaseID] = issue

	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || !strings.Contains(a.reason, "want 2") {
		t.Fatalf("incomplete acceptance matrix assessment = %+v", a)
	}
}

func TestAssessCandidateExplicitFailureStatesNeverReceiveCompletionRepair(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")

	for _, state := range []string{"blocked", "failed", "error", "cancelled", "canceled"} {
		t.Run(state, func(t *testing.T) {
			if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"`+state+`"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			a := r.assessCandidate(context.Background(), sl)
			if a.retryableBlock {
				t.Fatalf("failure state %q received completion repair: %+v", state, a)
			}
		})
	}
}

// TestMissingCompletionContractGetsOnlyOneRepair proves that a clean,
// committed candidate missing result.json receives one same-tier dispatch and
// then parks without reaching frontier implementation.
func TestMissingCompletionContractGetsOnlyOneRepair(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "preserved\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): preserve work")
	writeFile(t, filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "SUMMARY.md"), "done\n", 0o644)
	// candidateFixture keeps its narrow assessment fixture at <root>/candidate,
	// while dispatches use the production branch-derived name. Re-home this
	// test worktree so the correction retry exercises the real dispatch path.
	r.rec.WorktreeRoot = filepath.Dir(wt)
	runGit(t, r.rec.Root, "worktree", "remove", "--force", wt)
	reattached, err := worktree.Ensure(context.Background(), worktree.EnsureOpts{
		RepoRoot: r.rec.Root, WorktreeRoot: r.rec.WorktreeRoot,
		Branch: sl.Branch, Base: r.rec.DefaultBranch,
	})
	if err != nil {
		t.Fatal(err)
	}
	sl.Worktree = reattached.Path
	manifest, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	if err != nil {
		t.Fatal(err)
	}
	manifest.WorktreePath = reattached.Path
	if err := r.store.SaveManifest(r.run.RunID, sl.PhaseID, manifest); err != nil {
		t.Fatal(err)
	}

	backend := &capturingBackend{}
	r.adapter = &fakeSource{}
	r.backend = backend
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	r.quotaCfg = &quota.Config{}
	r.issues = map[string]beads.Issue{sl.PhaseID: {ID: sl.PhaseID, Title: "candidate"}}
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"

	r.finishCandidate(context.Background(), sl)
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after missing result = %d, want 1 completion repair", len(backend.specs))
	}
	next := r.run.Slots[sl.PhaseID]
	if next == nil || next.Attempts != 2 || next.Model != "sonnet" || next.Retry.CompletionRepairs != 1 {
		t.Fatalf("completion repair slot = %+v, want attempt 2 on frozen sonnet with spent repair", next)
	}
	if !strings.Contains(backend.specs[0].Prompt, "COMPLETION REPAIR ONLY") ||
		!strings.Contains(backend.specs[0].Prompt, "Do not edit source files") {
		t.Fatalf("completion repair prompt was not narrowly scoped:\n%s", backend.specs[0].Prompt)
	}
	r.finishCandidate(context.Background(), next)
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after repeated generic block = %d, want no third dispatch", len(backend.specs))
	}
	if next.Status != ledger.SlotBlocked {
		t.Errorf("repeated generic block status = %s, want blocked", next.Status)
	}
}

func TestAssessCandidateCapabilityBlockNeverRetries(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "preserved\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): preserve work")
	if err := os.WriteFile(sl.StatusPath, []byte(
		`{"state":"blocked","block_kind":"capability","capability":"runtime-canary","detail":"profile unavailable"}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}

	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || a.retryableBlock || !a.capabilityBlock {
		t.Fatalf("assessment = %+v, want terminal capability block", a)
	}
	if a.capability != "runtime-canary" || a.capabilityDetail != "profile unavailable" {
		t.Fatalf("assessment detail = %+v", a)
	}
}

func TestFinishCandidateCapabilityBlockParksSlotWithoutRunHandoff(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "preserved\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): preserve work")
	if err := os.WriteFile(sl.StatusPath, []byte(
		`{"state":"blocked","block_kind":"capability","capability":"beads-metadata","detail":"dependency mutation required"}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}
	sl.Model = "sonnet"
	beforeAttempts := sl.Attempts
	source := &fakeSource{}
	r.adapter = source
	r.cfg = &project.Config{}
	r.issues = map[string]beads.Issue{sl.PhaseID: {ID: sl.PhaseID, Title: "candidate"}}
	r.opts.ProjectID = "demo"

	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	log = obs.For("engine")
	defer func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(io.Discard, nil))
		log = obs.For("engine")
	}()

	r.finishCandidate(context.Background(), sl)
	if sl.Status != ledger.SlotBlocked || sl.Attempts != beforeAttempts || sl.Model != "sonnet" {
		t.Fatalf("slot = %+v", sl)
	}
	if !fakeBlocked(source, sl.PhaseID) {
		t.Fatalf("tracker was not reconciled to blocked: %+v", source.setStatus)
	}
	hold, ok, err := r.store.LoadCapabilityHold(sl.PhaseID)
	if err != nil || !ok {
		t.Fatalf("capability hold = %+v, %v, %v", hold, ok, err)
	}
	if hold.Capability != "beads-metadata" || len(hold.EvidenceHash) != 64 ||
		hold.RetryLimit != 1 {
		t.Fatalf("capability hold = %+v", hold)
	}
	var wakeEvent bool
	for _, rec := range capH.recs {
		if rec.Message == "engine.slot.capability_blocked" && rec.Level == slog.LevelError {
			wakeEvent = true
		}
		if rec.Message == "engine.slot.escalated" || rec.Message == "engine.slot.requeued" {
			t.Fatalf("capability block emitted retry/escalation event: %s", rec.Message)
		}
	}
	if !wakeEvent {
		t.Fatalf("missing ERROR capability wake event: %+v", capH.recs)
	}
	if r.activeCount() != 0 {
		t.Fatalf("blocked slot remained active: %+v", r.activeIDs())
	}
}

func TestMalformedStructuredBlockDoesNotEscalate(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "preserved\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): preserve work")
	if err := os.WriteFile(sl.StatusPath, []byte(
		`{"state":"blocked","block_kind":"capability","capability":"INVALID VALUE"}`,
	), 0o644); err != nil {
		t.Fatal(err)
	}
	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || a.retryableBlock || a.capabilityBlock {
		t.Fatalf("assessment = %+v, want terminal malformed block", a)
	}
}
