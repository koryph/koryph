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
//     configured runtime one-shot, retried up to Opts.Attempts times,
//     with a prompt containing `git diff --stat <base>...<branch>` (tail 40
//     lines) + the changed-file list, asking for STRICT JSON
//     {"blocking": bool, "findings":[{"severity","file","summary"}]}.
//     Env from account.Env (subscription). Timeout Opts.TimeoutSec — a single
//     unified value (default DefaultTimeoutSec, 1200s), the bead > project >
//     system > built-in winner resolved by the caller (koryph-w82i); no
//     escalation, no hard cap. Successful verdicts are persisted as immutable,
//     content-addressed review artifacts carrying cumulative stable-ID blocker
//     history.
package review

import (
	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/runtime"
)

// Finding is one review finding.
type Finding struct {
	Severity string `json:"severity"` // blocking|major|minor
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"` // 1-based line in File; 0 = whole-file/general
	Summary  string `json:"summary"`
	// TrackHistory marks substantive model/criterion blockers that belong in
	// cumulative repair history. Deterministic schema-error findings block the
	// current verdict but are not defects a later reviewer should re-evaluate.
	TrackHistory bool `json:"-"`
}

// CriterionAssessment is the reviewer's evidence-backed disposition for one
// acceptance criterion enumerated in the prompt as AC1, AC2, ...
type CriterionAssessment struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // satisfied|unsatisfied|not-applicable
	Evidence string `json:"evidence"`
}

// PriorFindingAssessment proves that a repair review rechecked one earlier
// blocking finding instead of progressively forgetting it.
type PriorFindingAssessment struct {
	ID       string `json:"id"`
	Status   string `json:"status"` // resolved|unresolved
	Evidence string `json:"evidence"`
}

// Verdict is the review outcome.
type Verdict struct {
	Blocking               bool                     `json:"blocking"`
	Criteria               []CriterionAssessment    `json:"criteria,omitempty"`
	PriorFindings          []PriorFindingAssessment `json:"prior_findings,omitempty"`
	Findings               []Finding                `json:"findings,omitempty"`
	SecurityReviewRequired bool                     `json:"security_review_required,omitempty"`
	SecurityEvidence       string                   `json:"security_evidence,omitempty"`
	Degraded               bool                     `json:"degraded,omitempty"` // review could not be obtained
	Reason                 string                   `json:"reason,omitempty"`   // why it degraded (never a black box)
	Attempts               int                      `json:"attempts,omitempty"` // reviewer spawns made
	// TimedOut marks a degraded verdict whose attempt was killed for exceeding
	// its wall-clock TimeoutSec (as opposed to a rate limit or bad reply). Not
	// serialized — it is an in-process diagnostic, not part of the persisted
	// verdict.
	TimedOut bool   `json:"-"`
	Raw      string `json:"-"`
	// Envelope is the raw runtime JSON envelope (including usage/cost fields)
	// from a successful reviewer spawn. It is persisted beside the parsed verdict
	// with the same versioned stem so cost/token data is available for audit and
	// future metrics pickup (same pattern as stage-*.json, koryph-qbc).
	Envelope string `json:"-"`
	// ArtifactPath and ArtifactDigest identify the immutable, content-addressed
	// review artifact persisted for this verdict. HistoryDigest authenticates
	// its cumulative blocker set. BlockingHistory is the same stable-ID set in
	// value form so the engine can audit or expose it without reparsing a file.
	ArtifactPath    string              `json:"-"`
	ArtifactDigest  string              `json:"-"`
	HistoryDigest   string              `json:"-"`
	BlockingHistory []HistoricalFinding `json:"-"`
}

// Contract is the runtime-neutral Bead and completion context a reviewer must
// evaluate in addition to local code quality.
type Contract struct {
	ID                 string
	Title              string
	Description        string
	AcceptanceCriteria string
	// CompletionEvidence is the exact evidence matrix authenticated from the
	// worker's terminal result manifest. General review validates and renders
	// it beside the same canonical criterion IDs. Security review deliberately
	// ignores it: that lane is a targeted security audit, not a second
	// acceptance review.
	CompletionEvidence phasecontrol.Evidence
	Labels             []string
	Runtime            string
	CompletionState    string
}

// Opts configures one review.
type Opts struct {
	RepoRoot string
	Worktree string
	Branch   string
	Base     string // default branch
	// CandidateSHA and BaseSHA pin the review to the exact authoritative gate
	// evidence. When set, Review diffs these immutable commits and verifies the
	// branch still names CandidateSHA before accepting a verdict.
	CandidateSHA string
	BaseSHA      string
	// PriorVerdictPath supplies the preceding blocking review on a repair pass.
	// Deprecated compatibility input: new engine code supplies every immutable
	// artifact through PriorVerdictPaths.
	PriorVerdictPath string
	// PriorVerdictPaths supplies immutable review artifacts from prior repair
	// passes. Each artifact carries the cumulative history, so the newest path
	// is sufficient after migration; accepting a slice makes bootstrap from
	// legacy verdicts and explicit chain audits deterministic.
	PriorVerdictPaths []string
	// Security selects the dedicated fail-closed security review prompt.
	Security bool
	Persona  string // default koryph-reviewer; security default is koryph-security-reviewer
	Model    string // default sonnet; security default is opus
	Effort   string // reasoning-effort hint; empty omits --effort (runtime default)
	Profile  account.Profile
	// ArtifactDir is the recommended persistence API. Review writes an
	// immutable content-addressed <kind>-<candidate>-<digest>.json beneath it
	// and returns that exact path in Verdict.ArtifactPath.
	ArtifactDir string
	// OutPath is a compatibility API for callers that choose the complete
	// versioned artifact path themselves. It is create-once: a byte-identical
	// replay is accepted, but different content is never allowed to overwrite
	// it. Do not reuse one OutPath across repair passes.
	OutPath   string
	ClaudeBin string          // default "claude"
	Runtime   runtime.Runtime // optional; defaults to Claude for compatibility
	Contract  Contract        // optional for standalone PR review; required by the engine
	// TimeoutSec is the reviewer's single wall-clock timeout in seconds
	// (koryph-w82i). The caller supplies the bead > project > system winner
	// (timeoutcfg.Resolve); <=0 falls back to DefaultTimeoutSec (1200). The
	// break-glass KORYPH_REVIEW_TIMEOUT_SEC env still overrides it. There is no
	// escalation and no hard ceiling — a large override is honored verbatim.
	TimeoutSec int
	Attempts   int // reviewer spawn attempts before degrading (default 4)

	// ProxyBaseURL is the project's registry-configured agent_proxy.base_url
	// (koryph-3l1.1), threaded from the caller's registry.Record via
	// registry.Record.ProxyBaseURL(). Empty (the common case) means direct —
	// no ANTHROPIC_BASE_URL override. See account.ChildEnvSpec.ProxyBaseURL.
	ProxyBaseURL string
}
