// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// refusingBackend satisfies dispatch.Backend without launching anything: the
// requeue decision (what these tests assert) happens before dispatch, and a
// dispatch failure downgrades the slot to blocked — never back to conflict.
type refusingBackend struct{}

func (refusingBackend) Dispatch(context.Context, dispatch.Spec) (dispatch.Handle, error) {
	return dispatch.Handle{}, errors.New("test backend refuses dispatch")
}

// A rebase conflict is changed code-defect evidence. It receives one
// standard-tier targeted repair, then parks visibly as blocked if unresolved.

func conflictSlot(t *testing.T, r *runner, id string, requeues int) *ledger.Slot {
	t.Helper()
	sl := &ledger.Slot{
		PhaseID:          id,
		Status:           ledger.SlotRunning,
		Attempts:         1,
		ConflictRequeues: requeues,
	}
	r.run.Slots[id] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return sl
}

func TestMergeConflictRequeuesWithinBudget(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	r.backend = refusingBackend{}
	sl := conflictSlot(t, r, "cb1", 0)

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if !requeued {
		t.Fatal("first rebase conflict must requeue, not park the slot")
	}
	got := r.run.Slots["cb1"]
	if got.Status == ledger.SlotConflict {
		t.Errorf("slot status = %q, want a non-terminal requeue state", got.Status)
	}
	if got.ConflictRequeues != 1 {
		t.Errorf("ConflictRequeues = %d, want 1", got.ConflictRequeues)
	}
	if got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("typed counters = code %d / merge %d, want 1 / 0",
			got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb1" && ss[1] == "open" {
			t.Error("bead must NOT be reset to open while the requeue budget remains")
		}
	}
}

// capturingBackend satisfies dispatch.Backend and SUCCEEDS, so a requeue's
// dispatchBead reaches SetSlot and REPLACES the ledger slot — the seam where
// koryph-qf6.1's counter loss lived. A refusingBackend never gets that far
// (blockSlot mutates the OLD slot in place), which is why the tests above
// could pass while every successful requeue was zeroing untracked counters.
type capturingBackend struct{ specs []dispatch.Spec }

func (b *capturingBackend) Dispatch(_ context.Context, spec dispatch.Spec) (dispatch.Handle, error) {
	b.specs = append(b.specs, spec)
	return dispatch.Handle{PID: 1, SessionID: spec.SessionID}, nil
}

func gateFailureFixture(t *testing.T, changedPath string) (*runner, *ledger.Slot, *capturingBackend, *finalizationLane) {
	t.Helper()
	r, sl, wt := candidateFixture(t)
	reg := registry.NewStore()
	rec, err := reg.Get("proj")
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	cfg, err := project.Load(rec.Root)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	worktreeRoot := rec.WorktreeRoot
	if worktreeRoot == "" {
		worktreeRoot = filepath.Join(filepath.Dir(rec.Root), filepath.Base(rec.Root)+"-worktrees")
	}
	canonicalWorktree := filepath.Join(worktreeRoot, strings.ReplaceAll(sl.Branch, "/", "-"))
	if err := os.MkdirAll(worktreeRoot, 0o755); err != nil {
		t.Fatalf("mkdir worktree root: %v", err)
	}
	runGit(t, rec.Root, "worktree", "move", wt, canonicalWorktree)
	wt = canonicalWorktree
	sl.Worktree = wt
	r.reg, r.rec, r.cfg = reg, rec, cfg
	r.opts = Options{ProjectID: "proj"}
	r.adapter = &fakeSource{}
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	backend := &capturingBackend{}
	r.backend = backend
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"
	writeFile(t, filepath.Join(wt, changedPath), "candidate change\n", 0o644)
	runGit(t, wt, "add", changedPath)
	runGit(t, wt, "commit", "--no-verify", "-m", "fix(candidate): scoped change")
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	lane := &finalizationLane{}
	lane.gates.ready = sync.NewCond(&lane.gates.mu)
	r.finalizer = lane
	return r, sl, backend, lane
}

// A broad gate failure is first confirmed by the engine without spending a
// model attempt. If the same candidate/base fails again and the evidence names
// a candidate-changed package, the one bounded repair receives the exact
// immutable failure.
func TestGateFailureConfirmsThenDispatchesScopedRepairWithExactEvidence(t *testing.T) {
	r, sl, backend, lane := gateFailureFixture(t, "internal/project/config.go")
	const gateOutput = "--- FAIL: TestConfigRoundTrip\ninternal/project/config.go:42: bad round trip\n"

	if requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusGateFailed, GateOutput: gateOutput,
	}); !requeued {
		t.Fatal("first gate failure must enter engine-owned confirmation")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("dispatches after first gate failure = %d, want 0", len(backend.specs))
	}
	if len(lane.gates.queue) != 1 {
		t.Fatalf("queued confirmations = %d, want 1", len(lane.gates.queue))
	}
	if sl.LastRevalidationKey == "" {
		t.Fatal("first gate failure did not persist its confirmation target")
	}

	if requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusGateFailed, GateOutput: gateOutput,
	}); !requeued {
		t.Fatal("confirmed candidate-scoped gate failure must dispatch its bounded repair")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	evidencePath := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "gate-failure-attempt-1.log")
	gotEvidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("read repair evidence: %v", err)
	}
	if string(gotEvidence) != gateOutput {
		t.Fatalf("repair evidence = %q, want exact gate output %q", gotEvidence, gateOutput)
	}
	for _, want := range []string{
		"### Required validation repair evidence", evidencePath,
		"Correct the reported root cause", "focused regression",
		"within this task contract", "report the scope mismatch",
	} {
		if !strings.Contains(backend.specs[0].Prompt, want) {
			t.Errorf("repair prompt missing %q:\n%s", want, backend.specs[0].Prompt)
		}
	}
}

func TestRepeatedUnrelatedGateFailureParksWithoutModelOrScopeExpansion(t *testing.T) {
	r, sl, backend, lane := gateFailureFixture(t, "internal/project/config.go")
	const gateOutput = "--- FAIL: TestRollingTimeout\nFAIL github.com/example/project/internal/engine\n"

	if !r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusGateFailed, GateOutput: gateOutput,
	}) {
		t.Fatal("first gate failure must enter engine-owned confirmation")
	}
	if len(lane.gates.queue) != 1 {
		t.Fatalf("queued confirmations = %d, want 1", len(lane.gates.queue))
	}
	if r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusGateFailed, GateOutput: gateOutput,
	}) {
		t.Fatal("repeated unrelated gate failure must park, not requeue")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("model dispatches = %d, want 0", len(backend.specs))
	}
	got := r.run.Slots[sl.PhaseID]
	if got.Status != ledger.SlotBlocked || got.OutcomeClass != string(OutcomeCodeDefect) {
		t.Fatalf("parked slot = status %q / outcome %q, want blocked / code-defect",
			got.Status, got.OutcomeClass)
	}
	if !strings.Contains(got.Note, "without naming a candidate-changed path") ||
		!strings.Contains(got.Note, "task scope was not expanded") {
		t.Fatalf("parked note does not explain containment: %q", got.Note)
	}
	evidencePath := filepath.Join(
		r.store.PhaseDir(r.run.RunID, sl.PhaseID),
		"gate-failure-attempt-1.log",
	)
	gotEvidence, err := os.ReadFile(evidencePath)
	if err != nil {
		t.Fatalf("read contained failure evidence: %v", err)
	}
	if string(gotEvidence) != gateOutput {
		t.Fatalf("contained evidence = %q, want %q", gotEvidence, gateOutput)
	}
}

func TestGateFailureScopeMatchingUsesPathBoundaries(t *testing.T) {
	changed := []string{"internal/project/config.go"}
	for _, tc := range []struct {
		name   string
		output string
		want   bool
	}{
		{name: "exact file", output: "internal/project/config.go:42: failed", want: true},
		{name: "package URL", output: "FAIL github.com/example/repo/internal/project", want: true},
		{name: "adjacent package is unrelated", output: "FAIL github.com/example/repo/internal/projectcache"},
		{name: "other package", output: "FAIL github.com/example/repo/internal/engine"},
		{name: "empty output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := gateFailureTouchesCandidate(tc.output, changed); got != tc.want {
				t.Fatalf("gateFailureTouchesCandidate(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// koryph-qf6.1 regression: dispatchBead builds a brand-new Slot on requeue,
// so every requeue path must thread ALL five budget counters into it. Before
// the fix ConflictRequeues was never threaded at all — its budget could never
// bind, because each successful requeue reset it to zero — and a conflict
// requeue dropped the spent rate-limit/budget-kill budgets, silently
// refilling them for causes it didn't own.
func TestConflictRequeuePreservesAllBudgetCounters(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	backend := &capturingBackend{}
	r.backend = backend

	sl := conflictSlot(t, r, "cb3", 0)
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"
	sl.GateRequeues, sl.MergeRequeues = 1, 2
	sl.RateLimitRequeues, sl.BudgetKillRequeues = 3, 1
	sl.BeadLabels, sl.SizeClass, sl.IssueType = []string{"area:sched"}, "M", "task"
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if !requeued {
		t.Fatal("conflict within budget must requeue")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1 (the requeue must reach the backend so SetSlot replaces the slot)", len(backend.specs))
	}

	got := r.run.Slots["cb3"]
	if got == sl {
		t.Fatal("slot was not replaced — this regression only manifests on the fresh-Slot path")
	}
	if got.ConflictRequeues != 1 {
		t.Errorf("ConflictRequeues = %d, want 1 (incremented AND threaded through the slot replacement)", got.ConflictRequeues)
	}
	if got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("typed counters = code %d / merge %d, want 1 / 0 across slot replacement",
			got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	if got.GateRequeues != 1 || got.MergeRequeues != 2 {
		t.Errorf("Gate/MergeRequeues = %d/%d, want 1/2 (preserved across a conflict requeue)", got.GateRequeues, got.MergeRequeues)
	}
	if got.RateLimitRequeues != 3 {
		t.Errorf("RateLimitRequeues = %d, want 3 (preserved across a conflict requeue)", got.RateLimitRequeues)
	}
	if got.BudgetKillRequeues != 1 {
		t.Errorf("BudgetKillRequeues = %d, want 1 (preserved across a conflict requeue)", got.BudgetKillRequeues)
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (a conflict requeue burns an attempt)", got.Attempts)
	}
	if len(got.BeadLabels) != 1 || got.BeadLabels[0] != "area:sched" ||
		got.SizeClass != "M" || got.IssueType != "task" {
		t.Errorf("features = %v/%q/%q, want [area:sched]/M/task frozen across the replacement (koryph-qf6.3)",
			got.BeadLabels, got.SizeClass, got.IssueType)
	}
}

func TestUnresolvedMergeConflictParksBlockedAfterStandardRepair(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	sl := conflictSlot(t, r, "cb2", 1)
	sl.Retry.CodeRepairs = 1
	sibling := &ledger.Slot{
		PhaseID: "sibling", BeadID: "sibling", Status: ledger.SlotRunning,
	}
	if err := r.store.SetSlot(r.run, sibling); err != nil {
		t.Fatalf("SetSlot sibling: %v", err)
	}
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	})
	if requeued {
		t.Fatal("exhausted budget must not requeue again")
	}
	if got := r.run.Slots["cb2"]; got.Status != ledger.SlotBlocked ||
		got.OutcomeClass != string(OutcomeCodeDefect) {
		t.Errorf("terminal slot = status %q / outcome %q, want blocked / code-defect",
			got.Status, got.OutcomeClass)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb2" && ss[1] == "open" {
			t.Errorf("unresolved code defect must park blocked, not reopen: %v", fake.setStatus)
		}
	}
	if r.dispatchCircuitReason != "" {
		t.Fatalf("exhausted candidate opened run circuit: %q", r.dispatchCircuitReason)
	}
	if got := r.run.Slots[sibling.PhaseID]; got == nil || got.Status != ledger.SlotRunning {
		t.Fatalf("sibling = %+v, want still running", got)
	}
}

func TestMergeConflictAllowsExactlyOneStandardRepairThenParks(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	fake := &fakeSource{}
	r.adapter = fake
	r.quotaCfg = &quota.Config{}
	r.rt = runtimetest.Stub{StubName: "claude"}
	backend := &capturingBackend{}
	r.backend = backend
	sl := conflictSlot(t, r, "cb4", 0)
	sl.Model, sl.Agent, sl.ModelWhy = "sonnet", "koryph-implementer", "test frozen"
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}

	if requeued := r.handleMergeFailure(t.Context(), sl, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	}); !requeued {
		t.Fatal("first unresolved conflict must receive its one typed code repair")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after first conflict = %d, want 1", len(backend.specs))
	}
	repaired := r.run.Slots["cb4"]
	if repaired.Retry.CodeRepairs != 1 || repaired.Retry.MergeRevalidations != 0 {
		t.Fatalf("typed counters after first conflict = code %d / merge %d, want 1 / 0",
			repaired.Retry.CodeRepairs, repaired.Retry.MergeRevalidations)
	}
	if backend.specs[0].Model != "sonnet" {
		t.Fatalf("conflict repair model = %q, want standard sonnet", backend.specs[0].Model)
	}

	if requeued := r.handleMergeFailure(t.Context(), repaired, merge.Result{
		Status: merge.StatusConflict, ConflictMD: "CONFLICT.md",
	}); requeued {
		t.Fatal("second unresolved conflict must park after the code-repair budget")
	}
	if len(backend.specs) != 1 {
		t.Fatalf("dispatches after exhausted conflict = %d, want 1", len(backend.specs))
	}
	if got := r.run.Slots["cb4"]; got.Status != ledger.SlotBlocked ||
		got.Retry.CodeRepairs != 1 || got.Retry.MergeRevalidations != 0 {
		t.Errorf("terminal slot = status %q / code %d / merge %d, want blocked / 1 / 0",
			got.Status, got.Retry.CodeRepairs, got.Retry.MergeRevalidations)
	}
	for _, ss := range fake.setStatus {
		if ss[0] == "cb4" && ss[1] == "open" {
			t.Errorf("terminal code defect reopened instead of parking: %v", fake.setStatus)
		}
	}
}

func TestMergeBaseMovedRecoveryNeverDispatchesModel(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.adapter = &fakeSource{}
	backend := &capturingBackend{}
	r.backend = backend
	sl := conflictSlot(t, r, "cb5", 0)

	if requeued := r.recoverTyped(t.Context(), sl, typedRecoveryRequest{
		outcome: OutcomeMergeBaseMoved,
		reason:  "future engine rebase",
	}); requeued {
		t.Fatal("engine-owned rebase/revalidate must not report a model dispatch")
	}
	if len(backend.specs) != 0 {
		t.Fatalf("model dispatches = %d, want 0", len(backend.specs))
	}
	if got := r.run.Slots["cb5"]; got.Status != ledger.SlotBlocked ||
		got.Retry.MergeRevalidations != 0 {
		t.Errorf("slot = status %q / revalidations %d, want blocked / 0 until engine handler exists",
			got.Status, got.Retry.MergeRevalidations)
	}
}
