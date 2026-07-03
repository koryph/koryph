// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/worktree"
)

// TestOrphanWorktreeKept pins the safety decision for koryph-47n's fresh-run
// orphan reconciliation: never discard uncommitted or landed work, but reclaim
// a clean, commitless orphan worktree so its bead can be reopened.
func TestOrphanWorktreeKept(t *testing.T) {
	ctx := context.Background()

	t.Run("clean commitless worktree is reclaimed", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		wtPath := ensureWorktreeAt(t, f, "tb1")

		sl := &ledger.Slot{PhaseID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: wtPath}
		kept, _ := r.orphanWorktreeKept(ctx, sl)
		if kept {
			t.Fatal("a clean, commitless orphan worktree should be reclaimed, not kept")
		}
		if _, err := os.Stat(wtPath); !os.IsNotExist(err) {
			t.Errorf("worktree not removed: stat err = %v", err)
		}
		if branchExists(f.repo, worktree.BranchFor("tb1")) {
			t.Error("branch survived removal of a commitless orphan worktree")
		}
	})

	t.Run("worktree with landed commits is preserved", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		wtPath := ensureWorktreeAt(t, f, "tb1")
		writeFile(t, filepath.Join(wtPath, "agent.txt"), "work\n", 0o644)
		runGit(t, wtPath, "add", "agent.txt")
		runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: agent work")

		sl := &ledger.Slot{PhaseID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: wtPath}
		kept, why := r.orphanWorktreeKept(ctx, sl)
		if !kept {
			t.Fatal("a branch with landed commits must be preserved for --resume")
		}
		if why == "" {
			t.Error("expected a non-empty preserve reason")
		}
		if _, err := os.Stat(wtPath); err != nil {
			t.Errorf("worktree carrying work was removed: %v", err)
		}
	})

	t.Run("dirty worktree is preserved", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		wtPath := ensureWorktreeAt(t, f, "tb1")
		writeFile(t, filepath.Join(wtPath, "wip.txt"), "half-done\n", 0o644) // uncommitted WIP

		sl := &ledger.Slot{PhaseID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: wtPath}
		kept, why := r.orphanWorktreeKept(ctx, sl)
		if !kept {
			t.Fatal("a dirty worktree must be preserved (never auto-delete WIP)")
		}
		if why != "dirty worktree" {
			t.Errorf("reason = %q, want %q", why, "dirty worktree")
		}
		if _, err := os.Stat(wtPath); err != nil {
			t.Errorf("dirty worktree was removed: %v", err)
		}
	})

	t.Run("missing worktree is safe to reopen", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		sl := &ledger.Slot{PhaseID: "tb1", Branch: worktree.BranchFor("tb1"), Worktree: filepath.Join(f.wtRoot, "nope")}
		if kept, _ := r.orphanWorktreeKept(ctx, sl); kept {
			t.Fatal("a missing worktree should be reopenable, not kept")
		}
	})
}
