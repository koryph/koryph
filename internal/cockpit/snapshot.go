// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package cockpit is the TUI-agnostic view-model layer for the koryph terminal
// cockpit (docs/designs/2026-07-tui-cockpit.md §1).
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

	// CapturedAt is when this snapshot was assembled.
	CapturedAt time.Time
}
