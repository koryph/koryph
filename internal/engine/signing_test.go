// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/signing/signingtest"
)

// signingConfig adds a required ssh signing policy to the fixture project
// (provider=file so the key loads from disk, no vault) and returns it.
func signingConfig(t *testing.T, f *fix) *signing.Config {
	t.Helper()
	priv, pub := signingtest.GenKey(t)
	cfg, err := project.Load(f.repo)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Signing = &signing.Config{
		Required: true, Mode: signing.ModeSSH, Provider: signing.ProviderFile,
		KeyRef: priv, Identity: "signer@example.com", PublicKey: pub,
	}
	if err := cfg.Save(f.repo); err != nil {
		t.Fatal(err)
	}
	return cfg.Signing
}

func TestRunSigningAgentNotReadyFailsClosed(t *testing.T) {
	signingtest.RequireTools(t, "ssh-keygen", "git")
	f := newFixture(t, fixOpts{})
	signingConfig(t, f)
	t.Setenv("SSH_AUTH_SOCK", "") // no scoped agent populated → preflight fails closed

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	if err == nil {
		t.Fatalf("Run should fail closed when the agent lacks the key; outcome=%+v", got)
	}
	if got.Code != ExitFatal {
		t.Errorf("Code = %d, want %d", got.Code, ExitFatal)
	}
	if !strings.Contains(err.Error(), "koryph signing enable --project proj") {
		t.Errorf("err = %v, want the operator remediation command", err)
	}
	if got.Dispatched != 0 {
		t.Errorf("Dispatched = %d, want 0 (fail closed before dispatch)", got.Dispatched)
	}
}

// TestRunSigningEndToEndSignedMerge proves the full loop: the preflight
// configures the repo, the (fake) agent's worktree commit signs
// automatically via the shared repo config, and the merge verifies + lands
// it on main with a good signature.
func TestRunSigningEndToEndSignedMerge(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen", "git")
	signingtest.IsolateGit(t)
	f := newFixture(t, fixOpts{})
	cfg := signingConfig(t, f)

	// Simulate `koryph signing enable`: load the key into the koryph SCOPED
	// signing agent — the socket the dispatched (fake) agent will use to sign,
	// isolated from any operator ambient agent.
	vault, err := signing.LoadVault()
	if err != nil {
		t.Fatal(err)
	}
	if err := signing.EnsureScopedAgent(context.Background(), vault, cfg); err != nil {
		t.Fatalf("EnsureScopedAgent: %v", err)
	}
	t.Cleanup(func() { _ = signing.StopScopedAgent() })

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 1 {
		t.Fatalf("Merged = %d, want 1 (outcome %+v)", got.Merged, got)
	}

	// The merged tip on main carries a GOOD signature.
	cmd := exec.Command("git", "log", "-1", "--format=%G?", "main")
	cmd.Dir = f.repo
	sig, cerr := cmd.Output()
	if cerr != nil {
		t.Fatalf("git log: %v", cerr)
	}
	if s := strings.TrimSpace(string(sig)); s != "G" {
		t.Errorf("main tip %%G? = %q, want G", s)
	}
}

func TestSigningPreflightNoopWhenUnconfigured(t *testing.T) {
	if err := signingPreflight(context.Background(), "p", t.TempDir(), nil); err != nil {
		t.Errorf("nil config: %v", err)
	}
	if err := signingPreflight(context.Background(), "p", t.TempDir(), &signing.Config{Required: false}); err != nil {
		t.Errorf("not-required config: %v", err)
	}
}
