// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
	return protectedAgainst(diffPaths, DefaultProtected, extra)
}

// ProtectedUnliftable is Protected minus the LiftableProtected subset
// (koryph-dcn): the check `--allow-protected` runs — routine CI/build paths
// are lifted, but agent-governance defaults and the project's extra paths
// still refuse.
func ProtectedUnliftable(diffPaths, extra []string) []string {
	base := make([]string, 0, len(DefaultProtected))
	for _, pre := range DefaultProtected {
		if !slices.Contains(LiftableProtected, pre) {
			base = append(base, pre)
		}
	}
	return protectedAgainst(diffPaths, base, extra)
}

// AllLiftable reports whether every protected-path hit falls under the
// LiftableProtected subset (.github/, Makefile) — i.e. `--allow-protected`
// would clear the block (koryph-zfg, F2). A hit matching any other governance
// default, or a project-declared protected path, is not liftable, so a mixed
// or governance-only touch returns false. An empty hit set returns false (no
// block to resolve).
func AllLiftable(hits []string) bool {
	if len(hits) == 0 {
		return false
	}
	for _, h := range hits {
		liftable := false
		for _, pre := range LiftableProtected {
			if matchProtected(h, pre) {
				liftable = true
				break
			}
		}
		if !liftable {
			return false
		}
	}
	return true
}

// protectedAgainst returns the diff paths matching any prefix in base+extra.
func protectedAgainst(diffPaths, base, extra []string) []string {
	prefixes := make([]string, 0, len(base)+len(extra))
	prefixes = append(prefixes, base...)
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
//
// Merge is a thin logging wrapper around mergeInner: it is the single choke
// point every caller passes through — the engine's auto-merge loop
// (internal/engine/poll.go), `koryph land` (internal/engine/land.go), and the
// operator-invoked `koryph merge` CLI (cmd/koryph/ops.go) — so instrumenting
// here, once, closes the "internal/merge has zero obs/slog calls" gap for all
// three instead of duplicating logging at each call site. Before this, an
// operator running `koryph merge`/`koryph land` directly left no structured
// telemetry trail at all; only the engine's wave loop separately audited its
// own merges (auditBlocked in poll.go).
func Merge(ctx context.Context, o Opts) (Result, error) {
	start := time.Now()
	res, err := mergeInner(ctx, o)
	logResult(o, res, err, time.Since(start))
	return res, err
}

func mergeInner(ctx context.Context, o Opts) (Result, error) {
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
	var reconciled []string
	var reconcileRounds int

	// D3: a prior attempt or a pipeline stage can leave the worktree dirty, and
	// `git rebase` then aborts with "cannot rebase: You have unstaged changes",
	// which looks exactly like a content conflict and parks a landable bead.
	// Uncommitted state is never part of the branch and never merges, so reset
	// the tracked tree to the branch tip first; the rebase then either succeeds
	// or fails on a genuine conflict.
	if dirty, derr := worktreeDirty(ctx, wt.Path); derr == nil && dirty {
		_, _ = gitRun(ctx, wt.Path, "reset", "--hard", "HEAD")
	}

	// D2: when the branch already contains <def> (e.g. it merged main in once to
	// resolve conflicts, and carries that merge commit), it is already a
	// fast-forward of the target and needs no rebase — and rebasing would
	// flatten the merge commit and re-raise the resolved conflict, parking a
	// branch that would otherwise land cleanly. Skip straight to the gate and
	// ff-merge in that case.
	rebaseNeeded := true
	if anc, aerr := gitRun(ctx, wt.Path, "merge-base", "--is-ancestor", def, "HEAD"); aerr == nil && anc.ExitCode == 0 {
		rebaseNeeded = false
	}
	if rebaseNeeded {
		rb, rerr := gitRun(ctx, wt.Path, "rebase", def)
		if rerr != nil {
			return Result{Status: StatusError}, rerr
		}
		if rb.ExitCode != 0 {
			// A rebase conflict confined ENTIRELY to configured generated files
			// (a migrations lockfile, a secrets baseline) self-heals: regenerate
			// each from the post-merge tree and continue, instead of aborting.
			// Any conflict touching a path with no reconciler — or any reconciler
			// failure — falls through to the unchanged abort path (design
			// docs/designs/2026-07-merge-reconcilers.md, invariants I1/I2).
			healed, paths, rounds, herr := reconcileRebase(ctx, wt.Path, o.Reconcilers)
			if herr != nil {
				_, _ = gitRun(ctx, wt.Path, "rebase", "--abort")
				return Result{Status: StatusError}, herr
			}
			if !healed {
				_, _ = gitRun(ctx, wt.Path, "rebase", "--abort")
				mdPath := filepath.Join(wt.Path, conflictBreadcrumb)
				_ = fsx.WriteAtomic(mdPath, []byte(conflictMarkdown(o.Branch, def, rb.Stdout+rb.Stderr)), 0o644)
				return Result{Status: StatusConflict, ConflictMD: mdPath}, nil
			}
			reconciled, reconcileRounds = paths, rounds
		}
	}

	// (5b) merge_prepare: normalize the rebased tree before the gate — its
	// canonical use is renumbering a newly added migration to the next free
	// sequence at merge time so two in-flight beads never land a duplicate
	// number (the renumber-cascade root cause; design
	// docs/designs/2026-07-merge-reconcilers.md L6). koryph commits any change
	// so it rides the ff-merge and is gated. A command regression is a
	// gate-shaped failure (requeue), not a hard error.
	prepared := false
	if len(o.Prepare) > 0 {
		p, pok, pout, perr := runMergePrepare(ctx, wt.Path, def, o.Prepare)
		if perr != nil {
			return Result{Status: StatusError}, perr
		}
		if !pok {
			// Discard any partial modification the failing step left, mirroring
			// the gate-failure cleanup, then requeue.
			_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
			return Result{Status: StatusGateFailed, GateOutput: tail(pout, gateOutputCap)}, nil
		}
		prepared = p
	}

	// (6) green gate AFTER rebase. A first failure is retried once from a clean
	// tree before it is allowed to count. The gate compiles and runs the
	// project's whole suite, so a load-sensitive flake there would otherwise
	// force a wasted requeue — and eventually a costly model escalation — on a
	// bead whose own code is fine. The retry only ever absorbs a transient
	// failure: a deterministic gate failure fails again and still returns
	// gate-failed, so a real regression is never masked (a genuinely
	// nondeterministic regression is the only edge the retry can hide — an
	// accepted trade against penalizing every infra flake).
	if !o.SkipGate && len(o.Gate) > 0 {
		ok, out := RunGate(ctx, wt.Path, o.Gate)
		if !ok {
			// pre-commit auto-fixers or a partial step may leave the tree dirty;
			// discard so the retry runs against the same clean state as the first.
			_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
			ok, out = RunGate(ctx, wt.Path, o.Gate)
		}
		if !ok {
			_, _ = gitRun(ctx, wt.Path, "checkout", "--", ".")
			// Keep a generous tail: the engine persists this to
			// <phase-dir>/gate-output.log, so a 2 KB clip was often too small to
			// see which gate command actually failed on a large build/test run.
			return Result{Status: StatusGateFailed, GateOutput: tail(out, gateOutputCap)}, nil
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
	result := Result{
		Status:          StatusMerged,
		MergedSHA:       strings.TrimSpace(shaRes.Stdout),
		Reconciled:      reconciled,
		ReconcileRounds: reconcileRounds,
		Prepared:        prepared,
	}

	// (9) push. Best-effort by design: a push is skipped when no remote exists so
	// the engine's auto-merge on a local-only project still lands (see poll.go's
	// "merge itself skips push when no remote exists"). Result.Pushed records
	// whether it actually happened, so a caller that REQUIRED the push — the
	// `koryph merge --push` CLI — can detect a no-op and refuse to report success
	// (koryph-8eh, enforced in cmdMerge, not here).
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
	// Protected-path check. AllowProtected (operator CLI only, koryph-dcn)
	// lifts just the LiftableProtected subset; governance defaults and the
	// project's extra paths always refuse.
	diffPaths, err := diffNames(ctx, o.RepoRoot, def+"..."+o.Branch)
	if err != nil {
		return Result{Status: StatusError}, false, err
	}
	check := Protected
	if o.AllowProtected {
		check = ProtectedUnliftable
	}
	if hits := check(diffPaths, o.Extra); len(hits) > 0 {
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

// conflictBreadcrumb is the file the engine drops in a worktree to explain an
// aborted-rebase conflict. It is an engine artifact, never part of the branch:
// it must be kept out of any engine-authored commit (a stale copy left in a
// reused worktree was once staged into the merge-normalization commit and the
// project's markdownlint rejected it, failing the merge — see mergeAddSpec).
const conflictBreadcrumb = "CONFLICT.md"

// conflictMarkdown renders the CONFLICT.md breadcrumb. The captured rebase
// output is fenced as ```text (not a bare ```): a bare fence trips
// markdownlint MD040 (fenced-code-language), and though koryph never commits
// this file, a project whose gate lints all *.md still flagged it.
func conflictMarkdown(branch, base, output string) string {
	return fmt.Sprintf(
		"# Merge conflict\n\nRebasing `%s` onto `%s` before merge hit a conflict and was aborted; the\nworktree is unchanged and nothing was merged. Resolve manually, then retry.\n\n```text\n%s\n```\n",
		branch, base, strings.TrimSpace(tail(output, 4000)),
	)
}

// gateOutputCap bounds the gate output carried in Result.GateOutput. It is
// generous (16 KB) because the engine persists this to <phase-dir>/gate-output.log
// for post-hoc diagnosis; a small clip lost the actual failing command on large
// build/test runs.
const gateOutputCap = 16000

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
