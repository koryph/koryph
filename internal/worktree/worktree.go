// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/textx"
)

// git runs a git subcommand in dir and returns the raw result (non-zero exit
// is not an error; callers inspect ExitCode).
func git(ctx context.Context, dir string, args ...string) (execx.Result, error) {
	return execx.Run(ctx, execx.Cmd{Dir: dir, Name: "git", Args: args})
}

// List returns every worktree registered against repoRoot, with Dirty filled
// in from `git status --porcelain` for each.
func List(ctx context.Context, repoRoot string) ([]Info, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: repoRoot, Name: "git", Args: []string{"worktree", "list", "--porcelain"},
	})
	if err != nil {
		return nil, err
	}
	var infos []Info
	var cur *Info
	flush := func() {
		if cur != nil {
			infos = append(infos, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()
			cur = &Info{Path: strings.TrimPrefix(line, "worktree ")}
		case cur == nil:
			// stray line outside a stanza; ignore
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			cur.Branch = ""
		}
	}
	flush()
	for i := range infos {
		dirty, err := IsDirty(ctx, infos[i].Path)
		if err != nil {
			return nil, err
		}
		infos[i].Dirty = dirty
	}
	return infos, nil
}

// IsDirty reports whether the working tree at path has staged or unstaged
// changes (including untracked files).
func IsDirty(ctx context.Context, path string) (bool, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"status", "--porcelain"},
	})
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(res.Stdout) != "", nil
}

// Ensure resolves (and if necessary creates) the worktree for o.Branch. It is
// idempotent: calling it again for an already-registered worktree ATTACHES
// (Created=false) instead of failing — this is the gw8f recover-vs-existing
// fix. It never clobbers an unrelated directory that happens to sit on the
// target path.
func Ensure(ctx context.Context, o EnsureOpts) (Info, error) {
	repoRoot := o.RepoRoot
	wtRoot := o.WorktreeRoot
	if wtRoot == "" {
		wtRoot = filepath.Join(filepath.Dir(repoRoot), filepath.Base(repoRoot)+"-worktrees")
	}
	name := o.Name
	if name == "" {
		name = strings.ReplaceAll(o.Branch, "/", "-")
	}
	target := filepath.Join(wtRoot, name)

	// gw8f: prune stale admin entries so a directory that git still believes
	// is a worktree (but was deleted out-of-band) does not wedge Ensure.
	if _, err := git(ctx, repoRoot, "worktree", "prune"); err != nil {
		return Info{}, err
	}

	list, err := List(ctx, repoRoot)
	if err != nil {
		return Info{}, err
	}
	var registered *Info
	for i := range list {
		if samePath(list[i].Path, target) {
			registered = &list[i]
			break
		}
	}

	if fsx.Exists(target) {
		if registered == nil {
			return Info{}, fmt.Errorf("path %s exists but is not a registered worktree (refusing to clobber)", target)
		}
		if registered.Branch != o.Branch {
			return Info{}, fmt.Errorf("worktree %s is on branch %q, want %q", target, registered.Branch, o.Branch)
		}
		info := *registered
		info.Created = false
		return info, nil
	}

	if err := os.MkdirAll(wtRoot, 0o755); err != nil {
		return Info{}, err
	}

	branchExists := false
	if res, err := git(ctx, repoRoot, "show-ref", "--verify", "--quiet", "refs/heads/"+o.Branch); err == nil && res.ExitCode == 0 {
		branchExists = true
	}

	if branchExists {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: repoRoot, Name: "git", Args: []string{"worktree", "add", target, o.Branch},
		}); err != nil {
			return Info{}, err
		}
	} else {
		if o.Base == "" {
			return Info{}, fmt.Errorf("branch %q does not exist and no Base was given", o.Branch)
		}
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: repoRoot, Name: "git", Args: []string{"worktree", "add", "-b", o.Branch, target, o.Base},
		}); err != nil {
			return Info{}, err
		}
	}

	dirty, err := IsDirty(ctx, target)
	if err != nil {
		return Info{}, err
	}
	head := ""
	if res, err := git(ctx, target, "rev-parse", "HEAD"); err == nil {
		head = strings.TrimSpace(res.Stdout)
	}
	return Info{Path: target, Branch: o.Branch, Head: head, Dirty: dirty, Created: true}, nil
}

// Bootstrap runs project bootstrap commands in path sequentially (via
// `direnv exec` when direnv is on PATH), stopping at the first failure and
// returning its combined-output tail.
func Bootstrap(ctx context.Context, path string, cmds []string, env []string) error {
	for _, c := range cmds {
		name, args := shellCmd(path, c)
		res, err := execx.Run(ctx, execx.Cmd{Dir: path, Env: env, Name: name, Args: args})
		if err == nil && res.ExitCode != 0 && name == "direnv" && strings.Contains(res.Stderr, "is blocked") {
			// Fresh worktrees carry a never-approved .envrc; fall back to a
			// plain shell rather than failing on environment ceremony.
			res, err = execx.Run(ctx, execx.Cmd{Dir: path, Env: env, Name: "sh", Args: []string{"-c", c}})
		}
		if err != nil {
			return fmt.Errorf("bootstrap %q: %w", c, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("bootstrap %q failed (exit %d): %s", c, res.ExitCode, textx.Tail(res.Stdout+res.Stderr, 2000))
		}
	}
	return nil
}

// Refresh rebases a clean, running worktree onto an advanced base when it has
// fallen far enough behind AND the base delta overlaps the branch footprint.
// See RefreshResult for the possible actions.
func Refresh(ctx context.Context, o RefreshOpts) (RefreshResult, error) {
	threshold := o.Threshold
	if threshold == 0 {
		threshold = 5
	}
	var result RefreshResult

	behindRes, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: o.RepoRoot, Name: "git", Args: []string{"rev-list", "--count", o.Branch + ".." + o.Base},
	})
	if err != nil {
		return result, err
	}
	behind, err := strconv.Atoi(strings.TrimSpace(behindRes.Stdout))
	if err != nil {
		return result, fmt.Errorf("parse behind count %q: %w", strings.TrimSpace(behindRes.Stdout), err)
	}
	result.Behind = behind
	if behind < threshold && !o.Force {
		result.Action = "none"
		return result, nil
	}

	overlap, err := footprintOverlap(ctx, o.RepoRoot, o.Base, o.Branch)
	if err != nil {
		return result, err
	}
	result.Overlap = overlap
	if !overlap && !o.Force {
		result.Action = "none"
		return result, nil
	}

	if o.CheckOnly {
		result.Action = "advised"
		return result, nil
	}

	dirty, err := IsDirty(ctx, o.Path)
	if err != nil {
		return result, err
	}
	if dirty {
		result.Action = "deferred-dirty"
		return result, nil
	}

	rb, err := git(ctx, o.Path, "rebase", o.Base)
	if err != nil {
		return result, err
	}
	if rb.ExitCode != 0 {
		_, _ = git(ctx, o.Path, "rebase", "--abort")
		md := ConflictMarkdown(o.Branch, o.Base, rb.Stdout+rb.Stderr)
		_ = fsx.WriteAtomic(filepath.Join(o.Path, "CONFLICT.md"), []byte(md), 0o644)
		result.Action = "conflict"
		return result, nil
	}
	result.Action = "refreshed"
	return result, nil
}

// footprintOverlap reports whether the branch's changed files intersect the
// base's changed files since they diverged (a plain full-path set intersection).
func footprintOverlap(ctx context.Context, repoRoot, base, branch string) (bool, error) {
	mbRes, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: repoRoot, Name: "git", Args: []string{"merge-base", branch, base},
	})
	if err != nil {
		return false, err
	}
	mb := strings.TrimSpace(mbRes.Stdout)

	branchFiles, err := diffNames(ctx, repoRoot, base+"..."+branch)
	if err != nil {
		return false, err
	}
	baseFiles, err := diffNames(ctx, repoRoot, mb+".."+base)
	if err != nil {
		return false, err
	}
	set := make(map[string]struct{}, len(branchFiles))
	for _, f := range branchFiles {
		set[f] = struct{}{}
	}
	for _, f := range baseFiles {
		if _, ok := set[f]; ok {
			return true, nil
		}
	}
	return false, nil
}

// Remove deletes the worktree at path. It refuses a dirty tree unless force is
// set (force only ever follows explicit human approval upstream). It best-effort
// prunes admin state afterward.
func Remove(ctx context.Context, path string, force bool) error {
	dirty, err := IsDirty(ctx, path)
	if err != nil {
		return err
	}
	if dirty && !force {
		return fmt.Errorf("refusing to remove dirty worktree %s", path)
	}
	repo, err := mainRepo(ctx, path)
	if err != nil {
		return err
	}
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, path)
	if _, err := execx.MustSucceed(ctx, execx.Cmd{Dir: repo, Name: "git", Args: args}); err != nil {
		return err
	}
	_, _ = git(ctx, repo, "worktree", "prune")
	return nil
}

// PatchSnapshot writes a WIP patch (tracked diff + untracked files via the
// `git add -N` trick) to outDir and returns its path. It returns "" without
// writing when there is nothing to capture, and never leaves intent-to-add
// entries staged.
func PatchSnapshot(ctx context.Context, path, outDir string) (string, error) {
	if _, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"add", "-N", "."},
	}); err != nil {
		return "", err
	}
	defer func() { _, _ = git(ctx, path, "reset") }()

	res, err := execx.MustSucceed(ctx, execx.Cmd{Dir: path, Name: "git", Args: []string{"diff"}})
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return "", nil
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	out := filepath.Join(outDir, "wip-"+stamp+".patch")
	if err := fsx.WriteAtomic(out, []byte(res.Stdout), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// DeleteBranch deletes branch from repoRoot, falling back to a force delete
// when the safe delete refuses (e.g. after a squash merge).
func DeleteBranch(ctx context.Context, repoRoot, branch string) error {
	if res, err := git(ctx, repoRoot, "branch", "-d", branch); err == nil && res.ExitCode == 0 {
		return nil
	}
	_, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: repoRoot, Name: "git", Args: []string{"branch", "-D", branch},
	})
	return err
}

// mainRepo returns the top-level directory of the main worktree that owns the
// worktree at path.
func mainRepo(ctx context.Context, path string) (string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: path, Name: "git", Args: []string{"rev-parse", "--path-format=absolute", "--git-common-dir"},
	})
	if err != nil {
		return "", err
	}
	return filepath.Dir(strings.TrimSpace(res.Stdout)), nil
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

// shellCmd builds a `sh -c` invocation, wrapped with `direnv exec <dir>` when
// direnv is available so project env is loaded.
func shellCmd(dir, command string) (string, []string) {
	if execx.LookPath("direnv") {
		return "direnv", []string{"exec", dir, "sh", "-c", command}
	}
	return "sh", []string{"-c", command}
}

// samePath compares two filesystem paths, resolving symlinks when possible so
// git's realpath'd worktree paths match caller-computed targets (macOS /var vs
// /private/var).
func samePath(a, b string) bool {
	if r, err := filepath.EvalSymlinks(a); err == nil {
		a = r
	}
	if r, err := filepath.EvalSymlinks(b); err == nil {
		b = r
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// ConflictMarkdown renders the CONFLICT.md breadcrumb written when a
// rebase-onto-base is aborted on conflict — both at worktree creation (here)
// and rebase-before-merge (internal/merge, which calls this rather than
// keeping its own copy: koryph-fiv finding #6). The captured rebase output is
// fenced as ```text (not a bare ```): a bare fence trips markdownlint MD040
// (fenced-code-language), and though koryph never commits this file, a project
// whose gate lints all *.md still flags it.
func ConflictMarkdown(branch, base, output string) string {
	return fmt.Sprintf(
		"# Rebase conflict\n\nRebasing `%s` onto `%s` hit a conflict and was aborted; the\nworktree is unchanged and nothing was merged. Resolve manually, then retry.\n\n```text\n%s\n```\n",
		branch, base, strings.TrimSpace(textx.Tail(output, 4000)),
	)
}
