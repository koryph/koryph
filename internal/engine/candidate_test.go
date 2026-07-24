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
	"github.com/koryph/koryph/internal/registry"
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
