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

import (
	"hash/fnv"
	"math"
)

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

// Auth modes (koryph-i3b, design docs/designs/2026-07-api-key-auth.md §4).
// AuthModeSubscription is the default and today's only behavior: OAuth
// login billed against a Claude subscription, identity verified via
// ExpectedIdentity's email. AuthModeAPIKey and AuthModeOAuthToken are
// long-lived-credential modes: identity is verified via
// IdentityFingerprint instead of an email (§5), and the credential itself
// is resolved via Credential (§6).
const (
	AuthModeSubscription = "subscription"
	AuthModeAPIKey       = "api-key"
	AuthModeOAuthToken   = "oauth-token"
)

// Credential source kinds (§6).
const (
	CredentialSourceVault = "vault"
	CredentialSourceEnv   = "env"
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

	// Forge is the resolved forge provider name for this project ("github",
	// "gitlab"). Populated from koryph.project.json at onboard/add time.
	// An absent field (empty string) means "github" — all records written
	// before this field was introduced default to GitHub behavior.
	Forge string `json:"forge,omitempty"`

	// Koryph
	EngineVersion   string `json:"koryph_engine_version,omitempty"`
	MigrationStatus string `json:"migration_status"`

	// Account / environment. ClaudeConfigDir=="" means the profile uses the
	// default unset-CLAUDE_CONFIG_DIR personal account.
	AccountProfile   string `json:"account_profile"`
	ClaudeConfigDir  string `json:"claude_config_dir,omitempty"`
	ExpectedIdentity string `json:"expected_identity"` // login email that MUST match at dispatch
	DirenvExpected   string `json:"direnv_expected,omitempty"`

	// AuthMode selects how this account authenticates (koryph-i3b, design
	// §4): AuthModeSubscription (default), AuthModeAPIKey, or
	// AuthModeOAuthToken. Empty ("") means AuthModeSubscription — every
	// record written before this field was introduced defaults to today's
	// behavior unchanged; read EffectiveAuthMode rather than this field
	// directly. subscription mode ignores Credential/IdentityFingerprint
	// and behaves exactly as today.
	AuthMode string `json:"auth_mode,omitempty"`
	// Credential resolves the long-lived credential for AuthModeAPIKey /
	// AuthModeOAuthToken (§6); nil for subscription mode.
	Credential *Credential `json:"credential,omitempty"`
	// IdentityFingerprint is a non-secret sha256 prefix of the resolved
	// credential, recorded at enrollment and re-derived at dispatch to
	// detect a swapped key/token (§5) — the identity signal for
	// AuthModeAPIKey / AuthModeOAuthToken, which have no email to verify.
	// Empty for subscription mode.
	IdentityFingerprint string `json:"identity_fingerprint,omitempty"`

	// Model policy
	AllowedModels       []string `json:"allowed_models"`        // e.g. ["opus","sonnet","haiku"]; add "fable" to permit explicit Fable
	PlannerModel        string   `json:"planner_model"`         // default "opus"
	ImplModel           string   `json:"impl_model"`            // default "sonnet"
	RecoveryModelPolicy string   `json:"recovery_model_policy"` // "upgrade-opus" (fixed; Fable never)

	// AgentMCP controls whether dispatched agents inherit the machine's
	// ambient MCP servers. "" / "inherit" (default) preserves current
	// behavior; "strict" passes --strict-mcp-config so the agent loads NO MCP
	// servers (koryph-kwv). koryph implementer/reviewer personas use only
	// file+bash tools, so their MCP instruction+schema block (~15-18k tokens,
	// re-read every turn) is dead weight; "strict" removes it. Opt-in per
	// project — leave unset unless a project's agents genuinely call an MCP.
	AgentMCP string `json:"agent_mcp,omitempty"`

	// Billing / batch
	BatchPolicy  string `json:"batch_policy"`              // "deny" | "explicit"
	APIFallback  string `json:"api_fallback"`              // "off" | "explicit"
	APIKeyEnvVar string `json:"api_key_env_var,omitempty"` // env var NAME holding the key (never the key itself)

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

	// RuntimeAccounts holds PER-RUNTIME account profiles, keyed by
	// runtime.Runtime.Name() (koryph-v8u.5): a project that dispatches
	// through more than one agent runtime can give each its own config-dir/
	// identity/env, instead of every runtime sharing the flat
	// AccountProfile/ClaudeConfigDir/ExpectedIdentity/EnvPassthrough fields
	// above. Additive JSON — nil/absent on every record written before this
	// bead — so it is PURELY OPT-IN; AccountFor's fallback is what makes an
	// absent entry (or an absent map entirely) behave exactly as those flat
	// fields already did. This is deliberately separate from project
	// config's default_runtime/runtimes block (koryph-v8u.3's territory,
	// runtime SELECTION) — this map only carries the account/identity data
	// for a runtime once one is selected.
	RuntimeAccounts map[string]RuntimeAccount `json:"runtime_accounts,omitempty"`

	// AgentProxy configures an optional local interception proxy that this
	// project's dispatched agents route their Anthropic traffic through
	// (koryph-3l1.1, design docs/designs/2026-07-token-economy.md §3 L5, §2
	// I4/I6). Absent (nil) means direct — no ANTHROPIC_BASE_URL override at
	// any of the four spawn sites (main dispatch, review, stage, epicreview);
	// every record written before this bead has no agent_proxy block at all,
	// so this is purely opt-in. See AgentProxy's doc for the loopback-only
	// load-time validation (I4: machine-checked, not just documented).
	AgentProxy *AgentProxy `json:"agent_proxy,omitempty"`

	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// Credential is a long-lived credential reference for AuthModeAPIKey /
// AuthModeOAuthToken accounts (koryph-i3b, design §6) — never the secret
// itself, only where to find it. Resolution happens elsewhere
// (internal/account.ResolveCredential); this type is pure data.
type Credential struct {
	// Source is CredentialSourceVault (fetch via signing.FetchSecret using
	// Provider+KeyRef) or CredentialSourceEnv (read the named EnvVar).
	Source string `json:"source"`
	// Provider is the vault provider name (any signing.VaultProviders
	// value) when Source == CredentialSourceVault; ignored otherwise.
	Provider string `json:"provider,omitempty"`
	// KeyRef is the vault item reference passed to FetchSecret when
	// Source == CredentialSourceVault; ignored otherwise.
	KeyRef string `json:"key_ref,omitempty"`
	// EnvVar names the environment variable holding the credential when
	// Source == CredentialSourceEnv; ignored otherwise. MUST NOT be
	// "ANTHROPIC_API_KEY" or "CLAUDE_CODE_OAUTH_TOKEN" — those are the
	// CANONICAL names ChildEnv injects the resolved value under, so reusing
	// one as the SOURCE var would let a dispatched agent's own ambient env
	// satisfy its own credential lookup, defeating the vault/named-var
	// indirection (mirrors the batch client's refusal,
	// internal/anthro/client.go:104-105). Machine-checked at load, not just
	// documented — see validateCredential in store.go.
	EnvVar string `json:"env_var,omitempty"`
}

// AgentProxy is one project's local interception-proxy configuration
// (koryph-3l1.1). BaseURL is validated at LOAD time (Store.Get/List, and at
// Store.Add) to parse as an "http" URL with a loopback host (127.0.0.0/8,
// "localhost", or "::1") — a non-loopback base_url would route the agent's
// Anthropic traffic, and whatever ANTHROPIC_API_KEY/subscription auth rides
// with it, to a non-local endpoint with none of the interception harness's
// guarantees, so this is refused at load rather than merely documented (I4).
type AgentProxy struct {
	// BaseURL is injected as ANTHROPIC_BASE_URL for every dispatched agent
	// (main dispatch, review, stage, epicreview) — the single sanctioned
	// source (account.ChildEnvSpec.ProxyBaseURL); never env_passthrough.
	BaseURL string `json:"base_url"`
	// Health is the proxy's health-check endpoint (consumed by
	// koryph-3l1.2's doctor checks; not otherwise interpreted here).
	Health string `json:"health,omitempty"`
	// Stats is the proxy's request-counter endpoint (consumed by
	// koryph-3l1.5's positive-routing-verification doctor check, design
	// docs/designs/2026-07-token-economy.md §3 L5 fourth check): it compares
	// a forwarded-request count reported here against koryph's own ledger
	// count of dispatches routed to this proxy's arm, so a proxy that is
	// healthy and correctly pinned but silently bypassed (§2 I1's
	// "fail-open means bypass") is still caught. Defaults to "/stats" when
	// unset — most proxies (including headroom-ai) expose a GET /stats
	// counter endpoint by convention, so doctor tries it even when the
	// operator never sets this explicitly; set it only to override the path.
	Stats string `json:"stats,omitempty"`
	// Pin is an opaque identity/version pin for the proxy configuration
	// (e.g. a config hash or deployment tag) folded into the ledger's
	// per-slot ProxyID stamp (see ledger.Slot.ProxyID and AgentProxy.ID)
	// so a re-pinned proxy segments estimator calibration separately from
	// the identity it replaced.
	Pin string `json:"pin,omitempty"`

	// Holdout is the fraction of dispatches that bypass this proxy even
	// though it is configured (koryph-3l1.3, design
	// docs/designs/2026-07-token-economy.md §3 L6, §2 I5): a PERMANENT
	// standing canary — not a one-time experiment — that stays live for as
	// long as agent_proxy is configured, so a claimed compression win is
	// never confused with "the beads that quarter happened to be smaller."
	// Assignment is deterministic per bead ID (see ArmFor) so a
	// requeue/resume of the same bead never flips arms mid-flight: arm
	// flapping would corrupt both the experiment (silently blending two
	// populations under one bead's accumulated ledger row) and the prompt
	// cache (a resumed session's ANTHROPIC_BASE_URL must stay byte-identical
	// to what built its cached prefix, or --resume replays against a
	// different endpoint than the one the cache was primed against).
	//
	// A nil pointer (unset) resolves to DefaultHoldout via EffectiveHoldout
	// — the common case, so most operators never set this explicitly. An
	// explicit 0 disables the holdout (100% proxied; not recommended outside
	// a short deliberate calibration window — I5 requires the tripwire to
	// stay live). An explicit 1 disables the proxy in practice (100%
	// holdout; a dry-run/rollout-freeze state that still exercises doctor's
	// health/pin checks without ever routing real traffic). Validated at
	// load: 0 <= *Holdout <= 1 (validateAgentProxy).
	Holdout *float64 `json:"holdout,omitempty"`
}

// DefaultHoldout is the holdout fraction assumed when agent_proxy is
// configured but Holdout is unset (nil) — see AgentProxy.Holdout's doc and
// design docs/designs/2026-07-token-economy.md §3 L6. Deliberately small: a
// standing canary needs enough traffic to detect quality regressions
// promptly, not a 50/50 split that would halve the very savings the proxy
// exists to measure.
const DefaultHoldout = 0.1

// EffectiveHoldout resolves p.Holdout to DefaultHoldout when unset (nil
// receiver or nil field) — the single place both ArmFor and any future
// caller (doctor, docs) should read the configured fraction from.
func (p *AgentProxy) EffectiveHoldout() float64 {
	if p == nil || p.Holdout == nil {
		return DefaultHoldout
	}
	return *p.Holdout
}

// ArmFor deterministically assigns one bead to the "holdout" (direct) or
// "proxied" arm of the standing canary (koryph-3l1.3, design §3 L6) and
// returns the (proxyID, proxyBaseURL) pair the caller should stamp/inject
// for it: the holdout arm returns ("", "") — byte-identical to "no proxy
// configured at all," which is exactly the population the estimator's
// calibKey segmentation (internal/quota) and completeSlot's Record calls
// are meant to compare against. The proxied arm returns (p.ID(),
// p.BaseURL).
//
// The arm is a pure function of beadID alone — never the attempt number,
// session ID, or wall-clock time — so every requeue/resume of the SAME bead
// lands in the SAME arm; see Holdout's doc for why flapping would corrupt
// both the experiment and the prompt cache. A nil AgentProxy or an empty
// BaseURL always returns ("", "") — direct dispatch, no experiment running,
// matching ID()'s and ProxyBaseURL()'s existing nil-safety.
func (p *AgentProxy) ArmFor(beadID string) (proxyID, proxyBaseURL string) {
	if p == nil || p.BaseURL == "" {
		return "", ""
	}
	if stableUnitInterval(beadID) < p.EffectiveHoldout() {
		return "", "" // holdout arm: direct, same as no proxy configured
	}
	return p.ID(), p.BaseURL
}

// stableUnitInterval maps s deterministically onto [0, 1) via a 64-bit FNV-1a
// hash (fast, stable across process restarts and Go versions — no crypto
// property is needed, only determinism and a roughly uniform spread so a
// configured holdout fraction is honored across a bead population).
func stableUnitInterval(s string) float64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s)) // fnv.Write never errors
	return float64(h.Sum64()) / float64(math.MaxUint64)
}

// ID returns the stable proxy identity string that ledger.Slot.ProxyID
// stamps at dispatch and that internal/quota's calibKey/RecordForProxy/
// EstimateItemForRuntimeProxy will key estimator segmentation by once
// koryph-3l1.1's holdout bead starts calling them with it: "<base_url>" when
// Pin is unset, "<base_url>#<pin>" when set. A nil AgentProxy (direct, no
// proxy configured) or an empty BaseURL returns "" — the empty proxyID that
// keeps quota's calibKey legacy "tier:size" population intact (never
// "@"-suffixed; see internal/quota/estimate.go's calibKey doc).
func (p *AgentProxy) ID() string {
	if p == nil || p.BaseURL == "" {
		return ""
	}
	if p.Pin != "" {
		return p.BaseURL + "#" + p.Pin
	}
	return p.BaseURL
}

// ProxyBaseURL returns the project's configured agent-proxy base URL, or ""
// when AgentProxy is absent (direct dispatch) — the single accessor every
// spawn site (main dispatch, review, stage, epicreview) threads into its
// ChildEnvSpec.ProxyBaseURL.
func (r *Record) ProxyBaseURL() string {
	if r.AgentProxy == nil {
		return ""
	}
	return r.AgentProxy.BaseURL
}

// StrictMCP reports whether dispatched agents for this project should run with
// --strict-mcp-config (no ambient MCP servers) — true only when AgentMCP ==
// "strict" (koryph-kwv). The default ("" / "inherit") is false, so dispatch
// behavior is unchanged unless a project opts in.
func (r *Record) StrictMCP() bool {
	return r.AgentMCP == "strict"
}

// EffectiveAuthMode returns r.AuthMode, defaulting empty (every record
// written before this field existed, and any record that never opted into
// a non-subscription mode) to AuthModeSubscription — the single accessor
// callers should use instead of reading AuthMode directly (koryph-i3b,
// design §4).
func (r *Record) EffectiveAuthMode() string {
	if r.AuthMode == "" {
		return AuthModeSubscription
	}
	return r.AuthMode
}

// RuntimeAccount is one runtime's account-scoped identity/env configuration
// (koryph-v8u.5) — the runtime-scoped counterpart of Record's flat
// AccountProfile/ClaudeConfigDir/ExpectedIdentity/EnvPassthrough fields. See
// Record.RuntimeAccounts and AccountFor for how a missing entry falls back to
// those flat fields.
type RuntimeAccount struct {
	// ConfigDir is this runtime's config directory for the profile (e.g.
	// CLAUDE_CONFIG_DIR for claude, CODEX_HOME for a future codex adapter);
	// "" means the runtime's own default/personal account.
	ConfigDir string `json:"config_dir,omitempty"`
	// ExpectedIdentity is the login identity that MUST match at dispatch
	// (fail closed via runtime.Runtime.VerifyIdentity) — e.g. claude's
	// oauthAccount.emailAddress.
	ExpectedIdentity string `json:"expected_identity,omitempty"`
	// APIKeyEnvVar names the env var holding this runtime's API key (never
	// the key itself) — the per-runtime counterpart of Record.APIKeyEnvVar,
	// for a runtime whose billing/auth genuinely differs from claude's.
	APIKeyEnvVar string `json:"api_key_env_var,omitempty"`
	// EnvPassthrough overrides Record.EnvPassthrough for this runtime only;
	// nil means "use Record.EnvPassthrough" (see AccountFor).
	EnvPassthrough []string `json:"env_passthrough,omitempty"`

	// AuthMode, Credential, and IdentityFingerprint are the per-runtime
	// counterparts of Record's fields of the same name (koryph-i3b, design
	// §4) — for a runtime whose auth mode genuinely differs from claude's.
	// Empty AuthMode falls back the same way Record.AuthMode does (see
	// EffectiveAuthMode); AccountFor synthesizes these from the flat
	// Record fields exactly as it does the fields above.
	AuthMode            string      `json:"auth_mode,omitempty"`
	Credential          *Credential `json:"credential,omitempty"`
	IdentityFingerprint string      `json:"identity_fingerprint,omitempty"`
}

// AccountFor resolves the effective account profile for the named runtime
// (koryph-v8u.5): an explicit RuntimeAccounts[name] entry wins; otherwise —
// including for every record written before this bead, which has no
// RuntimeAccounts block at all — the flat AccountProfile/ClaudeConfigDir/
// ExpectedIdentity/EnvPassthrough fields are synthesized as a RuntimeAccount.
// This is what makes RuntimeAccounts fully additive: "claude" (today's only
// real runtime) resolves identically whether or not a project has ever
// touched runtime_accounts.
func (r *Record) AccountFor(name string) RuntimeAccount {
	if ra, ok := r.RuntimeAccounts[name]; ok {
		return ra
	}
	return RuntimeAccount{
		ConfigDir:           r.ClaudeConfigDir,
		ExpectedIdentity:    r.ExpectedIdentity,
		APIKeyEnvVar:        r.APIKeyEnvVar,
		EnvPassthrough:      r.EnvPassthrough,
		AuthMode:            r.AuthMode,
		Credential:          r.Credential,
		IdentityFingerprint: r.IdentityFingerprint,
	}
}

// Event is one append-only audit entry (audit.jsonl).
type Event struct {
	At        string `json:"at"`
	Kind      string `json:"kind"` // register|update|set-account|dispatch|validate|onboard|quota|merge|drain|resize
	ProjectID string `json:"project_id,omitempty"`
	Actor     string `json:"actor,omitempty"` // e.g. "koryph@<host>:<pid>"
	Detail    any    `json:"detail,omitempty"`
}
