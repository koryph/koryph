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
type Config struct {
	MaxGlobalAgents int `json:"max_global_agents"`
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
