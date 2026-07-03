// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package registry is the central multi-project registry under
// ~/.koryph (git-backed: every mutation is a commit → explicit, logged,
// reversible). One JSON record per project in registry.d/<project-id>.json.
//
// Implementation contract (store.go):
//   - Store.Init: create KoryphHome, git init if needed, seed dirs.
//   - Store.Add / Get / List / Save: CRUD over records; Save validates,
//     writes atomically, then git add+commit with a conventional message.
//   - Store.SetAccount: the ONLY path that changes AccountProfile /
//     ClaudeConfigDir. Requires a non-empty reason; sets MigrationStatus
//     back to "registered" (forcing re-validation before dispatch);
//     appends an audit event; commits.
//   - Store.Audit: append an Event to audit.jsonl (never rewrite).
//   - All timestamps RFC3339 UTC.
package registry

// Account profiles. PROFILE names are user-facing; resolution to a
// CLAUDE_CONFIG_DIR happens in package account.
const (
	ProfilePersonal = "personal"
	ProfileWork     = "work"
)

// Migration statuses, in lifecycle order.
const (
	StatusRegistered  = "registered"  // known; not yet validated
	StatusInventoried = "inventoried" // mode-5 inventory only
	StatusMigrated    = "migrated"    // onboarding complete, not validated
	StatusValidated   = "validated"   // canary dispatch green; dispatchable
)

// Record is one managed project.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	ProjectID     string `json:"project_id"`
	Name          string `json:"name"`
	Root          string `json:"root"`
	Remote        string `json:"remote,omitempty"`
	DefaultBranch string `json:"default_branch"`

	// Beads
	BeadsRoot        string `json:"beads_root,omitempty"` // usually <root>/.beads
	BeadsStatus      string `json:"beads_status"`         // none|initialized|hardened
	BeadsHooksStatus string `json:"beads_hooks_status"`   // none|wired
	DoltMode         string `json:"dolt_mode,omitempty"`
	DoltRemoteRef    string `json:"dolt_remote_ref,omitempty"`

	// Koryph
	EngineVersion   string `json:"koryph_engine_version,omitempty"`
	MigrationStatus string `json:"migration_status"`

	// Account / environment. ClaudeConfigDir=="" means the profile uses the
	// default unset-CLAUDE_CONFIG_DIR personal account.
	AccountProfile   string `json:"account_profile"`
	ClaudeConfigDir  string `json:"claude_config_dir,omitempty"`
	ExpectedIdentity string `json:"expected_identity"` // login email that MUST match at dispatch
	DirenvExpected   string `json:"direnv_expected,omitempty"`

	// Model policy
	AllowedModels       []string `json:"allowed_models"`        // e.g. ["opus","sonnet","haiku"]; add "fable" to permit explicit Fable
	PlannerModel        string   `json:"planner_model"`         // default "opus"
	ImplModel           string   `json:"impl_model"`            // default "sonnet"
	RecoveryModelPolicy string   `json:"recovery_model_policy"` // "upgrade-opus" (fixed; Fable never)

	// Billing / batch
	BatchPolicy       string `json:"batch_policy"`              // "deny" | "explicit"
	APIFallback       string `json:"api_fallback"`              // "off" | "explicit"
	APIKeyEnvVar      string `json:"api_key_env_var,omitempty"` // env var NAME holding the key (never the key itself)
	PromptCachePolicy string `json:"prompt_cache_policy"`       // "on" | "off"

	// BillingGuard controls the quota governor's THROTTLING constraints
	// (preflight, drain/stop dispatch blocking, slot scaling) for this
	// project: "enforce" (default; "" means enforce) or "advisory"
	// (measure + log + warn, never block). Spend-AUTHORIZATION gates
	// (explicit API key, batch confirmation) are unaffected. The governor
	// is automatically advisory while the account is uncalibrated so a
	// baseline can always be established.
	BillingGuard string `json:"billing_guard,omitempty"`

	// Worktrees / sessions
	WorktreeRoot   string   `json:"worktree_root,omitempty"` // default <parent>/<repo>-worktrees
	ActiveSessions []string `json:"active_sessions,omitempty"`

	QuotaProfile   string `json:"quota_profile,omitempty"` // defaults to AccountProfile
	VisibilitySync string `json:"visibility_sync"`         // "off" (GitHub/Linear later phase)

	// EnvPassthrough names extra operator environment variables to forward into
	// dispatched agents beyond the credential-free allowlist (account.ChildEnv).
	// The escape hatch for projects that genuinely need a specific var; empty by
	// default so no secret leaks without an explicit opt-in.
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Event is one append-only audit entry (audit.jsonl).
type Event struct {
	At        string `json:"at"`
	Kind      string `json:"kind"` // register|update|set-account|dispatch|validate|onboard|quota|merge
	ProjectID string `json:"project_id,omitempty"`
	Actor     string `json:"actor,omitempty"` // e.g. "koryph@<host>:<pid>"
	Detail    any    `json:"detail,omitempty"`
}
