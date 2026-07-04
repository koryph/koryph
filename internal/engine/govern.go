// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os"
	"time"

	"github.com/koryph/koryph/internal/govern"
)

// The global concurrency governor caps concurrently running agents across ALL
// projects (koryph-1xk) so independent `koryph run` processes cannot
// collectively breach the Claude API rate limits. Every helper fails OPEN — a
// governor error never blocks dispatch — because a stuck governor must not wedge
// the engine; the cap is a safety rail, not a correctness dependency.

// providerAnthropic is the pool every lease this engine constructs is
// admitted against today (koryph-v8u.11, L5c: independent per-provider
// governor pools — see internal/govern's package doc). Hardcoded rather than
// resolved from the bead/project until the koryph-v8u.2 runtime adapters
// land and can supply the actual provider behind the dispatched agent; until
// then every agent this engine launches IS a claude/Anthropic session, so
// this constant is exactly today's (only) behavior, made explicit at the one
// place leases are constructed.
const providerAnthropic = govern.DefaultPool

// refreshDemand records this project's demand for slots (it has ready work).
func (r *runner) refreshDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.RefreshDemand(providerAnthropic, r.opts.ProjectID, os.Getpid())
}

// dropDemand withdraws this project from the fair-share denominator.
func (r *runner) dropDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.DropDemand(providerAnthropic, r.opts.ProjectID)
}

// warnIfOverFairShare logs, once per run, when this project's configured wave
// width exceeds its current global fair share — the deliberate per-project
// override the operator asked for, surfaced as a fairness/rate-limit risk.
func (r *runner) warnIfOverFairShare() {
	if r.gov == nil || r.govWarned {
		return
	}
	fs, err := r.gov.FairShareFor(providerAnthropic, r.opts.ProjectID)
	if err != nil || r.width <= fs {
		return
	}
	r.govWarned = true
	r.progress("warning: project width %d exceeds its global fair share %d (cap %d across active projects) — extra slots wait for others to idle and may pressure the Claude API rate limit",
		r.width, fs, r.gov.Cap(providerAnthropic))
}

// acquireGlobalSlot reserves a global concurrency slot for beadID (keyed to the
// project+bead under this engine's pid; the agent pid is attached later by
// bindGlobalSlot). Returns true when granted or when governance is unavailable.
func (r *runner) acquireGlobalSlot(beadID string) bool {
	if r.gov == nil {
		return true
	}
	ok, err := r.gov.Acquire(govern.Lease{
		Project:   r.opts.ProjectID,
		Bead:      beadID,
		EnginePID: os.Getpid(),
		Provider:  providerAnthropic,
	})
	if err != nil {
		r.progress("bead %s: global governor error (allowing dispatch): %v", beadID, err)
		return true
	}
	return ok
}

// holdGlobalSlot attaches the launched agent pid to the bead's lease (keyed to a
// process that outlives the engine) so the running agent is always counted —
// including a requeue/resume whose reservation was pruned. Cap admission already
// happened at acquireGlobalSlot; this is an unconditional 1:1 update.
func (r *runner) holdGlobalSlot(beadID string, agentPID int, model string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.Hold(govern.Lease{
		Project:   r.opts.ProjectID,
		Bead:      beadID,
		PID:       agentPID,
		EnginePID: os.Getpid(),
		Model:     model,
		Provider:  providerAnthropic,
	})
}

// releaseGlobalSlot frees the bead's global slot at a terminal transition.
// Idempotent — safe to call on any path that ends a slot's active life.
func (r *runner) releaseGlobalSlot(beadID string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.Release(providerAnthropic, r.opts.ProjectID, beadID)
}

// reportRateLimit informs the machine-wide governor of a rate-limit/overload
// signal from a dead agent's stream (koryph-2im.4): every engine on the host
// shares the same AIMD backoff state for this pool, so a rate limit observed
// by any one of them halves the cap for all of them — but only within THIS
// pool (koryph-v8u.11): an Anthropic rate limit never throttles another
// provider's pool. The bead id makes burst detection count distinct slots
// and lets half-open probe reports match exactly (koryph-2im.11). Fails open
// like every other governor helper — a stuck governor must never wedge
// completion handling.
func (r *runner) reportRateLimit(beadID string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.ReportRateLimit(providerAnthropic, r.opts.ProjectID, beadID, time.Now())
}
