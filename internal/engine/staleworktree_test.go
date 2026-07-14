// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestStaleWorktreePatrolSkipsLiveSlot proves the stale-worktrees patrol
// (koryph-050) never flags the worktree of a running agent that has not yet made
// its first commit. Such an agent has a dirty tree AND a HEAD still at the merged
// main tip — the exact orphan signature — so without the live-slot exclusion the
// patrol would recommend `git worktree remove --force` against in-progress work,
// and the more (and longer) work the agent has done pre-first-commit, the wider
// that window.
func TestStaleWorktreePatrolSkipsLiveSlot(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// t.TempDir hands out a /var/folders path while `git worktree list` reports
	// the canonical /private/var one; align rec.Root with git's view so the main
	// checkout is skipped by the exact-path guard (production roots are already
	// canonical). The linked-worktree exclusion under test relies on basename
	// matching, so it needs no such alignment.
	if real, err := filepath.EvalSymlinks(f.repo); err == nil {
		r.rec.Root = real
	}

	// A linked worktree detached at the main tip (HEAD is an ancestor of main →
	// "merged") with a dirty tree (an untracked file) — a pre-first-commit agent.
	wtPath := filepath.Join(f.wtRoot, "agent-live")
	runGit(t, f.repo, "worktree", "add", "--detach", wtPath, "HEAD")
	if err := os.WriteFile(filepath.Join(wtPath, "in-progress.txt"),
		[]byte("332 files of work, not yet committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Owned by a RUNNING slot → excluded → not flagged.
	r.run.Slots["live"] = &ledger.Slot{
		PhaseID: "live", BeadID: "live", Status: ledger.SlotRunning, Worktree: wtPath,
	}
	if got := staleWorktreeMsg(t, r, ctx); !strings.Contains(got, "stale-worktrees: none") {
		t.Errorf("live-slot worktree flagged as an orphan:\n%s", got)
	}

	// Same worktree with NO owning live slot → the orphan signature fires,
	// proving the live-slot exclusion (not some unrelated filter) is what
	// protected it above.
	delete(r.run.Slots, "live")
	if got := staleWorktreeMsg(t, r, ctx); !strings.Contains(got, "already-merged HEAD") ||
		!strings.Contains(got, "agent-live") {
		t.Errorf("orphan worktree not flagged once its slot is gone:\n%s", got)
	}

	// A TERMINAL slot must not protect it either — the agent has finished, so a
	// leftover dirty tree is a real orphan again.
	r.run.Slots["live"] = &ledger.Slot{
		PhaseID: "live", BeadID: "live", Status: ledger.SlotMerged, Worktree: wtPath,
	}
	if got := staleWorktreeMsg(t, r, ctx); !strings.Contains(got, "agent-live") {
		t.Errorf("terminal-slot worktree should still be flaggable:\n%s", got)
	}
}

// staleWorktreeMsg runs the patrol and returns its single finding's message.
func staleWorktreeMsg(t *testing.T, r *runner, ctx context.Context) string {
	t.Helper()
	fs := r.patrolCheckStaleWorktrees(ctx)
	if len(fs) != 1 {
		t.Fatalf("patrolCheckStaleWorktrees returned %d findings, want 1", len(fs))
	}
	return fs[0].message
}
