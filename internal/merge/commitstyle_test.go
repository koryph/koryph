// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestNonConventionalSubjects_Grammar(t *testing.T) {
	valid := []string{
		"feat: add a thing",
		"fix(scope): correct it",
		"docs: update readme",
		"chore(koryph-ufy.1): wire it up", // scope with dots/hyphens
		"refactor(a/b): move files",       // scope with slash
		"test: cover the edge",
		"ci: bump action",
		"build: goreleaser",
		"perf: speed up",
		"style: gofmt",
		"refactor!: drop the flag",   // breaking-change marker
		"fix(api)!: change response", // scoped breaking change
	}
	for _, s := range valid {
		if bad := nonConventionalSubjects([]string{s}); len(bad) != 0 {
			t.Errorf("subject %q flagged as non-conventional, want conforming", s)
		}
	}

	invalid := []string{
		"add a thing",        // no type
		"Feat: add",          // uppercase type
		"feature: add",       // unknown type
		"wip: something",     // unknown type
		"feat add",           // no colon
		"feat:no space",      // missing space after colon
		"feat: ",             // empty subject
		"Merge branch 'x'",   // merge commit
		"Revert \"feat: a\"", // revert without a conventional type
	}
	for _, s := range invalid {
		if bad := nonConventionalSubjects([]string{s}); len(bad) != 1 {
			t.Errorf("subject %q accepted, want flagged as non-conventional", s)
		}
	}
}

func TestNonConventionalSubjects_IgnoresBlanksAndPreservesOrder(t *testing.T) {
	in := []string{"feat: ok", "  ", "bad one", "", "also bad", "fix: fine"}
	got := nonConventionalSubjects(in)
	want := []string{"bad one", "also bad"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("nonConventionalSubjects = %v, want %v (blanks ignored, order preserved)", got, want)
	}
}

// TestMergeCommitStyleRejectsNonConventional proves the check runs read-only,
// before any merge: a bad subject yields status commit-style and leaves the
// default branch and the branch untouched.
func TestMergeCommitStyleRejectsNonConventional(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	mainBefore := headOf(t, repo, "main")
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add a feature") // non-conventional

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireConventional: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "commit-style" {
		t.Fatalf("Status=%q, want commit-style", res.Status)
	}
	if !strings.Contains(res.GateOutput, "add a feature") {
		t.Errorf("GateOutput=%q, want it to list the offending subject", res.GateOutput)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main advanced to %s (was %s); commit-style must not merge", got, mainBefore)
	}
	if b := strings.TrimSpace(mustGit(t, repo, "branch", "--list", "agent/x")); b == "" {
		t.Error("branch agent/x deleted; a rejected merge must keep it")
	}
}

func TestMergeCommitStyleConformingMerges(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "feat: add b")
	tip := headOf(t, wt.Path, "HEAD")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireConventional: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged", res.Status)
	}
	if got := headOf(t, repo, "main"); got != tip {
		t.Errorf("main=%s, want %s", got, tip)
	}
}

func TestMergeCommitStyleOptOutMergesNonConventional(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add a feature") // non-conventional
	tip := headOf(t, wt.Path, "HEAD")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireConventional: false, // opt-out
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged (opt-out must not enforce)", res.Status)
	}
	if got := headOf(t, repo, "main"); got != tip {
		t.Errorf("main=%s, want %s", got, tip)
	}
}

// TestMergeCommitStyleChecksEveryCommit proves validation covers the whole
// branch range (def..branch), not just HEAD: a conforming tip does not excuse
// a non-conforming earlier commit.
func TestMergeCommitStyleChecksEveryCommit(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "one\n", "sneaky non-conventional") // earlier, bad
	commitIn(t, wt.Path, "c.txt", "two\n", "feat: proper tip")        // HEAD, good

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireConventional: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "commit-style" {
		t.Fatalf("Status=%q, want commit-style (earlier commit is non-conventional)", res.Status)
	}
	if !strings.Contains(res.GateOutput, "sneaky non-conventional") {
		t.Errorf("GateOutput=%q, want the earlier offending subject listed", res.GateOutput)
	}
	if strings.Contains(res.GateOutput, "feat: proper tip") {
		t.Errorf("GateOutput=%q, must not list the conforming subject", res.GateOutput)
	}
}

// TestMergeOpenPRCommitStyleRejects proves the shared preflight fires on the
// PR-open path too: a bad subject is refused before any push or PR.
func TestMergeOpenPRCommitStyleRejects(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add a feature") // non-conventional

	pr := &fakePR{ready: true, url: "https://example/pull/1", num: 1}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireConventional: true,
		OpenPR: true, PR: pr,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "commit-style" {
		t.Fatalf("Status=%q, want commit-style", res.Status)
	}
	if pr.calls != 0 {
		t.Errorf("PR.Open calls=%d, want 0 (commit-style rejects before PR)", pr.calls)
	}
	if ls := strings.TrimSpace(mustGit(t, repo, "ls-remote", "origin", "refs/heads/agent/x")); ls != "" {
		t.Errorf("branch reached remote (%s); commit-style must not push", ls)
	}
}
