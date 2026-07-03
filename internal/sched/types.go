// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package sched builds conflict-free waves from a project's ready frontier.
//
// Implementation contract (footprint.go, wave.go, bdsource.go):
//   - FootprintFor(issue, cfg) Footprint — precedence: explicit fp:* labels →
//     cfg.AreaMap via area:* labels → cfg.Footprint rules over expected files
//     → the catch-all "domain:unknown" token (conflicts with everything,
//     serializing unknowns).
//   - Conflicts(a, b) bool — token-set intersection (any shared token
//     conflicts; HOT: tokens are just tokens that projects put on shared
//     paths so they intersect often).
//   - BuildWave(issues, cfg, opts) Wave — filter dispatch-eligible issues
//     (see package beads rules), preserve priority order, greedy-color by
//     footprint into at most opts.Max non-conflicting items; the rest go to
//     Deferred with reasons; dependency-unready → Blocked.
//   - Markdown source is out of scope for v1 BuildWave (legacy projects run
//     their fork until migrated); the WorkSource field exists so the engine
//     can refuse politely.
package sched

import "github.com/koryph/koryph/internal/beads"

// Footprint is a set of conflict tokens.
type Footprint struct {
	Tokens []string `json:"tokens"`
}

// Item is one schedulable work unit.
type Item struct {
	Issue     beads.Issue `json:"issue"`
	Footprint Footprint   `json:"footprint"`
	Model     string      `json:"model"`
	ModelWhy  string      `json:"model_rationale"`
	Persona   string      `json:"persona"`
	Effort    string      `json:"effort,omitempty"`
	EpicID    string      `json:"epic_id,omitempty"`
}

// Wave is the scheduler output.
type Wave struct {
	Source     string   `json:"source"`
	Max        int      `json:"max_concurrent"`
	ReadyCount int      `json:"ready_count"`
	Items      []Item   `json:"wave"`
	Deferred   []Reason `json:"deferred"`
	Blocked    []Reason `json:"blocked"`
	// Skipped records structurally non-dispatchable ready issues (non-task
	// issue_types, gt:* gate beads). Unlike Deferred these will NEVER dispatch
	// as-is — surfacing them is what stops a mis-typed bead from sitting in
	// `bd ready` forever with no runtime signal (koryph-6g2.1).
	Skipped []Reason `json:"skipped"`
}

// Reason explains a non-dispatched issue.
type Reason struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Reason string `json:"reason"`
}

// Opts tunes wave building.
type Opts struct {
	Max          int
	DefaultModel string          // model for beads without model:*/tier labels ("" → stage default)
	Parent       string          // epic scope; "" = whole frontier
	ActiveIDs    map[string]bool // beads already active in a ledger
}
