// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"fmt"
	"strings"

	glpkg "github.com/koryph/koryph/internal/forge/gitlab"
)

// CheckGitLabOptions configures 'koryph bot check --forge gitlab'.
type CheckGitLabOptions struct {
	// Name selects the bot to check (required).
	Name string
	// Project is the optional "namespace/project" to validate against.
	// When empty, only the local credentials and token identity are checked.
	Project string
}

// CheckGitLab runs the GitLab bot validator chain and returns all findings.
//
// Validators in order:
//  1. token-valid   — token resolves; GET /personal_access_tokens/self succeeds;
//     required scopes present; token active (not revoked)
//  2. token-expiry  — token not expired; WARN if expiry within ExpiryWarnDays
//  3. vars-present  — KORYPH_BOT_TOKEN + KORYPH_BOT_TOKEN_EXPIRY present in
//     project CI/CD variables (if --project set); 403 degrades to WARN
func CheckGitLab(ctx context.Context, cfg *GitLabConfig, opts CheckGitLabOptions) ([]CheckFinding, error) {
	var findings []CheckFinding

	// 1. Resolve token and validate via API.
	token, tokenFinding := checkGLTokenValid(ctx, cfg, opts)
	findings = append(findings, tokenFinding)
	if tokenFinding.Level == CheckFail {
		return findings, nil
	}

	// 2. Token expiry (already handled inside checkGLTokenValid — extract
	//    warning if any).
	if tokenFinding.Level == CheckWarn {
		// The expiry warning is embedded in the token-valid finding message;
		// we surface it as a separate expiry finding so the check table is
		// consistent with the GitHub bot check layout.
		if strings.Contains(tokenFinding.Message, "expires in") {
			expiryF := CheckFinding{
				Check:       "token-expiry",
				Level:       CheckWarn,
				Message:     tokenFinding.Message,
				Remediation: fmt.Sprintf("koryph bot create --forge gitlab --name %s   # create a new token", opts.Name),
			}
			findings = append(findings, expiryF)
			// Replace the token-valid finding with a clean OK message.
			findings[0] = CheckFinding{
				Check:   "token-valid",
				Level:   CheckOK,
				Message: "token active; scopes OK",
			}
		}
	} else {
		findings = append(findings, CheckFinding{
			Check:   "token-expiry",
			Level:   CheckOK,
			Message: expiryMessage(cfg),
		})
	}

	if opts.Project == "" {
		return findings, nil
	}

	// 3. CI variables present on the project.
	findings = append(findings, checkGLVarsPresent(ctx, token, opts)...)

	return findings, nil
}

// checkGLTokenValid resolves the token, calls the API, and checks scopes.
// Returns the resolved token string and a CheckFinding.
func checkGLTokenValid(ctx context.Context, cfg *GitLabConfig, opts CheckGitLabOptions) (string, CheckFinding) {
	token, err := ResolveGLToken(ctx, cfg)
	if err != nil {
		remediation := fmt.Sprintf("koryph bot create --forge gitlab --name %s   # re-provision if token is lost", opts.Name)
		if ve, ok := err.(*VaultErr); ok { //nolint:errorlint
			remediation = ve.Remediation
		}
		return "", CheckFinding{
			Check:       "token-valid",
			Level:       CheckFail,
			Message:     fmt.Sprintf("cannot resolve token: %v", err),
			Remediation: remediation,
		}
	}

	// Validate via GitLab API.
	_, warning, err := glpkg.ValidateToken(ctx, token, cfg.Host, defaultGLScopes, cfg.ExpiryWarnDays())
	if err != nil {
		return "", CheckFinding{
			Check:       "token-valid",
			Level:       CheckFail,
			Message:     fmt.Sprintf("token validation failed: %v", err),
			Remediation: fmt.Sprintf("koryph bot create --forge gitlab --name %s", opts.Name),
		}
	}

	if warning != "" {
		return token, CheckFinding{
			Check:   "token-valid",
			Level:   CheckWarn,
			Message: warning,
		}
	}
	return token, CheckFinding{
		Check:   "token-valid",
		Level:   CheckOK,
		Message: fmt.Sprintf("token active; host=%s scopes=%s", cfg.Host, strings.Join(defaultGLScopes, ",")),
	}
}

// checkGLVarsPresent verifies that KORYPH_BOT_TOKEN and
// KORYPH_BOT_TOKEN_EXPIRY are set as CI/CD variables on the project.
func checkGLVarsPresent(ctx context.Context, token string, opts CheckGitLabOptions) []CheckFinding {
	const (
		keyToken  = "KORYPH_BOT_TOKEN"
		keyExpiry = "KORYPH_BOT_TOKEN_EXPIRY"
	)
	required := []string{keyToken, keyExpiry}

	keys, err := glpkg.ListProjectVariables(ctx, token, opts.Project)
	if err != nil {
		// Degraded check: cannot list variables (likely 403 / insufficient scope).
		return []CheckFinding{{
			Check: "vars-present",
			Level: CheckWarn,
			Message: fmt.Sprintf("%s: cannot list CI variables (%v); run `koryph bot attach --forge gitlab --name %s --project %s` to set them",
				opts.Project, err, opts.Name, opts.Project),
		}}
	}

	present := make(map[string]bool, len(keys))
	for _, k := range keys {
		present[k] = true
	}

	var findings []CheckFinding
	for _, want := range required {
		if present[want] {
			findings = append(findings, CheckFinding{
				Check:   "vars-present",
				Level:   CheckOK,
				Message: fmt.Sprintf("%s: CI variable %s present", opts.Project, want),
			})
		} else {
			findings = append(findings, CheckFinding{
				Check:       "vars-present",
				Level:       CheckFail,
				Message:     fmt.Sprintf("%s: CI variable %s missing", opts.Project, want),
				Remediation: fmt.Sprintf("koryph bot attach --forge gitlab --name %s --project %s", opts.Name, opts.Project),
			})
		}
	}
	return findings
}

// expiryMessage builds a human-readable expiry status string for use in
// the token-expiry CheckOK finding.
func expiryMessage(cfg *GitLabConfig) string {
	if cfg.ExpiresAt == "" {
		return fmt.Sprintf("token %q has no expiry date", cfg.TokenName)
	}
	return fmt.Sprintf("token %q expires on %s (%d-day warning threshold)",
		cfg.TokenName, cfg.ExpiresAt, ExpiryWarnDays)
}

// ExpiryWarnDays returns the configured expiry warn threshold for the bot,
// defaulting to the package-level constant.
func (c *GitLabConfig) ExpiryWarnDays() int {
	return ExpiryWarnDays
}

// CheckGitLabCredentials performs an offline-only credential check on all
// stored GitLab bots. Called by koryph doctor.
func CheckGitLabCredentials() ([]CredentialFinding, error) {
	names, err := ListGitLab()
	if err != nil {
		return nil, err
	}
	var findings []CredentialFinding
	for _, name := range names {
		cfg, err := LoadGitLab(name)
		if err != nil {
			findings = append(findings, CredentialFinding{
				Name:    name,
				Level:   CheckFail,
				Message: fmt.Sprintf("load error: %v", err),
			})
			continue
		}
		findings = append(findings, glCredentialFindingsFor(cfg)...)
	}
	return findings, nil
}

// glCredentialFindingsFor runs the offline credential check for one GitLab bot.
func glCredentialFindingsFor(cfg *GitLabConfig) []CredentialFinding {
	var findings []CredentialFinding

	if cfg.IsPointerGL() {
		if cfg.KeyRef == "" {
			findings = append(findings, CredentialFinding{
				Name:    cfg.Name,
				Level:   CheckFail,
				Message: fmt.Sprintf("pointer-mode gitlab bot %q is missing key_ref", cfg.Name),
			})
		} else {
			msg := fmt.Sprintf("credentials ok (host=%s project=%s provider=%s)", cfg.Host, cfg.Project, cfg.Provider)
			if cfg.ExpiresAt != "" {
				msg += fmt.Sprintf(" expires=%s", cfg.ExpiresAt)
			}
			findings = append(findings, CredentialFinding{
				Name:    cfg.Name,
				Level:   CheckOK,
				Message: msg,
			})
		}
		return findings
	}

	// Inline mode: warn if token is non-empty (posture issue).
	if cfg.Token == "" {
		findings = append(findings, CredentialFinding{
			Name:    cfg.Name,
			Level:   CheckFail,
			Message: fmt.Sprintf("gitlab bot %q has no token and no vault pointer", cfg.Name),
		})
		return findings
	}

	findings = append(findings, CredentialFinding{
		Name:    cfg.Name,
		Level:   CheckOK,
		Message: fmt.Sprintf("credentials ok (host=%s project=%s inline)", cfg.Host, cfg.Project),
	})
	// Posture WARN: inline token.
	findings = append(findings, CredentialFinding{
		Name:  cfg.Name,
		Level: CheckWarn,
		Message: fmt.Sprintf(
			"%s: access token stored as plaintext in %s — "+
				"migrate to a secure provider with `koryph bot vault-migrate --forge gitlab --name %s`",
			cfg.Name, GitLabBotPath(cfg.Name), cfg.Name),
	})
	return findings
}
