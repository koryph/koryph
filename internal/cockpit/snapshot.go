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
//   - QuotaWindow5h / QuotaWindowWeekly: quota burn bars (ceiling from config;
//     live spent requires ccusage background — marked unavailable when absent).
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

import "time"

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
	LogPath     string // path to agent session log for 't' tail

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
	Attempt  int
	PID      int
	Branch   string
	Worktree string

	// Cost and estimate (zero means unknown/not-yet-set).
	CostUSD     float64
	EstimateUSD float64

	// Timing.
	DispatchedAt time.Time // zero if unknown
	Elapsed      time.Duration

	// StatusLine is the last human-readable state from the agent's status.json
	// (the "step" field). Empty when the file is absent or unreadable.
	StatusLine string

	// StatusJSON is the raw "state" field from the agent's status.json.
	StatusJSON string
}

// GovernorSnapshot is a point-in-time view of the machine-global governor
// across all provider pools.
type GovernorSnapshot struct {
	// Pools maps provider → PoolSnapshot.
	Pools map[string]PoolSnapshot
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

	// CapturedAt is when this snapshot was assembled.
	CapturedAt time.Time
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

	// QuotaWindow5hCeiling / QuotaWindowWeeklyCeiling are the configured
	// per-window USD ceilings. 0 means uncalibrated (run koryph quota calibrate).
	QuotaWindow5hCeiling     float64
	QuotaWindowWeeklyCeiling float64

	// QuotaWindow5hFrac / QuotaWindowWeeklyFrac are the spent/ceiling fractions
	// when live usage is available (from ccusage background refresh). NaN or
	// negative means unavailable.
	QuotaWindow5hFrac     float64
	QuotaWindowWeeklyFrac float64

	// QuotaWindow5hSpent / QuotaWindowWeeklySpent are the raw spent-USD values.
	// 0 when unavailable.
	QuotaWindow5hSpent     float64
	QuotaWindowWeeklySpent float64

	// QuotaSource identifies the data source for the quota window values:
	// "ccusage", "jsonl-scan", "unavailable", or "uncalibrated".
	QuotaSource string
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
