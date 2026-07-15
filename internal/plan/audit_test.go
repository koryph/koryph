// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package plan_test

import (
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/plan"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/sched"
)

// makeIssue builds a minimal beads.Issue for tests.
func makeIssue(id, issueType string, labels ...string) beads.Issue {
	return beads.Issue{
		ID:        id,
		Title:     "Test: " + id,
		IssueType: issueType,
		Status:    "open",
		Labels:    labels,
	}
}

// cfg builds a project config with the given area_map.
func cfg(areaMap map[string][]string) *project.Config {
	return &project.Config{
		ProjectID:   "test",
		WorkSource:  "bd",
		AreaMap:     areaMap,
		Gate:        []string{"true"},
		MergePolicy: project.PolicyManual,
	}
}

// TestAudit_Unlabeled verifies that issues without any fp: or area: labels land
// in the Unlabeled slice with a domain:unknown footprint.
func TestAudit_Unlabeled(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("a", "task"),              // no labels → unknown
		makeIssue("b", "task", "area:cli"),  // area: but no area_map entry → unknown
		makeIssue("c", "task", "fp:go:api"), // explicit fp: → not unknown
	}
	r := plan.Audit(issues, nil, cfg(nil))

	if got, want := len(r.Unlabeled), 2; got != want {
		t.Fatalf("Unlabeled: got %d, want %d", got, want)
	}
	ids := map[string]bool{}
	for _, it := range r.Unlabeled {
		ids[it.ID] = true
		if it.Footprint.Writes[0] != sched.TokenUnknown {
			t.Errorf("item %s: expected domain:unknown write, got %v", it.ID, it.Footprint)
		}
	}
	if !ids["a"] || !ids["b"] {
		t.Errorf("expected a and b in Unlabeled, got %v", ids)
	}
}

// TestAudit_NonDispatch verifies structural skip reasons.
func TestAudit_NonDispatch(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("epic1", "epic"),
		makeIssue("feat1", "feature"),
		makeIssue("dec1", "decision"),
		makeIssue("mr1", "merge-request"),
		makeIssue("gate1", "task", "gt:merge-ready"),
		makeIssue("nd1", "task", "no-dispatch"),
		makeIssue("rc1", "task", "refactor-core"),
		makeIssue("ok1", "task", "fp:cli"), // dispatch-eligible
	}
	r := plan.Audit(issues, nil, cfg(nil))

	if got, want := len(r.NonDispatch), 7; got != want {
		t.Errorf("NonDispatch: got %d, want %d", got, want)
		for _, nd := range r.NonDispatch {
			t.Logf("  %s: %s", nd.ID, nd.Reason)
		}
	}
}

// TestAudit_Conflicts verifies that unordered conflicting pairs are detected.
func TestAudit_Conflicts(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("a", "task", "fp:engine"),
		makeIssue("b", "task", "fp:engine"), // conflicts with a (both write engine)
		makeIssue("c", "task", "fp:cli"),    // no conflict with a or b
	}
	// No dependency ordering between any pair.
	r := plan.Audit(issues, nil, cfg(nil))

	if got, want := len(r.Conflicts), 1; got != want {
		t.Fatalf("Conflicts: got %d, want %d", got, want)
	}
	cp := r.Conflicts[0]
	if (cp.A.ID != "a" || cp.B.ID != "b") && (cp.A.ID != "b" || cp.B.ID != "a") {
		t.Errorf("expected conflict between a and b, got %s×%s", cp.A.ID, cp.B.ID)
	}
	if cp.Mode != "write-write" {
		t.Errorf("expected mode write-write, got %q", cp.Mode)
	}
	if len(cp.SharedTokens) != 1 || cp.SharedTokens[0] != "engine" {
		t.Errorf("expected shared token [engine], got %v", cp.SharedTokens)
	}
}

// TestAudit_DependencyOrderedNotConflict verifies that a pair where A depends
// on B is not reported as a conflict, even if their footprints would conflict.
func TestAudit_DependencyOrderedNotConflict(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("a", "task", "fp:engine"),
		makeIssue("b", "task", "fp:engine"), // would conflict with a, but a depends on b
	}
	// a depends on b: a→b (a is blocked until b closes).
	deps := map[string][]string{"a": {"b"}}
	r := plan.Audit(issues, deps, cfg(nil))

	if got := len(r.Conflicts); got != 0 {
		t.Errorf("expected 0 conflicts (dependency-ordered pair), got %d", got)
	}
}

// TestAudit_DerivedArtifact verifies the co-footprint risk heuristic: two beads
// that both touch a derived artifact but are write-disjoint and unordered are
// flagged (they will collide at merge invisibly to sched.Conflicts), while a
// shared write token or a dependency edge clears the risk.
func TestAudit_DerivedArtifact(t *testing.T) {
	desc := func(id, kw string, labels ...string) beads.Issue {
		return beads.Issue{
			ID: id, Title: "Test: " + id, IssueType: "task", Status: "open",
			Description: "adds a " + kw + " to the schema", Labels: labels,
		}
	}

	t.Run("write-disjoint derived-artifact beads are flagged", func(t *testing.T) {
		issues := []beads.Issue{
			desc("a", "migration", "fp:engine"),
			desc("b", "migration", "fp:cli"), // write-disjoint from a
			makeIssue("c", "task", "fp:api"), // no derived-artifact mention
		}
		r := plan.Audit(issues, nil, cfg(nil))
		if got := len(r.DerivedArtifactRisks); got != 1 {
			t.Fatalf("DerivedArtifactRisks: got %d, want 1", got)
		}
		if kw := r.DerivedArtifactRisks[0].Keyword; kw != "migration" {
			t.Errorf("keyword = %q, want migration", kw)
		}
	})

	t.Run("shared write token clears the risk", func(t *testing.T) {
		issues := []beads.Issue{
			desc("a", "migration", "fp:migrations"),
			desc("b", "migration", "fp:migrations"), // shares the token -> serialized
		}
		r := plan.Audit(issues, nil, cfg(nil))
		if got := len(r.DerivedArtifactRisks); got != 0 {
			t.Errorf("shared-token beads flagged: got %d risks, want 0", got)
		}
	})

	t.Run("dependency edge clears the risk", func(t *testing.T) {
		issues := []beads.Issue{
			desc("a", "migration", "fp:engine"),
			desc("b", "migration", "fp:cli"),
		}
		r := plan.Audit(issues, map[string][]string{"a": {"b"}}, cfg(nil))
		if got := len(r.DerivedArtifactRisks); got != 0 {
			t.Errorf("dependency-ordered beads flagged: got %d risks, want 0", got)
		}
	})
}

// TestAudit_TransitiveDependency verifies transitive ordering is respected.
func TestAudit_TransitiveDependency(t *testing.T) {
	// a → b → c (a depends on b, b depends on c); all share the same token.
	issues := []beads.Issue{
		makeIssue("a", "task", "fp:engine"),
		makeIssue("b", "task", "fp:engine"),
		makeIssue("c", "task", "fp:engine"),
	}
	deps := map[string][]string{
		"a": {"b"},
		"b": {"c"},
	}
	r := plan.Audit(issues, deps, cfg(nil))
	// All pairs are transitively dependency-ordered; no conflicts expected.
	if got := len(r.Conflicts); got != 0 {
		t.Errorf("expected 0 conflicts (all pairs transitively ordered), got %d", got)
		for _, cp := range r.Conflicts {
			t.Logf("  %s × %s (%s)", cp.A.ID, cp.B.ID, cp.Mode)
		}
	}
}

// TestAudit_WriteReadMode verifies the "write-read" conflict mode is reported
// when one bead writes a token and the other reads it.
func TestAudit_WriteReadMode(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("writer", "task", "fp:engine"),
		makeIssue("reader", "task", "fp:read:engine"),
	}
	r := plan.Audit(issues, nil, cfg(nil))

	if got := len(r.Conflicts); got != 1 {
		t.Fatalf("expected 1 conflict, got %d", got)
	}
	if got, want := r.Conflicts[0].Mode, "write-read"; got != want {
		t.Errorf("mode: got %q, want %q", got, want)
	}
}

// TestAudit_AreaMap verifies that area: labels resolve through the area_map.
func TestAudit_AreaMap(t *testing.T) {
	am := map[string][]string{"cli": {"go:cli"}}
	issues := []beads.Issue{
		makeIssue("a", "task", "area:cli"),
		makeIssue("b", "task", "area:cli"), // same resolved token → conflict
	}
	r := plan.Audit(issues, nil, cfg(am))

	if got := len(r.Conflicts); got != 1 {
		t.Fatalf("expected 1 conflict from area_map, got %d", got)
	}
	if r.Conflicts[0].SharedTokens[0] != "go:cli" {
		t.Errorf("expected shared token go:cli, got %v", r.Conflicts[0].SharedTokens)
	}
}

// TestAudit_ParallelWidth verifies that width metrics are sensible.
func TestAudit_ParallelWidth(t *testing.T) {
	// Three dispatch-eligible beads, all with domain:unknown (no labels).
	// Current width should be 1 (unknowns all conflict).
	// Potential width should be 3 (each virtual-labeled, no conflicts).
	issues := []beads.Issue{
		makeIssue("a", "task"),
		makeIssue("b", "task"),
		makeIssue("c", "task"),
	}
	r := plan.Audit(issues, nil, cfg(nil))

	if r.ParallelWidth.Current != 1 {
		t.Errorf("Current width: got %d, want 1", r.ParallelWidth.Current)
	}
	if r.ParallelWidth.Potential != 3 {
		t.Errorf("Potential width: got %d, want 3", r.ParallelWidth.Potential)
	}
}

// TestAudit_Stats verifies refactor-core and no-dispatch counts.
func TestAudit_Stats(t *testing.T) {
	issues := []beads.Issue{
		makeIssue("a", "task", "refactor-core"),
		makeIssue("b", "task", "refactor-core"),
		makeIssue("c", "task", "no-dispatch"),
		makeIssue("d", "task", "fp:ok"),
	}
	r := plan.Audit(issues, nil, cfg(nil))

	if r.Stats.RefactorCore != 2 {
		t.Errorf("RefactorCore: got %d, want 2", r.Stats.RefactorCore)
	}
	if r.Stats.NoDispatch != 1 {
		t.Errorf("NoDispatch: got %d, want 1", r.Stats.NoDispatch)
	}
}

// TestAudit_Empty verifies that an empty corpus produces an empty report.
func TestAudit_Empty(t *testing.T) {
	r := plan.Audit(nil, nil, cfg(nil))
	if r.TotalOpen != 0 {
		t.Errorf("TotalOpen: got %d, want 0", r.TotalOpen)
	}
	if len(r.Unlabeled) != 0 || len(r.Conflicts) != 0 || len(r.NonDispatch) != 0 {
		t.Errorf("expected empty slices for empty corpus")
	}
	if r.ParallelWidth.Current != 0 || r.ParallelWidth.Potential != 0 {
		t.Errorf("expected zero width for empty corpus")
	}
}
