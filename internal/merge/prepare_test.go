// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergePrepare_RenumbersAndCommits(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	commitFiles(t, repo, "seed migrations", map[string]string{"migrations/0001_seed.sql": "-- seed\n"})
	wtPath := worktreeOn(t, repo, "agent/x").Path

	// Both sides add migration number 0002 with distinct filenames: no git
	// conflict (the rebase is clean), but a duplicate sequence number that a
	// migration tool would reject. This is exactly the collision a footprint
	// label prevents and merge_prepare heals when one slips through.
	commitFiles(t, repo, "main 0002", map[string]string{"migrations/0002_main.sql": "-- main\n"})
	commitFiles(t, wtPath, "branch 0002", map[string]string{"migrations/0002_x.sql": "-- x\n"})

	// merge_prepare renumbers the branch's colliding migration to the next slot.
	prep := "if [ -f migrations/0002_x.sql ] && [ -f migrations/0002_main.sql ]; then mv migrations/0002_x.sql migrations/0003_x.sql; fi"
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Prepare: []string{prep},
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if !res.Prepared {
		t.Error("Prepared=false, want true (renumber left a committed change)")
	}
	if _, err := os.Stat(filepath.Join(repo, "migrations", "0003_x.sql")); err != nil {
		t.Errorf("migrations/0003_x.sql not on main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "migrations", "0002_x.sql")); err == nil {
		t.Error("migrations/0002_x.sql still present; want renumbered away")
	}
	// koryph committed the normalization itself, with a conventional message.
	if subj := strings.TrimSpace(mustGit(t, repo, "log", "-1", "--format=%s")); subj != mergePrepareCommitMsg {
		t.Errorf("HEAD subject=%q, want %q", subj, mergePrepareCommitMsg)
	}
}

// TestMergePrepare_ExcludesConflictBreadcrumb proves D13: a stale CONFLICT.md
// left in a reused worktree is never swept into the merge-normalization commit
// (where a project markdownlint hook would reject it and fail the merge). The
// real normalization still lands.
func TestMergePrepare_ExcludesConflictBreadcrumb(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := worktreeOn(t, repo, "agent/x").Path
	commitFiles(t, wtPath, "work", map[string]string{"b.txt": "b\n"})

	// A prior attempt's breadcrumb lingers untracked in the reused worktree.
	if err := os.WriteFile(filepath.Join(wtPath, conflictBreadcrumb), []byte("# Merge conflict\n\n```\nx\n```\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A prepare step dirties the tree so koryph makes a normalization commit.
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Prepare: []string{"printf normalized > normalized.txt"},
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged", res.Status)
	}
	// The normalization landed on main...
	if _, err := os.Stat(filepath.Join(repo, "normalized.txt")); err != nil {
		t.Errorf("normalized.txt not on main: %v", err)
	}
	// ...but the breadcrumb did not (else it would be checked out in repo after
	// the ff-merge).
	if _, err := os.Stat(filepath.Join(repo, conflictBreadcrumb)); !os.IsNotExist(err) {
		t.Errorf("%s reached main; the normalization commit must exclude it (err=%v)", conflictBreadcrumb, err)
	}
}

func TestMergePrepare_ExposesDefaultBranch(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := worktreeOn(t, repo, "agent/x").Path
	commitFiles(t, wtPath, "work", map[string]string{"b.txt": "b\n"})

	// A renumber-to-tip command needs to know the branch it is landing on.
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Prepare: []string{`printf '%s' "$KORYPH_DEFAULT_BRANCH" > branch.txt`},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if got := readFile(t, filepath.Join(repo, "branch.txt")); got != "main" {
		t.Errorf("KORYPH_DEFAULT_BRANCH seen by command = %q, want main", got)
	}
}

func TestMergePrepare_NoOpWhenClean(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := worktreeOn(t, repo, "agent/x").Path
	commitFiles(t, wtPath, "work", map[string]string{"b.txt": "b\n"})
	tip := headOf(t, wtPath, "HEAD")

	// A prepare step that changes nothing must not add a commit (the common case).
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Prepare: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusMerged {
		t.Fatalf("Status=%q, want merged", res.Status)
	}
	if res.Prepared {
		t.Error("Prepared=true, want false (clean tree = no-op)")
	}
	if got := headOf(t, repo, "main"); got != tip {
		t.Errorf("main=%s, want branch tip %s (no extra commit on a clean prepare)", got, tip)
	}
}

func TestMergePrepare_CommandFailureRequeues(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wtPath := worktreeOn(t, repo, "agent/x").Path
	commitFiles(t, wtPath, "work", map[string]string{"b.txt": "b\n"})
	mainTip := headOf(t, repo, "main")

	// A failing prepare command is a gate-shaped failure — requeue, do not merge.
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "o", Slot: &fakeSlot{},
		Prepare: []string{"exit 1"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusGateFailed {
		t.Fatalf("Status=%q, want gate-failed (a failing prepare must not merge)", res.Status)
	}
	if got := headOf(t, repo, "main"); got != mainTip {
		t.Errorf("main advanced to %s past %s despite prepare failure", got, mainTip)
	}
}
