// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// maxReconcileRounds bounds the auto-heal continue-loop (design
// docs/designs/2026-07-merge-reconcilers.md, invariant I4): a rebase that keeps
// producing generated-file conflicts past this many rounds aborts to
// StatusConflict rather than looping. The worst field cascade was 3 rounds
// (renumber-into-renumber-into-renumber); the cap is deliberately generous but
// finite so a misbehaving reconciler cannot spin forever.
const maxReconcileRounds = 20

// Reconciler auto-heals a rebase conflict confined to a single known
// generated / derived file — a checksum-over-a-directory (a migrations
// lockfile), a secrets baseline, a generated index — by regenerating it from
// the post-merge tree instead of surfacing the line-level divergence as a fatal
// conflict. See docs/designs/2026-07-merge-reconcilers.md.
type Reconciler struct {
	// Path is matched against each unmerged path with path.Match semantics
	// (exact paths like "migrations/atlas.sum" match trivially; globs like
	// "migrations/*.sum" are supported; no "**").
	Path string
	// Command reconciles the file. It runs via `sh -c` in the worktree with the
	// gate's allowlisted environment plus KORYPH_MERGE_PATH / _OURS / _THEIRS /
	// _BASE, and must leave $KORYPH_MERGE_PATH a valid, conflict-marker-free
	// file (staged by the engine on return).
	Command string
}

// reconcileRebase attempts to auto-heal an in-progress rebase whose conflicts
// are confined entirely to configured reconcilers (design L2). It is called
// with the rebase already STOPPED on a conflict (git rebase exited non-zero).
//
//   - healed=true: every conflict round was covered and the rebase ran to
//     completion; the caller proceeds to the green gate. paths lists the
//     distinct healed files, rounds the number of --continue rounds.
//   - healed=false: some unmerged path was not covered (I1), a reconciler
//     failed or left the file unresolved (I2/I6), the rebase stopped for a
//     reason we do not own, or the round cap was hit (I4). The rebase is left
//     mid-flight for the caller to `git rebase --abort` — abort is valid from
//     any rebase state.
//
// err is reserved for git-plumbing failures that should not happen; a
// reconciler command exiting non-zero is NOT an err — it is healed=false, the
// same requeue-for-agent outcome as an un-healable conflict (fail-safe, I2).
func reconcileRebase(ctx context.Context, wtPath string, recs []Reconciler) (healed bool, paths []string, rounds int, err error) {
	if len(recs) == 0 {
		return false, nil, 0, nil
	}
	seen := map[string]bool{}
	gateEnv := execx.GateEnv()
	// git rebase --continue reuses the original commit message; suppress the
	// editor so it cannot block on a non-interactive host.
	contEnv := append(execx.BaseEnv("GIT_EDITOR", "GIT_SEQUENCE_EDITOR"),
		"GIT_EDITOR=true", "GIT_SEQUENCE_EDITOR=true")

	for round := range maxReconcileRounds {
		unmerged, uerr := unmergedPaths(ctx, wtPath)
		if uerr != nil {
			return false, distinctSorted(seen), round, uerr
		}
		if len(unmerged) == 0 {
			// Rebase stopped with nothing unmerged: an empty commit after a
			// heal, or a stop we do not own. Fail safe — the caller aborts.
			return false, distinctSorted(seen), round, nil
		}
		// I1: every unmerged path must be covered before touching anything —
		// no partial resolution, ever.
		for _, p := range unmerged {
			if matchReconciler(recs, p) == nil {
				return false, distinctSorted(seen), round, nil
			}
		}
		for _, p := range unmerged {
			rec := matchReconciler(recs, p)
			ok, rerr := runReconciler(ctx, wtPath, p, *rec, gateEnv)
			if rerr != nil {
				return false, distinctSorted(seen), round, rerr
			}
			if !ok {
				// Command failed, left conflict markers, or the path is still
				// unmerged after staging: fail safe (I2/I6).
				return false, distinctSorted(seen), round, nil
			}
			seen[p] = true
		}
		cont, cerr := execx.Run(ctx, execx.Cmd{
			Dir: wtPath, Env: contEnv, Name: "git", Args: []string{"rebase", "--continue"},
		})
		if cerr != nil {
			return false, distinctSorted(seen), round + 1, cerr
		}
		if cont.ExitCode == 0 {
			// Rebase complete: --continue exits 0 only on full completion,
			// non-zero when it stops on the next commit's conflict.
			return true, distinctSorted(seen), round + 1, nil
		}
		// Non-zero: a new conflict on the next commit (the cascade) — loop and
		// re-read unmerged. A non-conflict failure surfaces next round as an
		// empty unmerged set → healed=false (fail-safe, I2).
	}
	return false, distinctSorted(seen), maxReconcileRounds, nil // I4: cap exceeded
}

// unmergedPaths lists the worktree's conflicted (unmerged) paths, relative to
// the worktree root, NUL-delimited to tolerate any filename.
func unmergedPaths(ctx context.Context, wtPath string) ([]string, error) {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"diff", "--name-only", "--diff-filter=U", "-z"},
	})
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		return nil, nil // no diff / not in a state with unmerged entries
	}
	var out []string
	for p := range strings.SplitSeq(res.Stdout, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out, nil
}

// matchReconciler returns the first reconciler whose Path matches p, or nil.
func matchReconciler(recs []Reconciler, p string) *Reconciler {
	for i := range recs {
		if pathMatches(recs[i].Path, p) {
			return &recs[i]
		}
	}
	return nil
}

// pathMatches reports whether pattern covers p: exact equality (the common
// fixed-path case) or path.Match glob semantics. A malformed pattern is
// rejected at config-validation time, so a match error here is treated as
// no-match (fail safe).
func pathMatches(pattern, p string) bool {
	if pattern == p {
		return true
	}
	ok, err := path.Match(pattern, p)
	return err == nil && ok
}

// runReconciler regenerates one conflicted path. It extracts the three conflict
// stages to temp files, runs the project command with the KORYPH_MERGE_* env,
// verifies the result carries no conflict markers, stages it, and confirms the
// path left the unmerged set. ok=false is a fail-safe (command failed / left
// the file unresolved); err is a git-plumbing failure.
func runReconciler(ctx context.Context, wtPath, rel string, rec Reconciler, gateEnv []string) (ok bool, err error) {
	tmp, err := os.MkdirTemp("", "koryph-merge-*")
	if err != nil {
		return false, err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	// git index stages: 1=base (merge-base), 2=ours (rebase target/<def>),
	// 3=theirs (the replayed bead commit). NOTE the rebase inversion: "ours" is
	// the base branch and "theirs" is the bead's commit — the reverse of a
	// normal merge (see the user guide). An absent stage yields an empty env
	// value.
	base := extractStage(ctx, wtPath, 1, rel, filepath.Join(tmp, "base"))
	ours := extractStage(ctx, wtPath, 2, rel, filepath.Join(tmp, "ours"))
	theirs := extractStage(ctx, wtPath, 3, rel, filepath.Join(tmp, "theirs"))

	abs := filepath.Join(wtPath, rel)
	env := append([]string(nil), gateEnv...)
	env = append(env,
		"KORYPH_MERGE_PATH="+abs,
		"KORYPH_MERGE_BASE="+base,
		"KORYPH_MERGE_OURS="+ours,
		"KORYPH_MERGE_THEIRS="+theirs,
	)

	name, args := shellCmd(wtPath, rec.Command)
	res, err := execx.Run(ctx, execx.Cmd{Dir: wtPath, Name: name, Args: args, Env: env})
	if err == nil && res.ExitCode != 0 && name == "direnv" && strings.Contains(res.Stderr, "is blocked") {
		// Fresh agent worktrees carry a never-approved .envrc; fall back to a
		// plain shell exactly as the gate does.
		res, err = execx.Run(ctx, execx.Cmd{Dir: wtPath, Name: "sh", Args: []string{"-c", rec.Command}, Env: env})
	}
	if err != nil || res.ExitCode != 0 {
		return false, nil // fail safe (I2)
	}
	if hasConflictMarkers(abs) {
		return false, nil // command did not actually resolve the file (I6)
	}
	if _, aerr := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"add", "--", rel},
	}); aerr != nil {
		return false, aerr
	}
	// Confirm the path left the unmerged set (git add clears stages 1-3).
	still, serr := unmergedPaths(ctx, wtPath)
	if serr != nil {
		return false, serr
	}
	if slices.Contains(still, rel) {
		return false, nil
	}
	return true, nil
}

// extractStage writes the given git index stage of rel to dest and returns
// dest, or "" when that stage does not exist (add/add has no base; a path added
// on one side only lacks the other side).
func extractStage(ctx context.Context, wtPath string, stage int, rel, dest string) string {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir: wtPath, Name: "git", Args: []string{"show", stageSpec(stage) + ":" + rel},
	})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	if werr := os.WriteFile(dest, []byte(res.Stdout), 0o600); werr != nil {
		return ""
	}
	return dest
}

func stageSpec(stage int) string {
	switch stage {
	case 1:
		return ":1"
	case 2:
		return ":2"
	default:
		return ":3"
	}
}

// hasConflictMarkers reports whether file still contains git conflict markers.
// It looks for the unambiguous 7-character start/end markers at line start
// (`<<<<<<<` / `>>>>>>>`); the `=======` separator is deliberately not matched
// because it occurs legitimately in many files. A read error is treated as
// "no markers" — the git-add + unmerged recheck in the caller is the
// authoritative resolution proof; this is a cheap extra guard.
func hasConflictMarkers(file string) bool {
	b, err := os.ReadFile(file)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(line, "<<<<<<<") || strings.HasPrefix(line, ">>>>>>>") {
			return true
		}
	}
	return false
}

func distinctSorted(m map[string]bool) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
