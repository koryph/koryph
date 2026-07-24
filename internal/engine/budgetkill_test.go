// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Requeue-policy regression tests for koryph-77r.10: budget-kill
// classification (dispatch.ParseBudgetKilled, pinned against the real
// captured canary fixture in internal/runtime/claude/events_test.go) and the
// warm-resume requeue policy layered on top of it in poll.go's
// requeueBudgetKilled.

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// budgetKillWarmResumeClaudeScript dies budget-killed with zero commits on
// its FIRST invocation (leaving a marker file, .attempted, in the worktree)
// and, if that marker is already present, completes normally instead — a
// resumed attempt only sees the marker if its worktree survived from the
// prior attempt (the koryph-137 cold-rebuild path would have wiped it along
// with the rest of the checkout).
const budgetKillWarmResumeClaudeScript = `#!/bin/sh
cat > /dev/null
if [ -f .attempted ]; then
  rm -f .attempted
  echo "work" > agent-work.txt
  git add agent-work.txt
  git commit -q --no-verify -m "feat(tb1): work"
  printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
  printf '{"type":"result","total_cost_usd":0.05,"is_error":false}\n'
  exit 0
fi
touch .attempted
printf '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.05,"errors":["Reached maximum budget ($0.05)"]}\n'
exit 0
`

// budgetKillAlwaysClaudeScript never commits and always dies budget-killed,
// with no usage block — so the thrash guard never fires (attemptUsage stays
// the zero value) and only the plain "second consecutive death" park path
// can trigger.
const budgetKillAlwaysClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.05,"errors":["Reached maximum budget ($0.05)"]}\n'
exit 0
`

// budgetKillThrashClaudeScript never commits, always dies budget-killed, and
// reports a pathologically large usage block on every attempt — the thrash
// guard's target shape.
const budgetKillThrashClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":0.05,"errors":["Reached maximum budget ($0.05)"],"usage":{"input_tokens":180000,"output_tokens":2000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}\n'
exit 0
`

// slotFor loads the latest run's ledger slot for beadID, failing the test if
// either is missing.
func slotFor(t *testing.T, repo, beadID string) *ledger.Slot {
	t.Helper()
	store := ledger.NewStore(repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots[beadID]
	if sl == nil {
		t.Fatalf("no slot %s in run: %+v", beadID, run.Slots)
	}
	return sl
}

// TestBudgetKillPreservesWorktreeForWarmResume is the "budget-death-
// preserves-worktree" case: a zero-commit budget-kill death requeues with
// the worktree and branch PRESERVED (not the koryph-137 cold rebuild), so
// the resumed attempt lands in the same checkout and can pick up where the
// first one left off — proven here by the resumed attempt actually finding
// its predecessor's marker file and completing the bead.
func TestBudgetKillPreservesWorktreeForWarmResume(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, budgetKillWarmResumeClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The whole point: the SECOND (warm-resumed) attempt found the first
	// attempt's marker file still sitting in its worktree and finished the
	// bead — impossible if the worktree had been torn down and rebuilt.
	if got.Merged != 1 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 merged / 0 blocked", got)
	}

	if !strings.Contains(out.String(), "preserving worktree and branch") {
		t.Errorf("engine output missing the worktree-preservation log line:\n%s", out.String())
	}
	if strings.Contains(out.String(), "requeue worktree rebuild skipped") {
		t.Errorf("engine output should not attempt a worktree rebuild on a budget-kill requeue:\n%s", out.String())
	}

	sl := slotFor(t, f.repo, "tb1")
	if sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}
	if sl.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (initial death + one warm-resume requeue)", sl.Attempts)
	}
	if sl.BudgetKillRequeues != 1 {
		t.Errorf("BudgetKillRequeues = %d, want 1", sl.BudgetKillRequeues)
	}

	// A WIP snapshot was captured before the (preserved, not rebuilt)
	// worktree's requeue — the AC's "WIP snapshot still taken" requirement.
	phaseDir := ledger.NewStore(f.repo).PhaseDir(got.RunID, "tb1")
	entries, _ := filepath.Glob(filepath.Join(phaseDir, "wip-*.patch"))
	if len(entries) == 0 {
		t.Error("expected a WIP snapshot patch to be captured even though the worktree was preserved")
	}
}

// TestBudgetKillSecondConsecutiveDeathParks is the "second-death-parks"
// case: a bead that is budget-killed again right after its one warm-resume
// requeue is parked needs-attention instead of spending a third
// --max-budget-usd cap.
func TestBudgetKillSecondConsecutiveDeathParks(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, budgetKillAlwaysClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Blocked != 1 || got.Merged != 0 {
		t.Errorf("Outcome = %+v, want 1 blocked / 0 merged", got)
	}

	sl := slotFor(t, f.repo, "tb1")
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "needs-attention") {
		t.Errorf("slot note = %q, want it to read needs-attention", sl.Note)
	}
	if !strings.Contains(sl.Note, "budget-killed twice in a row") {
		t.Errorf("slot note = %q, want it to name the second-consecutive-death reason", sl.Note)
	}
	if sl.DeathReason != "budget-killed" {
		t.Errorf("DeathReason = %q, want %q", sl.DeathReason, "budget-killed")
	}
	// One warm-resume requeue was spent before parking (attempt 1 died, attempt
	// 2 was the warm resume, attempt 2 also died and parked — never a 3rd).
	if sl.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (one warm-resume requeue, then parked — never a 3rd cold attempt)", sl.Attempts)
	}
	if sl.BudgetKillRequeues != 1 {
		t.Errorf("BudgetKillRequeues = %d, want 1 (budget exhausted, not exceeded)", sl.BudgetKillRequeues)
	}
	if !strings.Contains(out.String(), "preserving worktree and branch") {
		t.Errorf("engine output missing the FIRST warm-resume's worktree-preservation log line:\n%s", out.String())
	}
}

// TestBudgetKillThrashGuardSkipsFirstWarmResume is the "thrash-skip" case:
// even the FIRST budget-kill requeue is skipped (parked immediately) when
// the dying attempt had zero commits AND burned a pathological token volume
// — resuming it warm would likely just re-loop and burn a second cap for
// nothing.
func TestBudgetKillThrashGuardSkipsFirstWarmResume(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, budgetKillThrashClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Blocked != 1 || got.Merged != 0 {
		t.Errorf("Outcome = %+v, want 1 blocked / 0 merged", got)
	}

	sl := slotFor(t, f.repo, "tb1")
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "thrash guard") {
		t.Errorf("slot note = %q, want it to name the thrash guard", sl.Note)
	}
	if !strings.Contains(sl.Note, "needs-attention") {
		t.Errorf("slot note = %q, want it to read needs-attention", sl.Note)
	}
	// The thrash guard fires on the FIRST death — no warm resume is spent.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (thrash guard skips the warm resume entirely)", sl.Attempts)
	}
	if sl.BudgetKillRequeues != 0 {
		t.Errorf("BudgetKillRequeues = %d, want 0 (never spent — the guard skipped the requeue)", sl.BudgetKillRequeues)
	}
	if strings.Contains(out.String(), "preserving worktree and branch") {
		t.Errorf("engine output should not attempt a warm-resume requeue when the thrash guard fires:\n%s", out.String())
	}
}
