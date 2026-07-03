// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/worktree"
)

// SlotLocker serializes merges so only one branch lands at a time (a bd mutex
// in production). Acquire blocks (or retries) until the slot is held; Release
// frees it.
type SlotLocker interface {
	Acquire(ctx context.Context, owner string) error
	Release(ctx context.Context) error
}

// Protected returns the diff paths that match a DefaultProtected or extra
// prefix; a non-empty result rejects the merge outright.
func Protected(diffPaths, extra []string) []string {
	prefixes := make([]string, 0, len(DefaultProtected)+len(extra))
	prefixes = append(prefixes, DefaultProtected...)
	prefixes = append(prefixes, extra...)
	var hits []string
	for _, p := range diffPaths {
		for _, pre := range prefixes {
			if matchProtected(p, pre) {
				hits = append(hits, p)
				break
			}
		}
	}
	return hits
}

// matchProtected reports whether path falls under prefix. A prefix ending in
// "/" matches any path beneath it (or the bare directory itself); otherwise it
// matches the exact file or a path beneath a directory of that name. Matching
// is case-insensitive — so a case-insensitive filesystem like APFS cannot dodge
// `.github/` with `.Github/` — and the path is cleaned first, so `./.github/x`
// and `.github//x` still hit.
func matchProtected(path, prefix string) bool {
	p := strings.ToLower(filepath.ToSlash(filepath.Clean(path)))
	pre := strings.ToLower(prefix)
	if strings.HasSuffix(pre, "/") {
		return strings.HasPrefix(p, pre) || p == strings.TrimSuffix(pre, "/")
	}
	return p == pre || strings.HasPrefix(p, pre+"/")
}

// Merge lands o.Branch on the default branch. Expected non-success outcomes
// (protected, conflict, gate-failed) come back as a Result with that Status and
// a nil error; infrastructure and ff-only failures return a non-nil error.
func Merge(ctx context.Context, o Opts) (Result, error) {
	def := o.DefaultBranch
	if def == "" {
		def = "main"
	}

	// (1) merge slot — released on every exit path.
	if o.Slot != nil {
		if err := o.Slot.Acquire(ctx, o.SlotOwner); err != nil {
			return Result{Status: StatusError}, fmt.Errorf("acquire merge slot: %w", err)
		}
		defer func() { _ = o.Slot.Release(ctx) }()
	}

	// (2) locate the worktree that carries the branch.
	list, err := worktree.List(ctx, o.RepoRoot)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	var wt *worktree.Info
	for i := range list {
		if list[i].Branch == o.Branch {
			wt = &list[i]
			break
		}
	}
	if wt == nil {
		return Result{Status: StatusError}, fmt.Errorf("no worktree found for branch %q under %s", o.Branch, o.RepoRoot)
	}

	// (3) read-only preflight — protected paths, signatures, commit style. All
	// reject BEFORE any tree mutation (rebase would re-sign rewritten commits).
	if res, ok, err := preflight(ctx, o, wt, def); !ok {
		return res, err
	}

	// (4) sync the LOCAL default branch so the rebase base and the ff-merge
	// target are the SAME ref. The engine previously rebased onto origin/<def>
	// but ff-merged into local <def>; when the two diverged (e.g. a config
	// commit landed on local main but was not yet pushed) the rebase rewrote
	// the shared commit and the fast-forward became impossible, stranding every
	// in-flight branch. Fast-forward local <def> to origin, then rebase the
	// candidate onto local <def>. See koryph-3fs.
	hasRemote, err := remoteExists(ctx, o.RepoRoot)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"checkout", def},
	}); err != nil {
		return Result{Status: StatusError}, err
	}
	if hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"fetch", "origin", def},
		}); err != nil {
			return Result{Status: StatusError}, err
		}
		// Fast-forward local <def> up to origin/<def>. Local-ahead is a no-op
		// ("Already up to date"); a genuine divergence is surfaced as an error,
		// never force-reset.
		sync, err := gitRun(ctx, o.RepoRoot, "merge", "--ff-only", "origin/"+def)
		if err != nil {
			return Result{Status: StatusError}, err
		}
		if sync.ExitCode != 0 {
			return Result{Status: StatusError, GateOutput: tail(sync.Stdout+sync.Stderr, 2000)},
				fmt.Errorf("local %s cannot fast-forward to origin/%s (diverged); resolve before merging: %s",
					def, def, strings.TrimSpace(tail(sync.Stderr, 400)))
		}
	}

	// (5) rebase the worktree onto the LOCAL default (now synced to origin) —
	// the exact ref the ff-merge below targets.
	rb, err := gitRun(ctx, wt.Path, "rebase", def)
	if err != nil {
		return Result{Status: StatusError}, err
	}
	if rb.ExitCode != 0 {
		_, _ = gitRun(ctx, wt.Path, "rebase", "--abort")
		mdPath := filepath.Join(wt.Path, "CONFLICT.md")
		_ = fsx.WriteAtomic(mdPath, []byte(conflictMarkdown(o.Branch, def, rb.Stdout+rb.Stderr)), 0o644)
		return Result{Status: StatusConflict, ConflictMD: mdPath}, nil
	}

	// (6) green gate AFTER rebase.
	if !o.SkipGate && len(o.Gate) > 0 {
		ok, out := RunGate(ctx, wt.Path, o.Gate)
		if !ok {
			// pre-commit auto-fixers may leave the tree dirty; discard.
			_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
			return Result{Status: StatusGateFailed, GateOutput: tail(out, 2000)}, nil
		}
	}

	// (7) PR path diverges here: the branch is rebased, gated, and green.
	// Instead of fast-forward merging into the local default, push the branch
	// and open a pull request. The worktree and branch stay alive so a later
	// fast-forward landing step can resume them (koryph-ufy.1).
	if o.OpenPR {
		return openPR(ctx, o, def, hasRemote)
	}

	// (8) merge. RepoRoot is already on the synced <def>; the rebased branch is
	// now a strict fast-forward of it.
	if o.Squash {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"merge", "--squash", o.Branch},
		}); err != nil {
			return Result{Status: StatusError}, err
		}
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"commit", "-m", fmt.Sprintf("feat(%s): squash merge", o.Branch)},
		}); err != nil {
			return Result{Status: StatusError}, err
		}
	} else {
		ff, err := gitRun(ctx, o.RepoRoot, "merge", "--ff-only", o.Branch)
		if err != nil {
			return Result{Status: StatusError}, err
		}
		if ff.ExitCode != 0 {
			return Result{Status: StatusError, GateOutput: tail(ff.Stdout+ff.Stderr, 2000)},
				fmt.Errorf("ff-only merge of %q failed; the branch is not a fast-forward of %s (rebase or use squash): %s",
					o.Branch, def, strings.TrimSpace(tail(ff.Stderr, 400)))
		}
	}

	shaRes, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"rev-parse", "HEAD"},
	})
	if err != nil {
		return Result{Status: StatusError}, err
	}
	result := Result{Status: StatusMerged, MergedSHA: strings.TrimSpace(shaRes.Stdout)}

	// (9) push.
	if o.Push && hasRemote {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: o.RepoRoot, Name: "git", Args: []string{"push", "origin", def},
		}); err != nil {
			return result, fmt.Errorf("push origin %s: %w", def, err)
		}
		result.Pushed = true
	}

	// (10) cleanup — a dirty-tree removal failure downgrades to a warning.
	if !o.KeepWorktree {
		if err := worktree.Remove(ctx, wt.Path, false); err != nil {
			result.GateOutput = "cleanup-warning: worktree kept: " + err.Error()
		} else if err := worktree.DeleteBranch(ctx, o.RepoRoot, o.Branch); err != nil {
			result.GateOutput = "cleanup-warning: branch kept: " + err.Error()
		}
	}
	return result, nil
}

// preflight runs the read-only gates that must reject BEFORE any tree mutation:
// protected paths, commit-signature verification, and commit-style validation.
// These run before the rebase deliberately — rebasing rewrites commits, and
// with commit.gpgsign set the rewritten commits would be re-signed by the merge
// runner's own key, laundering unsigned agent work. ok=false means stop and
// return res: a rejection Result (StatusProtected/Unsigned/CommitStyle) with a
// nil error, or Result{StatusError} with the underlying error.
func preflight(ctx context.Context, o Opts, wt *worktree.Info, def string) (res Result, ok bool, err error) {
	// Protected-path check.
	diffPaths, err := diffNames(ctx, o.RepoRoot, def+"..."+o.Branch)
	if err != nil {
		return Result{Status: StatusError}, false, err
	}
	if hits := Protected(diffPaths, o.Extra); len(hits) > 0 {
		return Result{Status: StatusProtected, Protected: hits}, false, nil
	}

	// Commit-signature verification.
	if o.RequireSigned {
		bad, verr := signing.Verify(ctx, wt.Path, def, o.Branch)
		if verr != nil {
			return Result{Status: StatusError}, false, verr
		}
		if len(bad) > 0 {
			return Result{
				Status:     StatusUnsigned,
				GateOutput: "unsigned or unverifiable commits on " + o.Branch + ":\n" + strings.Join(bad, "\n"),
			}, false, nil
		}
	}

	// Commit-style validation: every candidate subject in <def>..<branch> must
	// match the Conventional Commits grammar (shared by ff-merge and PR paths).
	if o.RequireConventional {
		subjects, serr := logSubjects(ctx, o.RepoRoot, def, o.Branch)
		if serr != nil {
			return Result{Status: StatusError}, false, serr
		}
		if bad := nonConventionalSubjects(subjects); len(bad) > 0 {
			return Result{
				Status:     StatusCommitStyle,
				GateOutput: "non-conventional commit subject(s) on " + o.Branch + ":\n" + strings.Join(bad, "\n"),
			}, false, nil
		}
	}

	return Result{}, true, nil
}

// remoteExists reports whether the repo at dir has any configured git remote.
func remoteExists(ctx context.Context, dir string) (bool, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{Dir: dir, Name: "git", Args: []string{"remote"}})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}

// diffNames returns the non-empty file paths from `git diff --name-only <rev>`.
func diffNames(ctx context.Context, dir, rev string) ([]string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: dir, Name: "git", Args: []string{"diff", "--name-only", rev},
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(res.Stdout, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names = append(names, line)
		}
	}
	return names, nil
}

// gitRun runs a git subcommand in dir; a non-zero exit is not an error.
func gitRun(ctx context.Context, dir string, args ...string) (execx.Result, error) {
	return execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: args})
}

func conflictMarkdown(branch, base, output string) string {
	return fmt.Sprintf(
		"# Merge conflict\n\nRebasing `%s` onto `%s` before merge hit a conflict and was aborted; the\nworktree is unchanged and nothing was merged. Resolve manually, then retry.\n\n```\n%s\n```\n",
		branch, base, strings.TrimSpace(tail(output, 4000)),
	)
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
