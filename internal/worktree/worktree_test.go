// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package worktree

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
)

// isolateGit points git at throwaway global/system config so the developer's
// real ~/.gitconfig (gpg signing, hooks, templates) cannot influence tests.
func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "no-system"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	res, err := execx.MustSucceed(context.Background(), execx.Cmd{Dir: dir, Name: "git", Args: args})
	if err != nil {
		t.Fatalf("git %s (in %s): %v", strings.Join(args, " "), dir, err)
	}
	return res.Stdout
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// initRepo builds a throwaway git repo on branch main with one seed commit and
// returns its root.
func initRepo(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	repo := filepath.Join(base, "repo")
	mustGit(t, base, "init", "-q", "-b", "main", "repo")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test User")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	writeFile(t, filepath.Join(repo, "a.txt"), "seed\n")
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-qm", "seed")
	return repo
}

func TestEnsureFreshCreate(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	info, err := Ensure(context.Background(), EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if !info.Created {
		t.Errorf("Created=false, want true on fresh create")
	}
	if info.Branch != "agent/x" {
		t.Errorf("Branch=%q, want agent/x", info.Branch)
	}
	if filepath.Base(info.Path) != "agent-x" {
		t.Errorf("worktree dir name=%q, want agent-x", filepath.Base(info.Path))
	}
	if !fsx.Exists(info.Path) {
		t.Errorf("worktree path %s does not exist", info.Path)
	}
	if info.Head == "" {
		t.Errorf("Head not populated")
	}
}

// TestEnsureAttachExisting is the gw8f regression guard: a second Ensure for an
// already-registered worktree must ATTACH, not fail.
func TestEnsureAttachExisting(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	opts := EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"}
	first, err := Ensure(ctx, opts)
	if err != nil {
		t.Fatalf("first Ensure: %v", err)
	}
	second, err := Ensure(ctx, opts)
	if err != nil {
		t.Fatalf("second Ensure (attach) must succeed: %v", err)
	}
	if second.Created {
		t.Errorf("second Ensure Created=true, want false (attach)")
	}
	if !samePath(first.Path, second.Path) {
		t.Errorf("attach path %q != create path %q", second.Path, first.Path)
	}
}

func TestEnsureExistingUnregisteredDirErrors(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	wtRoot := filepath.Join(filepath.Dir(repo), filepath.Base(repo)+"-worktrees")
	target := filepath.Join(wtRoot, "agent-x")
	writeFile(t, filepath.Join(target, "junk.txt"), "not a worktree\n")
	_, err := Ensure(context.Background(), EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err == nil {
		t.Fatal("expected error when target exists but is not a registered worktree")
	}
	if !strings.Contains(err.Error(), "not a registered worktree") {
		t.Errorf("error=%q, want mention of not-a-registered-worktree", err)
	}
	if !fsx.Exists(filepath.Join(target, "junk.txt")) {
		t.Errorf("Ensure clobbered the pre-existing directory")
	}
}

func TestRemoveRefusesDirty(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "dirty.txt"), "wip\n")
	err = Remove(ctx, info.Path, false)
	if err == nil || !strings.Contains(err.Error(), "dirty worktree") {
		t.Fatalf("Remove(force=false) on dirty tree: err=%v, want 'dirty worktree'", err)
	}
	if !fsx.Exists(info.Path) {
		t.Fatal("worktree removed despite refusal")
	}
	if err := Remove(ctx, info.Path, true); err != nil {
		t.Fatalf("Remove(force=true): %v", err)
	}
	if fsx.Exists(info.Path) {
		t.Errorf("worktree still present after forced remove")
	}
}

func TestRefreshNotBehindNone(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := Refresh(ctx, RefreshOpts{RepoRoot: repo, Path: info.Path, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "none" {
		t.Errorf("Action=%q, want none", res.Action)
	}
	if res.Behind != 0 {
		t.Errorf("Behind=%d, want 0", res.Behind)
	}
}

func TestRefreshDeferredDirty(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	// Branch touches shared.txt.
	writeFile(t, filepath.Join(info.Path, "shared.txt"), "branch\n")
	mustGit(t, info.Path, "add", "shared.txt")
	mustGit(t, info.Path, "commit", "-qm", "branch shared")
	// Base advances, overlapping shared.txt.
	writeFile(t, filepath.Join(repo, "shared.txt"), "base\n")
	mustGit(t, repo, "add", "shared.txt")
	mustGit(t, repo, "commit", "-qm", "base shared")
	// Dirty the worktree.
	writeFile(t, filepath.Join(info.Path, "wip.txt"), "wip\n")

	res, err := Refresh(ctx, RefreshOpts{RepoRoot: repo, Path: info.Path, Branch: "agent/x", Base: "main", Threshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Action != "deferred-dirty" {
		t.Errorf("Action=%q, want deferred-dirty (Behind=%d Overlap=%v)", res.Action, res.Behind, res.Overlap)
	}
	if res.Behind < 1 || !res.Overlap {
		t.Errorf("expected Behind>=1 and Overlap=true, got Behind=%d Overlap=%v", res.Behind, res.Overlap)
	}
}

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

func TestListReportsWorktreesAndDirty(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	info, err := Ensure(ctx, EnsureOpts{RepoRoot: repo, Branch: "agent/x", Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(info.Path, "dirty.txt"), "x\n")

	list, err := List(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	var main, agent *Info
	for i := range list {
		switch list[i].Branch {
		case "main":
			main = &list[i]
		case "agent/x":
			agent = &list[i]
		}
	}
	if main == nil || agent == nil {
		t.Fatalf("expected main and agent/x worktrees, got %+v", list)
	}
	if main.Dirty {
		t.Errorf("main worktree unexpectedly dirty")
	}
	if !agent.Dirty {
		t.Errorf("agent worktree should be dirty")
	}
}
