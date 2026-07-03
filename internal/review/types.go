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
//     --model <model> --output-format json
//     with a prompt containing `git diff --stat <base>...<branch>` (tail 40
//     lines) + the changed-file list, asking for STRICT JSON
//     {"blocking": bool, "findings":[{"severity","file","summary"}]}.
//     Env from account.Env (subscription). Timeout Opts.Timeout (default
//     240s). Persist the raw verdict to Opts.OutPath (review.json).
package review

import "github.com/koryph/koryph/internal/account"

// Finding is one review finding.
type Finding struct {
	Severity string `json:"severity"` // blocking|major|minor
	File     string `json:"file,omitempty"`
	Summary  string `json:"summary"`
}

// Verdict is the review outcome.
type Verdict struct {
	Blocking bool      `json:"blocking"`
	Findings []Finding `json:"findings,omitempty"`
	Degraded bool      `json:"degraded,omitempty"` // review could not be obtained
	Reason   string    `json:"reason,omitempty"`   // why it degraded (never a black box)
	Attempts int       `json:"attempts,omitempty"` // reviewer spawns made
	Raw      string    `json:"-"`
}

// Opts configures one review.
type Opts struct {
	RepoRoot   string
	Worktree   string
	Branch     string
	Base       string // default branch
	Persona    string // default koryph-security-reviewer
	Model      string // default opus
	Profile    account.Profile
	OutPath    string // review.json destination
	ClaudeBin  string // default "claude"
	TimeoutSec int    // default 240
	Attempts   int    // reviewer spawn attempts before degrading (default 3)
}
