// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/worktree"
)

// runnerFromFixture assembles a minimal *runner over the fixture's registry and
// repo — enough to exercise the requeue/worktree-refresh path without driving a
// full Run().
func runnerFromFixture(t *testing.T, f *fix) *runner {
	t.Helper()
	reg := registry.NewStore()
	rec, err := reg.Get("proj")
	if err != nil {
		t.Fatalf("registry.Get: %v", err)
	}
	cfg, err := project.Load(rec.Root)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	store := ledger.NewStore(rec.Root)
	run, err := store.NewRun("proj", cfg.WorkSource, EngineVersion)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return &runner{
		opts:   Options{ProjectID: "proj", Out: &bytes.Buffer{}},
		reg:    reg,
		rec:    rec,
		cfg:    cfg,
		store:  store,
		run:    run,
		issues: map[string]beads.Issue{},
	}
}

// ensureWorktreeAt creates (or attaches) the agent worktree for beadID rooted at
// the fixture and returns its path.
func ensureWorktreeAt(t *testing.T, f *fix, beadID string) string {
	t.Helper()
	wt, err := worktree.Ensure(context.Background(), worktree.EnsureOpts{
		RepoRoot:     f.repo,
		WorktreeRoot: f.wtRoot,
		Branch:       worktree.BranchFor(beadID),
		Base:         "main",
	})
	if err != nil {
		t.Fatalf("worktree.Ensure: %v", err)
	}
	return wt.Path
}

// TestRequeueFreezesModel proves koryph-ehx: a requeue reuses the model,
// persona, effort, and rationale resolved on the FIRST attempt, so a `model:*`
// relabel mid-run cannot silently switch a retry to the wrong model.
// resolveModel is the single seam every dispatch — fresh and requeue — funnels
// through, so exercising it here covers all three poll.go requeue paths.
func TestRequeueFreezesModel(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	// The bead is currently labeled model:sonnet — as if the operator relabeled
	// it after the first attempt already went out on opus.
	relabeled := beads.Issue{ID: "tb1", Labels: []string{"model:sonnet"}}

	// A FRESH dispatch honors the live label (this is what a requeue used to do,
	// and why retries drifted to the wrong model).
	fresh, _, err := r.resolveModel(dispatchReq{issue: relabeled}, "claude")
	if err != nil {
		t.Fatalf("fresh resolveModel: %v", err)
	}
	if fresh.Model != "sonnet" {
		t.Fatalf("fresh model = %q, want sonnet (live label wins on a fresh dispatch)", fresh.Model)
	}

	// A REQUEUE carrying attempt 1's frozen resolution ignores the relabel and
	// re-runs opus, with the persona/effort/rationale carried forward verbatim.
	frozen, effort, err := r.resolveModel(dispatchReq{
		issue:          relabeled,
		attempt:        2,
		frozenModel:    "opus",
		frozenPersona:  "koryph-implementer",
		frozenModelWhy: "frozen from attempt 1",
		frozenEffort:   "high",
	}, "claude")
	if err != nil {
		t.Fatalf("frozen resolveModel: %v", err)
	}
	if frozen.Model != "opus" {
		t.Errorf("requeue model = %q, want opus (frozen from attempt 1, not the mid-run relabel)", frozen.Model)
	}
	if frozen.Persona != "koryph-implementer" {
		t.Errorf("requeue persona = %q, want koryph-implementer (frozen)", frozen.Persona)
	}
	if frozen.Rationale != "frozen from attempt 1" {
		t.Errorf("requeue rationale = %q, want the frozen rationale", frozen.Rationale)
	}
	if effort != "high" {
		t.Errorf("requeue effort = %q, want high (frozen)", effort)
	}
}

// TestRequeueFreezesBeadFeatures proves koryph-qf6.3: the similarity feature
// vector (labels, size bucket, issue type) is snapshotted from the live issue
// on a FIRST dispatch and carried verbatim from the persisted slot on every
// requeue — so a relabel mid-run cannot rewrite what a live slot is understood
// to look like, and the outcome learner joins outcomes to the features the
// bead was ROUTED with. featuresFor/featuresFromSlot are the single seams
// dispatchBead and all three poll.go requeue paths funnel through.
func TestRequeueFreezesBeadFeatures(t *testing.T) {
	live := beads.Issue{
		ID:          "fb1",
		Labels:      []string{"area:sched", "model:opus"},
		IssueType:   "bug",
		Description: strings.Repeat("x", 10),
	}

	// Fresh dispatch: snapshot the live issue.
	fresh := featuresFor(dispatchReq{issue: live})
	if len(fresh.labels) != 2 || fresh.labels[0] != "area:sched" {
		t.Errorf("fresh labels = %v, want the live issue's labels", fresh.labels)
	}
	if fresh.issueType != "bug" {
		t.Errorf("fresh issueType = %q, want bug", fresh.issueType)
	}
	if fresh.sizeClass != quota.SizeOf(10) {
		t.Errorf("fresh sizeClass = %q, want %q", fresh.sizeClass, quota.SizeOf(10))
	}

	// Requeue: the persisted slot's frozen vector wins over the (relabeled)
	// live issue.
	slotted := &ledger.Slot{
		BeadLabels: []string{"area:engine"},
		SizeClass:  "L",
		IssueType:  "task",
	}
	frozen := featuresFor(dispatchReq{issue: live, features: featuresFromSlot(slotted)})
	if len(frozen.labels) != 1 || frozen.labels[0] != "area:engine" {
		t.Errorf("frozen labels = %v, want the slot's [area:engine], not the live relabel", frozen.labels)
	}
	if frozen.sizeClass != "L" || frozen.issueType != "task" {
		t.Errorf("frozen size/type = %q/%q, want L/task (carried verbatim)", frozen.sizeClass, frozen.issueType)
	}

	// A slot that predates the fields yields nil — dispatchBead then derives
	// from the live issue, exactly like the resource fallback.
	if featuresFromSlot(&ledger.Slot{}) != nil {
		t.Error("featuresFromSlot on a legacy slot = non-nil, want nil (derive from live issue)")
	}
}

// escalationRunner assembles a runner whose dispatches succeed (capturing
// backend), so requeue paths run all the way through dispatchBead's slot
// replacement — the seam the koryph-qf6.4 escalation tests must observe.
func escalationRunner(t *testing.T, f *fix) (*runner, *capturingBackend) {
	t.Helper()
	r := runnerFromFixture(t, f)
	r.adapter = &fakeSource{}
	r.quotaCfg = &quota.Config{}
	backend := &capturingBackend{}
	r.backend = backend
	return r, backend
}

func escalationSlot(t *testing.T, r *runner, id string, attempts int) *ledger.Slot {
	t.Helper()
	sl := &ledger.Slot{
		PhaseID:  id,
		Status:   ledger.SlotRunning,
		Attempts: attempts,
		Model:    "sonnet",
		Agent:    "koryph-implementer",
		ModelWhy: "stage default (implement)",
	}
	r.run.Slots[id] = sl
	if err := r.store.SaveRun(r.run); err != nil {
		t.Fatalf("SaveRun: %v", err)
	}
	return sl
}

// TestFinalAttemptEscalatesModel proves koryph-qf6.4: a bead-fault requeue
// about to burn the FINAL MaxAttempts attempt on sonnet runs it on opus
// instead, with a rationale that records the escalation (the TUI's ↑ marker
// and the learner's training signal both key on it). Persona and effort stay
// frozen — only the tier changes.
func TestFinalAttemptEscalatesModel(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r, backend := escalationRunner(t, f)
	sl := escalationSlot(t, r, "esc1", ledger.MaxAttempts-1)

	r.requeueSlot(t.Context(), sl, "", "agent died with no commits")

	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	got := r.run.Slots["esc1"]
	if got.Model != "opus" {
		t.Errorf("final-attempt model = %q, want opus (escalated)", got.Model)
	}
	if !strings.Contains(got.ModelWhy, "escalated from sonnet") {
		t.Errorf("ModelWhy = %q, want an 'escalated from sonnet' rationale", got.ModelWhy)
	}
	if got.Agent != "koryph-implementer" {
		t.Errorf("persona = %q, want koryph-implementer (frozen — only the tier escalates)", got.Agent)
	}
	if got.Attempts != ledger.MaxAttempts {
		t.Errorf("Attempts = %d, want %d", got.Attempts, ledger.MaxAttempts)
	}
	if backend.specs[0].Model != "opus" {
		t.Errorf("dispatched spec model = %q, want opus", backend.specs[0].Model)
	}
}

// TestNonFinalAttemptStaysFrozen proves the koryph-ehx freeze still governs
// every retry BELOW the final attempt: no escalation on attempt 2 of 3.
func TestNonFinalAttemptStaysFrozen(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r, backend := escalationRunner(t, f)
	sl := escalationSlot(t, r, "esc2", 1)

	r.requeueSlot(t.Context(), sl, "", "agent died with no commits")

	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	got := r.run.Slots["esc2"]
	if got.Model != "sonnet" || strings.Contains(got.ModelWhy, "escalat") {
		t.Errorf("attempt-2 model/why = %q/%q, want frozen sonnet with no escalation", got.Model, got.ModelWhy)
	}
}

// TestEscalationSkipsMergeErrors proves a transient merge-error requeue never
// escalates even on the final attempt: the base moved or a push raced — not a
// model-capability failure worth an opus attempt.
func TestEscalationSkipsMergeErrors(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r, backend := escalationRunner(t, f)
	sl := escalationSlot(t, r, "esc3", ledger.MaxAttempts-1)

	r.requeueSlot(t.Context(), sl, "", mergeErrorRequeueNote)

	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	if got := r.run.Slots["esc3"]; got.Model != "sonnet" {
		t.Errorf("merge-error final attempt model = %q, want sonnet (no escalation)", got.Model)
	}
}

// TestEscalationRespectsAllowlist proves the allowlist gate the frozen-model
// path otherwise bypasses: a project that never allowed opus keeps its final
// attempt on the frozen tier.
func TestEscalationRespectsAllowlist(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r, backend := escalationRunner(t, f)
	r.rec.AllowedModels = []string{"haiku", "sonnet"}
	sl := escalationSlot(t, r, "esc4", ledger.MaxAttempts-1)

	r.requeueSlot(t.Context(), sl, "", "agent died with no commits")

	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	if got := r.run.Slots["esc4"]; got.Model != "sonnet" {
		t.Errorf("allowlist-denied model = %q, want sonnet", got.Model)
	}
}

// TestRateLimitRequeueNeverEscalates proves the environmental path is exempt:
// a rate-limited death re-runs the same model regardless of attempt count.
func TestRateLimitRequeueNeverEscalates(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r, backend := escalationRunner(t, f)
	sl := escalationSlot(t, r, "esc5", ledger.MaxAttempts-1)

	r.requeueRateLimited(t.Context(), sl)

	if len(backend.specs) != 1 {
		t.Fatalf("dispatches = %d, want 1", len(backend.specs))
	}
	got := r.run.Slots["esc5"]
	if got.Model != "sonnet" || strings.Contains(got.ModelWhy, "escalat") {
		t.Errorf("rate-limited requeue model/why = %q/%q, want frozen sonnet", got.Model, got.ModelWhy)
	}
	if got.Attempts != ledger.MaxAttempts-1 {
		t.Errorf("Attempts = %d, want %d (environmental failure burns no attempt)", got.Attempts, ledger.MaxAttempts-1)
	}
}

// TestRequeueRefreshRebasesWorktreeWithCommits proves the koryph-137 resume
// path: a worktree carrying landed commits is rebased onto an advanced main
// before re-dispatch, so the agent resumes on top of the main-side fix while
// keeping its own work.
func TestRequeueRefreshRebasesWorktreeWithCommits(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	wtPath := ensureWorktreeAt(t, f, "tb1")

	// Agent lands one commit on its branch.
	writeFile(t, filepath.Join(wtPath, "agent.txt"), "agent work\n", 0o644)
	runGit(t, wtPath, "add", "agent.txt")
	runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: agent work")

	// Main advances with a fix the stale checkout must pick up.
	writeFile(t, filepath.Join(f.repo, "settings.txt"), "fixed\n", 0o644)
	runGit(t, f.repo, "add", "settings.txt")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: main-side fix")

	sl := &ledger.Slot{PhaseID: "tb1", BeadID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: wtPath, Commits: 1}
	r.refreshWorktreeForRequeue(ctx, sl, false)

	// The worktree now carries BOTH the main-side fix and the agent's own work.
	if _, err := os.Stat(filepath.Join(wtPath, "settings.txt")); err != nil {
		t.Errorf("worktree missing main-side fix after requeue refresh: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wtPath, "agent.txt")); err != nil {
		t.Errorf("worktree lost the agent's landed work after requeue refresh: %v", err)
	}
	// And the branch is on top of advanced main (zero behind now).
	if n, err := r.commitCount(ctx, sl.Branch); err != nil || n != 1 {
		t.Errorf("branch commit count = %d (err %v), want 1 ahead of advanced main", n, err)
	}
}

// TestRequeueRebuildsStaleWorktreeWithoutCommits proves the koryph-137 fresh
// path: a no-commit worktree (even with dirty WIP) is torn down and its branch
// dropped, so the subsequent Ensure rebuilds a clean checkout from current main
// instead of reattaching the pre-fix tree.
func TestRequeueRebuildsStaleWorktreeWithoutCommits(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	wtPath := ensureWorktreeAt(t, f, "tb1")
	// Leave uncommitted WIP behind (the agent died mid-edit, no commits).
	writeFile(t, filepath.Join(wtPath, "wip.txt"), "half-done\n", 0o644)

	// Main advances with a fix.
	writeFile(t, filepath.Join(f.repo, "settings.txt"), "fixed\n", 0o644)
	runGit(t, f.repo, "add", "settings.txt")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: main-side fix")

	sl := &ledger.Slot{PhaseID: "tb1", BeadID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: wtPath, Commits: 0}
	r.refreshWorktreeForRequeue(ctx, sl, false)

	// The stale worktree and branch are gone...
	if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
		t.Errorf("stale worktree not rebuilt: stat err = %v", err)
	}
	if branchExists(f.repo, worktree.BranchFor("tb1")) {
		t.Errorf("stale branch %s survived a no-commit requeue", worktree.BranchFor("tb1"))
	}

	// A WIP snapshot was preserved for forensics.
	entries, _ := filepath.Glob(filepath.Join(r.store.PhaseDir(r.run.RunID, "tb1"), "wip-*.patch"))
	if len(entries) == 0 {
		t.Error("expected a WIP snapshot patch to be captured before rebuild")
	}

	// ...and dispatch's Ensure now builds a clean tree carrying the fix.
	fresh := ensureWorktreeAt(t, f, "tb1")
	if _, err := os.Stat(filepath.Join(fresh, "settings.txt")); err != nil {
		t.Errorf("rebuilt worktree missing main-side fix: %v", err)
	}
	if _, err := os.Stat(filepath.Join(fresh, "wip.txt")); !os.IsNotExist(err) {
		t.Errorf("rebuilt worktree should not carry stale WIP: stat err = %v", err)
	}
}

// TestRequeueNoCommitsMissingBranchIsClean proves the koryph-pln fix: when the
// operator has already deleted the agent's branch before the engine's
// refreshWorktreeForRequeue branch-reset step runs, the absent branch IS the
// clean state — the step must proceed silently rather than emitting a
// "dispatch may attach the old tip" warning.
func TestRequeueNoCommitsMissingBranchIsClean(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// Create a worktree for the bead, then remove both the worktree AND its
	// branch — simulating what happens when the operator manually cleans up
	// after killing the agent. The worktree path is gone and the branch doesn't
	// exist either.
	wtPath := ensureWorktreeAt(t, f, "tb1")
	branch := worktree.BranchFor("tb1")

	// Remove the worktree and branch so the slot has no worktree and no branch.
	runGit(t, f.repo, "worktree", "remove", "--force", wtPath)
	runGit(t, f.repo, "branch", "-D", branch)

	// Capture progress output so we can assert the warning is NOT emitted.
	var buf bytes.Buffer
	r.opts.Out = &buf

	sl := &ledger.Slot{PhaseID: "tb1", BeadID: "tb1", Branch: branch, Worktree: wtPath, Commits: 0}
	// Must not panic and must not warn about "dispatch may attach the old tip".
	r.refreshWorktreeForRequeue(ctx, sl, false)

	if strings.Contains(buf.String(), "dispatch may attach the old tip") {
		t.Errorf("unexpected warning for already-absent branch: %q", buf.String())
	}
}
