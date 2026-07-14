// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/worktree"
)

func isolateGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "no-global"))
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(t.TempDir(), "no-system"))
	t.Setenv("GIT_TERMINAL_PROMPT", "0")
	// Clear GIT_CONFIG_COUNT/KEY/VALUE variables that direnv may inject from
	// an outer project (e.g. safe.bareRepository=explicit), which would
	// interfere with bare-repo operations in test fixtures.
	t.Setenv("GIT_CONFIG_COUNT", "0")
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

// worktreeOn creates a worktree on branch (from main) and returns its info.
func worktreeOn(t *testing.T, repo, branch string) worktree.Info {
	t.Helper()
	info, err := worktree.Ensure(context.Background(), worktree.EnsureOpts{RepoRoot: repo, Branch: branch, Base: "main"})
	if err != nil {
		t.Fatalf("Ensure worktree %s: %v", branch, err)
	}
	return info
}

func commitIn(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), content)
	mustGit(t, dir, "add", file)
	mustGit(t, dir, "commit", "-qm", msg)
}

func headOf(t *testing.T, dir, rev string) string {
	t.Helper()
	return strings.TrimSpace(mustGit(t, dir, "rev-parse", rev))
}

type fakeSlot struct {
	owner              string
	acquired, released int
}

func (f *fakeSlot) Acquire(_ context.Context, owner string) error {
	f.acquired++
	f.owner = owner
	return nil
}
func (f *fakeSlot) Release(_ context.Context) error { f.released++; return nil }

func TestMergeHappyFastForward(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	tip := headOf(t, wt.Path, "HEAD")

	slot := &fakeSlot{}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, SlotOwner: "owner-1", Slot: slot,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if res.MergedSHA != tip {
		t.Errorf("MergedSHA=%s, want branch tip %s", res.MergedSHA, tip)
	}
	if got := headOf(t, repo, "main"); got != tip {
		t.Errorf("main=%s, want fast-forwarded to %s", got, tip)
	}
	if slot.acquired != 1 || slot.released != 1 {
		t.Errorf("slot acquired=%d released=%d, want 1/1", slot.acquired, slot.released)
	}
	if slot.owner != "owner-1" {
		t.Errorf("slot owner=%q, want owner-1", slot.owner)
	}
	if fsx.Exists(wt.Path) {
		t.Errorf("worktree not cleaned up: %s", wt.Path)
	}
	if b := strings.TrimSpace(mustGit(t, repo, "branch", "--list", "agent/x")); b != "" {
		t.Errorf("branch not deleted: %q", b)
	}
}

func TestMergeGateFailedLeavesMainUntouched(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	mainBefore := headOf(t, repo, "main")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main", Gate: []string{"false"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "gate-failed" {
		t.Fatalf("Status=%q, want gate-failed", res.Status)
	}
	if res.GateOutput == "" {
		t.Errorf("expected gate output tail")
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main moved on gate failure: %s != %s", got, mainBefore)
	}
	if !fsx.Exists(wt.Path) {
		t.Errorf("worktree removed on gate failure; must be kept")
	}
}

func TestMergeProtectedRejection(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, ".claude/x", "danger\n", "touch claude")
	mainBefore := headOf(t, repo, "main")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main", Gate: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "protected" {
		t.Fatalf("Status=%q, want protected", res.Status)
	}
	if len(res.Protected) == 0 || res.Protected[0] != ".claude/x" {
		t.Errorf("Protected=%v, want [.claude/x]", res.Protected)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main mutated on protected rejection")
	}
	if !fsx.Exists(wt.Path) {
		t.Errorf("worktree removed on protected rejection")
	}
}

// TestMergeAllowProtected is the koryph-dcn matrix: the operator flag lifts
// ONLY the routine CI/build subset (.github/, Makefile); the same diff is
// refused without the flag; and a project extra (governance) path is refused
// even WITH the flag.
func TestMergeAllowProtected(t *testing.T) {
	isolateGit(t)
	ctx := context.Background()

	t.Run("flag lands a CI-workflow diff", func(t *testing.T) {
		repo := initRepo(t)
		wt := worktreeOn(t, repo, "agent/ci")
		commitIn(t, wt.Path, ".github/workflows/x.yml", "on: push\n", "ci: add workflow")

		res, err := Merge(ctx, Opts{
			RepoRoot: repo, Branch: "agent/ci", DefaultBranch: "main",
			Gate: []string{"true"}, AllowProtected: true,
		})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if res.Status != StatusMerged {
			t.Fatalf("Status=%q, want merged (AllowProtected must lift .github/)", res.Status)
		}
	})

	t.Run("same diff refused without the flag", func(t *testing.T) {
		repo := initRepo(t)
		wt := worktreeOn(t, repo, "agent/ci2")
		commitIn(t, wt.Path, ".github/workflows/x.yml", "on: push\n", "ci: add workflow")

		res, err := Merge(ctx, Opts{
			RepoRoot: repo, Branch: "agent/ci2", DefaultBranch: "main", Gate: []string{"true"},
		})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if res.Status != StatusProtected {
			t.Fatalf("Status=%q, want protected (the default gate is the sandbox)", res.Status)
		}
	})

	t.Run("flag still refuses a project extra path", func(t *testing.T) {
		repo := initRepo(t)
		wt := worktreeOn(t, repo, "agent/gov")
		commitIn(t, wt.Path, "infra/prod.tf", "hcl\n", "chore: touch governance")

		res, err := Merge(ctx, Opts{
			RepoRoot: repo, Branch: "agent/gov", DefaultBranch: "main",
			Gate: []string{"true"}, Extra: []string{"infra/"}, AllowProtected: true,
		})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if res.Status != StatusProtected {
			t.Fatalf("Status=%q, want protected (extra paths are never liftable)", res.Status)
		}
		if len(res.Protected) == 0 || res.Protected[0] != "infra/prod.tf" {
			t.Errorf("Protected=%v, want [infra/prod.tf]", res.Protected)
		}
	})

	t.Run("flag still refuses governance defaults", func(t *testing.T) {
		repo := initRepo(t)
		wt := worktreeOn(t, repo, "agent/claude")
		commitIn(t, wt.Path, ".claude/settings.json", "{}\n", "chore: touch agent config")

		res, err := Merge(ctx, Opts{
			RepoRoot: repo, Branch: "agent/claude", DefaultBranch: "main",
			Gate: []string{"true"}, AllowProtected: true,
		})
		if err != nil {
			t.Fatalf("Merge: %v", err)
		}
		if res.Status != StatusProtected {
			t.Fatalf("Status=%q, want protected (.claude/ is not liftable)", res.Status)
		}
	})
}

func TestMergeConflictAbortsCleanly(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	// Divergent edit to the same line of the same file.
	commitIn(t, wt.Path, "a.txt", "branchside\n", "branch edits a")
	commitIn(t, repo, "a.txt", "mainside\n", "main edits a")
	mainBefore := headOf(t, repo, "main")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main", Gate: []string{"true"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "conflict" {
		t.Fatalf("Status=%q, want conflict", res.Status)
	}
	if res.ConflictMD == "" || !fsx.Exists(res.ConflictMD) {
		t.Errorf("CONFLICT.md not written (path=%q)", res.ConflictMD)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main mutated on conflict")
	}
	// Rebase must be aborted: HEAD back on the branch, no rebase in progress.
	if br := strings.TrimSpace(mustGit(t, wt.Path, "rev-parse", "--abbrev-ref", "HEAD")); br != "agent/x" {
		t.Errorf("worktree HEAD=%q after abort, want agent/x", br)
	}
	if fsx.Exists(filepath.Join(wt.Path, ".git", "rebase-merge")) {
		t.Errorf("rebase state left behind (not aborted)")
	}
}

func TestMergeSquash(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t)
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "one\n", "add b")
	commitIn(t, wt.Path, "c.txt", "two\n", "add c")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, Squash: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged", res.Status)
	}
	msg := strings.TrimSpace(mustGit(t, repo, "log", "-1", "--pretty=%s"))
	if !strings.Contains(msg, "squash merge") {
		t.Errorf("last commit subject=%q, want squash merge marker", msg)
	}
	if !fsx.Exists(filepath.Join(repo, "b.txt")) || !fsx.Exists(filepath.Join(repo, "c.txt")) {
		t.Errorf("squashed content not applied to main")
	}
	// seed + single squash commit == 2 commits total on main.
	if n := strings.TrimSpace(mustGit(t, repo, "rev-list", "--count", "main")); n != "2" {
		t.Errorf("main commit count=%s, want 2 (seed + squash)", n)
	}
	if fsx.Exists(wt.Path) {
		t.Errorf("worktree not cleaned up after squash merge")
	}
}

// initRepoWithRemote creates a local repo whose "main" tracks a local bare
// repo acting as the remote named "origin". Returns (repo path, bare path).
// All of the push/fetch plumbing required by Merge is wired up.
func initRepoWithRemote(t *testing.T) (repo, bare string) {
	t.Helper()
	base := t.TempDir()

	// bare remote (acts as origin).
	bare = filepath.Join(base, "bare.git")
	mustGit(t, base, "init", "--bare", "-q", "-b", "main", "bare.git")

	// local repo.
	repo = filepath.Join(base, "repo")
	mustGit(t, base, "init", "-q", "-b", "main", "repo")
	mustGit(t, repo, "config", "user.email", "test@example.com")
	mustGit(t, repo, "config", "user.name", "Test User")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	mustGit(t, repo, "remote", "add", "origin", bare)

	// Seed commit + push with -u so that main tracks origin/main;
	// git pull --ff-only (step 7 in Merge) requires tracking info.
	writeFile(t, filepath.Join(repo, "a.txt"), "seed\n")
	mustGit(t, repo, "add", "a.txt")
	mustGit(t, repo, "commit", "-qm", "seed")
	mustGit(t, repo, "push", "-q", "-u", "origin", "main")
	return repo, bare
}

// TestMergePushWithRemoteSetsResultPushed is the regression test for the
// pushed:false bug: when Push=true and a remote is present, a successful push
// must set Result.Pushed=true and advance the remote HEAD.
func TestMergePushWithRemoteSetsResultPushed(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	tip := headOf(t, wt.Path, "HEAD")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, Push: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if !res.Pushed {
		t.Errorf("Pushed=false, want true — remote exists and Push was requested")
	}
	// Verify the commit reached the remote via ls-remote (avoids running git
	// inside the bare repo, which safe.bareRepository=explicit can block).
	lsOut := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/main"))
	if len(lsOut) == 0 || lsOut[0] != tip {
		t.Errorf("remote refs/heads/main=%v, want %s (push did not reach remote)", lsOut, tip)
	}
}

// TestMergePushWithoutRemoteIsBestEffortNoPush documents the contract koryph-8eh
// depends on: merge.Merge with Push=true and NO remote lands the merge and
// records Pushed=false WITHOUT erroring — the engine's auto-merge relies on this
// so a local-only project still merges. Detecting the no-op and refusing to
// report success is the CLI's job (cmdMerge), not merge.Merge's.
func TestMergePushWithoutRemoteIsBestEffortNoPush(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t) // no remote configured
	ctx := context.Background()

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, Push: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s) — a missing remote must not fail the merge", err, res.Status)
	}
	if res.Status != "merged" {
		t.Errorf("Status=%q, want merged", res.Status)
	}
	if res.Pushed {
		t.Error("Pushed=true, want false — there was no remote to push to")
	}
}

// TestMergePushFalseWithRemoteDoesNotPush ensures that when Push=false the
// remote is not advanced even when a remote is present.
func TestMergePushFalseWithRemoteDoesNotPush(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t)
	ctx := context.Background()

	// Capture remote HEAD before merge via ls-remote (avoids bare-repo dir access).
	lsBefore := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/main"))
	if len(lsBefore) == 0 {
		t.Fatal("ls-remote returned no output for refs/heads/main before merge")
	}
	remoteBefore := lsBefore[0]

	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, Push: false,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s)", err, res.Status)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged", res.Status)
	}
	if res.Pushed {
		t.Errorf("Pushed=true, want false — Push was not requested")
	}
	lsAfter := strings.Fields(mustGit(t, repo, "ls-remote", "origin", "refs/heads/main"))
	if len(lsAfter) == 0 {
		t.Fatal("ls-remote returned no output for refs/heads/main after merge")
	}
	remoteAfter := lsAfter[0]
	if remoteAfter != remoteBefore {
		t.Errorf("remote HEAD moved (%s→%s) without Push=true", remoteBefore, remoteAfter)
	}
}

// TestMergeLocalDefAheadOfOriginMerges is the regression test for koryph-3fs:
// when local <def> is AHEAD of origin/<def> (an out-of-band local commit that
// has not been pushed), a candidate branch must still merge. The old code
// rebased onto origin/<def> but ff-merged into local <def>, so the divergence
// rewrote the shared commit and the fast-forward failed, stranding the bead.
func TestMergeLocalDefAheadOfOriginMerges(t *testing.T) {
	isolateGit(t)
	repo, _ := initRepoWithRemote(t) // local main == origin/main at the seed
	ctx := context.Background()

	// Feature branch based on the seed.
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")

	// Out-of-band LOCAL commit on main that is NOT pushed: local main is now
	// ahead of origin/main — the exact divergence that stranded fr3.1 and 5ov.
	commitIn(t, repo, "c.txt", "local-only\n", "local: out-of-band commit")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, Push: true,
	})
	if err != nil {
		t.Fatalf("Merge with local main ahead of origin: %v (status=%s output=%s)",
			err, res.Status, res.GateOutput)
	}
	if res.Status != "merged" {
		t.Fatalf("Status=%q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	// The ff-merge must combine BOTH the out-of-band local commit and the
	// feature — main now carries each file.
	for _, f := range []string{"b.txt", "c.txt"} {
		if _, err := os.Stat(filepath.Join(repo, f)); err != nil {
			t.Errorf("%s missing on main after merge: %v", f, err)
		}
	}
	if !res.Pushed {
		t.Error("Pushed=false; the combined local+feature history should have reached origin")
	}
}

func TestRunGate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if ok, out := RunGate(ctx, dir, []string{"true", "true"}); !ok {
		t.Errorf("all-true gate should pass, output=%s", out)
	}
	if ok, _ := RunGate(ctx, dir, []string{"false", "true"}); ok {
		t.Errorf("gate with a failing command should fail")
	}
}

func TestProtected(t *testing.T) {
	hits := Protected([]string{"src/main.go", ".claude/foo", "CLAUDE.md", "docs/x.md"}, nil)
	if len(hits) != 2 {
		t.Fatalf("hits=%v, want 2 (.claude/foo, CLAUDE.md)", hits)
	}
	// Extra prefixes.
	if got := Protected([]string{"infra/secret.tf"}, []string{"infra/"}); len(got) != 1 {
		t.Errorf("extra-prefix hit=%v, want [infra/secret.tf]", got)
	}
	// A file that merely shares a name prefix must NOT match an exact-file rule.
	if got := Protected([]string{"CLAUDE.md.bak"}, nil); len(got) != 0 {
		t.Errorf("CLAUDE.md.bak should not match CLAUDE.md, got %v", got)
	}
	// Newly hardcoded defaults: CI, gate Makefile, guard scripts, agents.
	for _, p := range []string{".github/workflows/ci.yml", "Makefile", "hooks/x.sh", "agents/impl.md", "AGENTS.md"} {
		if got := Protected([]string{p}, nil); len(got) != 1 {
			t.Errorf("%q should be protected by default, got %v", p, got)
		}
	}
	// Case-insensitive: a case-insensitive FS must not dodge .github/ via .Github/.
	if got := Protected([]string{".Github/workflows/evil.yml"}, nil); len(got) != 1 {
		t.Errorf(".Github/ should match .github/ case-insensitively, got %v", got)
	}
	if got := Protected([]string{"MAKEFILE"}, nil); len(got) != 1 {
		t.Errorf("MAKEFILE should match Makefile case-insensitively, got %v", got)
	}
	// Cleaned path forms still hit.
	if got := Protected([]string{"./.github/x.yml"}, nil); len(got) != 1 {
		t.Errorf("./.github/x.yml should hit after cleaning, got %v", got)
	}
	// Boundary safety: a sibling that only shares a directory-name prefix must NOT match.
	if got := Protected([]string{".githubfoo/x"}, nil); len(got) != 0 {
		t.Errorf(".githubfoo/ must not match .github/, got %v", got)
	}
	if got := Protected([]string{"Makefile.in"}, nil); len(got) != 0 {
		t.Errorf("Makefile.in must not match the exact Makefile rule, got %v", got)
	}
	// The bare protected directory itself (as a path) is caught.
	if got := Protected([]string{".github"}, nil); len(got) != 1 {
		t.Errorf(".github (bare dir) should be protected, got %v", got)
	}
}
