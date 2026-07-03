// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// slotStatus loads the latest run and returns the status of one bead's slot.
func slotStatus(t *testing.T, repo, bead string) *ledger.Slot {
	t.Helper()
	run, err := ledger.NewStore(repo).LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots[bead]
	if sl == nil {
		t.Fatalf("no slot for bead %q in run %s", bead, run.RunID)
	}
	return sl
}

// TestRunPRPolicyBlocksWithoutRemote verifies finishCandidate routes
// merge_policy pr to the PR path, and that a project with no git remote yields
// a clear block (not a crash or a silent drop) with the default branch
// untouched.
func TestRunPRPolicyBlocksWithoutRemote(t *testing.T) {
	f := newFixture(t, fixOpts{mergePolicy: "pr"})
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 || got.Merged != 0 || got.PROpened != 0 || got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 0 merged / 0 pr-opened / 1 blocked", got)
	}

	sl := slotStatus(t, f.repo, "tb1")
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "remote") {
		t.Errorf("slot note = %q, want it to mention the missing remote", sl.Note)
	}
	// The agent commit must NOT have landed on the default branch.
	if log := runGit(t, f.repo, "log", "--format=%s", "main"); strings.Contains(log, "feat(tb1): work") {
		t.Errorf("agent commit reached main on the PR path:\n%s", log)
	}
}

// fakeGhScript answers the three gh calls the PR path makes: `auth status`
// (ready), `pr view` (no existing PR), and `pr create` (prints a URL).
const fakeGhScript = `#!/bin/sh
if [ "$1" = "auth" ]; then exit 0; fi
if [ "$1" = "pr" ] && [ "$2" = "view" ]; then exit 1; fi
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo "https://github.com/acme/proj/pull/42"
  exit 0
fi
exit 1
`

// TestRunPRPolicyOpensPR is the happy end-to-end PR path: with a remote and a
// (faked) authenticated gh, a merge_policy pr wave pushes the agent branch,
// opens a PR, parks the slot in pr-opened, and never advances the default
// branch. The worktree and branch survive for a later landing step.
func TestRunPRPolicyOpensPR(t *testing.T) {
	f := newFixture(t, fixOpts{mergePolicy: "pr"})

	// Clear any direnv-injected git config (safe.bareRepository=explicit) that
	// would block pushing to the bare remote.
	t.Setenv("GIT_CONFIG_COUNT", "0")

	// A bare remote for the project, with main pushed and tracking.
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	runGit(t, tmp, "init", "--bare", "-b", "main", "bare.git")
	runGit(t, f.repo, "remote", "add", "origin", bare)
	runGit(t, f.repo, "push", "-u", "origin", "main")

	// A fake gh on PATH so GhCLI resolves it.
	ghBin := filepath.Join(tmp, "bin")
	writeFile(t, filepath.Join(ghBin, "gh"), fakeGhScript, 0o755)
	t.Setenv("PATH", ghBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	mainBefore := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main"))

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 || got.PROpened != 1 || got.Merged != 0 || got.Failed != 0 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 pr-opened", got)
	}

	sl := slotStatus(t, f.repo, "tb1")
	if sl.Status != ledger.SlotPROpened {
		t.Fatalf("slot status = %q, want pr-opened (note=%q)", sl.Status, sl.Note)
	}
	if !strings.Contains(sl.Note, "pull/42") {
		t.Errorf("slot note = %q, want it to carry the PR URL", sl.Note)
	}

	// The default branch was NOT advanced.
	if after := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main")); after != mainBefore {
		t.Errorf("main advanced to %s (was %s); PR path must not merge to default", after, mainBefore)
	}
	// The agent branch reached the remote.
	if ls := strings.TrimSpace(runGit(t, f.repo, "ls-remote", "origin", "refs/heads/agent/tb1")); ls == "" {
		t.Error("agent/tb1 not pushed to origin")
	}
	// The worktree and branch survive for the landing step.
	if _, err := os.Stat(filepath.Join(f.wtRoot, "agent-tb1")); err != nil {
		t.Errorf("worktree removed on PR path: %v", err)
	}
	if !branchExists(f.repo, "agent/tb1") {
		t.Error("branch agent/tb1 deleted on PR path; it must survive for landing")
	}
}
