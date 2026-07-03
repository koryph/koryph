// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// AllowedSignersFileName lives at the repo root, holds "identity pubkey"
// lines, and is meant to be COMMITTED by the operator so verification
// travels with the repo.
const AllowedSignersFileName = ".allowed_signers"

// ConfigureRepo idempotently applies the signing policy to the repo-level
// git config at repoRoot. Git worktrees share the main repo's .git/config,
// so agent worktrees pick this up automatically.
//
// Mode ssh:
//
//	gpg.format ssh; user.signingkey "key::<public key literal>" (the private
//	half is served by the SSH agent via SSH_AUTH_SOCK); commit.gpgsign true;
//	an .allowed_signers file at the repo root gains "identity pubkey" and
//	gpg.ssh.allowedSignersFile points at it (required for %G? verification).
//
// Mode gitsign (sigstore keyless):
//
//	gpg.format x509; gpg.x509.program gitsign; commit.gpgsign true. No agent
//	or vault key is involved, but the FIRST signature needs an interactive
//	browser for the OIDC flow.
func ConfigureRepo(ctx context.Context, repoRoot string, cfg *Config) error {
	if cfg == nil {
		return fmt.Errorf("signing: nil config")
	}
	if cfg.EffectiveMode() == ModeGitsign {
		return setGitConfig(ctx, repoRoot, map[string]string{
			"gpg.format":       "x509",
			"gpg.x509.program": "gitsign",
			"commit.gpgsign":   "true",
		})
	}

	if cfg.PublicKey == "" {
		return fmt.Errorf("signing: mode ssh needs public_key (run `koryph signing setup`)")
	}
	if cfg.Identity == "" {
		return fmt.Errorf("signing: identity (signer email) is required")
	}
	pub := keyBlob(cfg.PublicKey)
	if pub == "" {
		return fmt.Errorf("signing: public_key %q is not an SSH public key literal", cfg.PublicKey)
	}

	signersPath := filepath.Join(repoRoot, AllowedSignersFileName)
	if err := ensureAllowedSigner(signersPath, cfg.Identity, pub); err != nil {
		return err
	}
	return setGitConfig(ctx, repoRoot, map[string]string{
		"gpg.format":                 "ssh",
		"user.signingkey":            "key::" + pub,
		"commit.gpgsign":             "true",
		"gpg.ssh.allowedSignersFile": signersPath,
	})
}

// ensureAllowedSigner appends "identity type blob" to the allowed-signers
// file unless an equivalent line already exists (idempotent).
func ensureAllowedSigner(path, identity, pubBlob string) error {
	line := identity + " " + pubBlob
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("signing: %w", err)
	}
	for _, l := range strings.Split(string(data), "\n") {
		f := strings.Fields(l)
		if len(f) >= 3 && f[0] == identity && f[1]+" "+f[2] == pubBlob {
			return nil
		}
	}
	body := string(data)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	body += line + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return fmt.Errorf("signing: %w", err)
	}
	return nil
}

// setGitConfig applies key=value pairs to the repo-local git config.
func setGitConfig(ctx context.Context, repoRoot string, kv map[string]string) error {
	// Deterministic order for reproducible errors.
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	for _, k := range keys {
		if _, err := execx.MustSucceed(ctx, execx.Cmd{
			Dir: repoRoot, Name: "git", Args: []string{"config", k, kv[k]},
		}); err != nil {
			return fmt.Errorf("signing: %w", err)
		}
	}
	return nil
}

// RepoState is the signing-relevant slice of a repo's git config, for
// `koryph signing status`.
type RepoState struct {
	GPGFormat          string `json:"gpg_format"`
	SigningKey         string `json:"user_signingkey"`
	CommitGPGSign      string `json:"commit_gpgsign"`
	AllowedSignersFile string `json:"gpg_ssh_allowed_signers_file,omitempty"`
	X509Program        string `json:"gpg_x509_program,omitempty"`
}

// InspectRepo reads the signing-relevant git config from repoRoot (missing
// keys come back empty; never an error for unset config).
func InspectRepo(ctx context.Context, repoRoot string) RepoState {
	get := func(key string) string {
		res, err := execx.Run(ctx, execx.Cmd{
			Dir: repoRoot, Name: "git", Args: []string{"config", "--get", key},
		})
		if err != nil || res.ExitCode != 0 {
			return ""
		}
		return strings.TrimSpace(res.Stdout)
	}
	return RepoState{
		GPGFormat:          get("gpg.format"),
		SigningKey:         get("user.signingkey"),
		CommitGPGSign:      get("commit.gpgsign"),
		AllowedSignersFile: get("gpg.ssh.allowedSignersFile"),
		X509Program:        get("gpg.x509.program"),
	}
}
