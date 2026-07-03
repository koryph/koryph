// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package sched builds conflict-free waves from a project's ready frontier.
//
// Implementation contract (footprint.go, wave.go, bdsource.go):
//   - FootprintFor(issue, cfg) Footprint — precedence: explicit fp:read:*
//     labels → reads; explicit fp:* labels (any other suffix) → writes;
//     cfg.AreaMap via area:* labels → writes; else the catch-all
//     "domain:unknown" write token (conflicts with everything, serializing
//     unknowns).
//   - Conflicts(a, b) bool — RW conflict: true iff some token is shared AND
//     at least one side holds it as a write (koryph-2im.1). Two readers of
//     the same token co-run; only a writer excludes.
//   - BuildWave(issues, cfg, opts) Wave — filter dispatch-eligible issues
//     (see package beads rules), preserve priority order, defer anything
//     conflicting with an in-flight (opts.Active) footprint, then
//     greedy-color the remainder by footprint into at most opts.Max
//     non-conflicting items; the rest go to Deferred with reasons;
//     dependency-unready → Blocked.
//   - Markdown source is out of scope for v1 BuildWave (legacy projects run
//     their fork until migrated); the WorkSource field exists so the engine
//     can refuse politely.
package sched

import (
	"strings"

	"github.com/koryph/koryph/internal/beads"
)

// Footprint is a bead's declared conflict surface, split into read and write
// token sets (koryph-2im.1, design L4). Two footprints conflict only when a
// shared token is held as a write by at least one side — plain read/read
// overlap co-runs, so a docs bead that merely *reads* engine code no longer
// excludes an engine writer. Writes has no omitempty: an issue with zero
// declared tokens still carries TokenUnknown there, so the JSON always shows
// the (conservative) blast radius rather than an ambiguous empty array.
type Footprint struct {
	Reads  []string `json:"reads,omitempty"`
	Writes []string `json:"writes"`
}

// String renders a footprint for logs, e.g. "[r:docs w:go:engine]" — reads
// prefixed r:, then writes prefixed w:, space-separated. Used anywhere a
// human needs to see WHY two beads did or didn't conflict.
func (f Footprint) String() string {
	parts := make([]string, 0, len(f.Reads)+len(f.Writes))
	for _, t := range f.Reads {
		parts = append(parts, "r:"+t)
	}
	for _, t := range f.Writes {
		parts = append(parts, "w:"+t)
	}
	return "[" + strings.Join(parts, " ") + "]"
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

	// Active is the in-flight footprint of every currently-dispatched bead,
	// keyed by bead id (koryph-2im.1, design L2). A candidate whose footprint
	// Conflicts with any entry here is deferred before intra-batch greedy
	// coloring even runs — this is what makes rolling (mid-wave) dispatch
	// safe: a freshly built batch can never clash with work already running.
	// nil/empty reproduces pre-L2 behavior exactly (no in-flight gating).
	Active map[string]Footprint
}
