// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package plan provides corpus-level plan analysis tools.
//
// The primary entry point is Audit, which performs a deterministic, read-only
// conflict analysis of a project's open bead corpus under the project's current
// sched rules (FootprintFor + Conflicts). It surfaces four categories of issue:
//
//  1. Unlabeled beads — those whose footprint resolves to the catch-all
//     domain:unknown token, serializing them against every other unknown.
//  2. Non-dispatchable ready beads — structural type or label problems that
//     the loop silently skips; calling them out here gives operators a static
//     view without needing to run the loop.
//  3. Dependency-unordered conflicting pairs — pairs of open beads where
//     neither depends on the other but their footprints conflict; these are
//     the beads that will block each other in the scheduler even when both are
//     ready at the same time.
//  4. Width metrics — the achievable parallel width under current labels and
//     the potential width if all unlabeled beads were properly labeled.
//  5. Derived-artifact co-footprint risks — dependency-unordered beads that both
//     touch a checked-in derived artifact (a migrations lockfile, a secrets
//     baseline) yet are write-disjoint, so the scheduler may co-dispatch them
//     and their regenerated derived file collides at merge (the inverse of a
//     conflict finding — sched.Conflicts cannot see it).
package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/sched"
)

// AuditReport is the read-only corpus conflict analysis returned by Audit.
// It is safe to marshal to JSON directly.
type AuditReport struct {
	// ProjectID is the project being audited.
	ProjectID string `json:"project_id"`

	// TotalOpen is the number of open issues considered by the audit.
	TotalOpen int `json:"total_open"`

	// Unlabeled lists beads whose footprint resolved to domain:unknown.  They
	// serialize against every other unknown bead (only one runs per wave), so
	// adding an area:* or fp:* label immediately improves concurrency.
	Unlabeled []ItemSummary `json:"unlabeled"`

	// NonDispatch lists ready beads that are structurally non-dispatchable:
	// wrong issue_type (epic/feature/decision/merge-request), gt:* gate label,
	// no-dispatch or refactor-core label.  These will never dispatch as-is.
	NonDispatch []SkipSummary `json:"non_dispatchable"`

	// Conflicts lists every pair of open, dependency-unordered beads whose
	// footprints conflict under sched.Conflicts.  A dependency-unordered pair
	// is one where neither bead (transitively) depends on the other — meaning
	// they could in principle run simultaneously, but their footprints prevent
	// it.
	Conflicts []ConflictPair `json:"conflicts"`

	// DerivedArtifactRisks lists dependency-unordered pairs of beads that both
	// touch a checked-in derived artifact (a migrations lockfile, a secrets
	// baseline) yet are write-DISJOINT — the inverse of a Conflicts finding.
	// The scheduler may co-dispatch them, and they will regenerate the derived
	// file independently and collide at merge, invisibly to sched.Conflicts.
	DerivedArtifactRisks []DerivedArtifactRisk `json:"derived_artifact_risks"`

	// ParallelWidth reports the achievable and potential parallel widths.
	ParallelWidth WidthReport `json:"parallel_width"`

	// Stats is a summary of special-label counts across the full corpus.
	Stats CorpusStats `json:"stats"`
}

// ItemSummary is a bead with its computed footprint.
type ItemSummary struct {
	ID        string          `json:"id"`
	Title     string          `json:"title"`
	IssueType string          `json:"issue_type"`
	Footprint sched.Footprint `json:"footprint"`
}

// SkipSummary is a bead that will never dispatch as-is, with the reason.
type SkipSummary struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	IssueType string `json:"issue_type"`
	Reason    string `json:"reason"`
}

// ConflictPair is a dependency-unordered pair of open beads whose footprints
// conflict.  SharedTokens names each token where at least one side writes.
// Mode is one of "write-write", "write-read", or "mixed" (some tokens are
// write-write and others are write-read).
type ConflictPair struct {
	A            ItemSummary `json:"a"`
	B            ItemSummary `json:"b"`
	SharedTokens []string    `json:"shared_tokens"`
	Mode         string      `json:"mode"`
}

// DerivedArtifactRisk is a dependency-unordered pair of open beads that both
// touch a checked-in derived artifact yet are write-disjoint, so the scheduler
// may co-dispatch them and their independently regenerated derived file (a
// checksum-over-a-listing) collides at merge. The fix is a shared write token
// or a dependency edge; a merge_reconcilers / merge_prepare entry heals the
// residual (docs/user-guide/merge-reconcilers.md).
type DerivedArtifactRisk struct {
	A       ItemSummary `json:"a"`
	B       ItemSummary `json:"b"`
	Keyword string      `json:"keyword"`
}

// WidthReport carries the current and potential parallel widths.
type WidthReport struct {
	// Current is the maximum number of dispatch-eligible open beads that can
	// run simultaneously under the current footprint labeling, computed by the
	// same greedy coloring algorithm the scheduler uses (no concurrency cap).
	Current int `json:"current"`

	// Potential is the same metric computed after "virtually fixing" every
	// unlabeled bead by assigning each one a unique placeholder token.  It
	// shows how much concurrency is recoverable by adding area:*/fp:* labels.
	Potential int `json:"potential"`
}

// CorpusStats is a high-level count of special-label beads in the corpus.
type CorpusStats struct {
	// RefactorCore is the count of open beads labeled refactor-core (authored
	// on main by the orchestrator; never loop-dispatched).
	RefactorCore int `json:"refactor_core"`

	// NoDispatch is the count of open beads labeled no-dispatch (manually
	// deferred; will not dispatch until the label is removed).
	NoDispatch int `json:"no_dispatch"`
}

// Audit performs a deterministic, read-only conflict analysis of the supplied
// open issue corpus.
//
//   - issues is the full list of open issues (from beads.Adapter.List or
//     beads.Adapter.Ready with limit 0).
//   - deps maps each issue ID to the slice of issue IDs it directly depends on
//     (i.e., must complete first).  A nil or empty map means "no dependency
//     edges known".
//   - cfg is the project's current adapter configuration (used for area_map
//     resolution via sched.FootprintFor).
func Audit(issues []beads.Issue, deps map[string][]string, cfg *project.Config) *AuditReport {
	r := &AuditReport{
		ProjectID: projectID(cfg),
		TotalOpen: len(issues),
	}

	// Compute per-issue footprints.
	fps := make(map[string]sched.Footprint, len(issues))
	for _, iss := range issues {
		fps[iss.ID] = sched.FootprintFor(iss, cfg)
	}

	// Compute the transitive dependency closure over the full corpus so we can
	// test dependency-order for any pair.
	ids := make([]string, 0, len(issues))
	for _, iss := range issues {
		ids = append(ids, iss.ID)
	}
	reach := transitiveClosure(ids, deps)

	// --- 1. Unlabeled beads -------------------------------------------------

	for _, iss := range issues {
		fp := fps[iss.ID]
		if isUnknown(fp) {
			r.Unlabeled = append(r.Unlabeled, ItemSummary{
				ID: iss.ID, Title: iss.Title, IssueType: iss.IssueType, Footprint: fp,
			})
		}
	}

	// --- 2. Non-dispatchable ready beads ------------------------------------
	// Mirror the loop's skip logic statically across all open issues, not just
	// the ready frontier — an issue with the wrong type will never be
	// dispatchable regardless of its dependency state.

	for _, iss := range issues {
		if reason := nonDispatchReason(iss); reason != "" {
			r.NonDispatch = append(r.NonDispatch, SkipSummary{
				ID: iss.ID, Title: iss.Title, IssueType: iss.IssueType, Reason: reason,
			})
		}
	}

	// --- 3. Stats (refactor-core / no-dispatch counts) ----------------------

	for _, iss := range issues {
		if iss.HasLabel("refactor-core") {
			r.Stats.RefactorCore++
		}
		if iss.HasLabel("no-dispatch") {
			r.Stats.NoDispatch++
		}
	}

	// --- 4. Dependency-unordered conflicting pairs --------------------------
	// Compute footprints for all open issues, then check every pair.  Two
	// issues are dependency-ordered when one (transitively) depends on the
	// other — those pairs will never run simultaneously regardless of
	// footprint, so we skip them.

	// Sort for stable output.
	sorted := make([]beads.Issue, len(issues))
	copy(sorted, issues)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	for i := 0; i < len(sorted); i++ {
		for j := i + 1; j < len(sorted); j++ {
			a, b := sorted[i], sorted[j]
			if isDependencyOrdered(a.ID, b.ID, reach) {
				continue
			}
			fpA, fpB := fps[a.ID], fps[b.ID]
			if !sched.Conflicts(fpA, fpB) {
				continue
			}
			shared, mode := conflictingTokens(fpA, fpB)
			r.Conflicts = append(r.Conflicts, ConflictPair{
				A:            ItemSummary{ID: a.ID, Title: a.Title, IssueType: a.IssueType, Footprint: fpA},
				B:            ItemSummary{ID: b.ID, Title: b.Title, IssueType: b.IssueType, Footprint: fpB},
				SharedTokens: shared,
				Mode:         mode,
			})
		}
	}

	// --- 6. Derived-artifact co-footprint risks -----------------------------
	// Beads that each add an input to a directory with a checked-in derived
	// artifact must share a write token: the artifact is a
	// checksum-over-a-listing that collides at merge even when the added inputs
	// (distinct filenames) do not — a collision sched.Conflicts cannot see,
	// because write-disjoint footprints look safe. Flag keyword-matching,
	// dependency-unordered pairs that do NOT already conflict (share a token).

	for i := range sorted {
		kwA, okA := mentionsDerivedArtifact(sorted[i])
		if !okA {
			continue
		}
		for j := i + 1; j < len(sorted); j++ {
			if _, okB := mentionsDerivedArtifact(sorted[j]); !okB {
				continue
			}
			a, b := sorted[i], sorted[j]
			if isDependencyOrdered(a.ID, b.ID, reach) {
				continue // serialized by a dependency edge — safe
			}
			fpA, fpB := fps[a.ID], fps[b.ID]
			if sched.Conflicts(fpA, fpB) {
				continue // already serialized by a shared token — safe
			}
			r.DerivedArtifactRisks = append(r.DerivedArtifactRisks, DerivedArtifactRisk{
				A:       ItemSummary{ID: a.ID, Title: a.Title, IssueType: a.IssueType, Footprint: fpA},
				B:       ItemSummary{ID: b.ID, Title: b.Title, IssueType: b.IssueType, Footprint: fpB},
				Keyword: kwA,
			})
		}
	}

	// --- 5. Parallel width --------------------------------------------------

	eligible := dispatchEligible(issues)
	r.ParallelWidth = WidthReport{
		Current:   greedyWidth(eligible, fps, false),
		Potential: greedyWidth(eligible, fps, true),
	}

	return r
}

// --- helpers ---------------------------------------------------------------

// projectID extracts the project id from cfg, falling back to "-".
func projectID(cfg *project.Config) string {
	if cfg != nil && cfg.ProjectID != "" {
		return cfg.ProjectID
	}
	return "-"
}

// nonDispatchReason returns the static skip reason for an issue the loop would
// never dispatch as-is, or "" when it has no structural dispatch problem. It
// defers to sched.Eligible — the engine's own single source of dispatch
// eligibility — so the audit can never report a stale verdict when those rules
// change. activeIDs is nil here: "already active" is a live-run condition, not a
// static corpus property, so it never fires in the audit.
func nonDispatchReason(iss beads.Issue) string {
	if ok, reason := sched.Eligible(iss, nil); !ok {
		return reason
	}
	return ""
}

// isUnknown reports whether fp is the catch-all unknown footprint (exactly one
// write token equal to sched.TokenUnknown, no reads).
func isUnknown(fp sched.Footprint) bool {
	return len(fp.Reads) == 0 && len(fp.Writes) == 1 && fp.Writes[0] == sched.TokenUnknown
}

// derivedArtifactKeywords name checked-in derived artifacts whose beads must
// share a write footprint — a checksum-over-a-listing (a migrations lockfile, a
// secrets baseline, a generated index) collides at merge even when its inputs
// (distinct filenames) do not. Matched case-insensitively against a bead's
// title + description.
var derivedArtifactKeywords = []string{
	"migration", "atlas.sum", "atlas migrate", ".secrets.baseline",
	"secrets baseline", "lockfile", "generated index",
}

// mentionsDerivedArtifact reports whether a bead's title or description names a
// derived artifact, returning the matched keyword.
func mentionsDerivedArtifact(iss beads.Issue) (string, bool) {
	hay := strings.ToLower(iss.Title + "\n" + iss.Description)
	for _, kw := range derivedArtifactKeywords {
		if strings.Contains(hay, kw) {
			return kw, true
		}
	}
	return "", false
}

// dispatchEligible filters issues to those the loop can potentially dispatch
// (correct type, no no-dispatch/refactor-core/gt:* label — dependency state
// and active-slots checks are not applied here since we want the full corpus).
func dispatchEligible(issues []beads.Issue) []beads.Issue {
	out := make([]beads.Issue, 0, len(issues))
	for _, iss := range issues {
		if nonDispatchReason(iss) != "" {
			continue
		}
		out = append(out, iss)
	}
	return out
}

// greedyWidth returns the largest conflict-free set of issues, computed by the
// same greedy-coloring algorithm the scheduler uses (no cap).
//
// When virtualizeUnknown is true, each domain:unknown bead is assigned a
// unique placeholder token so unknowns do not conflict with each other — this
// approximates the potential width if all unlabeled beads were re-labeled.
func greedyWidth(eligible []beads.Issue, fps map[string]sched.Footprint, virtualizeUnknown bool) int {
	// Stable priority sort (P0 first), matching scheduler order.
	sorted := make([]beads.Issue, len(eligible))
	copy(sorted, eligible)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Priority < sorted[j].Priority
	})

	selected := make([]sched.Footprint, 0, len(sorted))
	for idx, iss := range sorted {
		fp := fps[iss.ID]
		if virtualizeUnknown && isUnknown(fp) {
			fp = sched.Footprint{Writes: []string{fmt.Sprintf("domain:unknown/virt/%d", idx)}}
		}
		conflict := false
		for _, sel := range selected {
			if sched.Conflicts(fp, sel) {
				conflict = true
				break
			}
		}
		if !conflict {
			selected = append(selected, fp)
		}
	}
	return len(selected)
}

// conflictingTokens returns the set of tokens that make a and b conflict (i.e.
// tokens shared by both footprints where at least one side writes), plus a mode
// label: "write-write", "write-read", or "mixed".
func conflictingTokens(a, b sched.Footprint) (shared []string, mode string) {
	bAll := tokenSet(b.Reads, b.Writes)
	aAll := tokenSet(a.Reads, a.Writes)
	bWrites := tokenSet(b.Writes)
	aWrites := tokenSet(a.Writes)

	seen := map[string]bool{}
	hasWW, hasWR := false, false

	// Tokens where A writes and B has (read or write).
	for _, t := range a.Writes {
		if bAll[t] && !seen[t] {
			seen[t] = true
			shared = append(shared, t)
			if bWrites[t] {
				hasWW = true
			} else {
				hasWR = true
			}
		}
	}
	// Tokens where B writes and A has (read or write) — skip already-seen.
	for _, t := range b.Writes {
		if aAll[t] && !seen[t] {
			seen[t] = true
			shared = append(shared, t)
			if aWrites[t] {
				hasWW = true
			} else {
				hasWR = true
			}
		}
	}

	sort.Strings(shared)
	switch {
	case hasWW && hasWR:
		mode = "mixed"
	case hasWW:
		mode = "write-write"
	default:
		mode = "write-read"
	}
	return shared, mode
}

// tokenSet unions one or more token slices into a membership set.
func tokenSet(sets ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, s := range sets {
		for _, t := range s {
			m[t] = true
		}
	}
	return m
}

// transitiveClosure computes the reachability relation for all ids using the
// dependency map (deps[id] = slice of ids it depends on). Returns a map where
// reach[A][B] == true means "A transitively depends on B" (B must close before
// A can start). The start node itself is not included in its own reachable set.
func transitiveClosure(ids []string, deps map[string][]string) map[string]map[string]bool {
	reach := make(map[string]map[string]bool, len(ids))
	for _, id := range ids {
		reach[id] = bfsReach(id, deps)
	}
	return reach
}

// bfsReach returns the set of all nodes reachable from start by following
// dependency edges (i.e., all transitive dependencies of start). start itself
// is not in the returned set.
func bfsReach(start string, deps map[string][]string) map[string]bool {
	visited := map[string]bool{}
	queue := []string{start}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, dep := range deps[cur] {
			if !visited[dep] {
				visited[dep] = true
				queue = append(queue, dep)
			}
		}
	}
	return visited
}

// isDependencyOrdered reports whether a and b are dependency-ordered: either a
// transitively depends on b, or b transitively depends on a.
func isDependencyOrdered(a, b string, reach map[string]map[string]bool) bool {
	return reach[a][b] || reach[b][a]
}
