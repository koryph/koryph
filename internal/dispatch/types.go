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
// Implementation contract (cli.go):
//   - CLIBackend.Dispatch(ctx, Spec) (Handle, error):
//     1. The resolved runtime.Runtime's VerifyIdentity (koryph-v8u.5; claude
//     delegates to account.VerifyExpected) — FAIL CLOSED on mismatch/error.
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
