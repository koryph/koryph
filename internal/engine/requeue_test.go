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
