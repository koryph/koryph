// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// mergePrepareCommitMsg is the conventional-commit subject koryph uses for the
// single normalization commit a merge_prepare step may leave behind. It is
// conventional so it clears the same commit-style expectation the bead's own
// commits do, and fixed so the message is never agent- or command-controlled.
const mergePrepareCommitMsg = "chore(merge): normalize generated artifacts for merge"

// runMergePrepare runs the project's merge_prepare commands in the worktree
// after a (possibly reconciler-healed) rebase and BEFORE the gate, then commits
// any resulting change so it rides the fast-forward merge and is gated. This is
// the merge-time migration-number allocation seam (design
// docs/designs/2026-07-merge-reconcilers.md L6): a renumber-to-tip command
// re-slots a newly added migration to the next free sequence against the branch
// it is landing on, so two in-flight beads never land a duplicate number and no
// renumber cascade is possible. The commands see KORYPH_DEFAULT_BRANCH.
//
//   - prepared=true when the commands left a change koryph committed; false on a
//     no-op (the common case — nothing needed normalizing).
//   - ok=false when a command exits non-zero: the caller returns
//     StatusGateFailed, the same requeue path as a gate regression.
//   - err is a git-plumbing failure (status/add/commit).
func runMergePrepare(ctx context.Context, wtPath, def string, cmds []string) (prepared, ok bool, output string, err error) {
	if len(cmds) == 0 {
		return false, true, "", nil
	}
	// The gate's allowlisted env (no orchestrator secrets), plus the branch the
	// rebased tree is landing on so a renumber-to-tip command can diff it.
	env := append(execx.GateEnv(), "KORYPH_DEFAULT_BRANCH="+def)
	var b strings.Builder
	for _, c := range cmds {
		name, args := shellCmd(wtPath, c)
		b.WriteString("$ ")
		b.WriteString(c)
		b.WriteString("\n")
		res, rerr := execx.Run(ctx, execx.Cmd{Dir: wtPath, Name: name, Args: args, Env: env})
		if rerr == nil && res.ExitCode != 0 && name == "direnv" && strings.Contains(res.Stderr, "is blocked") {
			// Fresh agent worktrees carry a never-approved .envrc; fall back to a
			// plain shell exactly as the gate does.
			b.WriteString("(direnv blocked; running without direnv)\n")
			res, rerr = execx.Run(ctx, execx.Cmd{Dir: wtPath, Name: "sh", Args: []string{"-c", c}, Env: env})
		}
		b.WriteString(res.Stdout)
		b.WriteString(res.Stderr)
		if rerr != nil {
			b.WriteString("\nerror: ")
			b.WriteString(rerr.Error())
			b.WriteString("\n")
			return false, false, b.String(), nil
		}
		if res.ExitCode != 0 {
			return false, false, b.String(), nil
		}
	}
	// Commit any change so it lands with the branch and passes through the gate.
	// A clean tree is a no-op — most merges need no normalization. koryph makes
	// the commit itself (not the command) so the message is conventional and the
	// signature configuration matches the rebase that just ran.
	dirty, derr := worktreeDirty(ctx, wtPath)
	if derr != nil {
		return false, false, b.String(), derr
	}
	if !dirty {
		return false, true, b.String(), nil
	}
	// Stage everything the prepare commands changed, but never the engine's own
	// CONFLICT.md breadcrumb: a stale copy left in a reused worktree would ride
	// this commit and trip a project markdownlint hook, failing the merge (D13).
	if _, aerr := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"add", "-A", "--", ".", ":(exclude)" + conflictBreadcrumb},
	}); aerr != nil {
		return false, false, b.String(), aerr
	}
	if _, cerr := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"commit", "-m", mergePrepareCommitMsg},
	}); cerr != nil {
		return false, false, b.String(), cerr
	}
	return true, true, b.String(), nil
}

// worktreeDirty reports whether the worktree has any staged, unstaged, or
// untracked change.
func worktreeDirty(ctx context.Context, wtPath string) (bool, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"status", "--porcelain"},
	})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}
