// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package dispatch launches one work item as a detached, headless Claude
// Code agent in its worktree, under the project's VERIFIED account.
//
// Subscription-first billing: the default backend shells out to the claude
// CLI with the account's OAuth (ANTHROPIC_API_KEY scrubbed). The api-key
// mode injects an explicitly configured key into the SAME CLI dispatch —
// used only when the registry record has APIFallback=="explicit", the
// caller passed --allow-api-spend, and the governor is at stop.
//
// A first-class api-key/oauth-token account (koryph-i3b, design
// docs/designs/2026-07-api-key-auth.md §4-§9) is a DIFFERENT, independent
// path: Spec.CredentialEnvVar/Credential carry a pre-resolved, pre-verified
// credential (see Spec's doc) that Command injects under its canonical name,
// entirely separate from the subscription-first Billing/APIKey fallback
// above.
//
// Implementation contract (cli.go):
//   - CLIBackend.Dispatch(ctx, Spec) (Handle, error):
//     1. Spec.CredentialEnvVar == "" (subscription mode, the default): the
//     resolved runtime.Runtime's VerifyIdentity (koryph-v8u.5; claude
//     delegates to account.VerifyExpected) — FAIL CLOSED on mismatch/error.
//     Non-empty CredentialEnvVar (api-key/oauth-token mode): identity was
//     already verified fail-closed once at engine startup
//     (account.VerifyAuth); Dispatch only confirms Credential is non-empty.
//     2. Write prompt.md, seed status.json {state:queued,step:dispatched,
//     pct:0}, write INBOX.md placeholder.
//     3. Build launch.sh (inspectable artifact) exec'ing:
//     claude -p --agent <persona> --session-id <uuid>
//     --permission-mode dontAsk --model <tier> [--effort <e>]
//     [--max-budget-usd <cap>] --fallback-model sonnet
//     --add-dir <phaseDir> --output-format stream-json
//     --include-partial-messages --verbose  < prompt.md
//     cwd = worktree; env = account.Env(profile, billing, key) +
//     KORYPH_* contract vars + BEADS_DIR + KORYPH_PHASE_ID.
//     Refuse any path containing a single quote.
//     4. nohup + detach (Setsid), redirect stream.jsonl / stderr.log,
//     record PID. Return Handle.
//   - Session naming: koryph/<project>/<bead>/a<attempt> via --name.
//   - ResumeSessionID non-empty → add `--resume <id> --fork-session`
//     (native resume, Tier 1-2 recovery); prompt still carries the RESUMING
//     block as belt-and-braces.
//   - ParseResultCost(streamPath) (float64, bool) — final type:"result"
//     line carries total_cost_usd. Use `has("is_error")` semantics, never
//     `// true` (false is a valid value).
//   - Alive(pid) bool; Stop(pid) graceful TERM (never KILL first).
//
// Env contract exported to agents (read by koryph-log tooling and hooks):
//
//	KORYPH_RUN_ID, KORYPH_PHASE_ID, KORYPH_DIR, KORYPH_LOG_PATH,
//	KORYPH_STATUS_PATH, KORYPH_SUMMARY_PATH, KORYPH_SESSION_ID, BEADS_DIR.
package dispatch

import (
	"context"

	"github.com/koryph/koryph/internal/account"
)

// Spec fully describes one dispatch.
type Spec struct {
	ProjectID string
	RepoRoot  string
	RunID     string
	PhaseID   string // bead id
	PhaseDir  string // <run>/<phase>/
	Worktree  string
	Branch    string

	Persona string
	Model   string
	Effort  string

	Profile          account.Profile
	ExpectedIdentity string
	Billing          account.BillingMode
	APIKey           string // resolved key when Billing==api-key; never logged

	// Credential and CredentialEnvVar carry the resolved long-lived
	// credential for a first-class api-key/oauth-token account (koryph-i3b,
	// design docs/designs/2026-07-api-key-auth.md §4/§6/§9), verified once
	// at engine startup (account.VerifyAuth) — the caller resolves the
	// credential and hands it here rather than Dispatch resolving it itself.
	// CredentialEnvVar must be the CANONICAL name ("ANTHROPIC_API_KEY" /
	// "CLAUDE_CODE_OAUTH_TOKEN"). Non-empty CredentialEnvVar signals a
	// first-class non-subscription account: Dispatch skips the OAuth
	// VerifyIdentity re-check (there is no .claude.json to read for a
	// bare-credential account) and instead confirms Credential is
	// non-empty — identity was already verified fail-closed once at engine
	// setup. Both empty (the default) is byte-for-byte the pre-koryph-i3b
	// behavior: Dispatch calls rt.VerifyIdentity exactly as before. This is
	// independent of the legacy Billing/APIKey fields above, which remain
	// the pre-existing api-key BILLING fallback under a still-subscription-
	// verified account (design §3, unchanged).
	Credential       string // never logged
	CredentialEnvVar string

	MaxBudgetUSD    float64
	Prompt          string
	SessionID       string // fresh uuid (deterministic transcript path)
	SessionName     string
	ResumeSessionID string // non-empty → native --resume --fork-session
	BeadsDir        string
	Attempt         int

	// SSHAuthSock is the koryph-managed signing-agent socket (holds ONLY the
	// signing key). Injected as SSH_AUTH_SOCK so agent commits sign without the
	// operator's ambient socket (and its other keys) ever reaching the agent.
	// Empty when signing is not required.
	SSHAuthSock string
	// EnvPassthrough forwards extra operator env vars into the agent (the
	// registry-declared escape hatch for projects that genuinely need one).
	EnvPassthrough []string

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1, registry.Record.ProxyBaseURL()); non-empty is injected
	// as ANTHROPIC_BASE_URL via account.ChildEnvSpec.ProxyBaseURL. Empty
	// (the default) means direct dispatch — no override.
	ProxyBaseURL string

	// StrictMCP mirrors registry.Record.StrictMCP() (koryph-kwv): true adds
	// --strict-mcp-config so the agent loads no ambient MCP servers. False
	// (default) is unchanged behavior. Carried to runtime.DispatchSpec via
	// toRuntimeSpec.
	StrictMCP bool
}

// Handle describes a launched agent.
type Handle struct {
	PID              int    `json:"pid"`
	SessionID        string `json:"session_id"`
	LaunchPath       string `json:"launch_path"`
	StreamPath       string `json:"stream_path"`
	StderrPath       string `json:"stderr_path"`
	StatusPath       string `json:"status_path"`
	VerifiedIdentity string `json:"verified_identity"`
}

// Backend launches agents.
type Backend interface {
	Dispatch(ctx context.Context, s Spec) (Handle, error)
}
