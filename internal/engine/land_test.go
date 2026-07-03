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
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/signing"
)

// prOpenedFixture runs a merge_policy pr wave against a bare remote (with a
// fake gh) and returns the fixture plus its registry record, with bead tb1
// parked in pr-opened and its branch pushed to origin.
func prOpenedFixture(t *testing.T) (*fix, *registry.Record) {
	t.Helper()
	f := newFixture(t, fixOpts{mergePolicy: "pr"})
	t.Setenv("GIT_CONFIG_COUNT", "0")

	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	runGit(t, tmp, "init", "--bare", "-b", "main", "bare.git")
	runGit(t, f.repo, "remote", "add", "origin", bare)
	runGit(t, f.repo, "push", "-u", "origin", "main")

	ghBin := filepath.Join(tmp, "bin")
	writeFile(t, filepath.Join(ghBin, "gh"), fakeGhScript, 0o755)
	t.Setenv("PATH", ghBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("pr wave: %v", err)
	}
	if got.PROpened != 1 {
		t.Fatalf("pr wave Outcome=%+v, want 1 pr-opened\n%s", got, out.String())
	}
	rec, err := registry.NewStoreAt(f.home).Get("proj")
	if err != nil {
		t.Fatalf("Get record: %v", err)
	}
	return f, rec
}

// TestLandFastForwardsPROpenedBead is the happy landing path: a pr-opened bead
// lands with the EXACT branch-tip SHA on the default branch (ff, no rewrite),
// the parked slot flips to merged, and the bead is closed.
func TestLandFastForwardsPROpenedBead(t *testing.T) {
	f, rec := prOpenedFixture(t)
	cfg, err := project.Load(f.repo)
	if err != nil {
		t.Fatalf("Load cfg: %v", err)
	}

	branchTip := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "agent/tb1"))

	var out bytes.Buffer
	res, err := Land(context.Background(), rec, cfg, LandOpts{Bead: "tb1", Reason: "landed", Out: &out})
	if err != nil {
		t.Fatalf("Land: %v", err)
	}
	if res.Status != "merged" {
		t.Fatalf("Land status=%q, want merged", res.Status)
	}
	// ff preserves the SHA: the landed commit IS the branch tip.
	if res.SHA != branchTip {
		t.Errorf("landed SHA=%s, want branch tip %s (ff must preserve the SHA)", res.SHA, branchTip)
	}
	if got := strings.TrimSpace(runGit(t, f.repo, "rev-parse", "main")); got != branchTip {
		t.Errorf("main=%s, want %s", got, branchTip)
	}
	// The default branch reached the remote at that same SHA.
	if ls := strings.Fields(runGit(t, f.repo, "ls-remote", "origin", "refs/heads/main")); len(ls) == 0 || ls[0] != branchTip {
		t.Errorf("remote main=%v, want %s", ls, branchTip)
	}
	// No merge commit: main is linear (the tip has one parent).
	parents := strings.Fields(runGit(t, f.repo, "rev-list", "--parents", "-n", "1", "main"))
	if len(parents) != 2 { // <commit> <single-parent>
		t.Errorf("main tip has %d parents (%v), want 1 (no merge commit)", len(parents)-1, parents)
	}
	// Slot flipped pr-opened -> merged; bead closed; worktree + branch cleaned.
	if sl := slotStatus(t, f.repo, "tb1"); sl.Status != ledger.SlotMerged {
		t.Errorf("slot status=%q, want merged", sl.Status)
	}
	if log := f.bdLog(t); !strings.Contains(log, "close tb1") {
		t.Errorf("bd.log missing close for tb1:\n%s", log)
	}
	if _, err := os.Stat(filepath.Join(f.wtRoot, "agent-tb1")); !os.IsNotExist(err) {
		t.Errorf("worktree not cleaned: %v", err)
	}
	if branchExists(f.repo, "agent/tb1") {
		t.Error("branch agent/tb1 still present after land")
	}
}

// TestLandRefusesSquashUnderSigning proves an override that would break
// signatures is refused up front (before touching git) with a clear error.
func TestLandRefusesSquashUnderSigning(t *testing.T) {
	cfg := &project.Config{
		Gate:    []string{"true"},
		Signing: &signing.Config{Required: true, Identity: "x@example.com"},
	}
	rec := &registry.Record{Root: t.TempDir(), DefaultBranch: "main"}
	_, err := Land(context.Background(), rec, cfg, LandOpts{Bead: "tb1", Method: "squash"})
	if err == nil || !strings.Contains(err.Error(), "signing.required") {
		t.Fatalf("Land err=%v, want a signing.required refusal", err)
	}
}
