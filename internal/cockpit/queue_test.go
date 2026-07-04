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
