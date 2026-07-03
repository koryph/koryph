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
