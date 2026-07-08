// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/sched"
)

// QueueNodeState is the computed dispatch state of a bead in the project queue.
type QueueNodeState string

const (
	// QueueStateRunning means a slot is actively running this bead.
	QueueStateRunning QueueNodeState = "running"
	// QueueStateReady means dep-unblocked, eligible, no footprint conflict.
	QueueStateReady QueueNodeState = "ready"
	// QueueStateDepBlocked means the bead has one or more open dependencies.
	QueueStateDepBlocked QueueNodeState = "dep-blocked"
	// QueueStateFootprintDeferred means the bead is in the ready frontier but
	// its footprint conflicts with a currently-running bead's footprint.
	QueueStateFootprintDeferred QueueNodeState = "footprint-deferred"
	// QueueStateHuman means the bead carries a no-dispatch or human-only label.
	QueueStateHuman QueueNodeState = "human"
	// QueueStateDeferredUntil means the bead carries a deferred-until:<date> label.
	QueueStateDeferredUntil QueueNodeState = "deferred-until"
	// QueueStateParked means the bead is parked (label or status).
	QueueStateParked QueueNodeState = "parked"
	// QueueStateContainer means the bead is an epic or container that is not
	// directly dispatchable (it has open children or a non-dispatch issue type).
	QueueStateContainer QueueNodeState = "container"
)

// QueueNode is one row in the hierarchical queue view.
type QueueNode struct {
	Issue beads.Issue
	State QueueNodeState
	// Reason is the human-readable deferral/block annotation. Non-empty for
	// dep-blocked (lists the open blocker IDs), footprint-deferred (names
	// the in-flight conflicting bead), and deferred-until (the date string).
	Reason string
	// Children are this node's direct open children (empty for leaf nodes).
	Children []QueueNode
}

// QueueSnapshot is a point-in-time view of the project work queue.
// The zero value is safe; consumers check NodeCount == 0 before rendering.
type QueueSnapshot struct {
	// Roots are the top-level nodes: epics and standalone tasks whose parent
	// is absent from the current open-issue set.
	Roots []QueueNode
	// NodeCount is the total number of nodes across the entire tree.
	NodeCount int
	// ComputedAt is when this snapshot was assembled.
	ComputedAt time.Time
}

// queueInput carries all data needed by computeQueue.
type queueInput struct {
	// allIssues is the full open-issue set from `bd list --json`.
	allIssues []beads.Issue
	// readyIDs is the set of issue IDs in the ready frontier (`bd ready`).
	readyIDs map[string]bool
	// runningIDs is the set of bead IDs currently active in ledger slots.
	runningIDs map[string]bool
	// runningFPs maps each running bead ID to its computed footprint, used
	// for footprint-conflict detection on ready-frontier candidates.
	runningFPs map[string]sched.Footprint
	// graph is the current dep-graph snapshot.
	graph GraphSnapshot
	// now is the reference time for ComputedAt.
	now time.Time
}

// computeQueue builds a QueueSnapshot from the provided input. It derives the
// true state for each issue (running / ready / dep-blocked / footprint-deferred
// / human / deferred-until / parked / container) and arranges them into a
// parent→children tree with epics at the top level.
func computeQueue(in queueInput) QueueSnapshot {
	if len(in.allIssues) == 0 {
		return QueueSnapshot{ComputedAt: in.now}
	}

	// Build lookup and parent→children index.
	byID := make(map[string]*beads.Issue, len(in.allIssues))
	for i := range in.allIssues {
		byID[in.allIssues[i].ID] = &in.allIssues[i]
	}

	childrenOf := make(map[string][]beads.Issue, 8)
	for _, iss := range in.allIssues {
		if iss.ParentID != "" && byID[iss.ParentID] != nil {
			childrenOf[iss.ParentID] = append(childrenOf[iss.ParentID], iss)
		}
	}

	// Collect root issues: no parent, or parent not in the open-issue set.
	var roots []beads.Issue
	for _, iss := range in.allIssues {
		if iss.ParentID == "" || byID[iss.ParentID] == nil {
			roots = append(roots, iss)
		}
	}
	sortIssues(roots)

	totalCount := 0
	rootNodes := make([]QueueNode, 0, len(roots))
	for _, root := range roots {
		node := buildQueueNode(root, childrenOf, in)
		totalCount += nodeCount(node)
		rootNodes = append(rootNodes, node)
	}

	return QueueSnapshot{
		Roots:      rootNodes,
		NodeCount:  totalCount,
		ComputedAt: in.now,
	}
}

// buildQueueNode recursively constructs a QueueNode and its subtree.
func buildQueueNode(iss beads.Issue, childrenOf map[string][]beads.Issue, in queueInput) QueueNode {
	kids := childrenOf[iss.ID]
	sortIssues(kids)

	childNodes := make([]QueueNode, 0, len(kids))
	for _, k := range kids {
		childNodes = append(childNodes, buildQueueNode(k, childrenOf, in))
	}

	state, reason := deriveQueueState(iss, len(childNodes) > 0, in)
	return QueueNode{
		Issue:    iss,
		State:    state,
		Reason:   reason,
		Children: childNodes,
	}
}

// deriveQueueState computes the QueueNodeState and reason annotation for a
// single issue. hasChildren indicates whether the issue has open children in
// the tree (used to classify container beads).
func deriveQueueState(iss beads.Issue, hasChildren bool, in queueInput) (QueueNodeState, string) {
	// 1. Running takes highest priority: a slot is actively working this bead.
	if in.runningIDs[iss.ID] {
		return QueueStateRunning, ""
	}

	// 2. Container epics/features with open children are not directly
	//    dispatchable; their state is "container" (they are dispatchable once
	//    all children close or when the scheduler considers them a leaf).
	switch iss.IssueType {
	case "epic", "feature", "decision", "merge-request":
		return QueueStateContainer, ""
	}

	// 3. Explicit label-based states.
	for _, l := range iss.Labels {
		if l == "parked" {
			return QueueStateParked, ""
		}
		if v, ok := strings.CutPrefix(l, "deferred-until:"); ok {
			return QueueStateDeferredUntil, v
		}
		if l == "human-only" || l == "human" {
			return QueueStateHuman, "requires human"
		}
		if l == "no-dispatch" {
			return QueueStateHuman, "no-dispatch label"
		}
	}
	// Also treat parked status (bd `status`).
	if iss.Status == "parked" {
		return QueueStateParked, ""
	}

	// 4. Container bead: an otherwise-eligible task that has open children
	//    (bd BuildWave would skip it as a "container bead"). Treat as container.
	if hasChildren {
		return QueueStateContainer, ""
	}

	// 5. Dep-blocked: the dep graph lists open blockers for this issue.
	if blockers, ok := in.graph.Deps[iss.ID]; ok && len(blockers) > 0 {
		// Limit to first 3 blockers for display.
		shown := blockers
		suffix := ""
		if len(shown) > 3 {
			shown = shown[:3]
			suffix = fmt.Sprintf(" +%d", len(blockers)-3)
		}
		return QueueStateDepBlocked, "on " + strings.Join(shown, ", ") + suffix
	}

	// 6. Footprint-deferred: in the ready frontier but footprint conflicts
	//    with a running bead's footprint.
	if in.readyIDs[iss.ID] && len(in.runningFPs) > 0 {
		fp := sched.FootprintFor(iss, nil)
		// Check in sorted order for deterministic reporting.
		conflictIDs := make([]string, 0, len(in.runningFPs))
		for id := range in.runningFPs {
			conflictIDs = append(conflictIDs, id)
		}
		sort.Strings(conflictIDs)
		for _, runID := range conflictIDs {
			if sched.Conflicts(fp, in.runningFPs[runID]) {
				return QueueStateFootprintDeferred, "conflict with " + runID
			}
		}
		// In the ready frontier, no conflict detected.
		return QueueStateReady, ""
	}

	// 7. Ready frontier.
	if in.readyIDs[iss.ID] {
		return QueueStateReady, ""
	}

	// 8. Not in ready frontier, not dep-blocked by the dep graph — could be
	//    a footprint conflict that bd ready didn't surface, or an eligibility
	//    issue. Surface as ready for now; the engine's wave log has specifics.
	return QueueStateReady, ""
}

// sortIssues sorts in place: epics/features first, then by priority asc,
// then by ID for stability.
func sortIssues(issues []beads.Issue) {
	sort.SliceStable(issues, func(i, j int) bool {
		a, b := issues[i], issues[j]
		aEpic := a.IssueType == "epic" || a.IssueType == "feature"
		bEpic := b.IssueType == "epic" || b.IssueType == "feature"
		if aEpic != bEpic {
			return aEpic
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.ID < b.ID
	})
}

// nodeCount returns the total number of nodes in the subtree rooted at n.
func nodeCount(n QueueNode) int {
	c := 1
	for _, child := range n.Children {
		c += nodeCount(child)
	}
	return c
}
