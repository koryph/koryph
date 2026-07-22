// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

// DispatchSpec fully describes one runtime-neutral dispatch request. It is a
// deliberate field-for-field mirror of internal/dispatch.Spec, minus the
// fields that package derives/hardcodes rather than reads (see the package
// doc's mapping table) — the intent is that koryph-v8u.2's Claude adapter
// extraction can convert dispatch.Spec <-> DispatchSpec with a flat struct
// literal, and no logic changes.
//
// Fields gated by a Capabilities flag are honored by Command only when the
// target Runtime reports that capability; Command must return an error
// (never a silent no-op) when a gated field is set on a Runtime that does
// not support it — see Runtime.Command's doc comment.
type DispatchSpec struct {
	ProjectID string
	RepoRoot  string
	RunID     string
	PhaseID   string // bead id
	PhaseDir  string // <run>/<phase>/
	Worktree  string
	Branch    string

	// Persona is gated by Capabilities.Personas.
	Persona string
	// Model is gated by Capabilities.ModelSelect.
	Model string
	// Effort is gated by Capabilities.EffortFlag.
	Effort string

	Profile          Profile
	ExpectedIdentity string
	Billing          BillingMode
	APIKey           string // resolved key when Billing==BillingAPIKey; never logged

	// Credential and CredentialEnvVar carry the resolved long-lived
	// credential for a first-class api-key/oauth-token account (koryph-i3b,
	// design docs/designs/2026-07-api-key-auth.md §4/§6/§9) — the
	// (envVar, value) pair account.ResolveCredential returns, verified once
	// at engine startup (account.VerifyAuth). CredentialEnvVar must be the
	// CANONICAL name: "ANTHROPIC_API_KEY" for api-key mode,
	// "CLAUDE_CODE_OAUTH_TOKEN" for oauth-token mode. Both empty (the
	// default, and the only shape subscription-mode dispatch uses) omits
	// this injection entirely and leaves the legacy Billing/APIKey
	// break-glass fallback above as the sole api-key-billing path — the two
	// are independent (mirrors account.ChildEnvSpec.Credential/
	// CredentialEnvVar, which Command passes these straight through to).
	Credential       string // never logged
	CredentialEnvVar string

	// MaxBudgetUSD is gated by Capabilities.BudgetFlag.
	MaxBudgetUSD float64
	Prompt       string
	SessionID    string // fresh uuid (deterministic transcript path)
	SessionName  string
	// ResumeSessionID is gated by Capabilities.Resume.
	ResumeSessionID string
	BeadsDir        string
	Attempt         int

	// SSHAuthSock is the koryph-managed signing-agent socket (holds ONLY the
	// signing key), injected so agent commits sign without the operator's
	// ambient socket (and its other keys) ever reaching the agent. Empty
	// when signing is not required.
	SSHAuthSock string
	// EnvPassthrough forwards extra operator env vars into the agent (the
	// registry-declared escape hatch for projects that genuinely need one).
	EnvPassthrough []string

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1, registry.Record.ProxyBaseURL()); non-empty is injected
	// as ANTHROPIC_BASE_URL via account.ChildEnvSpec.ProxyBaseURL. Empty
	// (the default) means direct dispatch — no override.
	ProxyBaseURL string

	// StrictMCP, when true, adds --strict-mcp-config to the agent invocation
	// so the dispatched agent loads NO ambient MCP servers (koryph-kwv,
	// registry.Record.StrictMCP()). False (the default) leaves MCP loading to
	// the CLI's normal resolution — argv is byte-identical to pre-koryph-kwv.
	StrictMCP bool
}
