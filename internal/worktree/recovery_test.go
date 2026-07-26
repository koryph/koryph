// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/fsx"
)

func TestPatchSnapshotCapturesUntracked(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "new.txt"), "hello patch body\n")
	outDir := t.TempDir()
	p, err := PatchSnapshot(ctx, info.Path, outDir)
	if err != nil {
		t.Fatal(err)
	}
	if p == "" {
		t.Fatal("expected a patch path, got empty")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "new.txt") || !strings.Contains(string(data), "hello patch body") {
		t.Errorf("patch missing untracked content:\n%s", data)
	}
	// The `git add -N` staging must NOT be left behind.
	st := mustGit(t, info.Path, "status", "--porcelain")
	if !strings.Contains(st, "?? new.txt") {
		t.Errorf("after snapshot expected untracked new.txt, status=%q", st)
	}
}

func TestPatchSnapshotPreservesExistingIndex(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged version\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "unstaged version\n")
	writeFile(t, filepath.Join(info.Path, "untracked.txt"), "untracked\n")

	statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")
	indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")
	if _, err := PatchSnapshot(ctx, info.Path, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if got := mustGit(t, info.Path, "status", "--porcelain=v1"); got != statusBefore {
		t.Fatalf("snapshot changed index/worktree status:\nbefore=%q\nafter =%q", statusBefore, got)
	}
	if got := mustGit(t, info.Path, "diff", "--cached", "--binary"); got != indexBefore {
		t.Fatal("snapshot changed staged index content")
	}
}

func TestRecoverOntoBasePreservesStagedUnstagedAndUntracked(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(info.Path, "feature.txt"), "committed feature\n")
	mustGit(t, info.Path, "add", "feature.txt")
	mustGit(t, info.Path, "commit", "-qm", "feature")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged version\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "unstaged version\n")
	writeFile(t, filepath.Join(info.Path, "untracked.txt"), "untracked payload\n")
	if err := os.Symlink("missing-target", filepath.Join(info.Path, "broken-link")); err != nil {
		t.Fatal(err)
	}

	statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")
	indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")
	writeFile(t, filepath.Join(repo, "base.txt"), "new base\n")
	mustGit(t, repo, "add", "base.txt")
	mustGit(t, repo, "commit", "-qm", "advance base")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))

	got, err := RecoverOntoBase(ctx, RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, SnapshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.HadWIP || got.WIPSnapshotPath == "" {
		t.Fatalf("recovery metadata = %+v, want WIP snapshot", got)
	}
	mustGit(t, info.Path, "merge-base", "--is-ancestor", base, "HEAD")
	if status := mustGit(t, info.Path, "status", "--porcelain=v1"); status != statusBefore {
		t.Fatalf("recovery changed staged/unstaged/untracked status:\nbefore=%q\nafter =%q", statusBefore, status)
	}
	if index := mustGit(t, info.Path, "diff", "--cached", "--binary"); index != indexBefore {
		t.Fatal("recovery changed staged index content")
	}
	for path, want := range map[string]string{
		"base.txt":      "new base\n",
		"feature.txt":   "committed feature\n",
		"staged.txt":    "unstaged version\n",
		"untracked.txt": "untracked payload\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(info.Path, path))
		if readErr != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", path, data, readErr, want)
		}
	}
	if target, err := os.Readlink(filepath.Join(info.Path, "broken-link")); err != nil || target != "missing-target" {
		t.Errorf("broken symlink target=%q, err=%v; want missing-target", target, err)
	}
}

func TestRecoverOntoBaseCleanCommittedCandidate(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "feature.txt"), "feature\n")
	mustGit(t, info.Path, "add", "feature.txt")
	mustGit(t, info.Path, "commit", "-qm", "feature")
	writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
	mustGit(t, repo, "add", "base.txt")
	mustGit(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))

	got, err := RecoverOntoBase(ctx, RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, SnapshotDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.HadWIP || got.WIPSnapshotPath != "" {
		t.Fatalf("clean recovery metadata = %+v", got)
	}
	mustGit(t, info.Path, "merge-base", "--is-ancestor", base, "HEAD")
	for _, path := range []string{"feature.txt", "base.txt"} {
		if !fsx.Exists(filepath.Join(info.Path, path)) {
			t.Errorf("recovered worktree missing %s", path)
		}
	}
}

func TestRecoverOntoBaseConflictRestoresExactWIP(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "a.txt"), "branch\n")
	mustGit(t, info.Path, "add", "a.txt")
	mustGit(t, info.Path, "commit", "-qm", "branch conflict")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "unstaged\n")
	writeFile(t, filepath.Join(info.Path, "untracked.txt"), "untracked\n")
	originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
	statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")
	indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")

	writeFile(t, filepath.Join(repo, "a.txt"), "base\n")
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-qm", "base conflict")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))
	hook := filepath.Join(repo, ".git", "hooks", "pre-rebase")
	writeFile(t, hook, "#!/bin/sh\nprintf 'concurrent\\n' > concurrent-during-rebase.txt\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := RecoverOntoBase(ctx, RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, SnapshotDir: t.TempDir(),
	})
	if err == nil || !strings.Contains(err.Error(), "original worktree restored") {
		t.Fatalf("RecoverOntoBase error = %v, want restored conflict", err)
	}
	if got.WIPSnapshotPath == "" {
		t.Fatalf("conflict lost snapshot metadata: %+v", got)
	}
	if head := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD")); head != originalHead {
		t.Fatalf("HEAD=%s, want original %s", head, originalHead)
	}
	if status := strings.ReplaceAll(
		mustGit(t, info.Path, "status", "--porcelain=v1"),
		"?? concurrent-during-rebase.txt\n", "",
	); status != statusBefore {
		t.Fatalf("conflict rollback changed status:\nbefore=%q\nafter =%q", statusBefore, status)
	}
	if index := mustGit(t, info.Path, "diff", "--cached", "--binary"); index != indexBefore {
		t.Fatal("conflict rollback changed staged index content")
	}
	for path, want := range map[string]string{
		"a.txt":                        "branch\n",
		"staged.txt":                   "unstaged\n",
		"untracked.txt":                "untracked\n",
		"concurrent-during-rebase.txt": "concurrent\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(info.Path, path))
		if readErr != nil || string(data) != want {
			t.Errorf("%s = %q, %v; want %q", path, data, readErr, want)
		}
	}
}

func TestRecoverOntoBaseRollbackOutlivesCanceledCaller(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	info, err := Ensure(t.Context(), EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "feature.txt"), "feature\n")
	mustGit(t, info.Path, "add", "feature.txt")
	mustGit(t, info.Path, "commit", "-qm", "feature")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "untracked.txt"), "untracked\n")
	originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
	indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")

	writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
	mustGit(t, repo, "add", "base.txt")
	mustGit(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	got, err := RecoverOntoBase(ctx, RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, SnapshotDir: t.TempDir(),
		beforeRebase: cancel,
	})
	if err == nil {
		t.Fatal("RecoverOntoBase unexpectedly succeeded after caller cancellation")
	}
	if got.WIPSnapshotPath == "" {
		t.Fatalf("canceled recovery lost WIP snapshot metadata: %+v", got)
	}
	if head := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD")); head != originalHead {
		t.Fatalf("canceled rollback HEAD=%s, want %s", head, originalHead)
	}
	if index := mustGit(t, info.Path, "diff", "--cached", "--binary"); index != indexBefore {
		t.Fatalf("canceled rollback changed staged index:\nbefore:\n%s\nafter:\n%s", indexBefore, index)
	}
	for path, want := range map[string]string{
		"staged.txt":    "staged\n",
		"untracked.txt": "untracked\n",
	} {
		data, readErr := os.ReadFile(filepath.Join(info.Path, path))
		if readErr != nil || string(data) != want {
			t.Errorf("%s=%q, %v; want %q", path, data, readErr, want)
		}
	}
}

func TestMoveRecoveryFilesRollsBackPartialMove(t *testing.T) {
	from := t.TempDir()
	to := t.TempDir()
	writeFile(t, filepath.Join(from, "a.txt"), "a\n")
	writeFile(t, filepath.Join(from, "b.txt"), "b\n")
	writeFile(t, filepath.Join(to, "b.txt"), "concurrent\n")
	a, err := inspectRecoveryFile(filepath.Join(from, "a.txt"), "a.txt")
	if err != nil {
		t.Fatal(err)
	}
	b, err := inspectRecoveryFile(filepath.Join(from, "b.txt"), "b.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := moveRecoveryFiles(from, to, []recoveryFile{a, b}); err == nil {
		t.Fatal("moveRecoveryFiles unexpectedly ignored destination collision")
	}
	for path, want := range map[string]string{"a.txt": "a\n", "b.txt": "b\n"} {
		data, readErr := os.ReadFile(filepath.Join(from, path))
		if readErr != nil || string(data) != want {
			t.Fatalf("partial rollback %s=%q, %v; want %q", path, data, readErr, want)
		}
	}
	data, err := os.ReadFile(filepath.Join(to, "b.txt"))
	if err != nil || string(data) != "concurrent\n" {
		t.Fatalf("destination collision was overwritten: %q, %v", data, err)
	}
	if pathExists(filepath.Join(to, "a.txt")) {
		t.Fatal("partial move left a.txt at destination")
	}
}

func TestRecoverOntoBaseReplaysEveryCrashPhase(t *testing.T) {
	cases := []string{
		"prepared-before-move",
		"after-untracked-move",
		"after-reset",
		"after-rebase",
		"after-wip-apply",
		"after-untracked-apply",
		"restored-before-cleanup",
	}
	for _, crashPoint := range cases {
		t.Run(crashPoint, func(t *testing.T) {
			isolateGit(t)
			repo := initRepo(t)
			ctx := t.Context()
			info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(info.Path, "feature.txt"), "feature\n")
			mustGit(t, info.Path, "add", "feature.txt")
			mustGit(t, info.Path, "commit", "-qm", "feature")
			writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged\n")
			mustGit(t, info.Path, "add", "staged.txt")
			writeFile(t, filepath.Join(info.Path, "staged.txt"), "unstaged\n")
			if err := os.WriteFile(filepath.Join(info.Path, "binary.bin"), []byte{0, 1, 2, 3, 0xff}, 0o700); err != nil {
				t.Fatal(err)
			}
			originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
			statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")
			indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")

			writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
			mustGit(t, repo, "add", "base.txt")
			mustGit(t, repo, "commit", "-qm", "base")
			base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))
			snapshotDir := t.TempDir()
			snapshot, err := PatchSnapshot(ctx, info.Path, snapshotDir)
			if err != nil {
				t.Fatal(err)
			}
			opts := RecoveryOpts{
				RepoRoot: repo, Path: info.Path, Branch: info.Branch,
				Base: base, SnapshotDir: snapshotDir,
			}
			state, err := suspendWIP(ctx, opts, originalHead, snapshot)
			if err != nil {
				t.Fatal(err)
			}

			rebase := func() string {
				mustGit(t, info.Path, "rebase", base)
				return strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
			}
			applyTracked := func() {
				if state.hasTracked {
					mustGit(t, info.Path, "stash", "apply", "--index", state.commit)
				}
			}
			switch crashPoint {
			case "prepared-before-move":
				if err := state.restoreOriginal(ctx, info.Path, originalHead); err != nil {
					t.Fatal(err)
				}
				if err := state.checkpoint(recoveryPhasePrepared, ""); err != nil {
					t.Fatal(err)
				}
			case "after-untracked-move":
				applyTracked()
				if err := state.checkpoint(recoveryPhasePrepared, ""); err != nil {
					t.Fatal(err)
				}
			case "after-reset":
				if err := state.checkpoint(recoveryPhaseUntrackedHeld, ""); err != nil {
					t.Fatal(err)
				}
			case "after-rebase":
				rebase()
				if err := state.checkpoint(recoveryPhaseSuspended, ""); err != nil {
					t.Fatal(err)
				}
			case "after-wip-apply":
				refreshed := rebase()
				if err := state.checkpoint(recoveryPhaseRebased, refreshed); err != nil {
					t.Fatal(err)
				}
				applyTracked() // crash before tracked-restored checkpoint
			case "after-untracked-apply":
				refreshed := rebase()
				if err := state.checkpoint(recoveryPhaseRebased, refreshed); err != nil {
					t.Fatal(err)
				}
				applyTracked()
				if err := state.checkpoint(recoveryPhaseTrackedRestored, ""); err != nil {
					t.Fatal(err)
				}
				if err := moveRecoveryFiles(state.holdingDir, info.Path, state.untracked); err != nil {
					t.Fatal(err)
				}
			case "restored-before-cleanup":
				refreshed := rebase()
				if err := state.checkpoint(recoveryPhaseRebased, refreshed); err != nil {
					t.Fatal(err)
				}
				if err := state.restoreOntoCurrent(ctx, info.Path); err != nil {
					t.Fatal(err)
				}
			}

			got, err := RecoverOntoBase(ctx, opts)
			if err != nil {
				t.Fatalf("restart recovery at %s: %v", crashPoint, err)
			}
			if got.OriginalHead != originalHead || got.Head == originalHead {
				t.Fatalf("restart metadata at %s = %+v", crashPoint, got)
			}
			mustGit(t, info.Path, "merge-base", "--is-ancestor", base, "HEAD")
			if status := mustGit(t, info.Path, "status", "--porcelain=v1"); status != statusBefore {
				t.Fatalf("%s changed WIP status:\nbefore=%q\nafter =%q", crashPoint, statusBefore, status)
			}
			if index := mustGit(t, info.Path, "diff", "--cached", "--binary"); index != indexBefore {
				t.Fatalf("%s changed staged index:\nbefore:\n%s\nafter:\n%s", crashPoint, indexBefore, index)
			}
			data, err := os.ReadFile(filepath.Join(info.Path, "binary.bin"))
			if err != nil || string(data) != string([]byte{0, 1, 2, 3, 0xff}) {
				t.Fatalf("%s lost binary WIP: %v %v", crashPoint, data, err)
			}
			if refs := strings.TrimSpace(mustGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/koryph/recovery")); refs != "" {
				t.Fatalf("%s leaked recovery refs: %s", crashPoint, refs)
			}
		})
	}
}

type suspendedRecoveryFixture struct {
	repo         string
	info         Info
	opts         RecoveryOpts
	state        suspendedWIP
	originalHead string
	statusBefore string
	indexBefore  string
	base         string
}

func newSuspendedRecoveryFixture(t *testing.T) suspendedRecoveryFixture {
	t.Helper()
	isolateGit(t)
	repo := initRepo(t)
	ctx := t.Context()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "feature.txt"), "feature\n")
	mustGit(t, info.Path, "add", "feature.txt")
	mustGit(t, info.Path, "commit", "-qm", "feature")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "nested", "untracked.txt"), "untracked\n")
	originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
	statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")
	indexBefore := mustGit(t, info.Path, "diff", "--cached", "--binary")

	writeFile(t, filepath.Join(repo, "base.txt"), "base\n")
	mustGit(t, repo, "add", "base.txt")
	mustGit(t, repo, "commit", "-qm", "base")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))
	snapshotDir := t.TempDir()
	snapshot, err := PatchSnapshot(ctx, info.Path, snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, SnapshotDir: snapshotDir,
	}
	state, err := suspendWIP(ctx, opts, originalHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return suspendedRecoveryFixture{
		repo: repo, info: info, opts: opts, state: state,
		originalHead: originalHead, statusBefore: statusBefore,
		indexBefore: indexBefore, base: base,
	}
}

func TestRecoveryRetirementFailureKeepsReplayableJournalAndRef(t *testing.T) {
	fixture := newSuspendedRecoveryFixture(t)
	ctx := t.Context()
	if err := fixture.state.restoreOriginal(ctx, fixture.info.Path, fixture.originalHead); err != nil {
		t.Fatal(err)
	}

	injected := errors.New("injected holding retirement failure")
	err := fixture.state.retireWith(ctx, fixture.repo, func(state *suspendedWIP) error {
		if state.holdingDir != fixture.state.holdingDir {
			t.Fatalf("retiring holding dir %q, want %q", state.holdingDir, fixture.state.holdingDir)
		}
		return injected
	})
	if !errors.Is(err, injected) {
		t.Fatalf("retireWith error=%v, want injected failure", err)
	}
	if _, err := os.Stat(fixture.state.journalPath); err != nil {
		t.Fatalf("retirement failure removed journal: %v", err)
	}
	if got, err := revParseCommit(ctx, fixture.repo, fixture.state.ref); err != nil || got != fixture.state.commit {
		t.Fatalf("retirement failure dropped authenticating ref: got=%q err=%v", got, err)
	}

	got, err := RecoverOntoBase(ctx, fixture.opts)
	if err != nil {
		t.Fatalf("restart could not replay anchored journal: %v", err)
	}
	if got.OriginalHead != fixture.originalHead {
		t.Fatalf("restart original head=%s, want %s", got.OriginalHead, fixture.originalHead)
	}
	mustGit(t, fixture.info.Path, "merge-base", "--is-ancestor", fixture.base, "HEAD")
	if status := mustGit(t, fixture.info.Path, "status", "--porcelain=v1"); status != fixture.statusBefore {
		t.Fatalf("restart changed WIP:\nbefore=%q\nafter=%q", fixture.statusBefore, status)
	}
	if index := mustGit(t, fixture.info.Path, "diff", "--cached", "--binary"); index != fixture.indexBefore {
		t.Fatal("restart changed staged index")
	}
	if pathExists(fixture.state.holdingDir) {
		t.Fatalf("restart left retired holding directory %s", fixture.state.holdingDir)
	}
	if refs := strings.TrimSpace(mustGit(t, fixture.repo, "for-each-ref", "--format=%(refname)", "refs/koryph/recovery")); refs != "" {
		t.Fatalf("restart leaked recovery refs: %s", refs)
	}
}

func TestRecoveryRetirementRefusesUnknownHoldingEntry(t *testing.T) {
	fixture := newSuspendedRecoveryFixture(t)
	ctx := t.Context()
	if err := fixture.state.restoreOriginal(ctx, fixture.info.Path, fixture.originalHead); err != nil {
		t.Fatal(err)
	}
	unknown := filepath.Join(fixture.state.holdingDir, "unrelated.bin")
	if err := os.WriteFile(unknown, []byte{0, 1, 2, 0xff}, 0o600); err != nil {
		t.Fatal(err)
	}

	err := fixture.state.retire(ctx, fixture.repo)
	if err == nil || !strings.Contains(err.Error(), "unexpected recovery holding entry") {
		t.Fatalf("retire error=%v, want unknown-entry refusal", err)
	}
	if data, err := os.ReadFile(unknown); err != nil || string(data) != string([]byte{0, 1, 2, 0xff}) {
		t.Fatalf("retire changed unknown data: data=%v err=%v", data, err)
	}
	if _, err := os.Stat(fixture.state.journalPath); err != nil {
		t.Fatalf("retire removed journal despite unknown entry: %v", err)
	}
	if got, err := revParseCommit(ctx, fixture.repo, fixture.state.ref); err != nil || got != fixture.state.commit {
		t.Fatalf("retire dropped ref despite unknown entry: got=%q err=%v", got, err)
	}

	if err := os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if err := fixture.state.retire(ctx, fixture.repo); err != nil {
		t.Fatalf("retire after removing unrelated entry: %v", err)
	}
}

func TestRecoveryArtifactsArePrivateAndBinarySafe(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := t.Context()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte{0, 1, 2, 3, 0xff, 0, 4}
	if err := os.WriteFile(filepath.Join(info.Path, "binary.bin"), binary, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot, err := PatchSnapshot(ctx, info.Path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(snapshot); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot mode=%v err=%v, want 0600", info, err)
	}
	if data, err := os.ReadFile(snapshot); err != nil || len(data) == 0 {
		t.Fatalf("binary snapshot empty: len=%d err=%v", len(data), err)
	}

	originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
	opts := RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: "main", SnapshotDir: filepath.Dir(snapshot),
	}
	state, err := suspendWIP(ctx, opts, originalHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	journalInfo, err := os.Stat(state.journalPath)
	if err != nil || journalInfo.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode=%v err=%v, want 0600", journalInfo, err)
	}
	if err := state.restoreOriginal(ctx, info.Path, originalHead); err != nil {
		t.Fatal(err)
	}
	if err := state.retire(ctx, repo); err != nil {
		t.Fatalf("retire recovery state: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(info.Path, "binary.bin"))
	if err != nil || string(data) != string(binary) {
		t.Fatalf("binary replay=%v err=%v", data, err)
	}
}

func TestEnsureReplaysInterruptedDetachedRecovery(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := t.Context()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "a.txt"), "branch\n")
	mustGit(t, info.Path, "add", "a.txt")
	mustGit(t, info.Path, "commit", "-qm", "branch conflict")
	writeFile(t, filepath.Join(info.Path, "staged.txt"), "staged\n")
	mustGit(t, info.Path, "add", "staged.txt")
	writeFile(t, filepath.Join(info.Path, "untracked.txt"), "untracked\n")
	originalHead := strings.TrimSpace(mustGit(t, info.Path, "rev-parse", "HEAD"))
	statusBefore := mustGit(t, info.Path, "status", "--porcelain=v1")

	writeFile(t, filepath.Join(repo, "a.txt"), "base\n")
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-qm", "base conflict")
	base := strings.TrimSpace(mustGit(t, repo, "rev-parse", "main"))
	snapshotDir := t.TempDir()
	snapshot, err := PatchSnapshot(ctx, info.Path, snapshotDir)
	if err != nil {
		t.Fatal(err)
	}
	opts := RecoveryOpts{
		RepoRoot: repo, Path: info.Path, Branch: info.Branch,
		Base: base, ExpectedHead: originalHead, SnapshotDir: snapshotDir,
	}
	_, err = suspendWIP(ctx, opts, originalHead, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	rebase, err := git(ctx, info.Path, "rebase", base)
	if err != nil {
		t.Fatal(err)
	}
	if rebase.ExitCode == 0 {
		t.Fatal("test rebase unexpectedly succeeded")
	}
	// The journal intentionally remains "suspended": this is the crash window
	// after Git entered a detached rebase but before Koryph checkpointed it.
	attached, err := Ensure(ctx, EnsureOpts{
		RepoRoot: repo, Branch: info.Branch, Base: base,
	})
	if err != nil {
		t.Fatalf("Ensure did not replay interrupted recovery: %v", err)
	}
	if attached.Created || attached.Branch != info.Branch || attached.Head != originalHead {
		t.Fatalf("Ensure replay result=%+v", attached)
	}
	if status := mustGit(t, info.Path, "status", "--porcelain=v1"); status != statusBefore {
		t.Fatalf("Ensure replay changed WIP:\nbefore=%q\nafter=%q", statusBefore, status)
	}
	if refs := strings.TrimSpace(mustGit(t, repo, "for-each-ref", "--format=%(refname)", "refs/koryph/recovery")); refs != "" {
		t.Fatalf("Ensure replay leaked recovery refs: %s", refs)
	}
}

func TestPatchSnapshotEmptyReturnsNoPath(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	p, err := PatchSnapshot(ctx, info.Path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if p != "" {
		t.Errorf("clean worktree should yield no patch, got %q", p)
	}
}
