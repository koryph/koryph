// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// TestFlagStageFollowUp proves D6's degrade breadcrumb: when a required stage is
// skipped after a timeout, the engine records a discoverable follow-up on the
// bead via the tracker's label + comment surface (no bead-create needed).
func TestFlagStageFollowUp(t *testing.T) {
	fake := &fakeSource{}
	r := &runner{adapter: fake, run: &ledger.Run{RunID: "run-1"}}

	r.flagStageFollowUp(context.Background(), "b1", "docs", 600)

	if len(fake.addLabels) != 1 || fake.addLabels[0] != [2]string{"b1", "followup:docs"} {
		t.Errorf("addLabels = %v, want one followup:docs label on b1", fake.addLabels)
	}
	if len(fake.comments) != 1 || fake.comments[0][0] != "b1" ||
		!strings.Contains(fake.comments[0][1], "docs") || !strings.Contains(fake.comments[0][1], "600") {
		t.Errorf("comments = %v, want one comment on b1 naming the stage and timeout", fake.comments)
	}
}

// --- rebaseWorktreeBestEffort git integration --------------------------------

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL="+filepath.Join(t.TempDir(), "noglobal"), "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// gitRepoWithBranch builds a repo on main with one commit and an agent/x branch
// worktree carrying its own commit, returning (repoRoot, worktreePath).
func gitRepoWithBranch(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	// Persist a committer identity in the repo's own config, the way every
	// real koryph-managed repo has one (signing setup writes user.email /
	// user.name into repo config). rebaseWorktreeBestEffort replays commits
	// via a plain `git rebase` that inherits the ambient environment, NOT the
	// GIT_AUTHOR_* env this helper injects into its own git invocations — so
	// without a config identity the replay fails with "no email was given and
	// auto-detection is disabled" on any host whose git can't synthesize one
	// (e.g. CI runners), even though the rebase is conflict-free. Setting it
	// here makes the test hermetic and faithful to production.
	git(t, repo, "config", "user.email", "t@t")
	git(t, repo, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "seed")

	wt := filepath.Join(t.TempDir(), "wt")
	git(t, repo, "worktree", "add", "-q", "-b", "agent/x", wt)
	if err := os.WriteFile(filepath.Join(wt, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "feature")
	return repo, wt
}

// TestRebaseWorktreeBestEffort_CleanRebase proves a stage worktree is rebased
// onto an advanced default branch, so the stage writes against the latest tree.
func TestRebaseWorktreeBestEffort_CleanRebase(t *testing.T) {
	repo, wt := gitRepoWithBranch(t)
	// main advances non-conflictingly after the branch forked.
	if err := os.WriteFile(filepath.Join(repo, "main-added.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "main advance")

	r := &runner{rec: &registry.Record{Root: repo, DefaultBranch: "main"}}
	if ok := r.rebaseWorktreeBestEffort(context.Background(), wt); !ok {
		t.Fatal("clean rebase must return true")
	}
	// The worktree now contains main's new file (rebased onto it).
	if _, err := os.Stat(filepath.Join(wt, "main-added.txt")); err != nil {
		t.Errorf("worktree not rebased onto advanced main: %v", err)
	}
}

// TestRebaseWorktreeBestEffort_ConflictAborts proves a conflicting rebase is
// aborted and the branch is left exactly as it was (the stage runs on it as-is).
func TestRebaseWorktreeBestEffort_ConflictAborts(t *testing.T) {
	repo, wt := gitRepoWithBranch(t)
	// Both edit the same new file divergently → the rebase conflicts.
	if err := os.WriteFile(filepath.Join(wt, "shared.txt"), []byte("branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, wt, "add", ".")
	git(t, wt, "commit", "-q", "-m", "branch shared")
	if err := os.WriteFile(filepath.Join(repo, "shared.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-q", "-m", "main shared")

	headBefore := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD"))
	r := &runner{rec: &registry.Record{Root: repo, DefaultBranch: "main"}}
	if ok := r.rebaseWorktreeBestEffort(context.Background(), wt); ok {
		t.Error("a conflicting rebase must return false")
	}
	if head := strings.TrimSpace(git(t, wt, "rev-parse", "HEAD")); head != headBefore {
		t.Errorf("branch head moved after an aborted rebase: %s != %s", head, headBefore)
	}
	if _, err := os.Stat(filepath.Join(wt, ".git", "rebase-merge")); err == nil {
		t.Error("rebase state left behind — abort did not clean up")
	}
}
