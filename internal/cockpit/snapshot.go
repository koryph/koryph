// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package cockpit is the TUI-agnostic view-model layer for the koryph terminal
// cockpit (docs/designs/2026-07-tui-cockpit.md §1).
//
// EfficiencySnapshot (koryph-9af.4) — efficiency + calibration dashboard data:
//   - DispatchSparkline: daily dispatch counts (sparkline data).
//   - AchievedConcurrency / PermittedConcurrency: live vs AIMD cap.
//   - TopDeferralTokens: most-contended footprint write-tokens from active slots.
//   - GovernorPools: per-pool cap/probe/settle/breaker detail.
//   - EstimatorRows: per-(tier:size) bucket n/bias/MAPE/corrected.
//   - ProviderQuotas: per-AI-provider 5h/weekly quota burn bars (ceiling from
//     config; live spend requires ccusage/transcript-scan background — marked
//     unavailable when absent).
//
// It provides typed snapshots and subscription interfaces over the koryph run
// ledger, global governor, and quota store so that both the Bubble Tea TUI
// (internal/tui) and the VS Code extension (koryph-ew2) can consume the same
// data without re-implementing file access.
//
// # Refresh model
//
// Consumers call Provider.Refresh() on a timer (recommended: 100ms poll) to
// obtain a fresh Snapshot. All providers are read-only; they never write
// ledger, governor, or quota state.
//
// # Minimum terminal floor
//
// The cockpit itself carries no terminal-size logic; see internal/tui for the
// 80×24 enforcement.
package cockpit

import (
	"sort"
	"time"

	"github.com/koryph/koryph/internal/govern"
)

// AttemptRecord captures the outcome of one dispatch attempt for a bead,
// assembled from the run ledger for the bead detail panel.
type AttemptRecord struct {
	Attempt      int
	Status       string
	RequeueCause string // "gate", "merge", "ratelimit", "manual", or ""
	CostUSD      float64
	Elapsed      time.Duration
	Model        string
	Branch       string
	Worktree     string
	DispatchedAt time.Time
}

// BeadDetailSnapshot is a point-in-time view of one bead's full detail.
// Zero value (BeadID == "") means no bead is focused.
type BeadDetailSnapshot struct {
	BeadID      string
	Title       string
	Description string
	Acceptance  string // acceptance criteria section
	Labels      []string
	Notes       string
	Status      string
	Priority    int
	IssueType   string
	ParentID    string

	// Dependency graph.
	Deps        []string // issue IDs this bead depends on
	ReverseDeps []string // issue IDs that depend on this bead

	// Slot-derived fields.
	Branch      string
	Worktree    string
	CostUSD     float64
	EstimateUSD float64
	// DeathReason / SlotNote mirror the ledger slot's failure classification
	// and free-form note (block hints, park reasons) — the "why did this
	// thread die / why is it blocked" answer, for escalation decisions.
	DeathReason string
	SlotNote    string
	LogPath     string // path to agent session log for 't' tail
	// StreamPath is the agent's stream.jsonl (Slot.Stream) — the source for
	// the 'T' thinking tail (koryph-xvk): extended-thinking deltas, with
	// subagent segments marked by parent_tool_use_id transitions.
	StreamPath string

	// Timing + resource usage (koryph process-metrics). StartedAt/FinishedAt are
	// the dispatch and terminal wall-clock instants; the resource aggregates are
	// the slot's agent process-cohort usage. Zero when unavailable.
	StartedAt       time.Time
	FinishedAt      time.Time
	PeakRSSMB       int
	AvgRSSMB        int
	CPUSeconds      float64
	CPUUtilPct      float64
	IOReadMB        float64
	IOWriteMB       float64
	ResourceSamples int

	// AttemptHistory is chronological, most-recent last.
	AttemptHistory []AttemptRecord

	ComputedAt time.Time
}

// SlotSnapshot is a point-in-time view of one running/recent ledger slot.
// All fields are display-ready strings or zero values when unknown.
type SlotSnapshot struct {
	PhaseID  string // bead id or phase slug
	BeadID   string
	Title    string // from bd, or PhaseID if unavailable
	Stage    string // ledger status: running/review/merge-pending/merged/…
	Model    string
	ModelWhy string // ledger ModelWhy: rationale for the model tier (escalation, etc.)
	Attempt  int
	PID      int
	Branch   string
	Worktree string

	// Requeue counters, mirrored from the ledger slot (koryph-2im.*, koryph-3as,
	// koryph-77r.10). Each records retries already spent for a specific cause;
	// the Threads tab surfaces them so an operator can see WHY a thread has been
	// re-dispatched (retries + model-escalation visibility). All zero on a
	// first, still-clean attempt.
	GateRequeues       int
	MergeRequeues      int
	ConflictRequeues   int
	RateLimitRequeues  int
	BudgetKillRequeues int

	// Terminal reports whether Stage is a terminal ledger status (merged, done,
	// failed, conflict, blocked, pr-opened, merge-pending). The Threads tab
	// hides terminal slots from its default "active" filter — a merged thread is
	// finished work retained in the ledger for history/recovery, not a live
	// worker — so they no longer masquerade as active threads.
	Terminal bool

	// Zombie reports whether the slot is non-terminal (the ledger still says
	// it's live work) but its recorded PID is no longer alive. Computed with a
	// best-effort, read-only signal-0 probe (dispatch.Alive) — never persisted,
	// never used to mutate the slot. False whenever PID is unset (dispatch
	// hadn't recorded one yet) or the probe couldn't run. koryph-k6o: closes
	// the incident where a read-only view kept rendering a dead-pid slot as
	// "running" for hours because only board's separate LIVE column hinted at
	// the truth and nothing correlated the two.
	Zombie bool

	// Cost and estimate (zero means unknown/not-yet-set).
	CostUSD     float64
	EstimateUSD float64

	// Timing.
	DispatchedAt time.Time     // start clock; zero if unknown
	FinishedAt   time.Time     // stop clock; zero while live/unknown
	Elapsed      time.Duration // wall time (finished-dispatched, or now-dispatched)

	// Resource-usage aggregates for this slot's agent process cohort, mirrored
	// from the ledger (koryph process-metrics). Zero when no sample has landed
	// or the metric is unavailable on the platform (macOS supplies no per-process
	// disk I/O). CPUUtilPct is derived from CPUSeconds over the wall window.
	PeakRSSMB       int
	AvgRSSMB        int
	CPUSeconds      float64
	CPUUtilPct      float64
	IOReadMB        float64
	IOWriteMB       float64
	ResourceSamples int

	// StatusLine is the last human-readable state from the agent's status.json
	// (the "step" field). Empty when the file is absent or unreadable.
	StatusLine string

	// StatusJSON is the raw "state" field from the agent's status.json.
	StatusJSON string

	// StatusAge is how long ago the agent last rewrote its status.json (the
	// step-boundary heartbeat). Zero when the file is absent. A running slot
	// whose heartbeat is old is stalled — structured stuck-detection from
	// work-assignment + heartbeat + liveness, never output scraping.
	StatusAge time.Duration

	// DeathReason is the engine's classification of the most recent attempt's
	// death ("budget-killed", "operator-stop", …). Empty while live or when
	// the ledger predates the field.
	DeathReason string

	// Note is the slot's free-form ledger note (block hints, park reasons).
	Note string

	// ModelActual is the model that ACTUALLY served the most recent attempt
	// (fallback downgrades make it diverge from the requested Model). Empty
	// when unrecorded.
	ModelActual string
}

// GovernorSnapshot is a point-in-time view of the machine-global governor
// across all provider pools.
type GovernorSnapshot struct {
	// Pools maps provider → PoolSnapshot.
	Pools map[string]PoolSnapshot

	// Resources is the per-kind external resource ledger state (capacity,
	// live holders, reserved-vs-materialized MB, ramp state), sourced from
	// govern.Store.ResourcesStatus() (koryph-4ql.1 L7, koryph-4ql.10 —
	// design docs/designs/2026-07-resource-governor.md §4 "Cockpit
	// snapshots"). Additive: nil on an old snapshot, when the governor is
	// unavailable, or when nothing has ever declared/configured a res:<kind>
	// — old TUI/IDE builds that don't know this field simply render no
	// resources section.
	Resources []ResourceSnapshot
}

// ResourceSnapshot is one external resource kind's live observable state for
// the cockpit governor view (koryph-4ql.1 L7, koryph-4ql.10). It mirrors
// govern.ResourceStatus field-for-field so the TUI and the VS Code extension
// never need to import internal/govern directly — the same boundary
// PoolSnapshot already draws for per-pool state.
type ResourceSnapshot struct {
	Kind        string
	Capacity    int // resolved (default 1 when unconfigured — govern.DefaultResourceCapacity)
	MemMB       int // configured per-holder reservation (0 = uncalibrated)
	RampSeconds int // resolved ramp window
	Probe       string
	Holders     []ResourceHolderSnapshot

	// ReservedMB is the sum of MemReserveMB across holders still ramping
	// (still subtracted from the live memory reading); MaterializedMB is the
	// rest (past ramp, assumed showing in the real reading). See
	// govern.ResourceStatus for the accounting detail.
	ReservedMB     int
	MaterializedMB int
}

// ResourceHolderSnapshot identifies one live lease holding a resource kind,
// mirroring govern.ResourceHolder.
type ResourceHolderSnapshot struct {
	Project      string
	Bead         string
	MemReserveMB int
	Ramping      bool
}

// PoolSnapshot is one provider pool's live state.
type PoolSnapshot struct {
	Provider     string
	Cap          int // operator-configured cap
	Dynamic      int // current AIMD cap (== Cap when not adaptive)
	Adaptive     bool
	Leases       int    // active leases
	BreakerState string // ""|"open"|"half-open"
}

// PrimaryPool returns the pool that is actually gating dispatch, for display:
// the pool holding the most active leases (work runs there), falling back to
// the default (anthropic) pool and then the alphabetically-first pool when
// the machine is idle. Deterministic — ties break by name. ok is false when
// no pools exist. Every consumer that shows "the" governor state (status bar,
// concurrency gauge) MUST use this instead of ranging over the Pools map —
// map iteration order is random per call, which made the status bar flicker
// between pools on every render (a machine with anthropic/personal/work
// pools rotated through all three).
func (g GovernorSnapshot) PrimaryPool() (PoolSnapshot, bool) {
	if len(g.Pools) == 0 {
		return PoolSnapshot{}, false
	}
	names := make([]string, 0, len(g.Pools))
	for name := range g.Pools {
		names = append(names, name)
	}
	sort.Strings(names)

	best, bestName := PoolSnapshot{}, ""
	for _, name := range names {
		ps := g.Pools[name]
		if bestName == "" || ps.Leases > best.Leases {
			best, bestName = ps, name
		}
	}
	if best.Leases > 0 {
		return best, true
	}
	if ps, ok := g.Pools[govern.DefaultPool]; ok {
		return ps, true
	}
	return g.Pools[names[0]], true
}

// GraphSnapshot is a point-in-time view of the beads dependency graph for one
// project. It is populated by GraphProvider at graphTTL cadence and embedded in
// Snapshot so that multiple tabs (queue, detail) can consume the same cached
// read without each tab independently calling bd.
//
// The zero value is safe: consumers check NodeCount == 0 or len(Deps) == 0
// before rendering graph-derived content.
type GraphSnapshot struct {
	// Deps maps each issue ID to the list of issue IDs it directly depends on
	// (its blockers). Derived from `bd list --format digraph` output.
	// A missing key means the issue has no dependencies.
	Deps map[string][]string

	// NodeCount is the number of issues present in the graph (len(Deps) after
	// including issues that appear only as targets, not sources).
	NodeCount int

	// EdgeCount is the total number of directed dependency edges.
	EdgeCount int

	// ComputedAt is when this snapshot was assembled.
	ComputedAt time.Time
}

// Snapshot is the full point-in-time view of one project delivered to the TUI.
// The zero value is safe; every field is optional.
type Snapshot struct {
	ProjectID string
	RunID     string
	RunStatus string // running/done/drained/…
	Wave      int

	// Slots holds the live and recently-terminal slots for this project's
	// current (latest) run, ordered by PhaseID for stable display.
	Slots []SlotSnapshot

	// Governor is the machine-wide governor state (cross-project).
	Governor GovernorSnapshot

	// Burndown holds trajectory projections for the burndown tab (koryph-9af.7).
	// Populated by LedgerProvider at burndownTTL cadence; zero when unavailable.
	Burndown BurndownSnapshot

	// Graph holds the beads dependency graph snapshot (koryph-9af.8).
	// Populated by LedgerProvider via GraphProvider at graphTTL cadence.
	// Consumed read-only by queue and detail tabs; never written by tab code.
	Graph GraphSnapshot

	// Efficiency holds the efficiency + calibration dashboard data (koryph-9af.4).
	// Populated by LedgerProvider at efficiencyTTL cadence; zero when unavailable.
	Efficiency EfficiencySnapshot

	// Detail holds the full detail for the currently-focused bead (koryph-9af.3).
	// Zero value (BeadID == "") means no bead is focused.
	Detail BeadDetailSnapshot

	// Queue holds the hierarchical work-queue snapshot (koryph-9af.2).
	// Populated by LedgerProvider at queueTTL cadence; zero when bd is absent.
	// Consumed read-only by the Queue tab; never written by tab code.
	Queue QueueSnapshot

	// Events is the live events feed for this project (koryph-9af.5).
	// Populated by LedgerProvider from ledger state transitions and the
	// machine-wide audit log. The zero value (empty slice) is safe.
	Events EventsSnapshot

	// Patrol is the run ledger's health-patrol history (engine koryph-gus):
	// stuck claims, stale worktrees, and other in-loop findings. Mirrored so
	// the events feed can surface warn-level findings. Nil when the ledger
	// predates patrols or none have run.
	Patrol []PatrolEventSnapshot

	// CapturedAt is when this snapshot was assembled.
	CapturedAt time.Time
}

// ProviderQuotaSnapshot is one AI provider's usage-window burn state — the
// 5-hour and weekly allocation an operator watches to decide whether to
// change models or serialize dispatch to stay inside a subscription's
// allowances. koryph measures usage per RUNTIME (each has its own
// account/config-dir and its own usage source), so Runtime is the primary
// display key; Provider is the runtime's billing/rate-limit identity
// (runtime.Runtime.Provider() — the same string used as a governor pool key)
// for cases where multiple runtimes share one provider's limits.
type ProviderQuotaSnapshot struct {
	// Runtime is the runtime this quota was measured for (runtime.Runtime.Name():
	// "claude" today; "codex"/"gemini"/"grok" once those adapters exist).
	Runtime string
	// Provider is the billing/rate-limit identity Runtime bills against
	// (runtime.Runtime.Provider(): "anthropic" for claude).
	Provider string

	// Window5hCeiling / WeeklyCeiling are the configured per-window USD
	// ceilings. 0 means uncalibrated (run koryph quota calibrate).
	Window5hCeiling float64
	WeeklyCeiling   float64

	// Window5hFrac / WeeklyFrac are the spent/ceiling fractions when live
	// usage is available. Negative means unavailable.
	Window5hFrac float64
	WeeklyFrac   float64

	// Window5hSpent / WeeklySpent are the raw spent-USD values. 0 when
	// unavailable.
	Window5hSpent float64
	WeeklySpent   float64

	// Source identifies the data source for the window values: "ccusage",
	// "jsonl-scan", "unavailable", or "uncalibrated".
	Source string
}

// EfficiencySnapshot is the efficiency + calibration dashboard data assembled
// for the Efficiency tab (koryph-9af.4, design §2.4).
//
// The zero value is safe: all slice fields are nil (renders "no data") and
// numeric fields are 0 ("not available").
type EfficiencySnapshot struct {
	ComputedAt time.Time

	// DispatchSparkline is the number of slots dispatched per day for the last
	// SparklineLen days (index 0 = oldest, last index = today). Used to render
	// the dispatched-per-refill sparkline.
	DispatchSparkline []float64

	// AchievedConcurrency is the current count of running/dispatching slots.
	AchievedConcurrency int

	// PermittedConcurrency is the governor's effective (AIMD dynamic) cap for
	// the default pool — the upper bound on concurrency right now.
	PermittedConcurrency int

	// TopDeferralTokens lists the footprint write-tokens held by the most
	// active slots, in descending hold-count order. These are the tokens most
	// likely to be causing deferral for waiting beads (the coupling metric).
	TopDeferralTokens []DeferralToken

	// GovernorPools is the expanded per-pool state (cap/probe/settle/breaker),
	// one entry per pool present in governor.json.
	GovernorPools []GovernorPoolDetail

	// EstimatorRows is the per-(tier:size) bucket accuracy table derived from
	// quota.Config.ErrorStats + quota.Config.Calibration (koryph-6bl).
	EstimatorRows []EstimatorRow

	// ProviderQuotas is the usage-window burn state, one entry per AI
	// provider/runtime with a measurable quota. A koryph project can dispatch
	// threads under different runtimes (ledger.Slot.Runtime — "claude" today,
	// with codex/gemini/grok adapters on the roadmap per internal/runtime's
	// Registry), and each runtime is billed against its OWN provider's rate
	// limits with its own measurement source (runtime.Capabilities.UsageSource).
	// A single flat pair of "the" 5h/weekly fields would silently mean "Claude"
	// without saying so and would have nowhere to put a second provider's
	// numbers once one exists — this is the per-provider join point instead.
	// Exactly one entry ("claude") is populated today; nil when nothing is
	// calibrated. See ProviderQuotaSnapshot for field meaning.
	ProviderQuotas []ProviderQuotaSnapshot

	// TokenRows is the per-bead token composition table (koryph-77r.3, design
	// §3 L1). Assembled from historical ledger slots; most-recent beads first.
	// Nil when no token data has accumulated (ledger predates token fields).
	TokenRows []TokenCompositionRow

	// FleetCacheHitRatio is the fleet-wide cache_read share of total input
	// tokens: cache_read / (input_fresh + cache_read + cache_creation).
	// Range [0,1]; 0 when no token data available. Spans all retained history.
	FleetCacheHitRatio float64

	// FleetCacheHit24h is the same ratio over slots dispatched in the last
	// 24 h — the actionable "is caching working right NOW" number. Negative
	// when no slot in that window carries token data.
	FleetCacheHit24h float64

	// CacheHitTripwire is the I7 tripwire state for the cache-hit ratio.
	// "" = OK / insufficient data; "warn" = the recent-window cache_read
	// share is below threshold (design §2 I7).
	CacheHitTripwire string

	// ModelRows is the per-model token/cost rollup (keyed on the model that
	// actually served — ModelActual — with the requested model as fallback),
	// sorted by total tokens descending. This is the "do I need to change
	// models" table.
	ModelRows []ModelTokenRow

	// TokensPerBeadTrend is a sparkline series of mean tokens-per-bead over
	// the last SparklineLen days (index 0 = oldest, last = today).
	// Entry is 0 for days with no completed beads.
	TokensPerBeadTrend []float64
}

// DeferralToken is one footprint write-token held by active slots, with a
// count of how many slots are holding it as a write-lock.
type DeferralToken struct {
	// Token is the footprint token string, e.g. "area:engine" or "domain:unknown".
	Token string
	// HeldBy is the number of currently-active slots holding this token as a write.
	HeldBy int
}

// GovernorPoolDetail is one pool's full observable state for the efficiency tab.
// It extends PoolSnapshot with probe/settle/breaker detail for the governor section.
type GovernorPoolDetail struct {
	Provider     string
	Cap          int // operator-configured MaxGlobalAgents
	Dynamic      int // current AIMD cap (= Cap when not adaptive)
	Leases       int // active leases
	Adaptive     bool
	BreakerState string // ""|"open"|"half-open"
	Settling     bool   // true when inside a settle window
	SettleUntil  string // RFC3339 settle-window deadline; "" when not settling
	ProbeProject string // project/bead of the current half-open probe; both "" when none
	ProbeBead    string
}

// EstimatorRow is one row of the per-(tier:size) estimator accuracy table.
// Derived from quota.Config.ErrorStats (koryph-6bl) and quota.Config.Calibration.
type EstimatorRow struct {
	// Bucket is the "<tier>:<size>" key, e.g. "sonnet:M".
	Bucket string
	// N is the total observation count (not EWMA-decayed).
	N int
	// Bias is the EWMA of (actual/estimate) ratios: 1.0 = perfect;
	// >1 = under-estimating; <1 = over-estimating.
	Bias float64
	// MAPE is the EWMA mean absolute percentage error.
	MAPE float64
	// Corrected is the calibrated USD estimate from quota.Config.Calibration.
	// 0 means not yet calibrated.
	Corrected float64
	// Base is the uncalibrated base cost (PerTierUSD * SizeMultiplier) —
	// the fallback estimate before calibration data accumulates.
	Base float64
}

// ModelTokenRow is one row of the per-model token/cost rollup: every token
// class plus accumulated cost, aggregated across all retained ledger history
// for slots served by Model.
type ModelTokenRow struct {
	// Model is the serving model (Slot.ModelActual, falling back to the
	// requested Slot.Model; "unknown" when neither is recorded).
	Model string
	// Beads is the number of slots aggregated into this row.
	Beads int
	// TotalTokens is the sum of all token classes.
	TotalTokens int64
	// InputFresh / CacheRead / CacheCreation / Output break the total down.
	InputFresh    int64
	CacheRead     int64
	CacheCreation int64
	Output        int64
	// CostUSD is the accumulated ledger cost across these slots.
	CostUSD float64
}

// TokenCompositionRow is one row of the per-bead token composition table
// (koryph-77r.3, design §3 L1). Derived from the ledger's accumulated slot
// token fields (InputTokens, OutputTokens, CacheReadTokens, CacheCreationTokens).
type TokenCompositionRow struct {
	// BeadID is the bead or phase identifier.
	BeadID string
	// Title is the bead's display title (from beads metadata or PhaseID fallback).
	Title string
	// TotalTokens is the sum of all token classes for this bead.
	TotalTokens int64
	// InputFresh is the fresh-input token count (not from cache).
	InputFresh int64
	// CacheRead is the cache_read token count.
	CacheRead int64
	// CacheCreation is the cache_creation token count.
	CacheCreation int64
	// Output is the output token count.
	Output int64
	// CacheHitRatio is cache_read / (input_fresh + cache_read + cache_creation).
	// Range [0,1]; 0 when denominator is 0 (no token data).
	CacheHitRatio float64
	// CostUSD is the slot's accumulated cost.
	CostUSD float64
}

// PatrolFindingSnapshot mirrors ledger.PatrolFinding for cockpit consumers.
type PatrolFindingSnapshot struct {
	Check   string
	Level   string // "ok" | "warn"
	Message string
	Fixed   bool
}

// PatrolEventSnapshot is one health-patrol run's findings.
type PatrolEventSnapshot struct {
	At       time.Time
	Findings []PatrolFindingSnapshot
}

// TUIEvent is one entry in the live events feed (Events tab, koryph-9af.5).
// It is a display-ready value type: no pointers, no methods.
type TUIEvent struct {
	// Time is when the event occurred (or was observed).
	Time time.Time

	// Kind classifies the event for filtering and coloring.
	// Values: "dispatch", "merge", "requeue", "drain", "resize",
	// "nudge", "cap-change", "review", "patrol".
	Kind string

	// Level is the severity: "info", "warn", "error".
	Level string

	// BeadID is the bead or phase identifier, empty for project-level events.
	BeadID string

	// Message is the human-readable one-line summary.
	Message string
}

// EventsSnapshot is the events tab's view-model — a bounded ring of TUIEvents.
// The zero value is safe; consumers check len(Events) > 0.
type EventsSnapshot struct {
	// Events holds the most recent events in ascending time order (oldest first,
	// newest last). The ring is bounded to eventsRingMax entries.
	Events []TUIEvent
}
