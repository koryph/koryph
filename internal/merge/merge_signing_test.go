// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/signing/signingtest"
)

func TestMergeRequireSignedRejectsUnsignedBeforeAnyMutation(t *testing.T) {
	isolateGit(t)
	repo := initRepo(t) // commit.gpgsign=false → branch commits are unsigned
	ctx := context.Background()
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	tip := headOf(t, wt.Path, "HEAD")
	mainBefore := headOf(t, repo, "main")

	slot := &fakeSlot{}
	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireSigned: true,
		SlotOwner: "owner-1", Slot: slot,
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != "unsigned" {
		t.Fatalf("Status = %q, want unsigned (output=%s)", res.Status, res.GateOutput)
	}
	if !strings.Contains(res.GateOutput, tip) {
		t.Errorf("GateOutput = %q, want offending SHA %s listed", res.GateOutput, tip)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main moved on unsigned rejection: %s != %s", got, mainBefore)
	}
	if !fsx.Exists(wt.Path) {
		t.Errorf("worktree removed on unsigned rejection; must be kept")
	}
	if slot.acquired != 1 || slot.released != 1 {
		t.Errorf("slot acquired=%d released=%d, want 1/1 (slot released on rejection)", slot.acquired, slot.released)
	}
}

// TestMergeRequireSignedCatchesUnsignedPostRebaseCommit is the koryph audit
// finding #30 fix: preflight's RequireSigned check only ever inspects
// <default>..<branch> BEFORE the rebase runs, so a commit introduced AFTER
// preflight — here, by a merge_prepare step, which runs after the rebase and
// before the gate — never appeared in that range at check time and used to
// land completely unverified. This reproduces it by having the Prepare
// command make its own commit with --no-gpg-sign (bypassing the repo's
// otherwise-automatic signing), which the pre-rebase preflight pass could
// never have seen. Without a post-rebase re-check this merges silently;
// with it, the unsigned injected commit must be caught before landing.
func TestMergeRequireSignedCatchesUnsignedPostRebaseCommit(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen", "git")
	isolateGit(t)
	signingtest.SpawnAgent(t)
	priv, pub := signingtest.GenKey(t)
	signingtest.AddKey(t, priv)

	repo := initRepo(t)
	ctx := context.Background()
	sc := &signing.Config{
		Required: true, Mode: signing.ModeSSH, Provider: signing.ProviderFile,
		KeyRef: priv, Identity: "test@example.com", PublicKey: pub,
	}
	if err := signing.ConfigureRepo(ctx, repo, sc); err != nil {
		t.Fatalf("ConfigureRepo: %v", err)
	}

	// Worktrees share the repo config: this commit signs automatically and
	// passes preflight on its own.
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	mainBefore := headOf(t, repo, "main")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireSigned: true,
		// Simulates a merge_prepare step (or any tree mutation between
		// preflight and landing) whose own commit bypasses signing — the
		// exact class of commit the pre-rebase-only check could never see.
		Prepare: []string{"echo injected >c.txt && git add c.txt && git commit -q --no-gpg-sign -m 'chore: unsigned injection'"},
	})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if res.Status != StatusUnsigned {
		t.Fatalf("Status = %q, want %q (output=%s)", res.Status, StatusUnsigned, res.GateOutput)
	}
	if got := headOf(t, repo, "main"); got != mainBefore {
		t.Errorf("main moved on post-rebase unsigned rejection: %s != %s", got, mainBefore)
	}
}

func TestMergeRequireSignedAcceptsSignedBranch(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen", "git")
	isolateGit(t)
	signingtest.SpawnAgent(t)
	priv, pub := signingtest.GenKey(t)
	signingtest.AddKey(t, priv)

	repo := initRepo(t)
	ctx := context.Background()
	sc := &signing.Config{
		Required: true, Mode: signing.ModeSSH, Provider: signing.ProviderFile,
		KeyRef: priv, Identity: "test@example.com", PublicKey: pub,
	}
	if err := signing.ConfigureRepo(ctx, repo, sc); err != nil {
		t.Fatalf("ConfigureRepo: %v", err)
	}

	// Worktrees share the repo config: this commit signs automatically.
	wt := worktreeOn(t, repo, "agent/x")
	commitIn(t, wt.Path, "b.txt", "feature\n", "add b")
	tip := headOf(t, wt.Path, "HEAD")

	res, err := Merge(ctx, Opts{
		RepoRoot: repo, Branch: "agent/x", DefaultBranch: "main",
		Gate: []string{"true"}, RequireSigned: true,
	})
	if err != nil {
		t.Fatalf("Merge: %v (status=%s output=%s)", err, res.Status, res.GateOutput)
	}
	if res.Status != "merged" {
		t.Fatalf("Status = %q, want merged (output=%s)", res.Status, res.GateOutput)
	}
	if got := headOf(t, repo, "main"); got != tip {
		t.Errorf("main = %s, want fast-forwarded to signed tip %s", got, tip)
	}
}
