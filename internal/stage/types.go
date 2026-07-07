// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package stage runs a single post-implement pipeline stage: a persona agent
// executed synchronously in the implementer's worktree, after its commits land
// and before review/merge. Unlike the read-only review pass, a stage agent
// WRITES — it may edit files and add commits (docs, tests, changelog) — so it
// runs with --permission-mode dontAsk under the same account/billing/identity
// guarantees as a normal dispatch.
//
// Contract (stage.go):
//   - Run(ctx, Opts) Result — verify identity (fail closed, before any exec),
//     build a stage prompt (bead context + changed-file list + the agent
//     boundary rules + per-stage extra instructions), then run the account-
//     scoped claude one-shot:
//     claude -p --agent <persona> --permission-mode dontAsk --model <model>
//     [--effort <e>] [--max-budget-usd <n>] --fallback-model sonnet
//     --output-format json
//     with the prompt on stdin and a Timeout (default 600s). The raw result
//     envelope is persisted to <PhaseDir>/stage-<name>.json and its
//     total_cost_usd surfaced on Result.CostUSD.
//   - Result.OK is true only on a clean exit; identity failure, spawn/timeout,
//     or a non-zero exit yields OK=false with Note explaining why.
package stage

import "github.com/koryph/koryph/internal/account"

// Opts configures one stage run.
type Opts struct {
	RepoRoot         string
	Worktree         string
	Branch           string
	Base             string // default branch (default "main")
	Stage            string // stage name (docs/test/...)
	Persona          string // agent; required (resolved by the caller)
	Model            string // model tier; required (resolved by the caller)
	Effort           string // optional effort hint
	ExtraPrompt      string // per-stage instruction text appended to the prompt
	BeadID           string
	BeadTitle        string
	Profile          account.Profile
	ExpectedIdentity string
	Billing          account.BillingMode
	APIKey           string
	SSHAuthSock      string // koryph scoped signing socket (post-implement stages may commit)
	MaxBudgetUSD     float64
	PhaseDir         string // where stage-<name>.json is written
	ClaudeBin        string // default "claude"
	TimeoutSec       int    // default 600

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1), threaded from the caller's registry.Record via
	// registry.Record.ProxyBaseURL(). Empty (the common case) means direct —
	// no ANTHROPIC_BASE_URL override. See account.ChildEnvSpec.ProxyBaseURL.
	ProxyBaseURL string
}

// Result reports what a stage run did.
type Result struct {
	Ran     bool    // the agent process was started
	OK      bool    // clean exit (identity ok, no timeout, exit 0)
	CostUSD float64 // parsed from the result envelope (0 when absent)
	Note    string  // failure reason when !OK
}
