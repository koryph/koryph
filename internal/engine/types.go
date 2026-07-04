// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package engine is the wave loop: scan → batch → preflight → dispatch →
// poll → review → merge → record. One Run() call executes one run over one
// project (one or more waves; --once = exactly one wave). The outer cadence
// (loop mode) is the CALLER re-invoking Run until Outcome.Drained.
//
// Hard rules:
//   - engine MUST NOT import internal/anthro (subscription-first; enforced
//     by a guardrail test in anthro). Per-token spend happens only via the
//     dispatch api-key mode under Options.AllowAPISpend.
//   - Account verification failures block the slot and never fall through.
//   - Quota: loop dispatch consults the governor (warn 80 / drain 90 /
//     stop 95); manual single dispatch is exempt (Options.Manual).
//   - Merge policy: epic label merge:auto|manual|pr > project config
//     MergePolicy; auto-merge only merges review-clean work.
//   - Every slot writes a manifest v2 checkpoint at dispatch and at every
//     poll transition.
//
// Implementation contract (run.go, poll.go, recover.go — replaces
// run_stub.go):
//   - Load registry record (MigrationStatus must be validated OR
//     Options.AllowUnvalidated for canary runs) + project config + bd
//     adapter + ledger store + run lock.
//   - Resume path: classify the latest non-terminal run and re-dispatch
//     per ledger.Classify decisions (native session resume via
//     Spec.ResumeSessionID when the manifest carries a session id and the
//     worktree still exists).
//   - Wave: quota snapshot (loop: preflight + scale slots), sched.BuildWave,
//     modelroute.Resolve per item (+PersonaMeta effort), worktree.Ensure +
//     Bootstrap (fresh trees only), promptc.Compile, dispatch via Backend,
//     ledger slot + manifest writes, stagger between launches.
//   - Poll: every PollSec, refresh heartbeat mtime + commit count + cost
//     (dispatch.ParseResultCost); stuck when BOTH heartbeat and last-commit
//     age exceed StuckSec; dead+SUMMARY ready-for-merge (or dead+commits) →
//     review (when Options.Review) → merge per policy (ASK is not possible
//     headless: policy manual → leave merge-pending); dead+no-commits →
//     requeue up to ledger.MaxAttempts with backoff.
//   - Drained: source bd && ready==0 && no active slots → Outcome.Drained,
//     exit code 4 contract for the CLI.
//   - Quota drain/stop mid-run: drain → no new dispatch, finish active;
//     stop → no new dispatch, wait for active agents to finish their
//     current process (never SIGKILL), mark run paused-quota, checkpoint.
package engine

import (
	"io"

	"github.com/koryph/koryph/internal/version"
)

// Exit codes (CLI contract, wire-compatible with the bash engine).
const (
	ExitOK      = 0
	ExitFatal   = 1
	ExitUsage   = 2
	ExitDrained = 4
)

// Options configures one engine run.
type Options struct {
	ProjectID        string
	Max              int     // wave width cap (project config may lower it)
	Once             bool    // exactly one wave
	DryRun           bool    // plan + print, no dispatch
	Resume           bool    // classify + re-dispatch the latest run first
	Parent           string  // epic scope for the bd frontier
	Only             string  // dispatch only this specific ready bead id ("" = whole frontier)
	BudgetUSD        float64 // per-run cost ceiling in USD (0 = unlimited)
	DefaultModel     string  // model for label-less beads
	AutoMerge        bool    // allow auto-merge for merge:auto/config-auto items
	Direct           bool    // owner override: skip PRs, merge straight to the default branch
	Review           bool    // post-implementation review pass before merge
	Manual           bool    // single manual dispatch semantics (quota-exempt)
	AllowAPISpend    bool    // permit api-key billing fallback at governor stop
	AllowUnvalidated bool    // permit runs on non-validated projects (canary)
	NoPreflight      bool
	// NoBillingGuard disables the governor's throttling constraints for
	// this run (preflight, drain/stop blocking, slot scaling): usage is
	// still measured, logged, and calibrated, but never blocks dispatch.
	// Billing stays on subscription (the api-key stop-fallback never fires
	// in advisory mode). Defaults to enforced; the governor is also
	// automatically advisory while the account is uncalibrated (baseline
	// establishment is never blocked).
	NoBillingGuard bool
	PollSec        int // default 10; project config poll_seconds and KORYPH_POLL_SEC env can also set it (koryph-2im.2)
	StuckSec       int // default 900
	// DispatchMode selects the dispatch loop: "rolling" (default,
	// koryph-2im.8) or "wave"
	// (koryph-2im.3). Precedence: this flag, when non-empty, wins over the
	// project config's dispatch_mode; empty defers to config, then "wave".
	// --once runs today's wave semantics in both modes. Any other value is a
	// usage error (see Run's validation).
	DispatchMode string
	Out          io.Writer // human-readable progress; nil = silent
}

// Outcome summarizes a run.
type Outcome struct {
	Code       int    `json:"code"`
	RunID      string `json:"run_id"`
	Dispatched int    `json:"dispatched"`
	Merged     int    `json:"merged"`
	PROpened   int    `json:"pr_opened,omitempty"`
	Failed     int    `json:"failed"`
	Blocked    int    `json:"blocked"`
	Drained    bool   `json:"drained"`
	Reason     string `json:"reason,omitempty"`
}

// EngineVersion is stamped into ledgers, manifests, and prompts. The
// authoritative value lives in internal/version (semver; releases tag it).
const EngineVersion = version.Engine
