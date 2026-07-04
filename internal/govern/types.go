// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package govern is the machine-global concurrency governor: a cross-process
// cap on the number of agents running at once across ALL projects, so
// independent `koryph run` invocations cannot collectively breach the Claude
// API concurrency / rate limits.
//
// It coordinates through files under ~/.koryph (paths.SlotsDir) guarded by a
// short flock — no daemon:
//
//   - governor.json          machine-wide cap {max_global_agents} (default 8)
//   - slots/<lease>.json      one lease per running agent (keyed to the AGENT pid)
//   - slots/demand/<proj>.json per-project demand heartbeat (fair-share input)
//
// Slots are allocated fair-share across the projects that currently have ready
// work: each demander gets floor(cap/n) slots, the cap%n remainder rotates over
// time (so no project starves when projects outnumber slots), and idle capacity
// is handed out work-conservingly when every other demander already holds its
// share. See docs/developer-guide/global-governor.md.
package govern

// DefaultMaxGlobalAgents is the cap used when governor.json is absent. Raised
// to 8 to let a single self-hosting project run a wider wave; being monitored
// for Claude API rate limiting — drop to 6 if beads start getting throttled. A
// governor.json still overrides this per machine.
const DefaultMaxGlobalAgents = 8

// Config is the machine-wide concurrency governor config (governor.json).
//
// The AIMD overlay fields (koryph-2im.4,
// docs/designs/2026-07-scheduler-throughput.md L5) are additive: a
// governor.json written before they existed unmarshals them all to their zero
// values, i.e. Adaptive=false, which reproduces today's static-cap behavior
// byte-for-byte — see Config.EffectiveCap.
//
// The settle-window / circuit-breaker / dispatch-smoothing fields
// (koryph-2im.11, docs/designs/2026-07-scheduler-throughput.md L5b) are
// likewise additive and, like the rest of the AIMD overlay, only take effect
// when Adaptive is on — see internal/govern/aimd.go.
type Config struct {
	MaxGlobalAgents int `json:"max_global_agents"`

	// Adaptive enables the AIMD overlay: the effective cap floats between 1
	// and HardMax (probing up on quiet, halving on rate-limit) instead of
	// pinning to MaxGlobalAgents.
	Adaptive bool `json:"adaptive,omitempty"`
	// HardMax bounds upward probing while Adaptive is on; ignored otherwise.
	HardMax int `json:"hard_max,omitempty"`
	// DynamicCap is the current floating cap. Seeded to MaxGlobalAgents when
	// adaptive is enabled; then adjusted by ReportRateLimit (halve) and the
	// lazy additive probe (see Store.EffectiveCap).
	DynamicCap int `json:"dynamic_cap,omitempty"`
	// LastDecreaseAt is the RFC3339 timestamp of the most recent multiplicative
	// decrease. Observability only as of koryph-2im.11 — the additive probe's
	// elapsed-time clock now anchors on SettleUntil (see applyProbe), since
	// the settle window subsumes the old decrease-cooldown's role.
	LastDecreaseAt string `json:"last_decrease_at,omitempty"`
	// LastRateLimitAt is the RFC3339 timestamp of the most recent rate-limit
	// report, applied or merely counted while settling/open.
	LastRateLimitAt string `json:"last_rate_limit_at,omitempty"`
	// LastProbeAt is internal bookkeeping for the additive-increase probe: the
	// RFC3339 timestamp the probe last advanced from. Persisted (not just
	// in-memory) so probing survives an engine restart.
	LastProbeAt string `json:"last_probe_at,omitempty"`
	// RateLimitEvents counts every ReportRateLimit call — applied or
	// suppressed by settle/breaker state — for operator observability
	// (`governor show`, `koryph doctor`).
	RateLimitEvents int `json:"rate_limit_events,omitempty"`

	// --- settle window / burst detection (koryph-2im.11) -------------------

	// SettleSeconds is how long, after ANY DynamicCap change (decrease or
	// additive increase), further changes in either direction are frozen.
	// <=0 uses DefaultSettleSeconds. Subsumes the old 60s decrease cooldown.
	SettleSeconds int `json:"settle_seconds,omitempty"`
	// SettleUntil is the RFC3339 deadline of the current freeze; "" or a past
	// time means not settling. Also anchors the additive probe's clock (the
	// quiet-clock starts at settle expiry, not at the change itself).
	SettleUntil string `json:"settle_until,omitempty"`
	// RecentRateLimitEvents is a small bounded (pruned-on-write) history of
	// rate-limit events within the last 30s, used only to count DISTINCT
	// (project, bead) slots for the burst-scaled decrease.
	RecentRateLimitEvents []RateLimitEvent `json:"recent_rate_limit_events,omitempty"`
	// RecentDecreases is a small bounded (pruned-on-write) history of
	// multiplicative-decrease timestamps within the last 10 minutes, used
	// only to trip the circuit breaker on 3 decreases in that window.
	RecentDecreases []string `json:"recent_decreases,omitempty"`

	// --- circuit breaker (koryph-2im.11) ------------------------------------

	// BreakSeconds is the base open-state duration; doubled per consecutive
	// re-open (BreakerReopenCount), capped at maxBreakSeconds. <=0 uses
	// DefaultBreakSeconds.
	BreakSeconds int `json:"break_seconds,omitempty"`
	// BreakerState is "" (closed), "open", or "half-open".
	BreakerState string `json:"breaker_state,omitempty"`
	// BreakerOpenAt is the RFC3339 timestamp the breaker most recently opened.
	BreakerOpenAt string `json:"breaker_open_at,omitempty"`
	// BreakerBreakSeconds is the concrete (already-doubled, already-capped)
	// duration of the CURRENT open period, computed once at open time so a
	// later BreakSeconds config edit cannot retroactively change it.
	BreakerBreakSeconds int `json:"breaker_break_seconds,omitempty"`
	// BreakerReopenCount is the number of consecutive times the breaker has
	// re-opened after a failed half-open probe; resets to 0 on a clean close.
	BreakerReopenCount int `json:"breaker_reopen_count,omitempty"`
	// ProbeProject/ProbeBead identify the single lease admitted while
	// half-open (the probe dispatch); both empty means no probe outstanding.
	ProbeProject string `json:"probe_project,omitempty"`
	ProbeBead    string `json:"probe_bead,omitempty"`
	// ProbeAdmittedAt is the RFC3339 timestamp the probe was admitted, used by
	// the crashed-probe timeout fallback (a probe that dies without a release
	// or a rate-limit report cannot wedge the breaker half-open forever).
	ProbeAdmittedAt string `json:"probe_admitted_at,omitempty"`

	// --- dispatch smoothing (koryph-2im.11) ---------------------------------

	// MinDispatchIntervalSeconds is the machine-wide minimum spacing between
	// admitted dispatches, jittered ±50%. <=0 uses
	// DefaultMinDispatchIntervalSeconds. Gated on Adaptive, like the rest of
	// this section, for zero behavior change on non-adaptive setups.
	MinDispatchIntervalSeconds int `json:"min_dispatch_interval_seconds,omitempty"`
	// LastAdmitAt is the RFC3339 timestamp of the most recent admitted
	// dispatch (any project) — the spacing clock's anchor.
	LastAdmitAt string `json:"last_admit_at,omitempty"`
}

// RateLimitEvent is one bounded entry in Config.RecentRateLimitEvents: enough
// identity (project+bead) to count DISTINCT in-flight slots reporting a
// rate-limit within the burst window.
type RateLimitEvent struct {
	At      string `json:"at"`
	Project string `json:"project,omitempty"`
	Bead    string `json:"bead,omitempty"`
}

// Lease records one running agent holding a global slot. It is keyed to the
// detached AGENT pid so the lease survives an engine restart/resume and frees
// only when the real agent process dies.
type Lease struct {
	Project    string `json:"project"`
	Bead       string `json:"bead"`
	PID        int    `json:"pid"`        // agent process id
	EnginePID  int    `json:"engine_pid"` // owning koryph run pid
	Model      string `json:"model,omitempty"`
	AcquiredAt string `json:"acquired_at"` // RFC3339
}

// Demand is a project's "I have ready work and want slots" heartbeat, refreshed
// each wave and pruned when stale or its engine dies. The set of live demands
// is the fair-share denominator.
type Demand struct {
	Project   string `json:"project"`
	EnginePID int    `json:"engine_pid"`
	UpdatedAt string `json:"updated_at"` // RFC3339
}
