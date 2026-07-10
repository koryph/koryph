// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/sched"
)

// makeIssue constructs a minimal Issue for use in tests.
func makeIssue(id, title, issueType string, labels ...string) beads.Issue {
	return beads.Issue{
		ID:        id,
		Title:     title,
		IssueType: issueType,
		Status:    "open",
		Labels:    labels,
	}
}

// makeChild returns an issue with the given parentID.
func makeChild(id, title, parentID string) beads.Issue {
	return beads.Issue{
		ID:        id,
		Title:     title,
		IssueType: "task",
		Status:    "open",
		ParentID:  parentID,
	}
}

func TestComputeQueue_Empty(t *testing.T) {
	snap := computeQueue(queueInput{now: time.Now()})
	if snap.NodeCount != 0 {
		t.Errorf("expected empty snapshot, got %d nodes", snap.NodeCount)
	}
}

func TestComputeQueue_StandaloneTask(t *testing.T) {
	now := time.Now()
	task := makeIssue("t1", "Do the thing", "task")
	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{task},
		readyIDs:  map[string]bool{"t1": true},
		now:       now,
	})
	if snap.NodeCount != 1 {
		t.Fatalf("expected 1 node, got %d", snap.NodeCount)
	}
	if len(snap.Roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(snap.Roots))
	}
	if snap.Roots[0].State != QueueStateReady {
		t.Errorf("expected ready, got %s", snap.Roots[0].State)
	}
}

func TestComputeQueue_EpicWithChildren(t *testing.T) {
	now := time.Now()
	epic := makeIssue("e1", "Epic", "epic")
	child1 := makeChild("c1", "Child 1", "e1")
	child2 := makeChild("c2", "Child 2", "e1")
	child2.Labels = []string{"no-dispatch"}

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{epic, child1, child2},
		readyIDs:  map[string]bool{"c1": true},
		now:       now,
	})

	if snap.NodeCount != 3 {
		t.Fatalf("expected 3 nodes, got %d", snap.NodeCount)
	}
	// Epic is the single root.
	if len(snap.Roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(snap.Roots))
	}
	root := snap.Roots[0]
	if root.State != QueueStateContainer {
		t.Errorf("epic should be container, got %s", root.State)
	}
	if len(root.Children) != 2 {
		t.Fatalf("expected 2 children, got %d", len(root.Children))
	}
	// Find child states.
	states := map[string]QueueNodeState{}
	for _, c := range root.Children {
		states[c.Issue.ID] = c.State
	}
	if states["c1"] != QueueStateReady {
		t.Errorf("c1: expected ready, got %s", states["c1"])
	}
	if states["c2"] != QueueStateHuman {
		t.Errorf("c2: expected human, got %s", states["c2"])
	}
}

// TestComputeQueue_ClosedParentReconstructed verifies that open children of a
// parent epic that is ABSENT from the open-issue set (closed → dropped by
// `bd list`) are regrouped under a synthesized container rather than orphaned
// to the top level as a flat list. This is the "no longer groups the hierarchy"
// regression: over a multi-day run epics close while stragglers remain open.
func TestComputeQueue_ClosedParentReconstructed(t *testing.T) {
	now := time.Now()
	// e1 is NOT in allIssues (closed). Its two children remain open.
	c1 := makeChild("e1.1", "Straggler 1", "e1")
	c2 := makeChild("e1.2", "Straggler 2", "e1")
	standalone := makeIssue("t9", "Standalone", "task")

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{c1, c2, standalone},
		readyIDs:  map[string]bool{"e1.1": true, "t9": true},
		closedParents: map[string]beads.Issue{
			"e1": {ID: "e1", Title: "Closed Epic", IssueType: "epic", Status: "closed"},
		},
		now: now,
	})

	// Roots: the synthesized e1 container + the standalone task (2), NOT the two
	// children floating at top level (which would be 3 flat roots).
	if len(snap.Roots) != 2 {
		t.Fatalf("expected 2 roots (container + standalone), got %d: %+v", len(snap.Roots), rootIDs(snap.Roots))
	}
	// Node count includes the synthesized container: e1 + e1.1 + e1.2 + t9 = 4.
	if snap.NodeCount != 4 {
		t.Errorf("expected 4 nodes (incl. synthesized parent), got %d", snap.NodeCount)
	}
	// Locate the synthesized container.
	var container *QueueNode
	for i := range snap.Roots {
		if snap.Roots[i].Issue.ID == "e1" {
			container = &snap.Roots[i]
		}
	}
	if container == nil {
		t.Fatalf("synthesized container e1 not found among roots %v", rootIDs(snap.Roots))
	}
	if container.State != QueueStateContainer {
		t.Errorf("container e1: expected container state, got %s", container.State)
	}
	if container.Issue.Title != "Closed Epic" {
		t.Errorf("container e1: expected title from closedParents, got %q", container.Issue.Title)
	}
	if len(container.Children) != 2 {
		t.Fatalf("container e1: expected 2 children nested, got %d", len(container.Children))
	}
}

// TestComputeQueue_ClosedParentNoMetadata verifies the ID-only fallback when a
// closed parent's metadata could not be fetched (bd.Show failed): the children
// still nest under a bare-ID container rather than going flat.
func TestComputeQueue_ClosedParentNoMetadata(t *testing.T) {
	now := time.Now()
	c1 := makeChild("e2.1", "Child", "e2")
	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{c1},
		readyIDs:  map[string]bool{"e2.1": true},
		// closedParents intentionally nil — no metadata available.
		now: now,
	})
	if len(snap.Roots) != 1 {
		t.Fatalf("expected 1 synthesized root, got %d", len(snap.Roots))
	}
	root := snap.Roots[0]
	if root.Issue.ID != "e2" || root.State != QueueStateContainer {
		t.Errorf("expected bare-ID container e2, got id=%q state=%s", root.Issue.ID, root.State)
	}
	if len(root.Children) != 1 || root.Children[0].Issue.ID != "e2.1" {
		t.Errorf("expected child e2.1 nested under e2, got %+v", root.Children)
	}
}

// rootIDs is a test helper listing root node IDs for failure messages.
func rootIDs(roots []QueueNode) []string {
	ids := make([]string, len(roots))
	for i, r := range roots {
		ids[i] = r.Issue.ID
	}
	return ids
}

func TestComputeQueue_DepBlocked(t *testing.T) {
	now := time.Now()
	// t2 depends on t1 (t1 is not closed yet, so t2 is dep-blocked).
	t1 := makeIssue("t1", "Task 1", "task")
	t2 := makeIssue("t2", "Task 2 (blocked by t1)", "task")
	// t2 is NOT in readyIDs; the dep graph shows t2 depends on t1.
	graph := GraphSnapshot{
		Deps: map[string][]string{
			"t2": {"t1"},
		},
		NodeCount: 2,
	}

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{t1, t2},
		readyIDs:  map[string]bool{"t1": true},
		graph:     graph,
		now:       now,
	})

	states := map[string]QueueNodeState{}
	reasons := map[string]string{}
	for _, n := range snap.Roots {
		states[n.Issue.ID] = n.State
		reasons[n.Issue.ID] = n.Reason
	}
	if states["t1"] != QueueStateReady {
		t.Errorf("t1: expected ready, got %s", states["t1"])
	}
	if states["t2"] != QueueStateDepBlocked {
		t.Errorf("t2: expected dep-blocked, got %s", states["t2"])
	}
	if reasons["t2"] == "" {
		t.Errorf("t2 dep-blocked reason should be non-empty")
	}
}

func TestComputeQueue_Running(t *testing.T) {
	now := time.Now()
	t1 := makeIssue("t1", "Running task", "task")

	snap := computeQueue(queueInput{
		allIssues:  []beads.Issue{t1},
		readyIDs:   map[string]bool{"t1": true},
		runningIDs: map[string]bool{"t1": true},
		now:        now,
	})

	if len(snap.Roots) != 1 || snap.Roots[0].State != QueueStateRunning {
		t.Errorf("expected running state, got %v", snap.Roots)
	}
}

func TestComputeQueue_LabelStates(t *testing.T) {
	now := time.Now()
	parked := makeIssue("p1", "Parked", "task", "parked")
	deferred := makeIssue("d1", "Deferred", "task", "deferred-until:2026-08-01")
	human := makeIssue("h1", "Human-only", "task", "human-only")

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{parked, deferred, human},
		readyIDs:  map[string]bool{},
		now:       now,
	})

	states := map[string]QueueNodeState{}
	reasons := map[string]string{}
	for _, n := range snap.Roots {
		states[n.Issue.ID] = n.State
		reasons[n.Issue.ID] = n.Reason
	}

	if states["p1"] != QueueStateParked {
		t.Errorf("p1: expected parked, got %s", states["p1"])
	}
	if states["d1"] != QueueStateDeferredUntil {
		t.Errorf("d1: expected deferred-until, got %s", states["d1"])
	}
	if reasons["d1"] != "2026-08-01" {
		t.Errorf("d1 reason: expected 2026-08-01, got %q", reasons["d1"])
	}
	if states["h1"] != QueueStateHuman {
		t.Errorf("h1: expected human, got %s", states["h1"])
	}
}

func TestComputeQueue_NodeCount(t *testing.T) {
	now := time.Now()
	epic := makeIssue("e1", "Epic", "epic")
	c1 := makeChild("c1", "Child 1", "e1")
	c2 := makeChild("c2", "Child 2", "e1")
	c3 := makeChild("c3", "Child 3", "e1")
	standalone := makeIssue("s1", "Standalone", "task")

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{epic, c1, c2, c3, standalone},
		now:       now,
	})

	if snap.NodeCount != 5 {
		t.Errorf("expected 5 nodes, got %d", snap.NodeCount)
	}
	if len(snap.Roots) != 2 {
		t.Fatalf("expected 2 roots (epic + standalone), got %d", len(snap.Roots))
	}
}

// --- resource-deferred (koryph-4ql.10) --------------------------------------

// TestFormatParseResourceDeferralReason pins the exact wording
// formatResourceDeferralReason/parseResourceDeferralReason share with
// sched/wave.go's resourceBlocker and the engine's classifyAdmit (design
// docs/designs/2026-07-resource-governor.md L3/L4): "resource <kind> at
// capacity (held by <id>)". Feeds a raw reason string (including a
// cross-project "project/bead" holder) and asserts the parsed kind+holder.
func TestFormatParseResourceDeferralReason(t *testing.T) {
	cases := []struct {
		name       string
		kind       string
		holder     string
		wantReason string
	}{
		{"same-project holder", "kind-cluster", "koryph-abc", "resource kind-cluster at capacity (held by koryph-abc)"},
		{"cross-project holder", "docker", "otherproj/koryph-xyz", "resource docker at capacity (held by otherproj/koryph-xyz)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatResourceDeferralReason(tc.kind, tc.holder)
			if got != tc.wantReason {
				t.Fatalf("formatResourceDeferralReason(%q, %q) = %q, want %q", tc.kind, tc.holder, got, tc.wantReason)
			}
			kind, holder, ok := parseResourceDeferralReason(got)
			if !ok || kind != tc.kind || holder != tc.holder {
				t.Errorf("parseResourceDeferralReason(%q) = (%q, %q, %v), want (%q, %q, true)",
					got, kind, holder, ok, tc.kind, tc.holder)
			}
		})
	}
}

// TestParseResourceDeferralReason_Malformed feeds strings that do not match
// the expected shape and asserts ok == false rather than a garbage split.
func TestParseResourceDeferralReason_Malformed(t *testing.T) {
	cases := []string{
		"",
		"footprint conflict with t1",
		"resource kind-cluster at capacity", // missing "(held by ...)"
		"resource  at capacity (held by koryph-abc)",     // empty kind
		"resource kind-cluster at capacity (held by )",   // empty holder
		"resource kind-cluster at capacity (held by abc", // missing trailing paren
	}
	for _, reason := range cases {
		if kind, holder, ok := parseResourceDeferralReason(reason); ok {
			t.Errorf("parseResourceDeferralReason(%q) = (%q, %q, true), want ok=false", reason, kind, holder)
		}
	}
}

// TestComputeQueue_ResourceDeferred is the state+kind+holder assertion: a
// ready-frontier bead declaring res:kind-cluster, with the live resource
// ledger showing kind-cluster at capacity 1/1 held by another bead, must
// classify as resource-deferred and carry the parsed kind/holder on the node
// (koryph-4ql.10).
func TestComputeQueue_ResourceDeferred(t *testing.T) {
	now := time.Now()
	t2 := makeIssue("t2", "Needs kind-cluster", "task", "res:kind-cluster")

	resources := []ResourceSnapshot{
		{
			Kind:     "kind-cluster",
			Capacity: 1,
			Holders: []ResourceHolderSnapshot{
				{Project: "proj", Bead: "t1", MemReserveMB: 6144},
			},
		},
	}

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{t2},
		readyIDs:  map[string]bool{"t2": true},
		resources: resources,
		projectID: "proj",
		now:       now,
	})

	if len(snap.Roots) != 1 {
		t.Fatalf("expected 1 root, got %d", len(snap.Roots))
	}
	node := snap.Roots[0]
	if node.State != QueueStateResourceDeferred {
		t.Fatalf("t2: expected resource-deferred, got %s", node.State)
	}
	if node.ResourceKind != "kind-cluster" {
		t.Errorf("ResourceKind = %q, want kind-cluster", node.ResourceKind)
	}
	if node.ResourceHolder != "t1" {
		t.Errorf("ResourceHolder = %q, want t1 (same-project holder, no prefix)", node.ResourceHolder)
	}
	wantReason := "resource kind-cluster at capacity (held by t1)"
	if node.Reason != wantReason {
		t.Errorf("Reason = %q, want %q", node.Reason, wantReason)
	}
}

// TestComputeQueue_ResourceDeferredCrossProject asserts the "project/bead"
// holder formatting when the holder belongs to a different project than the
// queue being rendered.
func TestComputeQueue_ResourceDeferredCrossProject(t *testing.T) {
	now := time.Now()
	t2 := makeIssue("t2", "Needs docker", "task", "res:docker")

	resources := []ResourceSnapshot{
		{
			Kind:     "docker",
			Capacity: 1,
			Holders: []ResourceHolderSnapshot{
				{Project: "otherproj", Bead: "koryph-xyz"},
			},
		},
	}

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{t2},
		readyIDs:  map[string]bool{"t2": true},
		resources: resources,
		projectID: "proj",
		now:       now,
	})

	node := snap.Roots[0]
	if node.State != QueueStateResourceDeferred {
		t.Fatalf("expected resource-deferred, got %s", node.State)
	}
	if node.ResourceHolder != "otherproj/koryph-xyz" {
		t.Errorf("ResourceHolder = %q, want otherproj/koryph-xyz", node.ResourceHolder)
	}
}

// TestComputeQueue_ResourceUnderCapacityStaysReady: a declared kind with
// holders below capacity does not defer.
func TestComputeQueue_ResourceUnderCapacityStaysReady(t *testing.T) {
	now := time.Now()
	t2 := makeIssue("t2", "Needs kind-cluster", "task", "res:kind-cluster")

	resources := []ResourceSnapshot{
		{Kind: "kind-cluster", Capacity: 2, Holders: []ResourceHolderSnapshot{{Project: "proj", Bead: "t1"}}},
	}

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{t2},
		readyIDs:  map[string]bool{"t2": true},
		resources: resources,
		projectID: "proj",
		now:       now,
	})

	if snap.Roots[0].State != QueueStateReady {
		t.Errorf("expected ready (capacity 2, 1 holder), got %s", snap.Roots[0].State)
	}
}

// TestComputeQueue_ResourceDeferredFailsOpenWithoutLedger: an empty/absent
// resources ledger (old snapshot, or governor unavailable) must NOT
// spuriously classify a res:*-declaring bead as deferred (I6 fail-open).
func TestComputeQueue_ResourceDeferredFailsOpenWithoutLedger(t *testing.T) {
	now := time.Now()
	t1 := makeIssue("t1", "Needs kind-cluster", "task", "res:kind-cluster")

	snap := computeQueue(queueInput{
		allIssues: []beads.Issue{t1},
		readyIDs:  map[string]bool{"t1": true},
		// resources intentionally nil.
		now: now,
	})

	if snap.Roots[0].State != QueueStateReady {
		t.Errorf("expected ready (no resources ledger), got %s", snap.Roots[0].State)
	}
}

// TestComputeQueue_ResourceDeferredCheckedBeforeFootprint: a bead that is
// BOTH at resource capacity and would footprint-conflict must report
// resource-deferred (design L4 ordering: resources checked before
// footprints), matching sched.BuildWave.
func TestComputeQueue_ResourceDeferredCheckedBeforeFootprint(t *testing.T) {
	now := time.Now()
	t1 := makeIssue("t1", "Running", "task") // no fp labels → domain:unknown write
	t2 := makeIssue("t2", "Deferred", "task", "res:kind-cluster")

	runningFPs := map[string]sched.Footprint{
		"t1": {Writes: []string{"domain:unknown"}},
	}
	resources := []ResourceSnapshot{
		{Kind: "kind-cluster", Capacity: 1, Holders: []ResourceHolderSnapshot{{Project: "proj", Bead: "t1"}}},
	}

	snap := computeQueue(queueInput{
		allIssues:  []beads.Issue{t1, t2},
		readyIDs:   map[string]bool{"t1": true, "t2": true},
		runningIDs: map[string]bool{"t1": true},
		runningFPs: runningFPs,
		resources:  resources,
		projectID:  "proj",
		now:        now,
	})

	states := map[string]QueueNodeState{}
	for _, n := range snap.Roots {
		states[n.Issue.ID] = n.State
	}
	if states["t2"] != QueueStateResourceDeferred {
		t.Errorf("t2: expected resource-deferred (resource checked before footprint), got %s", states["t2"])
	}
}

func TestComputeQueue_FootprintDeferred(t *testing.T) {
	now := time.Now()
	// Two tasks in the ready frontier; t1 is running with domain:unknown footprint.
	// t2 also has domain:unknown footprint → footprint conflict.
	t1 := makeIssue("t1", "Running", "task")  // no fp labels → domain:unknown write
	t2 := makeIssue("t2", "Deferred", "task") // same → conflict with t1

	runningFPs := map[string]sched.Footprint{
		"t1": {Writes: []string{"domain:unknown"}},
	}

	snap := computeQueue(queueInput{
		allIssues:  []beads.Issue{t1, t2},
		readyIDs:   map[string]bool{"t1": true, "t2": true},
		runningIDs: map[string]bool{"t1": true},
		runningFPs: runningFPs,
		now:        now,
	})

	states := map[string]QueueNodeState{}
	for _, n := range snap.Roots {
		states[n.Issue.ID] = n.State
	}
	if states["t1"] != QueueStateRunning {
		t.Errorf("t1: expected running, got %s", states["t1"])
	}
	if states["t2"] != QueueStateFootprintDeferred {
		t.Errorf("t2: expected footprint-deferred, got %s", states["t2"])
	}
}
