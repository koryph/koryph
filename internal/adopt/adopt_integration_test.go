// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package adopt

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// fakeAdoptBD is a stand-in `bd` binary covering exactly the subcommands the
// adopt integration flow below touches. It logs nothing (unlike
// internal/beads' fakeInitBD) since no test here inspects argv — only that
// each mutation lands on disk. Mirrors the fake-bd-script idiom established by
// internal/beads/initx_test.go.
const fakeAdoptBD = `#!/bin/sh
case "$1" in
  version)
    echo "bd version 1.1.0"
    ;;
  init)
    mkdir -p .beads
    printf 'issues.jsonl\n' > .beads/.gitignore
    printf 'sync:\n  remote: "git+https://github.com/testowner/testrepo.git"\n' > .beads/config.yaml
    exit 0
    ;;
  doctor)
    exit 0
    ;;
  ready)
    echo '[]'
    ;;
  hooks)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`

// writeFakeAdoptBD installs fakeAdoptBD into a scratch dir and points
// KORYPH_BD_BIN at it for the duration of the test.
func writeFakeAdoptBD(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bd")
	if err := os.WriteFile(bin, []byte(fakeAdoptBD), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("KORYPH_BD_BIN", bin)
	return bin
}

// writeFakeClaudeHome creates a fake $HOME containing a .claude.json that
// verifies as email, so account.Discover finds exactly one verified
// candidate (the personal profile) without touching the operator's real
// ~/.claude.json. Mirrors internal/account/account_test.go's writeConfig
// fixture shape.
func writeFakeClaudeHome(t *testing.T, email string) string {
	t.Helper()
	home := t.TempDir()
	writeTestFile(t, filepath.Join(home, ".claude.json"), `{"oauthAccount":{"emailAddress":"`+email+`"}}`)
	t.Setenv("HOME", home)
	return home
}

// TestAdoptIntegration_FreshRepoEndToEnd drives the same sequence
// cmd/koryph/adopt.go's cmdAdopt does — Detect -> BuildPlan -> the
// non-interactive resolvers -> ExecuteBeads -> RegisterAndConfigure ->
// InstallAssets — entirely through the adopt package's exported API, with no
// cmd-layer prompting/streaming involved. Hermetic: KORYPH_HOME, HOME, and
// KORYPH_BD_BIN all point at scratch fixtures for the duration of the test,
// so it never touches the operator's real ~/.koryph, ~/.claude.json, or PATH
// bd (git itself is still the real binary, matching every other test in this
// repo — see internal/beads/initx_test.go and internal/onboard/onboard_test.go).
func TestAdoptIntegration_FreshRepoEndToEnd(t *testing.T) {
	ctx := context.Background()

	// --- hermetic sandbox --------------------------------------------------
	root := initGitRepo(t)
	writeTestFile(t, filepath.Join(root, "Makefile"), ".PHONY: test\ntest:\n\tgo test ./...\n")
	writeTestFile(t, filepath.Join(root, "src", "main.go"), "package main\n\nfunc main() {}\n")
	runGitCmd(t, root, "remote", "add", "origin", "https://github.com/testowner/testrepo.git")

	koryphHome := t.TempDir()
	t.Setenv("KORYPH_HOME", koryphHome)
	writeFakeAdoptBD(t)
	writeFakeClaudeHome(t, "t@t.t")

	// --- detect --------------------------------------------------------------
	snap, err := Detect(ctx, root)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// --- plan (structural coverage lives in plan_test.go; just build it here,
	// as adopt.go does, before acting on anything) -----------------------------
	_ = BuildPlan(snap)

	// --- confirm: non-interactive resolvers -----------------------------------
	acct, err := ResolveAccountNonInteractive(snap.AccountCandidates, "", "", "")
	if err != nil {
		t.Fatalf("ResolveAccountNonInteractive: %v", err)
	}
	if acct.Identity != "t@t.t" {
		t.Fatalf("resolved account identity = %q, want t@t.t (candidates=%+v)", acct.Identity, snap.AccountCandidates)
	}

	gate, err := ResolveGateNonInteractive(snap.GateProposals, nil)
	if err != nil {
		t.Fatalf("ResolveGateNonInteractive: %v", err)
	}
	if strings.Join(gate, ",") != "make test" {
		t.Fatalf("inferred gate = %v, want [make test]", gate)
	}

	forgeName, err := ResolveForgeNonInteractive(snap.ForgeProposal, "", snap.Inventory.Remote)
	if err != nil {
		t.Fatalf("ResolveForgeNonInteractive: %v", err)
	}
	if forgeName != "github" {
		t.Fatalf("inferred forge = %q, want github", forgeName)
	}

	// --- execute: beads --------------------------------------------------------
	if _, _, err := ExecuteBeads(ctx, root, BeadsOpts{
		Prefix:    snap.ProjectID,
		RemoteURL: snap.Inventory.Remote,
	}); err != nil {
		t.Fatalf("ExecuteBeads: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".beads")); err != nil {
		t.Fatalf(".beads not created: %v", err)
	}

	// --- execute: register + config --------------------------------------------
	store := registry.NewStore()
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store init: %v", err)
	}
	rec, _, err := RegisterAndConfigure(ctx, store, snap, acct, gate, forgeName, snap.AreaMap, false)
	if err != nil {
		t.Fatalf("RegisterAndConfigure: %v", err)
	}

	cfg, err := project.Load(root)
	if err != nil {
		t.Fatalf("project.Load: %v", err)
	}
	if strings.Join(cfg.Gate, ",") != "make test" {
		t.Errorf("koryph.project.json gate = %v, want [make test]", cfg.Gate)
	}
	if cfg.Forge != "github" {
		t.Errorf("koryph.project.json forge = %q, want github", cfg.Forge)
	}

	storedRec, err := store.Get(rec.ProjectID)
	if err != nil {
		t.Fatalf("registry record missing after register: %v", err)
	}
	if storedRec.ExpectedIdentity != "t@t.t" || storedRec.AccountProfile != "personal" {
		t.Errorf("registry record account = %+v, want personal / t@t.t", storedRec)
	}

	// --- execute: assets ---------------------------------------------------------
	InstallAssets(io.Discard, root)

	agentEntries, err := os.ReadDir(filepath.Join(root, ".claude", "agents"))
	if err != nil || len(agentEntries) == 0 {
		t.Fatalf(".claude/agents not populated: err=%v entries=%d", err, len(agentEntries))
	}

	settingsRaw, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
	if err != nil {
		t.Fatalf("read .claude/settings.json: %v", err)
	}
	var settings struct {
		Hooks struct {
			SessionStart []struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"SessionStart"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(settingsRaw, &settings); err != nil {
		t.Fatalf("parse settings.json: %v\nraw: %s", err, settingsRaw)
	}
	bdPrimeCount := 0
	for _, entry := range settings.Hooks.SessionStart {
		for _, h := range entry.Hooks {
			if strings.Contains(h.Command, "bd prime") {
				bdPrimeCount++
			}
		}
	}
	if bdPrimeCount != 1 {
		t.Errorf("SessionStart entries whose command contains %q = %d, want exactly 1\nraw: %s", "bd prime", bdPrimeCount, settingsRaw)
	}

	// --- idempotence at the plan level: re-Detect + BuildPlan marks done -------
	snap2, err := Detect(ctx, root)
	if err != nil {
		t.Fatalf("second Detect: %v", err)
	}
	plan2 := BuildPlan(snap2)
	for _, id := range []StepID{StepBeads, StepRegister, StepConfig, StepAssets} {
		st := findStep(t, plan2, id)
		if st.State != StateDone {
			t.Errorf("post-run step %q State = %q, want done (detail: %s)", id, st.State, st.Detail)
		}
	}
}
