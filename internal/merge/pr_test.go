// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/fsx"
)

// fakePR is an injected PROpener that records its inputs and never touches a
// real GitHub remote.
type fakePR struct {
	ready bool
	url   string
	num   int

	calls     int
	gotBranch string
	gotBase   string
	gotTitle  string
	gotBody   string
}

func (f *fakePR) Ready(context.Context, string) bool { return f.ready }

func (f *fakePR) Open(_ context.Context, _, branch, base, title, body string) (string, int, error) {
	f.calls++
	f.gotBranch, f.gotBase, f.gotTitle, f.gotBody = branch, base, title, body
	return f.url, f.num, nil
}

// TestMergeOpenPRPushesBranchAndOpensPR is the happy path: with a remote and a
// ready PR host, the OpenPR path rebases + gates like a merge, then pushes the
// branch and opens a PR instead of fast-forwarding the default branch. The
// default branch is untouched and the worktree/branch are kept.
// TestIsDefaultBranchNameGuardsForcePush asserts the branch-name guard that
// keeps pushBranch's force-with-lease fallback off any integration branch:
// engine branches are always agent/<bead-id>, so a force-push of main/master/
// etc. is never a legitimate engine action.
func TestIsDefaultBranchNameGuardsForcePush(t *testing.T) {
	for _, b := range []string{"", "main", "master", "trunk", "develop", "development", "release", "  main  "} {
		if !isDefaultBranchName(b) {
			t.Errorf("isDefaultBranchName(%q) = false, want true (must never force-push)", b)
		}
	}
	for _, b := range []string{"agent/koryph-42", "koryph/cn-9", "feature/x", "mainline"} {
		if isDefaultBranchName(b) {
			t.Errorf("isDefaultBranchName(%q) = true, want false (a real agent branch)", b)
		}
	}
}

func TestMergeOpenPRPushesBranchAndOpensPR(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	mainBefore := headOf(t, repo, "main")
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "feat: add b")
	tip := headOf(t, wt.Path, "HEAD")

	pr := &fakePR{ready: true, url: "https://github.com/acme/proj/pull/7", num: 7}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, OpenPR: true, PR: pr,
		PRTitle: "feat(bead-1): add b", PRBody: "body text",
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != "pr-opened" {
		t.Fatalf("Status=%q, want pr-opened (output=%s)", res.Status, res.GateOutput)
	}
	if res.PRURL != pr.url || res.PRNumber != 7 {
		t.Errorf("PRURL=%q PRNumber=%d, want %q/7", res.PRURL, res.PRNumber, pr.url)
	}
	if !res.Pushed {
		t.Error("Pushed=false, want true")
	}
	if pr.calls != 1 {
		t.Fatalf("PR.Open calls=%d, want 1", pr.calls)
	}
	if pr.gotBranch != "agent/x" || pr.gotBase != "main" {
		t.Errorf("Open(branch=%q base=%q), want agent/x/main", pr.gotBranch, pr.gotBase)
	}
	if pr.gotTitle != "feat(bead-1): add b" || pr.gotBody != "body text" {
		t.Errorf("Open(title=%q body=%q), want the passed title/body", pr.gotTitle, pr.gotBody)
	}

	// The branch reached the remote at its tip (rebase onto an unchanged main
	// is a no-op, so the tip is unchanged).
	ls := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/agent/x"))
	if len(ls) == 0 || ls[0] != tip {
		t.Errorf("remote refs/heads/agent/x=%v, want %s (branch not pushed)", ls, tip)
	}
	// The default branch was NOT advanced.
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main=%s, want unchanged %s (PR path must not merge to default)", got, mainBefore)
	}
	// The worktree and branch are kept for a later landing step.
	if !fsx.Exists(wt.Path) {
		t.Errorf("worktree removed: %s (PR path must keep it)", wt.Path)
	}
	if b := strings.TrimSpace(mustGit(t, repo, "branch", "--list", "agent/x")); b == "" {
		t.Error("branch agent/x deleted; PR path must keep it")
	}
}

// TestMergeOpenPRNoRemote blocks cleanly (no crash, no PR host call) when the
// project has no git remote.
func TestMergeOpenPRNoRemote(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t) // no remote
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "feat: add b")

	pr := &fakePR{ready: true}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, OpenPR: true, PR: pr,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "pr-no-remote" {
		t.Fatalf("Status=%q, want pr-no-remote", res.Status)
	}
	if pr.calls != 0 {
		t.Errorf("PR.Open calls=%d, want 0 (no remote)", pr.calls)
	}
	if b := strings.TrimSpace(mustGit(t, repo, "branch", "--list", "agent/x")); b == "" {
		t.Error("branch agent/x deleted; a blocked PR path must keep it")
	}
}

// TestMergeOpenPRHostNotReady blocks cleanly when the PR host (gh) is missing
// or unauthenticated, without attempting to push or open a PR.
func TestMergeOpenPRHostNotReady(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "feat: add b")

	pr := &fakePR{ready: false}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, OpenPR: true, PR: pr,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "pr-no-gh" {
		t.Fatalf("Status=%q, want pr-no-gh", res.Status)
	}
	if pr.calls != 0 {
		t.Errorf("PR.Open calls=%d, want 0 (host not ready)", pr.calls)
	}
	// Nothing reached the remote.
	if ls := strings.TrimSpace(mustGit(t, repo, "ls-remote", "origin", "refs/heads/agent/x")); ls != "" {
		t.Errorf("branch reached remote (%s); host-not-ready must not push", ls)
	}
}

// TestMergeOpenPRProtectedRejectedBeforePR proves the shared preflight still
// runs on the PR path: a protected-path hit rejects before any push or PR.
func TestMergeOpenPRProtectedRejectedBeforePR(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "koryph.project.json", "{}\n", "chore: touch protected")

	pr := &fakePR{ready: true, url: "https://example/pull/1", num: 1}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, OpenPR: true, PR: pr,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "protected" {
		t.Fatalf("Status=%q, want protected", res.Status)
	}
	if pr.calls != 0 {
		t.Errorf("PR.Open calls=%d, want 0 (protected rejects before PR)", pr.calls)
	}
}
