// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package worktree

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/textx"
)

// PatchSnapshot writes a WIP patch containing the complete tracked worktree
// delta plus untracked files to outDir. It returns "" without writing when
// there is nothing to capture.
//
// This deliberately does not use the historical `git add -N` + `git reset`
// technique. A plain reset destroys an already-staged index, so taking a
// snapshot could itself corrupt the exact WIP it was meant to preserve.
func PatchSnapshot(ctx context.Context, path, outDir string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"diff", "--binary", "HEAD", "--"},
	})
	if err != nil {
		return "", err
	}
	var patch strings.Builder
	patch.WriteString(res.Stdout)

	untracked, err := untrackedFiles(ctx, path)
	if err != nil {
		return "", err
	}
	for _, rel := range untracked {
		// `git diff --no-index` returns 1 when it successfully found a
		// difference. Use the raw runner so that expected status is not
		// mistaken for an execution failure.
		part, runErr := git(ctx, path, "diff", "--no-index", "--binary", "--", "/dev/null", rel)
		if runErr != nil {
			return "", runErr
		}
		if part.ExitCode != 0 && part.ExitCode != 1 {
			return "", fmt.Errorf("snapshot untracked file %s: git diff exited %d: %s",
				rel, part.ExitCode, strings.TrimSpace(part.Stderr))
		}
		patch.WriteString(part.Stdout)
	}
	if strings.TrimSpace(patch.String()) == "" {
		return "", nil
	}
	if err := os.MkdirAll(outDir, 0o700); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for sequence := 0; ; sequence++ {
		out := filepath.Join(outDir, fmt.Sprintf("wip-%s-%d-%d.patch", stamp, os.Getpid(), sequence))
		file, err := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		remove := true
		defer func() {
			if remove {
				_ = os.Remove(out)
			}
		}()
		if _, err := file.WriteString(patch.String()); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
		if dir, err := os.Open(outDir); err == nil {
			err = dir.Sync()
			_ = dir.Close()
			if err != nil {
				return "", err
			}
		}
		remove = false
		return out, nil
	}
}

func replayIncompleteRecovery(ctx context.Context, o RecoveryOpts) error {
	parent := filepath.Dir(o.Path)
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	var matches []string
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), ".koryph-recovery-") {
			continue
		}
		journalPath := filepath.Join(parent, entry.Name(), "recovery-journal.json")
		info, err := os.Lstat(journalPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
			return fmt.Errorf("recovery journal %s must be a regular 0600 file", journalPath)
		}
		var journal recoveryJournal
		if err := fsx.ReadJSON(journalPath, &journal); err != nil {
			return err
		}
		if journal.SchemaVersion != 1 {
			return fmt.Errorf("recovery journal %s has unsupported schema %d", journalPath, journal.SchemaVersion)
		}
		if !samePath(journal.WorktreePath, o.Path) {
			continue
		}
		matches = append(matches, journalPath)
	}
	if len(matches) == 0 {
		return nil
	}
	if len(matches) != 1 {
		return fmt.Errorf("worktree %s has %d recovery journals; refusing ambiguous replay", o.Path, len(matches))
	}

	journalPath := matches[0]
	var journal recoveryJournal
	if err := fsx.ReadJSON(journalPath, &journal); err != nil {
		return err
	}
	if !validRecoveryPhase(journal.Phase) {
		return fmt.Errorf("recovery journal %s has unknown phase %q", journalPath, journal.Phase)
	}
	if journal.Branch != o.Branch {
		return fmt.Errorf("recovery journal branch %q does not match requested branch %q", journal.Branch, o.Branch)
	}
	if err := validateRecoveryContainer(ctx, o); err != nil {
		return err
	}

	state := suspendedWIP{
		ref:         journal.RecoveryRef,
		commit:      journal.RecoveryCommit,
		holdingDir:  filepath.Dir(journalPath),
		journalPath: journalPath,
		untracked:   journal.Untracked,
		hasTracked:  journal.RecoveryCommit != "",
		journal:     journal,
	}
	if state.hasTracked {
		refHead, err := revParseCommit(ctx, o.RepoRoot, state.ref)
		if err != nil || refHead != state.commit {
			return fmt.Errorf("recovery ref %s does not authenticate checkpoint %s", state.ref, state.commit)
		}
	}

	replayCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := state.restoreOriginal(replayCtx, o.Path, journal.OriginalHead); err != nil {
		return fmt.Errorf("replay phase %s from %s: %w", journal.Phase, journalPath, err)
	}
	if err := validateRecoveryOwnership(replayCtx, o, journal.OriginalHead); err != nil {
		return fmt.Errorf("validate replayed phase %s from %s: %w", journal.Phase, journalPath, err)
	}
	if err := state.retire(replayCtx, o.RepoRoot); err != nil {
		return fmt.Errorf("retire replayed recovery state: %w", err)
	}
	return nil
}

func validRecoveryPhase(phase string) bool {
	switch phase {
	case recoveryPhasePrepared, recoveryPhaseUntrackedHeld, recoveryPhaseSuspended,
		recoveryPhaseRebased, recoveryPhaseTrackedRestored, recoveryPhaseRestored:
		return true
	default:
		return false
	}
}

func validateRecoveryContainer(ctx context.Context, o RecoveryOpts) error {
	infos, err := List(ctx, o.RepoRoot)
	if err != nil {
		return err
	}
	found := false
	for _, info := range infos {
		if samePath(info.Path, o.Path) {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("path %s is no longer a registered worktree of %s", o.Path, o.RepoRoot)
	}
	owner, err := mainRepo(ctx, o.Path)
	if err != nil {
		return err
	}
	if !samePath(owner, o.RepoRoot) {
		return fmt.Errorf("worktree %s belongs to repository %s, not %s", o.Path, owner, o.RepoRoot)
	}
	top, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.Path, Name: "git", Args: []string{"rev-parse", "--show-toplevel"},
	})
	if err != nil {
		return err
	}
	if !samePath(strings.TrimSpace(top.Stdout), o.Path) {
		return fmt.Errorf("worktree top-level changed from %s to %s", o.Path, strings.TrimSpace(top.Stdout))
	}
	return nil
}

func validateRecoveryOwnership(ctx context.Context, o RecoveryOpts, expectedHead string) error {
	if err := validateRecoveryContainer(ctx, o); err != nil {
		return err
	}
	infos, err := List(ctx, o.RepoRoot)
	if err != nil {
		return err
	}
	for _, info := range infos {
		if !samePath(info.Path, o.Path) {
			continue
		}
		if info.Branch != o.Branch {
			return fmt.Errorf("worktree branch changed from %q to %q", o.Branch, info.Branch)
		}
		if info.Head != expectedHead {
			return fmt.Errorf("worktree HEAD changed from %s to %s", expectedHead, info.Head)
		}
		break
	}
	symbolic, err := git(ctx, o.Path, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return err
	}
	if symbolic.ExitCode != 0 || strings.TrimSpace(symbolic.Stdout) != o.Branch {
		return fmt.Errorf("worktree symbolic branch changed from %q to %q",
			o.Branch, strings.TrimSpace(symbolic.Stdout))
	}
	head, err := revParseCommit(ctx, o.Path, "HEAD")
	if err != nil {
		return err
	}
	if head != expectedHead {
		return fmt.Errorf("worktree HEAD changed from %s to %s", expectedHead, head)
	}
	return nil
}

// RecoverOntoBase safely refreshes a previously registered agent worktree
// before a reopened bead is launched again. It preserves three independent
// pieces of state:
//
//   - the index (including staged-vs-unstaged distinctions) in a private,
//     temporary stash commit referenced under refs/koryph/recovery;
//   - untracked files by moving them to a same-filesystem private directory;
//   - a human-readable, non-mutating PatchSnapshot in SnapshotDir.
//
// The branch is rebased only while clean. On a rebase or WIP-restore conflict,
// the branch and WIP are rolled back to their exact original state and an
// error is returned, so an engine must fail closed instead of dispatching
// against a stale checkout. If even rollback cannot complete, the private ref
// and untracked holding directory are retained and named in the error.
func RecoverOntoBase(ctx context.Context, o RecoveryOpts) (RecoveryResult, error) {
	var out RecoveryResult
	if strings.TrimSpace(o.Path) == "" || strings.TrimSpace(o.Branch) == "" ||
		strings.TrimSpace(o.Base) == "" || strings.TrimSpace(o.SnapshotDir) == "" {
		return out, fmt.Errorf("recover worktree: path, branch, base, and snapshot directory are required")
	}
	if err := replayIncompleteRecovery(ctx, o); err != nil {
		return out, fmt.Errorf("replay interrupted worktree recovery: %w", err)
	}

	head, err := revParseCommit(ctx, o.Path, "HEAD")
	if err != nil {
		return out, fmt.Errorf("recover worktree head: %w", err)
	}
	out.OriginalHead = head
	if o.ExpectedHead != "" && o.ExpectedHead != head {
		return out, fmt.Errorf("attached worktree HEAD changed after Ensure: observed %s, now %s",
			o.ExpectedHead, head)
	}
	if err := validateRecoveryOwnership(ctx, o, out.OriginalHead); err != nil {
		return out, fmt.Errorf("revalidate attached worktree before recovery: %w", err)
	}

	dirty, err := IsDirty(ctx, o.Path)
	if err != nil {
		return out, fmt.Errorf("recover worktree status: %w", err)
	}
	out.HadWIP = dirty
	if dirty {
		out.WIPSnapshotPath, err = PatchSnapshot(ctx, o.Path, o.SnapshotDir)
		if err != nil {
			return out, fmt.Errorf("snapshot recovered WIP: %w", err)
		}
	}
	if err := validateRecoveryOwnership(ctx, o, out.OriginalHead); err != nil {
		return out, fmt.Errorf("revalidate attached worktree before checkpoint: %w", err)
	}

	state, err := suspendWIP(ctx, o, out.OriginalHead, out.WIPSnapshotPath)
	if err != nil {
		return out, fmt.Errorf("checkpoint recovered WIP: %w", err)
	}
	rollback := func() error {
		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		return state.restoreOriginal(rollbackCtx, o.Path, out.OriginalHead)
	}
	cleanup := func() error {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		return state.retire(cleanupCtx, o.RepoRoot)
	}
	withCleanup := func(cause error) error {
		if cleanupErr := cleanup(); cleanupErr != nil {
			return fmt.Errorf("%v; retire recovery state: %w", cause, cleanupErr)
		}
		return cause
	}

	if err := validateRecoveryOwnership(ctx, o, out.OriginalHead); err != nil {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("revalidate before rebase: %v; rollback failed: %w; preserved ref=%s files=%s",
				err, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf("revalidate before rebase: %w", err))
	}
	if o.beforeRebase != nil {
		o.beforeRebase()
	}
	rebase, runErr := git(ctx, o.Path, "rebase", o.Base)
	if runErr != nil {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("run recovery rebase: %v; rollback failed: %w; preserved ref=%s files=%s",
				runErr, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf("run recovery rebase: %w", runErr))
	}
	if rebase.ExitCode != 0 {
		detail := strings.TrimSpace(rebase.Stdout + rebase.Stderr)
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("rebase %s onto %s conflicted: %s; rollback failed: %w; preserved ref=%s files=%s",
				o.Branch, o.Base, textx.Tail(detail, 2000), restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf(
			"rebase %s onto %s conflicted; original worktree restored: %s",
			o.Branch, o.Base, textx.Tail(detail, 2000),
		))
	}

	newHead, err := revParseCommit(ctx, o.Path, "HEAD")
	if err != nil {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("verify refreshed head: %v; rollback failed: %w; preserved ref=%s files=%s",
				err, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf("verify refreshed head: %w", err))
	}
	ancestor, runErr := git(ctx, o.Path, "merge-base", "--is-ancestor", o.Base, newHead)
	if runErr != nil || ancestor.ExitCode != 0 {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("refreshed head %s does not descend from %s; rollback failed: %w; preserved ref=%s files=%s",
				newHead, o.Base, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf(
			"refreshed head %s does not descend from dispatch base %s; original worktree restored",
			newHead, o.Base,
		))
	}
	if err := state.checkpoint(recoveryPhaseRebased, newHead); err != nil {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("checkpoint rebased recovery: %v; rollback failed: %w; preserved ref=%s files=%s",
				err, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf("checkpoint rebased recovery: %w", err))
	}
	if err := validateRecoveryOwnership(ctx, o, newHead); err != nil {
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("revalidate before WIP restore: %v; rollback failed: %w; preserved ref=%s files=%s",
				err, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf("revalidate before WIP restore: %w", err))
	}

	if err := state.restoreOntoCurrent(ctx, o.Path); err != nil {
		// Restoring WIP onto the refreshed base can itself conflict (for
		// example, main gained a path that was previously untracked). Put the
		// branch and all WIP back exactly where they started.
		if restoreErr := rollback(); restoreErr != nil {
			return out, fmt.Errorf("restore WIP onto refreshed base: %v; rollback failed: %w; preserved ref=%s files=%s",
				err, restoreErr, state.ref, state.holdingDir)
		}
		return out, withCleanup(fmt.Errorf(
			"restore WIP onto refreshed base conflicted; original worktree restored: %w",
			err,
		))
	}

	out.Head = newHead
	out.Refreshed = newHead != out.OriginalHead
	if err := cleanup(); err != nil {
		return out, fmt.Errorf("retire completed recovery state: %w", err)
	}
	return out, nil
}

type suspendedWIP struct {
	ref         string
	commit      string
	holdingDir  string
	journalPath string
	untracked   []recoveryFile
	hasTracked  bool
	journal     recoveryJournal
}

type recoveryFile struct {
	Path       string `json:"path"`
	Mode       uint32 `json:"mode"`
	Digest     string `json:"digest"`
	LinkTarget string `json:"link_target,omitempty"`
}

type recoveryJournal struct {
	SchemaVersion   int            `json:"schema_version"`
	Phase           string         `json:"phase"`
	WorktreePath    string         `json:"worktree_path"`
	Branch          string         `json:"branch"`
	Base            string         `json:"base"`
	OriginalHead    string         `json:"original_head"`
	RecoveryRef     string         `json:"recovery_ref,omitempty"`
	RecoveryCommit  string         `json:"recovery_commit,omitempty"`
	RefreshedHead   string         `json:"refreshed_head,omitempty"`
	WIPSnapshotPath string         `json:"wip_snapshot_path,omitempty"`
	Untracked       []recoveryFile `json:"untracked,omitempty"`
}

const (
	recoveryPhasePrepared        = "prepared"
	recoveryPhaseUntrackedHeld   = "untracked-held"
	recoveryPhaseSuspended       = "suspended"
	recoveryPhaseRebased         = "rebased"
	recoveryPhaseTrackedRestored = "tracked-restored"
	recoveryPhaseRestored        = "restored"
)

func suspendWIP(ctx context.Context, o RecoveryOpts, originalHead, snapshotPath string) (suspendedWIP, error) {
	state := suspendedWIP{}
	untracked, err := untrackedInventory(ctx, o.Path)
	if err != nil {
		return state, err
	}
	state.untracked = untracked

	stash, err := git(ctx, o.Path, "stash", "create", "koryph recovery checkpoint")
	if err != nil {
		return state, err
	}
	if stash.ExitCode != 0 {
		return state, fmt.Errorf("git stash create exited %d: %s",
			stash.ExitCode, strings.TrimSpace(stash.Stderr))
	}
	state.commit = strings.TrimSpace(stash.Stdout)
	state.hasTracked = state.commit != ""
	if state.hasTracked {
		state.ref = fmt.Sprintf("refs/koryph/recovery/%d-%d", os.Getpid(), time.Now().UnixNano())
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.Path, Name: "git", Args: []string{"update-ref", state.ref, state.commit},
		}); err != nil {
			return state, err
		}
	}

	// Keep untracked data on the same filesystem as the worktree so each move
	// is atomic. The path is retained on rollback failure and included in the
	// returned error for manual recovery.
	holdingParent := filepath.Dir(o.Path)
	state.holdingDir, err = os.MkdirTemp(holdingParent, ".koryph-recovery-")
	if err != nil {
		if dropErr := state.dropRef(ctx, o.Path); dropErr != nil {
			return state, fmt.Errorf("%v; drop recovery ref: %w", err, dropErr)
		}
		return state, err
	}
	state.journalPath = filepath.Join(state.holdingDir, "recovery-journal.json")
	state.journal = recoveryJournal{
		SchemaVersion:   1,
		Phase:           recoveryPhasePrepared,
		WorktreePath:    o.Path,
		Branch:          o.Branch,
		Base:            o.Base,
		OriginalHead:    originalHead,
		RecoveryRef:     state.ref,
		RecoveryCommit:  state.commit,
		WIPSnapshotPath: snapshotPath,
		Untracked:       state.untracked,
	}
	if err := state.saveJournal(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if cleanupErr := state.retireWith(cleanupCtx, o.Path, func(s *suspendedWIP) error {
			return retireEmptyRecoveryHoldingDurably(s.holdingDir)
		}); cleanupErr != nil {
			return state, fmt.Errorf("%v; retire failed recovery journal: %w", err, cleanupErr)
		}
		return state, err
	}
	if err := validateRecoveryOwnership(ctx, o, originalHead); err != nil {
		return state, fmt.Errorf("revalidate before untracked quarantine: %w; recovery journal retained at %s",
			err, state.journalPath)
	}
	if err := moveRecoveryFiles(o.Path, state.holdingDir, state.untracked); err != nil {
		return state, fmt.Errorf(
			"%w; recovery ref %s and journal retained at %s",
			err, state.ref, state.journalPath,
		)
	}
	if err := state.checkpoint(recoveryPhaseUntrackedHeld, ""); err != nil {
		return state, fmt.Errorf("checkpoint untracked quarantine: %w; recovery journal retained at %s",
			err, state.journalPath)
	}

	if state.hasTracked {
		if err := validateRecoveryOwnership(ctx, o, originalHead); err != nil {
			return state, fmt.Errorf("revalidate before tracked reset: %w; recovery journal retained at %s",
				err, state.journalPath)
		}
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.Path, Name: "git", Args: []string{"reset", "--hard", "HEAD"},
		}); err != nil {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if restoreErr := state.restoreOriginal(rollbackCtx, o.Path, originalHead); restoreErr != nil {
				return state, fmt.Errorf("%v; rollback failed: %w; preserved ref=%s files=%s",
					err, restoreErr, state.ref, state.holdingDir)
			}
			if cleanupErr := state.retire(rollbackCtx, o.Path); cleanupErr != nil {
				return state, fmt.Errorf("%v; retire restored recovery state: %w", err, cleanupErr)
			}
			return state, err
		}
	}
	if err := state.checkpoint(recoveryPhaseSuspended, ""); err != nil {
		return state, fmt.Errorf("checkpoint suspended WIP: %w; recovery journal retained at %s",
			err, state.journalPath)
	}
	return state, nil
}

func (s *suspendedWIP) saveJournal() error {
	return fsx.WriteJSONAtomicPerm(s.journalPath, s.journal, 0o600)
}

func (s *suspendedWIP) checkpoint(phase, refreshedHead string) error {
	s.journal.Phase = phase
	if refreshedHead != "" {
		s.journal.RefreshedHead = refreshedHead
	}
	return s.saveJournal()
}

func (s *suspendedWIP) restoreOntoCurrent(ctx context.Context, path string) error {
	if s.hasTracked {
		applied, err := git(ctx, path, "stash", "apply", "--index", s.commit)
		if err != nil {
			return err
		}
		if applied.ExitCode != 0 {
			return fmt.Errorf("git stash apply exited %d: %s",
				applied.ExitCode, strings.TrimSpace(applied.Stdout+applied.Stderr))
		}
	}
	if err := s.checkpoint(recoveryPhaseTrackedRestored, ""); err != nil {
		return err
	}
	for _, file := range s.untracked {
		if pathExists(filepath.Join(path, file.Path)) {
			return fmt.Errorf("untracked path %s now exists on refreshed base", file.Path)
		}
	}
	if err := moveRecoveryFiles(s.holdingDir, path, s.untracked); err != nil {
		return err
	}
	return s.checkpoint(recoveryPhaseRestored, "")
}

func (s *suspendedWIP) restoreOriginal(ctx context.Context, path, originalHead string) error {
	_, _ = git(ctx, path, "rebase", "--abort")
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"reset", "--hard", originalHead},
	}); err != nil {
		return err
	}
	if s.hasTracked {
		applied, err := git(ctx, path, "stash", "apply", "--index", s.commit)
		if err != nil {
			return err
		}
		if applied.ExitCode != 0 {
			return fmt.Errorf("restore checkpoint exited %d: %s",
				applied.ExitCode, strings.TrimSpace(applied.Stdout+applied.Stderr))
		}
	}
	return restoreRecoveryInventory(s.holdingDir, path, s.untracked)
}

// retire removes the durable journal/holding directory and fsyncs its parent
// before deleting the private ref that authenticates the journal. A crash or
// removal failure therefore leaves either a fully replayable journal+ref pair
// or, after durable journal retirement, a harmless orphan ref.
func (s *suspendedWIP) retire(ctx context.Context, repo string) error {
	return s.retireWith(ctx, repo, retireRecoveryHoldingDurably)
}

func (s *suspendedWIP) retireWith(
	ctx context.Context,
	repo string,
	retireHolding func(*suspendedWIP) error,
) error {
	if s.holdingDir != "" {
		if err := retireHolding(s); err != nil {
			return fmt.Errorf("retire recovery holding directory %s: %w", s.holdingDir, err)
		}
	}
	if err := s.dropRef(ctx, repo); err != nil {
		return fmt.Errorf("drop retired recovery ref %s: %w", s.ref, err)
	}
	return nil
}

// retireRecoveryHoldingDurably removes only paths authenticated by the
// recovery journal. Unknown files, directories, or symlinks cause a refusal
// before the journal is touched, so cleanup can never erase concurrent state.
func retireRecoveryHoldingDurably(s *suspendedWIP) error {
	holdingInfo, err := os.Lstat(s.holdingDir)
	if err != nil {
		return fmt.Errorf("inspect recovery holding directory: %w", err)
	}
	if !holdingInfo.IsDir() || holdingInfo.Mode()&os.ModeSymlink != 0 ||
		holdingInfo.Mode().Perm() != 0o700 {
		return fmt.Errorf("recovery holding path %s must be a real 0700 directory", s.holdingDir)
	}
	if !strings.HasPrefix(filepath.Base(s.holdingDir), ".koryph-recovery-") ||
		!samePath(filepath.Dir(s.holdingDir), filepath.Dir(s.journal.WorktreePath)) {
		return fmt.Errorf(
			"recovery holding directory %s is outside the journaled worktree sibling area",
			s.holdingDir,
		)
	}
	info, err := os.Lstat(s.journalPath)
	if err != nil {
		return fmt.Errorf("inspect recovery journal: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("recovery journal %s must be a regular 0600 file", s.journalPath)
	}
	var journal recoveryJournal
	if err := fsx.ReadJSON(s.journalPath, &journal); err != nil {
		return err
	}
	if journal.SchemaVersion != 1 ||
		!samePath(journal.WorktreePath, s.journal.WorktreePath) ||
		journal.Branch != s.journal.Branch ||
		journal.OriginalHead != s.journal.OriginalHead ||
		journal.RecoveryRef != s.ref ||
		journal.RecoveryCommit != s.commit {
		return fmt.Errorf("recovery journal %s no longer authenticates this transaction", s.journalPath)
	}

	expectedDirs := make(map[string]struct{})
	for _, file := range journal.Untracked {
		parent := filepath.Dir(filepath.Clean(file.Path))
		for parent != "." {
			if parent == ".." || filepath.IsAbs(parent) ||
				strings.HasPrefix(parent, ".."+string(filepath.Separator)) {
				return fmt.Errorf("unsafe recovery inventory parent %q", parent)
			}
			expectedDirs[parent] = struct{}{}
			parent = filepath.Dir(parent)
		}
	}
	journalRel, err := filepath.Rel(s.holdingDir, s.journalPath)
	if err != nil || journalRel != "recovery-journal.json" {
		return fmt.Errorf("recovery journal %s is outside holding directory %s", s.journalPath, s.holdingDir)
	}

	var dirs []string
	err = filepath.WalkDir(s.holdingDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(s.holdingDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			if !entry.IsDir() {
				return fmt.Errorf("recovery holding path %s is not a directory", path)
			}
			return nil
		}
		if rel == journalRel {
			if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
				return fmt.Errorf("recovery journal %s changed type during retirement", path)
			}
			return nil
		}
		if !entry.IsDir() {
			return fmt.Errorf("unexpected recovery holding entry %s", path)
		}
		if _, ok := expectedDirs[rel]; !ok {
			return fmt.Errorf("unexpected recovery holding directory %s", path)
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		return err
	}

	if err := fsx.RemoveDurable(s.journalPath); err != nil {
		return fmt.Errorf("remove recovery journal: %w", err)
	}
	sort.Slice(dirs, func(i, j int) bool {
		return strings.Count(dirs[i], string(filepath.Separator)) >
			strings.Count(dirs[j], string(filepath.Separator))
	})
	for _, dir := range dirs {
		if err := fsx.RemoveDurable(dir); err != nil {
			return fmt.Errorf("remove recovery inventory directory %s: %w", dir, err)
		}
	}
	if err := fsx.RemoveDurable(s.holdingDir); err != nil {
		return fmt.Errorf("remove recovery holding directory: %w", err)
	}
	return nil
}

func retireEmptyRecoveryHoldingDurably(path string) error {
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	if len(entries) != 0 {
		return fmt.Errorf("recovery holding directory %s is not empty after journal-write failure", path)
	}
	return fsx.RemoveDurable(path)
}

func (s *suspendedWIP) dropRef(ctx context.Context, repo string) error {
	if s.ref == "" || s.commit == "" {
		return nil
	}
	_, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: repo, Name: "git", Args: []string{"update-ref", "-d", s.ref, s.commit},
	})
	return err
}

func moveRecoveryFiles(from, to string, files []recoveryFile) error {
	moved := make([]recoveryFile, 0, len(files))
	for _, file := range files {
		rel := filepath.Clean(file.Path)
		if rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe untracked path %q", rel)
		}
		src := filepath.Join(from, rel)
		dst := filepath.Join(to, rel)
		if !pathExists(src) {
			return rollbackMovedFiles(from, to, moved, fmt.Errorf("untracked path %s is missing from recovery inventory", rel))
		}
		if err := validateRecoveryFile(src, file); err != nil {
			return rollbackMovedFiles(from, to, moved, fmt.Errorf("validate untracked path %s: %w", rel, err))
		}
		if pathExists(dst) {
			return rollbackMovedFiles(from, to, moved, fmt.Errorf("cannot restore untracked path %s: destination exists", rel))
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return rollbackMovedFiles(from, to, moved, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return rollbackMovedFiles(from, to, moved, fmt.Errorf("move untracked path %s: %w", rel, err))
		}
		if err := validateRecoveryFile(dst, file); err != nil {
			return rollbackMovedFiles(from, to, append(moved, file),
				fmt.Errorf("validate moved untracked path %s: %w", rel, err))
		}
		moved = append(moved, file)
	}
	return nil
}

// restoreRecoveryInventory reconciles an interrupted move. Each original file
// may be either still in the worktree (crash before its rename) or held in the
// recovery directory (crash after rename), but never both. Existing worktree
// content is accepted only after exact mode/digest/symlink validation.
func restoreRecoveryInventory(holdingDir, worktreePath string, files []recoveryFile) error {
	for _, file := range files {
		held := filepath.Join(holdingDir, file.Path)
		live := filepath.Join(worktreePath, file.Path)
		heldExists := pathExists(held)
		liveExists := pathExists(live)
		switch {
		case heldExists && liveExists:
			return fmt.Errorf("recovery collision for untracked path %s: both journal and worktree contain data", file.Path)
		case heldExists:
			if err := validateRecoveryFile(held, file); err != nil {
				return fmt.Errorf("validate held untracked path %s: %w", file.Path, err)
			}
			if err := os.MkdirAll(filepath.Dir(live), 0o700); err != nil {
				return err
			}
			if err := os.Rename(held, live); err != nil {
				return fmt.Errorf("restore held untracked path %s: %w", file.Path, err)
			}
			if err := validateRecoveryFile(live, file); err != nil {
				return fmt.Errorf("validate restored untracked path %s: %w", file.Path, err)
			}
		case liveExists:
			if err := validateRecoveryFile(live, file); err != nil {
				return fmt.Errorf("validate live untracked path %s: %w", file.Path, err)
			}
		default:
			return fmt.Errorf("recovery inventory lost untracked path %s", file.Path)
		}
	}
	return nil
}

func rollbackMovedFiles(from, to string, moved []recoveryFile, cause error) error {
	for i := len(moved) - 1; i >= 0; i-- {
		file := moved[i]
		src := filepath.Join(to, file.Path)
		dst := filepath.Join(from, file.Path)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("%v; partial-move rollback mkdir failed: %w", cause, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("%v; partial-move rollback failed for %s: %w", cause, file.Path, err)
		}
		if err := validateRecoveryFile(dst, file); err != nil {
			return fmt.Errorf("%v; partial-move rollback validation failed for %s: %w", cause, file.Path, err)
		}
	}
	return cause
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !os.IsNotExist(err)
}

func untrackedFiles(ctx context.Context, path string) ([]string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"ls-files", "--others", "--exclude-standard", "-z"},
	})
	if err != nil {
		return nil, err
	}
	var files []string
	for _, rel := range strings.Split(res.Stdout, "\x00") {
		if rel != "" {
			files = append(files, rel)
		}
	}
	return files, nil
}

func untrackedInventory(ctx context.Context, path string) ([]recoveryFile, error) {
	files, err := untrackedFiles(ctx, path)
	if err != nil {
		return nil, err
	}
	out := make([]recoveryFile, 0, len(files))
	for _, rel := range files {
		item, err := inspectRecoveryFile(filepath.Join(path, rel), rel)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, nil
}

func inspectRecoveryFile(path, rel string) (recoveryFile, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return recoveryFile{}, err
	}
	item := recoveryFile{Path: rel, Mode: uint32(info.Mode())}
	hash := sha256.New()
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return recoveryFile{}, err
		}
		item.LinkTarget = target
		_, _ = io.WriteString(hash, target)
	case info.Mode().IsRegular():
		file, err := os.Open(path)
		if err != nil {
			return recoveryFile{}, err
		}
		_, copyErr := io.Copy(hash, file)
		closeErr := file.Close()
		if copyErr != nil {
			return recoveryFile{}, copyErr
		}
		if closeErr != nil {
			return recoveryFile{}, closeErr
		}
	default:
		return recoveryFile{}, fmt.Errorf("unsupported untracked file type %s at %s", info.Mode(), rel)
	}
	item.Digest = fmt.Sprintf("sha256:%x", hash.Sum(nil))
	return item, nil
}

func validateRecoveryFile(path string, want recoveryFile) error {
	got, err := inspectRecoveryFile(path, want.Path)
	if err != nil {
		return err
	}
	if got.Mode != want.Mode || got.Digest != want.Digest || got.LinkTarget != want.LinkTarget {
		return fmt.Errorf("metadata/content changed: got mode=%#o digest=%s link=%q, want mode=%#o digest=%s link=%q",
			got.Mode, got.Digest, got.LinkTarget, want.Mode, want.Digest, want.LinkTarget)
	}
	return nil
}

func revParseCommit(ctx context.Context, path, rev string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"rev-parse", "--verify", rev + "^{commit}"},
	})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Stdout), nil
}
