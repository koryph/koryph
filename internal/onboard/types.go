// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package onboard inspects, registers, and validates projects for the
// central koryph. v1 implements: Inspect (read-only inventory, always
// safe), Register (registry record + adapter config scaffold; refuses to
// overwrite), and Validate (the pre-dispatch gate). Beads initialization
// and legacy-fork migration are driven interactively via the
// koryph-onboard skill on top of these primitives.
//
// Implementation contract (inspect.go, register.go, validate.go):
//   - Inspect(ctx, root) (*Inventory, error) — READ ONLY. Detect: git repo +
//     default branch (init.defaultBranch / origin/HEAD / main|master) +
//     remote; .beads presence, bd availability, beads hardening
//     (issues.jsonl gitignored? sync.remote set? hooks under core.hookspath
//     with a BEADS INTEGRATION marker?); Claude wiring (.claude/settings.json
//     contains "bd prime"? agents dir? which personas); legacy koryph/
//     fork presence + generation hints (scheduler.sh? --source bd?
//     workflows.sh?); .envrc claude-account managed block → declared profile
//     (personal-unset | personal-explicit-DEPRECATED | work + dir) — parse
//     the comment/markers only, NEVER source it; worktrees (git worktree
//     list) + dirty flags; koryph.project.json presence; plans dir.
//   - Register(ctx, store, inv, RegisterOpts) (*registry.Record, error) —
//     build the Record from inventory + explicit opts (account NEVER
//     inferred silently: RegisterOpts.AccountProfile is always REQUIRED;
//     ExpectedIdentity is REQUIRED as a login email for the default
//     subscription AuthMode, optional/free-form for AuthModeAPIKey /
//     AuthModeOAuthToken, which instead REQUIRE Credential and have their
//     identity_fingerprint resolved+recorded here; error if the
//     .envrc-declared profile disagrees unless Force). Write
//     koryph.project.json scaffold (project.Default + detected gate hints)
//     only when absent. store.Add. Never mutates beads, .envrc, or git
//     state.
//   - Validate(ctx, store, projectID, out) (*Validation, error) — checks:
//     record + adapter config load; root is a git repo on disk; account
//     identity verifies against ExpectedIdentity (fail closed); bd
//     available + `bd ready` parses (when work_source=bd); scheduler
//     dry-run BuildWave succeeds; hooks wiring present (warn-only);
//     governor snapshot for the account (warn when uncalibrated); worktree
//     root writable. All results recorded; ok = no ERROR-level failures.
//     On success the caller promotes MigrationStatus registered→migrated;
//     validated is reserved for a green canary dispatch.
package onboard

import "github.com/koryph/koryph/internal/registry"

// Inventory is the read-only project inspection result.
type Inventory struct {
	Root          string `json:"root"`
	IsGitRepo     bool   `json:"is_git_repo"`
	DefaultBranch string `json:"default_branch,omitempty"`
	Remote        string `json:"remote,omitempty"`

	HasBeads      bool   `json:"has_beads"`
	BeadsHardened bool   `json:"beads_hardened"`
	BeadsHooks    bool   `json:"beads_hooks"`
	BDAvailable   bool   `json:"bd_available"`
	BDVersion     string `json:"bd_version,omitempty"`

	ClaudeSettings bool     `json:"claude_settings"`
	BDPrimeHook    bool     `json:"bd_prime_hook"`
	Personas       []string `json:"personas,omitempty"`

	LegacyKoryph bool     `json:"legacy_koryph"`
	LegacyHints  []string `json:"legacy_hints,omitempty"`

	EnvrcProfile string `json:"envrc_profile,omitempty"` // personal-unset|personal-explicit-deprecated|work|none
	EnvrcDir     string `json:"envrc_config_dir,omitempty"`

	Worktrees      []WorktreeState `json:"worktrees,omitempty"`
	AdapterPresent bool            `json:"adapter_present"`
	PlansDir       string          `json:"plans_dir,omitempty"`
}

// WorktreeState is one linked worktree in the inventory.
type WorktreeState struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	Dirty  bool   `json:"dirty"`
}

// RegisterOpts are the explicit (never inferred) registration inputs.
type RegisterOpts struct {
	ProjectID       string // default: repo dir name slugified
	AccountProfile  string // REQUIRED: personal|work
	ClaudeConfigDir string // "" for personal; e.g. ~/.claude-work for work
	// ExpectedIdentity is REQUIRED and must be a login email for
	// AuthMode subscription (the default). For AuthModeAPIKey /
	// AuthModeOAuthToken it is optional and free-form (a display label —
	// there is no email to verify for a bare credential; see
	// docs/designs/2026-07-api-key-auth.md §4-§5).
	ExpectedIdentity string
	// AuthMode selects how this account authenticates (koryph-i3b, design
	// §4): "" (== registry.AuthModeSubscription, the default),
	// registry.AuthModeAPIKey, or registry.AuthModeOAuthToken.
	AuthMode string
	// Credential resolves the long-lived credential for AuthModeAPIKey /
	// AuthModeOAuthToken (design §6) — REQUIRED for those modes, ignored
	// for subscription. Register resolves it once, at enrollment time, to
	// compute the identity_fingerprint recorded on the Record; the
	// resolved secret value itself is never persisted.
	Credential    *registry.Credential
	AllowedModels []string // default ["haiku","sonnet","opus"]
	Force         bool     // override .envrc-disagreement refusal
}

// Check is one validation check result.
type Check struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok|warn|error
	Detail string `json:"detail,omitempty"`
}

// Validation is the full validation report.
type Validation struct {
	ProjectID string  `json:"project_id"`
	OK        bool    `json:"ok"`
	Checks    []Check `json:"checks"`
}

// ensure registry import is used in signatures documented above.
var _ = registry.StatusRegistered
