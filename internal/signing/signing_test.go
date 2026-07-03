// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/signing/signingtest"
)

// git runs a git command in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// initGitRepo builds a git repo on main with one seed commit (unsigned; made
// before any signing configuration).
func initGitRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "config", "user.name", "Sign Tester")
	git(t, repo, "config", "user.email", "signer@example.com")
	if err := os.WriteFile(filepath.Join(repo, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", "seed.txt")
	git(t, repo, "-c", "commit.gpgsign=false", "commit", "-qm", "seed")
	return repo
}

func commitFile(t *testing.T, repo, name, msg string, extraGitArgs ...string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(repo, name), []byte(name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", name)
	args := append(extraGitArgs, "commit", "-qm", msg)
	git(t, repo, args...)
	return strings.TrimSpace(git(t, repo, "rev-parse", "HEAD"))
}

// TestSSHSigningEndToEnd exercises the whole ssh path hermetically: a REAL
// throwaway ssh-agent, a generated ed25519 key loaded via the file-provider
// fallback (Fetch → `ssh-add -`), idempotent repo configuration, a signed
// commit that verifies clean, and an unsigned commit that gets flagged.
func TestSSHSigningEndToEnd(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen", "git")
	signingtest.IsolateGit(t)
	signingtest.SpawnAgent(t)
	t.Setenv("KORYPH_HOME", t.TempDir())
	ctx := context.Background()

	priv, pub := signingtest.GenKey(t)
	cfg := &Config{
		Required:  true,
		Mode:      ModeSSH,
		Provider:  ProviderFile,
		KeyRef:    priv,
		Identity:  "signer@example.com",
		PublicKey: pub,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	vault, err := LoadVault()
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}
	if AgentReady(ctx, pub) {
		t.Fatal("fresh agent must not hold the key yet")
	}
	if err := EnsureAgent(ctx, vault, cfg); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !AgentReady(ctx, pub) {
		t.Fatal("AgentReady = false after EnsureAgent")
	}

	repo := initGitRepo(t)
	if err := ConfigureRepo(ctx, repo, cfg); err != nil {
		t.Fatalf("ConfigureRepo: %v", err)
	}
	if err := ConfigureRepo(ctx, repo, cfg); err != nil {
		t.Fatalf("ConfigureRepo (2nd, idempotency): %v", err)
	}

	st := InspectRepo(ctx, repo)
	if st.GPGFormat != "ssh" || st.CommitGPGSign != "true" {
		t.Errorf("repo config = %+v, want gpg.format ssh + commit.gpgsign true", st)
	}
	if !strings.HasPrefix(st.SigningKey, "key::ssh-ed25519 ") {
		t.Errorf("user.signingkey = %q, want key:: public-key literal", st.SigningKey)
	}
	signers := filepath.Join(repo, AllowedSignersFileName)
	if st.AllowedSignersFile != signers {
		t.Errorf("allowedSignersFile = %q, want %q", st.AllowedSignersFile, signers)
	}
	data, err := os.ReadFile(signers)
	if err != nil {
		t.Fatalf("read %s: %v", signers, err)
	}
	if n := strings.Count(string(data), "signer@example.com"); n != 1 {
		t.Errorf(".allowed_signers has %d identity lines, want exactly 1 (idempotent):\n%s", n, data)
	}

	// Signed branch commit verifies clean.
	git(t, repo, "checkout", "-qb", "work")
	signedSHA := commitFile(t, repo, "signed.txt", "feat: signed work")
	bad, err := Verify(ctx, repo, "main", "work")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(bad) != 0 {
		t.Fatalf("Verify flagged signed commits: %v", bad)
	}

	// Unsigned commit (explicit gpgsign=false override) gets flagged.
	unsignedSHA := commitFile(t, repo, "unsigned.txt", "feat: sneaky unsigned", "-c", "commit.gpgsign=false")
	bad, err = Verify(ctx, repo, "main", "work")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(bad) != 1 {
		t.Fatalf("Verify = %v, want exactly the unsigned commit", bad)
	}
	if !strings.Contains(bad[0], unsignedSHA) || !strings.Contains(bad[0], "no signature") {
		t.Errorf("bad[0] = %q, want %s flagged as no signature", bad[0], unsignedSHA)
	}
	if strings.Contains(bad[0], signedSHA) {
		t.Errorf("signed commit %s wrongly flagged", signedSHA)
	}
}

// TestEnsureAgentProtonPassTemplate simulates `pass-cli ssh-agent load` with
// a fake pass-cli that records its argv and execs the REAL ssh-add against a
// fixture key — chosen over stubbing ssh-add so the "key lands in the system
// agent" semantics stay honest while no Proton account is needed.
func TestEnsureAgentProtonPassTemplate(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent", "ssh-add", "ssh-keygen")
	signingtest.SpawnAgent(t)
	dir := t.TempDir()
	t.Setenv("KORYPH_HOME", dir)
	ctx := context.Background()

	priv, pub := signingtest.GenKey(t)
	script := fakeCLI(t, dir, `exec ssh-add -q -t 3600 "`+priv+`"`)
	vault := DefaultVault()
	vault.Providers[ProviderProtonPass] = ProviderTemplates{
		AgentLoad: []string{script, "ssh-agent", "load", "--vault-name", "{ref}"},
		LoginHint: "pass-cli login",
	}
	if err := SaveVault(vault); err != nil {
		t.Fatalf("SaveVault: %v", err)
	}
	loaded, err := LoadVault()
	if err != nil {
		t.Fatalf("LoadVault: %v", err)
	}

	cfg := &Config{
		Required: true, Mode: ModeSSH, Provider: ProviderProtonPass,
		KeyRef: "Engineering", Identity: "a@b", PublicKey: pub,
	}
	if err := EnsureAgent(ctx, loaded, cfg); err != nil {
		t.Fatalf("EnsureAgent: %v", err)
	}
	if !AgentReady(ctx, pub) {
		t.Fatal("agent should hold the key after the load template ran")
	}
	if log := argvLog(t, dir); !strings.Contains(log, "ssh-agent load --vault-name Engineering") {
		t.Errorf("argv log = %q, want ssh-agent load with substituted vault name", log)
	}
}

func TestEnsureAgentLoadFailureCarriesLoginHint(t *testing.T) {
	signingtest.RequireTools(t, "ssh-agent")
	signingtest.SpawnAgent(t)
	dir := t.TempDir()
	script := fakeCLI(t, dir, `echo "no session" >&2; exit 1`)
	vault := DefaultVault()
	vault.Providers[ProviderProtonPass] = ProviderTemplates{
		AgentLoad: []string{script, "ssh-agent", "load"},
		LoginHint: "pass-cli login",
	}
	cfg := &Config{Mode: ModeSSH, Provider: ProviderProtonPass}
	err := EnsureAgent(context.Background(), vault, cfg)
	if err == nil || !strings.Contains(err.Error(), "pass-cli login") {
		t.Errorf("err = %v, want the pass-cli login hint", err)
	}
}

func TestEnsureAgentNoSocketIsActionable(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	cfg := &Config{Mode: ModeSSH, Provider: ProviderProtonPass}
	err := EnsureAgent(context.Background(), DefaultVault(), cfg)
	if err == nil || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
		t.Errorf("err = %v, want SSH_AUTH_SOCK guidance", err)
	}
}

func TestEnsureAgentGitsignIsNoop(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	cfg := &Config{Mode: ModeGitsign}
	if err := EnsureAgent(context.Background(), DefaultVault(), cfg); err != nil {
		t.Errorf("gitsign EnsureAgent = %v, want nil (no agent needed)", err)
	}
}

func TestConfigureRepoGitsign(t *testing.T) {
	signingtest.RequireTools(t, "git")
	signingtest.IsolateGit(t)
	ctx := context.Background()
	repo := initGitRepo(t)
	cfg := &Config{Required: true, Mode: ModeGitsign, Identity: "a@b"}
	if err := ConfigureRepo(ctx, repo, cfg); err != nil {
		t.Fatalf("ConfigureRepo: %v", err)
	}
	st := InspectRepo(ctx, repo)
	if st.GPGFormat != "x509" || st.X509Program != "gitsign" || st.CommitGPGSign != "true" {
		t.Errorf("gitsign repo config = %+v", st)
	}
}

func TestKeyBlobNormalization(t *testing.T) {
	if got := keyBlob("ssh-ed25519 AAAAC3Nza comment@host"); got != "ssh-ed25519 AAAAC3Nza" {
		t.Errorf("keyBlob = %q", got)
	}
	if got := keyBlob("not a key line"); got != "" {
		t.Errorf("keyBlob(non-key) = %q, want empty", got)
	}
	if got := keyBlob(""); got != "" {
		t.Errorf("keyBlob(empty) = %q, want empty", got)
	}
}
