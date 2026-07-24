// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package onboard

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
)

// Register builds a registry.Record from the inventory plus EXPLICIT opts and
// adds it to the store. The account is never inferred: AccountProfile and
// ExpectedIdentity are required, and a .envrc-declared profile that disagrees
// with the request is refused unless opts.Force. It scaffolds a default
// koryph.project.json only when the adapter is absent; it never mutates
// beads, .envrc, or git state.
func Register(ctx context.Context, store *registry.Store, inv *Inventory, opts RegisterOpts) (*registry.Record, error) {
	if inv == nil {
		return nil, fmt.Errorf("onboard: register requires a non-nil inventory")
	}
	if err := validateRegisterOpts(opts); err != nil {
		return nil, err
	}
	if err := checkEnvrcAgreement(inv, opts); err != nil {
		return nil, err
	}

	fingerprint, err := resolveIdentityFingerprint(ctx, opts)
	if err != nil {
		return nil, err
	}

	projectID := opts.ProjectID
	if projectID == "" {
		projectID = slugify(filepath.Base(inv.Root))
	}

	rec := buildRecord(projectID, inv, opts, fingerprint)

	if err := store.Add(ctx, rec); err != nil {
		return nil, err
	}

	// Scaffold the per-project adapter only when absent — never overwrite.
	if !inv.AdapterPresent {
		if err := project.DefaultForRuntime(projectID, opts.RuntimeName).Save(inv.Root); err != nil {
			return rec, fmt.Errorf("onboard: registered %s but failed to scaffold %s: %w",
				projectID, project.ConfigFileName, err)
		}
	}

	return rec, nil
}

// validateRegisterOpts enforces the explicit-account contract. The
// email-shaped ExpectedIdentity requirement applies only to the default
// subscription auth mode; api-key/oauth-token accounts have no login email
// to require and instead must carry a Credential to resolve (koryph-i3b,
// design §4/§8).
func validateRegisterOpts(opts RegisterOpts) error {
	if strings.TrimSpace(opts.AccountProfile) == "" {
		return fmt.Errorf("onboard: account profile is required (personal|work)")
	}
	if (opts.RuntimeName == "" || opts.RuntimeName == "claude") && opts.AccountProfile != registry.ProfilePersonal && strings.TrimSpace(opts.ClaudeConfigDir) == "" {
		return fmt.Errorf("onboard: config dir (CLAUDE_CONFIG_DIR) is required for the %q account", opts.AccountProfile)
	}
	mode := opts.AuthMode
	if mode == "" {
		mode = registry.AuthModeSubscription
	}
	switch mode {
	case registry.AuthModeSubscription:
		if strings.TrimSpace(opts.ExpectedIdentity) == "" {
			return fmt.Errorf("onboard: expected identity is required")
		}
		if (opts.RuntimeName == "" || opts.RuntimeName == "claude") && !strings.Contains(opts.ExpectedIdentity, "@") {
			return fmt.Errorf("onboard: expected identity %q must be a login email (contains '@')", opts.ExpectedIdentity)
		}
	case registry.AuthModeAPIKey, registry.AuthModeOAuthToken:
		if opts.Credential == nil {
			return fmt.Errorf("onboard: auth mode %q requires a credential (source vault|env)", mode)
		}
	default:
		return fmt.Errorf("onboard: unrecognized auth mode %q", mode)
	}
	return nil
}

// resolveIdentityFingerprint computes the identity_fingerprint to record at
// registration for non-subscription auth modes (koryph-i3b, design §5/§8):
// it resolves opts.Credential through the SAME account.ResolveCredential
// seam dispatch-time verification uses, then fingerprints the resolved
// value — the raw credential itself is discarded immediately after and
// never persisted. Subscription mode (the default) has no credential and
// returns "".
func resolveIdentityFingerprint(ctx context.Context, opts RegisterOpts) (string, error) {
	mode := opts.AuthMode
	if mode == "" {
		mode = registry.AuthModeSubscription
	}
	if mode == registry.AuthModeSubscription {
		return "", nil
	}
	// opts.Credential is *registry.Credential, which account.Credential
	// aliases (both alias internal/authmode.Credential), so it passes
	// through to ResolveCredential with no conversion.
	_, value, err := account.ResolveCredential(ctx, account.AuthMode(mode), opts.Credential)
	if err != nil {
		return "", fmt.Errorf("onboard: register: %w", err)
	}
	return account.Fingerprint(value), nil
}

// checkEnvrcAgreement refuses a request whose account class disagrees with the
// .envrc-declared profile, unless Force overrides it.
func checkEnvrcAgreement(inv *Inventory, opts RegisterOpts) error {
	declClass, ok := envrcClass(inv.EnvrcProfile)
	if !ok {
		return nil // nothing declared (or unrecognized) — no conflict
	}
	reqClass := "work"
	if opts.AccountProfile == registry.ProfilePersonal {
		reqClass = "personal"
	}
	if declClass == reqClass || opts.Force {
		return nil
	}
	return fmt.Errorf(
		"onboard: .envrc declares the %s account (%s) but you requested the %s account (%q); "+
			"fix .envrc to match the intended account, or re-run with --force and an explicit reason "+
			"confirming this project genuinely runs under %q",
		declClass, inv.EnvrcProfile, reqClass, opts.AccountProfile, opts.AccountProfile)
}

// envrcClass maps a declared .envrc profile to "personal"/"work". ok is false
// for none/unknown (no conflict is possible).
func envrcClass(profile string) (string, bool) {
	switch profile {
	case "personal-unset", "personal-explicit-deprecated":
		return "personal", true
	case "work":
		return "work", true
	default:
		return "", false
	}
}

// buildRecord assembles the registry record with conservative policy
// defaults. identityFingerprint is the value resolveIdentityFingerprint
// computed for opts.AuthMode ("" for subscription mode).
func buildRecord(projectID string, inv *Inventory, opts RegisterOpts, identityFingerprint string) *registry.Record {
	beadsStatus := "none"
	if inv.HasBeads {
		beadsStatus = "initialized"
		if inv.BeadsHardened {
			beadsStatus = "hardened"
		}
	}
	hooksStatus := "none"
	if inv.BeadsHooks {
		hooksStatus = "wired"
	}
	beadsRoot := ""
	if inv.HasBeads {
		beadsRoot = filepath.Join(inv.Root, ".beads")
	}

	allowed := opts.AllowedModels
	if len(allowed) == 0 {
		allowed = []string{"haiku", "sonnet", "opus"}
	}
	defBranch := inv.DefaultBranch
	if defBranch == "" {
		defBranch = "main"
	}
	direnvExpected := ""
	if inv.EnvrcProfile != "" && inv.EnvrcProfile != "none" {
		direnvExpected = inv.EnvrcProfile
	}
	worktreeRoot := filepath.Join(filepath.Dir(inv.Root), filepath.Base(inv.Root)+"-worktrees")

	rec := &registry.Record{
		ProjectID:     projectID,
		Name:          projectID,
		Root:          inv.Root,
		Remote:        inv.Remote,
		DefaultBranch: defBranch,

		BeadsRoot:        beadsRoot,
		BeadsStatus:      beadsStatus,
		BeadsHooksStatus: hooksStatus,

		MigrationStatus: registry.StatusRegistered,

		AccountProfile:   opts.AccountProfile,
		ClaudeConfigDir:  opts.ClaudeConfigDir,
		ExpectedIdentity: opts.ExpectedIdentity,
		DirenvExpected:   direnvExpected,

		AuthMode:            opts.AuthMode,
		Credential:          opts.Credential,
		IdentityFingerprint: identityFingerprint,

		AllowedModels:       allowed,
		PlannerModel:        "opus",
		ImplModel:           "sonnet",
		RecoveryModelPolicy: "upgrade-opus",

		BatchPolicy:       "explicit",
		APIFallback:       "off",
		PromptCachePolicy: registry.PromptCacheOn,
		BillingGuard:      "enforce",

		WorktreeRoot:   worktreeRoot,
		QuotaProfile:   opts.AccountProfile,
		VisibilitySync: "off",
	}
	if opts.RuntimeName != "" && opts.RuntimeName != "claude" {
		rec.RuntimeAccounts = map[string]registry.RuntimeAccount{
			opts.RuntimeName: {
				ConfigDir:           opts.ClaudeConfigDir,
				ExpectedIdentity:    opts.ExpectedIdentity,
				AuthMode:            opts.AuthMode,
				Credential:          opts.Credential,
				IdentityFingerprint: identityFingerprint,
			},
		}
	}
	return rec
}

// slugify lowercases s and collapses every run of non [a-z0-9] into a single
// dash, trimming leading/trailing dashes. Empty input yields "project".
func slugify(s string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "project"
	}
	return out
}
