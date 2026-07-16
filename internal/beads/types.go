// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package beads adapts the bd (beads) CLI. It is the ONLY path by which
// koryph reads or mutates a project's issue graph. All calls run `bd`
// with the project root as working directory; koryph never opens the
// Dolt database directly.
//
// Implementation contract (adapter.go):
//   - Version(ctx) (string, error) — `bd version`; used for skew checks.
//   - Ready(ctx, ReadyOpts) ([]Issue, error) — `bd ready --json --limit 0`
//     (+ --parent when Parent != ""), preserving priority order P0→P3.
//   - Show(ctx, id) (Issue, error) — `bd show --json <id>`.
//   - ListChildren(ctx, id) ([]Issue, error) — `bd list --parent <id> --json`.
//   - ListByLabel(ctx, label) ([]Issue, error) — `bd list --label <label>
//     --json --limit 0`; used for idempotency checks (e.g. gh-intake dedupe).
//   - Create(ctx, CreateInput) (id string, error) — `bd create --silent
//     --body-file -` for a single bead; returns the new id.
//   - Comment(ctx, id, text) error
//   - AppendNotes(ctx, id, text) error — `bd update <id> --append-notes
//     <text>`; the reliable pre-dispatch nudge channel (koryph-o72): unlike
//     INBOX.md (only exists once a specific dispatch's phase dir exists),
//     `bd show`/`bd ready` always return Issue.Notes, and promptc.Compile
//     folds it into every future dispatch's prompt.
//   - Close(ctx, id, reason) error
//   - Claim(ctx, id) error / SetStatus(ctx, id, status) error
//   - CreateGraph(ctx, graphJSON, dryRun) (string, error)
//   - MergeSlotAcquire(ctx, slotID, owner) / MergeSlotRelease — the bd-backed
//     merge mutex (gt:slot bead). Graceful no-op if bd is absent.
//   - Snapshot(ctx) (path, error) — Dolt-level backup before migrations.
//   - Remember(ctx, text) error — `bd remember`.
//
// Dispatch-eligibility filtering (used by sched):
//   - exclude IssueType in {epic, feature, decision, merge-request}
//   - exclude labels: no-dispatch, refactor-core, gt:*
//   - exclude container beads (open children)
//   - exclude beads already active in a ledger
package beads

// Issue is the subset of bd's JSON the engine consumes.
type Issue struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	// Notes carries bd's free-form `notes` field verbatim, as populated by
	// `bd update --notes`/`--append-notes`. This is the reliable channel for
	// an operator addendum sent while a bead is still queued (koryph-o72):
	// `bd show --json`/`bd ready --json` return it unconditionally, so
	// promptc.Compile can fold it into whichever future dispatch picks the
	// bead up — unlike INBOX.md, which only exists once a specific dispatch
	// has created a phase dir (see cmd/koryph cmdNudge).
	Notes           string   `json:"notes,omitempty"`
	Status          string   `json:"status"`
	Priority        int      `json:"priority"`
	IssueType       string   `json:"issue_type"`
	Labels          []string `json:"labels"`
	DependencyCount int      `json:"dependency_count,omitempty"`
	DependentCount  int      `json:"dependent_count,omitempty"`
	// ParentID is bd's parent linkage. bd emits the key as "parent" in both
	// `bd show --json` and `bd ready --json` (NOT "parent_id" — that key is
	// always null; live repro 2026-07-05, first consumer koryph-wo0.4's
	// epic-validation trigger).
	ParentID string `json:"parent,omitempty"`
	// UpdatedAt is bd's RFC3339 `updated_at` timestamp — the last time ANY
	// field on this issue changed (status, notes, labels, ...). bd's `bd
	// list`/`bd show`/`bd ready` JSON has always carried this key; it was
	// simply never parsed into Issue until patrolCheckStaleClaims
	// (internal/engine/health.go) needed a staleness signal for self-parked
	// in_progress claims (2026-07-15/16 stampede-games handoff).
	UpdatedAt string `json:"updated_at,omitempty"`
}

// ReadyOpts scopes the ready-frontier query.
type ReadyOpts struct {
	Parent string // epic/molecule id; "" = whole graph
}

// HasLabel reports whether the issue carries the exact label.
func (i Issue) HasLabel(l string) bool {
	for _, x := range i.Labels {
		if x == l {
			return true
		}
	}
	return false
}

// LabelValues returns the values of labels with the given prefix, e.g.
// LabelValues("fp:") over ["fp:go:api","fp:app:web"] → ["go:api","app:web"].
func (i Issue) LabelValues(prefix string) []string {
	var out []string
	for _, x := range i.Labels {
		if len(x) > len(prefix) && x[:len(prefix)] == prefix {
			out = append(out, x[len(prefix):])
		}
	}
	return out
}

// Adapter runs bd within one project.
type Adapter struct {
	RepoRoot string
	BeadsDir string // usually <RepoRoot>/.beads; exported as BEADS_DIR to agents
	Bin      string // bd binary; ResolveBin() unless overridden (tests)
}

// New returns an adapter for the project rooted at repoRoot. The bd binary
// honors the KORYPH_BD_BIN override (via ResolveBin) so every koryph surface —
// including the cockpit/TUI, which previously hardcoded "bd" — resolves the same
// bd, and setting KORYPH_BD_BIN to a newer bd is a working stopgap everywhere.
func New(repoRoot string) *Adapter {
	return &Adapter{RepoRoot: repoRoot, BeadsDir: repoRoot + "/.beads", Bin: ResolveBin()}
}
