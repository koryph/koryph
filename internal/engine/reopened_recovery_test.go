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
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/worktree"
)

func seedTerminalCandidate(t *testing.T, f *fix, beadID string) string {
	t.Helper()
	wt, err := worktree.Ensure(t.Context(), worktree.EnsureOpts{
		RepoRoot: f.repo, WorktreeRoot: f.wtRoot,
		Branch: worktree.BranchFor(beadID), Base: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewStore(f.repo)
	run, err := store.NewRun("proj", "bd", EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(run, &ledger.Slot{
		PhaseID: beadID, BeadID: beadID, Branch: wt.Branch, Worktree: wt.Path,
		Status: ledger.SlotBlocked, Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeRun(run); err != nil {
		t.Fatal(err)
	}
	return wt.Path
}

func TestFreshDispatchRecoversDirtyTerminalCandidateWithTruthfulMetadata(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "reopened"
	wtPath := seedTerminalCandidate(t, f, beadID)

	writeFile(t, filepath.Join(wtPath, "candidate.txt"), "candidate commit\n", 0o644)
	runGit(t, wtPath, "add", "candidate.txt")
	runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: preserved candidate")
	writeFile(t, filepath.Join(wtPath, "staged.txt"), "staged\n", 0o644)
	runGit(t, wtPath, "add", "staged.txt")
	writeFile(t, filepath.Join(wtPath, "staged.txt"), "unstaged\n", 0o644)
	writeFile(t, filepath.Join(wtPath, "untracked.txt"), "untracked\n", 0o644)
	statusBefore := runGit(t, wtPath, "status", "--porcelain=v1")
	indexBefore := runGit(t, wtPath, "diff", "--cached", "--binary")

	writeFile(t, filepath.Join(f.repo, "base.txt"), "advanced base\n", 0o644)
	runGit(t, f.repo, "add", "base.txt")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: advance base")
	base := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))

	issue := beads.Issue{ID: beadID, Title: "reopened candidate"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(spec dispatch.Spec) error {
		backendCalls++
		runGit(t, spec.Worktree, "merge-base", "--is-ancestor", base, "HEAD")
		if got := runGit(t, spec.Worktree, "status", "--porcelain=v1"); got != statusBefore {
			t.Fatalf("launch changed recovered status:\nbefore=%q\nafter =%q", statusBefore, got)
		}
		if got := runGit(t, spec.Worktree, "diff", "--cached", "--binary"); got != indexBefore {
			t.Fatal("launch changed recovered index content")
		}
		if !strings.Contains(spec.Prompt, "uncommitted work was snapshotted") {
			t.Fatalf("dispatch prompt lacks WIP recovery metadata:\n%s", spec.Prompt)
		}
		return nil
	})

	r.dispatchBead(t.Context(), dispatchReq{origin: dispatchOriginFrontier, issue: issue, attempt: 1})
	if backendCalls != 1 {
		t.Fatalf("backend calls=%d, want 1", backendCalls)
	}
	sl := r.run.Slots[beadID]
	if sl == nil || sl.Status != ledger.SlotRunning {
		t.Fatalf("recovered slot=%+v, want running", sl)
	}
	if sl.DispatchBaseSHA != base || sl.ResumeSHA != base ||
		!strings.Contains(sl.Note, "recovered reopened terminal candidate") ||
		!strings.Contains(sl.Note, "recovery_evidence=") {
		t.Fatalf("recovery metadata is not truthful: %+v", sl)
	}
	runGit(t, wtPath, "merge-base", "--is-ancestor", sl.DispatchBaseSHA, "HEAD")
	var evidence reopenedCandidateRecoveryResult
	evidencePath := filepath.Join(r.store.PhaseDir(r.run.RunID, beadID), "reopened-recovery.json")
	if err := fsx.ReadJSON(evidencePath, &evidence); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(evidencePath); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("recovery evidence mode=%v err=%v, want 0600", info, err)
	}
	if evidence.Status != "recovered" || evidence.OriginalHead == "" ||
		evidence.RefreshedHead == "" || evidence.DispatchBaseSHA != base ||
		evidence.WIPSnapshotPath == "" || evidence.PriorRunID == "" {
		t.Fatalf("structured recovery evidence=%+v", evidence)
	}
}

func TestFreshDispatchRecoveryConflictFailsClosedAndPreservesWork(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "conflict"
	wtPath := seedTerminalCandidate(t, f, beadID)
	writeFile(t, filepath.Join(wtPath, "README.md"), "branch\n", 0o644)
	runGit(t, wtPath, "add", "README.md")
	runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: conflicting candidate")
	writeFile(t, filepath.Join(wtPath, "staged.txt"), "staged\n", 0o644)
	runGit(t, wtPath, "add", "staged.txt")
	writeFile(t, filepath.Join(wtPath, "staged.txt"), "unstaged\n", 0o644)
	writeFile(t, filepath.Join(wtPath, "untracked.txt"), "untracked\n", 0o644)
	originalHead := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD"))
	statusBefore := runGit(t, wtPath, "status", "--porcelain=v1")
	indexBefore := runGit(t, wtPath, "diff", "--cached", "--binary")

	writeFile(t, filepath.Join(f.repo, "README.md"), "base\n", 0o644)
	runGit(t, f.repo, "add", "README.md")
	runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: conflicting base")

	issue := beads.Issue{ID: beadID, Title: "conflicting reopened candidate"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		backendCalls++
		return nil
	})
	r.dispatchBead(t.Context(), dispatchReq{origin: dispatchOriginFrontier, issue: issue, attempt: 1})

	if backendCalls != 0 {
		t.Fatalf("backend launched %d time(s) after recovery conflict", backendCalls)
	}
	sl := r.run.Slots[beadID]
	if sl == nil || sl.Status != ledger.SlotBlocked || sl.Worktree == "" ||
		!strings.Contains(sl.Note, "original worktree restored") ||
		!strings.Contains(sl.Note, "recovery evidence=") {
		t.Fatalf("conflict slot=%+v, want preserved fail-closed evidence", sl)
	}
	if head := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")); head != originalHead {
		t.Fatalf("HEAD=%s, want original %s", head, originalHead)
	}
	if got := runGit(t, wtPath, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("recovery conflict changed WIP status:\nbefore=%q\nafter =%q", statusBefore, got)
	}
	if got := runGit(t, wtPath, "diff", "--cached", "--binary"); got != indexBefore {
		t.Fatal("recovery conflict changed staged index content")
	}
	var evidence reopenedCandidateRecoveryResult
	evidencePath := filepath.Join(r.store.PhaseDir(r.run.RunID, beadID), "reopened-recovery.json")
	if err := fsx.ReadJSON(evidencePath, &evidence); err != nil {
		t.Fatal(err)
	}
	if evidence.Status != "blocked" || evidence.Error == "" ||
		evidence.OriginalHead != originalHead || evidence.WIPSnapshotPath == "" {
		t.Fatalf("structured blocked recovery evidence=%+v", evidence)
	}
}

func TestFreshDispatchWithoutExistingWorktreeKeepsOrdinaryPath(t *testing.T) {
	f := newFixture(t, fixOpts{})
	issue := beads.Issue{ID: "brand-new", Title: "brand new"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(spec dispatch.Spec) error {
		backendCalls++
		return nil
	})
	r.dispatchBead(t.Context(), dispatchReq{origin: dispatchOriginFrontier, issue: issue, attempt: 1})

	if backendCalls != 1 {
		t.Fatalf("backend calls=%d, want 1", backendCalls)
	}
	sl := r.run.Slots[issue.ID]
	if sl == nil || sl.Status != ledger.SlotRunning || sl.ResumeSHA != "" ||
		strings.Contains(sl.Note, "recovered reopened") {
		t.Fatalf("ordinary fresh slot changed behavior: %+v", sl)
	}
}

func TestAttachedWorktreeRequiresExplicitFrontierOrigin(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "origin"
	seedTerminalCandidate(t, f, beadID)
	issue := beads.Issue{ID: beadID, Title: "origin required"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		backendCalls++
		return nil
	})
	r.dispatchBead(t.Context(), dispatchReq{issue: issue, attempt: 1})
	if backendCalls != 0 {
		t.Fatalf("backend calls=%d, want 0 without explicit origin", backendCalls)
	}
	if sl := r.run.Slots[beadID]; sl == nil || sl.Status != ledger.SlotBlocked ||
		!strings.Contains(sl.Note, "explicit dispatch origin") {
		t.Fatalf("origin-less attached slot=%+v", sl)
	}
}

func TestReopenedRecoveryUsesNewestAuthoritativeOwner(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "authoritative"
	wt, err := worktree.Ensure(t.Context(), worktree.EnsureOpts{
		RepoRoot: f.repo, WorktreeRoot: f.wtRoot,
		Branch: worktree.BranchFor(beadID), Base: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	store := ledger.NewStore(f.repo)
	older, err := store.NewRun("proj", "bd", EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(older, &ledger.Slot{
		PhaseID: beadID, Branch: wt.Branch, Worktree: wt.Path,
		Status: ledger.SlotRunning, Attempts: 1,
	}); err != nil {
		t.Fatal(err)
	}
	newer, err := store.NewRun("proj", "bd", EngineVersion)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSlot(newer, &ledger.Slot{
		PhaseID: beadID, Branch: wt.Branch, Worktree: wt.Path,
		Status: ledger.SlotBlocked, Attempts: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.FinalizeRun(newer); err != nil {
		t.Fatal(err)
	}

	issue := beads.Issue{ID: beadID, Title: "authoritative owner"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		backendCalls++
		return nil
	})
	r.dispatchBead(t.Context(), dispatchReq{
		origin: dispatchOriginFrontier, issue: issue, attempt: 1,
	})
	if backendCalls != 1 {
		t.Fatalf("backend calls=%d, want newest terminal owner to supersede older dead ledger state", backendCalls)
	}
}

func TestReopenedRecoveryRejectsMismatchedTerminalOwnership(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "mismatch"
	seedTerminalCandidate(t, f, beadID)
	store := ledger.NewStore(f.repo)
	prior, err := store.LoadLatest()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSlot(prior, beadID, func(sl *ledger.Slot) {
		sl.Worktree = filepath.Join(f.wtRoot, "different-worktree")
	}); err != nil {
		t.Fatal(err)
	}
	issue := beads.Issue{ID: beadID, Title: "mismatched owner"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		backendCalls++
		return nil
	})
	r.dispatchBead(t.Context(), dispatchReq{
		origin: dispatchOriginFrontier, issue: issue, attempt: 1,
	})
	if backendCalls != 0 {
		t.Fatalf("backend calls=%d, want mismatched owner refusal", backendCalls)
	}
	if sl := r.run.Slots[beadID]; sl == nil || sl.Status != ledger.SlotBlocked ||
		!strings.Contains(sl.Note, "not attached branch") {
		t.Fatalf("mismatched owner slot=%+v", sl)
	}
}

func TestFreshDispatchOwnershipRaceRefusesWithoutBackendOrWorktreeMutation(t *testing.T) {
	f := newFixture(t, fixOpts{})
	const beadID = "ownership-race"
	wtPath := seedTerminalCandidate(t, f, beadID)
	originalHead := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD"))
	issue := beads.Issue{ID: beadID, Title: "ownership race"}
	backendCalls := 0
	r := dispatchIdentityRunner(t, f, issue, func(dispatch.Spec) error {
		backendCalls++
		return nil
	})
	var racedHead string
	r.dispatchBead(t.Context(), dispatchReq{
		origin: dispatchOriginFrontier, issue: issue, attempt: 1,
		beforeRecovery: func() {
			writeFile(t, filepath.Join(wtPath, "concurrent-owner.txt"), "owner\n", 0o644)
			runGit(t, wtPath, "add", "concurrent-owner.txt")
			runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: concurrent owner")
			racedHead = strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD"))
		},
	})
	if racedHead == "" || racedHead == originalHead {
		t.Fatal("test did not advance worktree HEAD between Ensure and recovery")
	}
	if backendCalls != 0 {
		t.Fatalf("backend calls=%d, want 0 after ownership race", backendCalls)
	}
	if head := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")); head != racedHead {
		t.Fatalf("ownership refusal mutated raced HEAD: got %s want %s", head, racedHead)
	}
	if status := strings.TrimSpace(runGit(t, wtPath, "status", "--porcelain=v1")); status != "" {
		t.Fatalf("ownership refusal mutated worktree status: %q", status)
	}
	if sl := r.run.Slots[beadID]; sl == nil || sl.Status != ledger.SlotBlocked ||
		!strings.Contains(sl.Note, "HEAD changed after Ensure") {
		t.Fatalf("ownership-race slot=%+v", sl)
	}
}

func TestReopenedCleanCandidateDispatchesAcrossWaveAndRolling(t *testing.T) {
	for _, mode := range []string{"wave", "rolling"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t, fixOpts{})
			wtPath := seedTerminalCandidate(t, f, "tb1")
			writeFile(t, filepath.Join(wtPath, "recovered.txt"), "preserved\n", 0o644)
			runGit(t, wtPath, "add", "recovered.txt")
			runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: preserved terminal candidate")
			writeFile(t, filepath.Join(f.repo, "base.txt"), "advanced\n", 0o644)
			runGit(t, f.repo, "add", "base.txt")
			runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: advance base")

			var out bytes.Buffer
			opts := baseOptions(&out)
			opts.DispatchMode = mode
			opts.Once = mode == "wave"
			got, err := Run(context.Background(), opts)
			if err != nil {
				t.Fatalf("Run(%s): %v\n%s", mode, err, out.String())
			}
			if got.Merged != 1 {
				t.Fatalf("Run(%s) outcome=%+v\n%s", mode, got, out.String())
			}
			data, err := os.ReadFile(filepath.Join(f.repo, "recovered.txt"))
			if err != nil || string(data) != "preserved\n" {
				t.Fatalf("Run(%s) lost recovered commit: %q, %v", mode, data, err)
			}
			if _, err := os.Stat(filepath.Join(f.repo, "base.txt")); err != nil {
				t.Fatalf("Run(%s) lost current base: %v", mode, err)
			}
		})
	}
}

func TestReopenedRefusalsNeverCallBackendAcrossWaveAndRolling(t *testing.T) {
	const backendMarkerScript = `#!/bin/sh
touch "$FAKE_BD_DIR/backend-called"
exit 73
`
	for _, mode := range []string{"wave", "rolling"} {
		for _, cause := range []string{"conflict", "ownership"} {
			t.Run(mode+"/"+cause, func(t *testing.T) {
				f := newFixture(t, fixOpts{claudeScript: backendMarkerScript})
				wtPath := seedTerminalCandidate(t, f, "tb1")
				switch cause {
				case "conflict":
					writeFile(t, filepath.Join(wtPath, "README.md"), "branch\n", 0o644)
					runGit(t, wtPath, "add", "README.md")
					runGit(t, wtPath, "commit", "--no-verify", "-m", "feat: conflicting candidate")
					writeFile(t, filepath.Join(wtPath, "staged.txt"), "staged\n", 0o644)
					runGit(t, wtPath, "add", "staged.txt")
					writeFile(t, filepath.Join(f.repo, "README.md"), "base\n", 0o644)
					runGit(t, f.repo, "add", "README.md")
					runGit(t, f.repo, "commit", "--no-verify", "-m", "chore: conflicting base")
				case "ownership":
					store := ledger.NewStore(f.repo)
					prior, err := store.LoadLatest()
					if err != nil {
						t.Fatal(err)
					}
					if err := store.UpdateSlot(prior, "tb1", func(sl *ledger.Slot) {
						sl.Worktree = filepath.Join(f.wtRoot, "foreign-owner")
					}); err != nil {
						t.Fatal(err)
					}
				}
				headBefore := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD"))
				statusBefore := runGit(t, wtPath, "status", "--porcelain=v1")

				var out bytes.Buffer
				opts := baseOptions(&out)
				opts.DispatchMode = mode
				opts.Once = mode == "wave"
				got, err := Run(t.Context(), opts)
				if err != nil {
					t.Fatalf("Run(%s/%s): %v\n%s", mode, cause, err, out.String())
				}
				if got.Dispatched != 0 {
					t.Fatalf("Run(%s/%s) dispatched=%d, want zero\n%s", mode, cause, got.Dispatched, out.String())
				}
				if _, err := os.Stat(filepath.Join(f.bdDir, "backend-called")); !os.IsNotExist(err) {
					t.Fatalf("Run(%s/%s) called backend: stat err=%v", mode, cause, err)
				}
				if head := strings.TrimSpace(runGit(t, wtPath, "rev-parse", "HEAD")); head != headBefore {
					t.Fatalf("Run(%s/%s) changed HEAD=%s want %s", mode, cause, head, headBefore)
				}
				if status := runGit(t, wtPath, "status", "--porcelain=v1"); status != statusBefore {
					t.Fatalf("Run(%s/%s) changed status:\nbefore=%q\nafter=%q", mode, cause, statusBefore, status)
				}
			})
		}
	}
}
