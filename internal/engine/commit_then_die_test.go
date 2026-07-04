// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

// Regression tests for koryph-ek2: committed-then-died attempts were
// misclassified as "no commits" when the progress probe had not yet updated
// sl.Commits at the time of death detection.
//
// Two scenarios are covered:
//  1. Agent commits then crashes (exit 1, no result line, no SUMMARY.md).
//     The poll fires on SIGCHLD before any timer tick, so sl.Commits == 0.
//     With the fix the commitCount fallback detects the work and routes to
//     finishCandidate → merge rather than burning an attempt on a requeue.
//  2. Resumed agent exits cleanly (exit 0, result line) without new commits.
//     The new slot starts at Commits == 0; sl.Commits is never updated before
//     the clean exit fires another SIGCHLD.  The same fallback detects the
//     prior-attempt commits on the branch and routes to finishCandidate.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// commitThenDieClaudeScript commits one file and then exits 1 WITHOUT writing
// SUMMARY.md or a result JSON line — simulating a process that was killed
// mid-flight after committing its work but before completing the handshake.
// The script runs fast enough that the SIGCHLD fires before any poll timer
// tick, guaranteeing sl.Commits == 0 at the time completeSlot is called.
const commitThenDieClaudeScript = `#!/bin/sh
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
exit 1
`

// cleanExitNoNewCommitsClaudeScript simulates a resumed agent that checks the
// worktree, concludes the work is already done, and exits cleanly (exit 0,
// non-error result line) without making any new commits.  The new slot starts
// at Commits == 0; the prior-attempt commits sit on the branch.
const cleanExitNoNewCommitsClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"result","total_cost_usd":0.05,"is_error":false}\n'
exit 0
`

// TestCommitThenCrashProceedsToMerge is fixture 1 for koryph-ek2: an agent
// that commits then crashes (no result line, no SUMMARY.md) must be routed to
// the review→merge pipeline, not requeued.  Before the fix the engine saw
// sl.Commits == 0 (progress probe hadn't run) and classified the death as
// "agent died with no commits", burning an attempt on a no-op requeue.
func TestCommitThenCrashProceedsToMerge(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, commitThenDieClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The agent's commit must reach main — no requeue should have occurred.
	if got.Merged != 1 {
		t.Errorf("Merged = %d, want 1 (commit-then-crash must proceed to merge)", got.Merged)
	}
	if got.Blocked != 0 {
		t.Errorf("Blocked = %d, want 0", got.Blocked)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}

	// Exactly one attempt consumed — no requeue must have fired.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (no attempt wasted on requeue)", sl.Attempts)
	}
	if sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}

	// The agent commit landed on main.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(log, "feat(tb1): work") {
		t.Errorf("main log missing agent commit:\n%s", log)
	}
}

// TestResumedAgentCleanExitProceedsToMerge is fixture 2 for koryph-ek2: a
// resumed agent that exits cleanly without adding new commits (having
// determined the work already exists on the branch) must be routed to the
// review→merge pipeline, not misclassified as "no commits" and requeued.
//
// Setup: the branch is pre-loaded with one commit ahead of main (simulating
// the prior-attempt work) before the engine runs.  The fake claude then exits
// cleanly with no new commits.  The slot starts at Commits == 0 (new
// dispatch); the fix's commitCount fallback must find the pre-existing commit.
func TestResumedAgentCleanExitProceedsToMerge(t *testing.T) {
	f := newFixture(t, fixOpts{})

	// Pre-load the agent branch with one commit ahead of main — this simulates
	// the state after a prior attempt committed work and was then rebased by
	// refreshWorktreeForRequeue, leaving branch commits that the new slot's
	// Commits field does not yet know about.
	agentBranch := "agent/tb1"
	runGit(t, f.repo, "checkout", "-b", agentBranch)
	writeFile(t, filepath.Join(f.repo, "prior-work.txt"), "done\n", 0o644)
	runGit(t, f.repo, "add", "prior-work.txt")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "feat(tb1): prior work")
	runGit(t, f.repo, "checkout", "main")

	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, cleanExitNoNewCommitsClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The pre-existing branch commit must reach main.
	if got.Merged != 1 {
		t.Errorf("Merged = %d, want 1 (resumed clean-exit must proceed to merge)", got.Merged)
	}
	if got.Blocked != 0 {
		t.Errorf("Blocked = %d, want 0", got.Blocked)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}

	// One attempt only — the clean exit must not trigger another requeue.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (clean exit must not burn another attempt)", sl.Attempts)
	}
	if sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}

	// The prior-attempt commit landed on main.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); !strings.Contains(log, "feat(tb1): prior work") {
		t.Errorf("main log missing prior-work commit:\n%s", log)
	}
}
