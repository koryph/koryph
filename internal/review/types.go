// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package review runs the post-implementation review pass: a read-only
// reviewer agent over the branch diff, returning a blocking/non-blocking
// verdict that the engine bounces back to the implementer (bounded
// iterations). A transient reviewer failure (rate/usage limit, timeout, a
// one-off unparseable reply) is RETRIED with EXPONENTIAL backoff (Opts.Attempts,
// default 4) so a rate limit is given progressively more time to clear; only
// when every attempt fails is Degraded=true returned, carrying a
// human-readable Reason. Review never PANICS the loop — but the engine, not
// this package, sets merge policy: with review enabled the loop fails CLOSED,
// so a degraded verdict blocks the merge rather than silently passing it
// (koryph-b2h). The Reason exists so a degradation is never a black box.
//
// Implementation contract (review.go):
//   - Review(ctx, Opts) Verdict — runs, in the worktree, the account-scoped
//     claude CLI one-shot, retried up to Opts.Attempts times:
//     claude -p --agent <persona> --permission-mode plan
//     --model <model> [--effort <effort>] --output-format json
//     with a prompt containing `git diff --stat <base>...<branch>` (tail 40
//     lines) + the changed-file list, asking for STRICT JSON
//     {"blocking": bool, "findings":[{"severity","file","summary"}]}.
//     Env from account.Env (subscription). Timeout Opts.TimeoutSec (default
//     600s), escalated toward Opts.MaxTimeoutSec on a wall-clock timeout and
//     hard-capped at MaxTimeoutSec (20 min). Persist the raw verdict to
//     Opts.OutPath (review.json).
package review

import "github.com/koryph/koryph/internal/account"

// Finding is one review finding.
type Finding struct {
	Severity string `json:"severity"` // blocking|major|minor
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"` // 1-based line in File; 0 = whole-file/general
	Summary  string `json:"summary"`
}

// Verdict is the review outcome.
type Verdict struct {
	Blocking bool      `json:"blocking"`
	Findings []Finding `json:"findings,omitempty"`
	Degraded bool      `json:"degraded,omitempty"` // review could not be obtained
	Reason   string    `json:"reason,omitempty"`   // why it degraded (never a black box)
	Attempts int       `json:"attempts,omitempty"` // reviewer spawns made
	// TimedOut marks a degraded verdict whose attempt was killed for exceeding
	// its wall-clock TimeoutSec (as opposed to a rate limit or bad reply). The
	// retry loop reads it to escalate the timeout before the next attempt. Not
	// serialized — it is an in-process signal, not part of the persisted verdict.
	TimedOut bool   `json:"-"`
	Raw      string `json:"-"`
	// Envelope is the raw Claude CLI JSON envelope (including usage/cost fields)
	// from a successful reviewer spawn. It is persisted beside the parsed verdict
	// as review-envelope.json so cost/token data is available for audit and
	// future metrics pickup (same pattern as stage-*.json, koryph-qbc).
	Envelope string `json:"-"`
}

// Opts configures one review.
type Opts struct {
	RepoRoot  string
	Worktree  string
	Branch    string
	Base      string // default branch
	Persona   string // default koryph-security-reviewer
	Model     string // default opus
	Effort    string // reasoning-effort hint; empty omits --effort (runtime default)
	Profile   account.Profile
	OutPath   string // review.json destination
	ClaudeBin string // default "claude"
	// TimeoutSec is the STARTING per-attempt wall-clock timeout in seconds
	// (<=0 → KORYPH_REVIEW_TIMEOUT_SEC env, else defaultTimeoutSec, 600). On a
	// wall-clock timeout the retry loop escalates it toward MaxTimeoutSec; it is
	// always clamped to MaxTimeoutSec (the 20-minute hard cap).
	TimeoutSec int
	// MaxTimeoutSec caps how far a timeout may escalate (<=0 → MaxTimeoutSec,
	// the 1200s / 20-minute hard ceiling). Values above the hard ceiling are
	// clamped down: no single review may exceed 20 minutes.
	MaxTimeoutSec int
	Attempts      int // reviewer spawn attempts before degrading (default 4)

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1), threaded from the caller's registry.Record via
	// registry.Record.ProxyBaseURL(). Empty (the common case) means direct —
	// no ANTHROPIC_BASE_URL override. See account.ChildEnvSpec.ProxyBaseURL.
	ProxyBaseURL string
}
