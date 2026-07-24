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

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
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
		Status: ledger.SlotRunning, Attempts: 1,
	}
	if err := store.SetSlot(run, sl); err != nil {
		t.Fatal(err)
	}
	r := &runner{
		rec:   &registry.Record{Root: f.repo, DefaultBranch: "main"},
		run:   run,
		store: store,
	}
	return r, sl, wt
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

func TestCandidateEligibleAcceptsCleanCommittedClaudeOrCodexOutput(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"done","runtime_specific_field":true}`), 0o644); err != nil {
		t.Fatal(err)
	}

	ok, reason := r.candidateEligible(context.Background(), sl)
	if !ok {
		t.Fatalf("candidateEligible = false: %s", reason)
	}
}

func TestAssessCandidateRetriesOnlyCleanCommittedSelfBlocksWithinBudget(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "done\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): work")
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"blocked"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	a := r.assessCandidate(context.Background(), sl)
	if a.eligible || !a.retryableBlock {
		t.Fatalf("assessment = %+v, want non-eligible retryable self-block", a)
	}

	sl.Attempts = ledger.MaxAttempts
	a = r.assessCandidate(context.Background(), sl)
	if a.retryableBlock {
		t.Fatalf("assessment at max attempts = %+v, want terminal", a)
	}

	sl.Attempts = 2
	a = r.assessCandidate(context.Background(), sl)
	if a.retryableBlock {
		t.Fatalf("assessment after one correction retry = %+v, want terminal", a)
	}
}

// TestGenericCompletionBlockGetsOnlyOneCorrectionRetry reproduces the host
// block sequence: attempt 1 emits an old generic state=blocked heartbeat,
// attempt 2 repeats it, and the engine parks rather than dispatching an
// unhelpful third worker or escalating to the frontier tier.
func TestGenericCompletionBlockGetsOnlyOneCorrectionRetry(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, filepath.Join(wt, "work.txt"), "preserved\n", 0o644)
	runGit(t, wt, "add", "work.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): preserve work")
	if err := os.WriteFile(sl.StatusPath, []byte(`{"state":"blocked"}`), 0o644); err != nil {
		t.Fatal(err)
	}
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
		t.Fatalf("dispatches after first generic block = %d, want 1 correction retry", len(backend.specs))
	}
	next := r.run.Slots[sl.PhaseID]
	if next == nil || next.Attempts != 2 || next.Model != "sonnet" {
		t.Fatalf("correction slot = %+v, want attempt 2 on frozen sonnet", next)
	}
	next.StatusPath = filepath.Join(t.TempDir(), "second-status.json")
	if err := os.WriteFile(next.StatusPath, []byte(`{"state":"blocked"}`), 0o644); err != nil {
		t.Fatal(err)
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

func TestFinishCandidateCapabilityBlockWakesWithoutAttemptOrModelChange(t *testing.T) {
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
	if !r.capabilityBlocked || r.capabilityBlockBead != sl.PhaseID {
		t.Fatalf("handoff state = blocked:%v bead:%q", r.capabilityBlocked, r.capabilityBlockBead)
	}
	if !fakeBlocked(source, sl.PhaseID) {
		t.Fatalf("tracker was not reconciled to blocked: %+v", source.setStatus)
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
	outcome, err := r.capabilityHandoff()
	if err != nil || outcome.Code != ExitFatal || !strings.Contains(outcome.Reason, sl.PhaseID) {
		t.Fatalf("handoff outcome=%+v err=%v", outcome, err)
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
