// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import (
	"context"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// JSONSpec is the runtime-neutral request for a ONE-SHOT, structured-JSON
// agent spawn — the non-streaming sibling of DispatchSpec (koryph-fiv finding
// #1). Dispatch drives a long-lived, stream-json implementer through
// Runtime.Command; the reviewer (internal/review), post-implement stages
// (internal/stage), and epic review (internal/epicreview) instead run a
// single blocking agent that emits one `--output-format json` result envelope
// on stdout, which the caller parses. Before this type those three sites each
// hand-built claude's argv inline, so a second runtime would have left four of
// five spawn sites claude-only; JSONSpec + Runtime.CommandJSON route them all
// through the resolved Runtime instead.
//
// The env-building fields (Profile/Billing/APIKey/SSHAuthSock/ProxyBaseURL/
// EnvPassthrough/Credential*) carry exactly the same allowlist-plus-explicit-
// injection contract Runtime.Command documents — the adapter is responsible
// for turning them into the COMPLETE child environment, never a fragment on
// top of the ambient shell.
type JSONSpec struct {
	// RepoRoot is the primary repository root. Adapters that need explicit
	// access to a linked worktree's shared Git metadata may use RepoRoot/.git
	// as a narrowly-scoped writable root; it is never the working directory.
	RepoRoot string
	// Persona is the named subagent/persona to run (claude's --agent).
	Persona string
	// Model is the concrete model id (claude's --model).
	Model string
	// Effort is the reasoning-effort hint; "" omits the flag (runtime default).
	Effort string
	// MaxBudgetUSD, when > 0, caps the invocation's spend (claude's
	// --max-budget-usd); 0 omits the flag.
	MaxBudgetUSD float64
	// PermissionMode selects the agent's permission posture — "plan" for the
	// read-only reviewer/epic-review agents, "dontAsk" for a stage agent that
	// mutates the worktree. "" defaults to "plan" (the safe, read-only posture).
	PermissionMode string
	// Fallback requests the runtime's fallback-model flag (claude's
	// --fallback-model). Only the stage agent sets it today; the reviewers omit
	// it, preserving their pre-seam argv exactly.
	Fallback bool
	// SpawnKind tags the spawn for env/observability accounting (the value
	// account.ChildEnvSpec.SpawnKind carried inline before the seam): "review",
	// "stage", or "epicreview".
	SpawnKind string

	// Profile / Billing / APIKey / SSHAuthSock / ProxyBaseURL / EnvPassthrough /
	// Credential / CredentialEnvVar mirror DispatchSpec's env-selection fields;
	// see Runtime.Command's env contract.
	Profile          Profile
	Billing          BillingMode
	APIKey           string
	SSHAuthSock      string
	ProxyBaseURL     string
	EnvPassthrough   []string
	Credential       string
	CredentialEnvVar string
}

// JSONExec is the per-invocation execution context for SpawnJSON — the
// worktree to run in, the prompt piped to stdin, and the wall-clock timeout.
// It is separate from JSONSpec (which is purely the runtime-neutral argv/env
// request) so the same spec shape can, in principle, be rendered without
// executing (tests, launch.sh inspection) the way Runtime.Command is.
type JSONExec struct {
	// Dir is the working directory (the worktree root).
	Dir string
	// Stdin is the prompt piped to the agent.
	Stdin string
	// Timeout bounds the spawn; <= 0 means no timeout.
	Timeout time.Duration
}

// SpawnJSON resolves spec into this runtime's concrete argv+env via
// Runtime.CommandJSON and execs it, returning the raw execx.Result so the
// caller keeps full control over envelope parsing, degraded-verdict handling,
// and stderr persistence (each JSON spawn site does this differently). A
// non-zero exit is NOT an error — callers inspect Result.ExitCode/TimedOut, as
// with execx.Run directly. An error is returned only for a CommandJSON
// rejection (an unsupported capability) or a spawn/timeout failure.
func SpawnJSON(ctx context.Context, rt Runtime, spec JSONSpec, ex JSONExec) (execx.Result, error) {
	if preparer, ok := rt.(PromptPreparer); ok {
		prepared, err := preparer.PreparePrompt(ex.Dir, spec.Persona, ex.Stdin)
		if err != nil {
			return execx.Result{}, err
		}
		ex.Stdin = prepared
	}
	argv, env, err := rt.CommandJSON(spec)
	if err != nil {
		return execx.Result{}, err
	}
	return execx.Run(ctx, execx.Cmd{
		Dir:     ex.Dir,
		Env:     env,
		Name:    argv[0],
		Args:    argv[1:],
		Stdin:   ex.Stdin,
		Timeout: ex.Timeout,
	})
}
