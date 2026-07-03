// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package account

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/execx"
)

// Valid reports whether m is a known billing mode.
func (m BillingMode) Valid() bool {
	return m == BillingSubscription || m == BillingAPIKey
}

// Env builds the complete child environment for a dispatched agent.
//
// It starts from the parent environment with CLAUDE_CONFIG_DIR and
// ANTHROPIC_API_KEY scrubbed, then re-injects per the account rules:
//
//   - p.ConfigDir != ""  → CLAUDE_CONFIG_DIR=<dir> (work / custom profile).
//     Personal (ConfigDir == "") stays UNSET — never point it at ~/.claude.
//   - billing == BillingAPIKey → ANTHROPIC_API_KEY=<apiKey>. Callers must
//     validate the key is non-empty (Dispatch does); Env keeps the documented
//     signature and injects whatever it is given.
func Env(p Profile, billing BillingMode, apiKey string) []string {
	env := execx.BaseEnv("CLAUDE_CONFIG_DIR", "ANTHROPIC_API_KEY")
	if p.ConfigDir != "" {
		env = append(env, "CLAUDE_CONFIG_DIR="+p.ConfigDir)
	}
	if billing == BillingAPIKey {
		env = append(env, "ANTHROPIC_API_KEY="+apiKey)
	}
	return env
}

// ConfigJSONPath resolves the .claude.json path for a profile:
// $HOME/.claude.json for the personal default profile (ConfigDir == ""),
// otherwise <ConfigDir>/.claude.json.
func ConfigJSONPath(p Profile) string {
	if p.ConfigDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.Getenv("HOME")
		}
		return filepath.Join(home, ".claude.json")
	}
	return filepath.Join(p.ConfigDir, ".claude.json")
}

// claudeConfig is the subset of .claude.json we verify against.
type claudeConfig struct {
	OAuthAccount struct {
		EmailAddress     string `json:"emailAddress"`
		OrganizationName string `json:"organizationName"`
	} `json:"oauthAccount"`
}

// Verify reads and parses the profile's .claude.json and returns the
// logged-in identity. A missing file, unparseable JSON, or an empty
// emailAddress is an error — verification fails closed.
func Verify(ctx context.Context, p Profile) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	path := ConfigJSONPath(p)
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("account verify (profile %q): reading %s: %w — refusing dispatch", p.Name, path, err)
	}
	var cfg claudeConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Identity{}, fmt.Errorf("account verify (profile %q): parsing %s: %w — refusing dispatch", p.Name, path, err)
	}
	if cfg.OAuthAccount.EmailAddress == "" {
		return Identity{}, fmt.Errorf("account verify (profile %q): %s has no oauthAccount.emailAddress (not logged in?) — refusing dispatch", p.Name, path)
	}
	return Identity{
		Email:        cfg.OAuthAccount.EmailAddress,
		Organization: cfg.OAuthAccount.OrganizationName,
		ConfigPath:   path,
	}, nil
}

// VerifyExpected verifies the profile identity and compares the logged-in
// email to the registry's expected identity, case-insensitively. Any error
// or mismatch refuses dispatch (fail closed).
func VerifyExpected(ctx context.Context, p Profile, expected string) (Identity, error) {
	id, err := Verify(ctx, p)
	if err != nil {
		return Identity{}, err
	}
	if expected == "" {
		return Identity{}, fmt.Errorf("account verify (profile %q): registry expected identity is empty (config: %s) — refusing dispatch", p.Name, id.ConfigPath)
	}
	if !strings.EqualFold(id.Email, expected) {
		return Identity{}, fmt.Errorf("account mismatch: logged in as %s, registry expects %s (config: %s) — refusing dispatch", id.Email, expected, id.ConfigPath)
	}
	return id, nil
}
