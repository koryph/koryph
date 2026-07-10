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
	// QueueStateResourceDeferred means the bead is in the ready frontier but
	// a declared res:<kind> label (sched.ResourcesFor) is at capacity across
	// the live cross-project resource ledger (design
	// docs/designs/2026-07-resource-governor.md L4/L7, koryph-4ql.10).
	QueueStateResourceDeferred QueueNodeState = "resource-deferred"
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
	// the in-flight conflicting bead), resource-deferred (names the kind and
	// holder, formatResourceDeferralReason), and deferred-until (the date
	// string).
	Reason string
	// ResourceKind and ResourceHolder are populated only for
	// QueueStateResourceDeferred — the at-capacity kind and its current
	// holder (bead id, or "project/bead" for a cross-project holder) — so
	// the TUI/IDE can explain the wait without re-parsing Reason
	// (koryph-4ql.10).
	ResourceKind   string
	ResourceHolder string
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
	// resources is the live per-kind external resource ledger (capacity +
	// cross-project holders), from GovernorSnapshot.Resources
	// (govern.Store.ResourcesStatus(), koryph-4ql.1 L7), used to classify
	// ready-frontier res:<kind> beads as resource-deferred (koryph-4ql.10).
	// Empty/nil means unavailable — fails open (no resource-deferred
	// classifications), matching I6.
	resources []ResourceSnapshot
	// closedParents supplies metadata (title/type/status) for parent epics
	// referenced by an open child but ABSENT from allIssues — i.e. the epic has
	// been closed while some of its children remain open. `bd list` omits closed
	// issues, so without this the open children would orphan up to the top level
	// and the queue would render flat, losing the epic grouping (the "no longer
	// groups the hierarchy" regression). computeQueue synthesizes a container
	// node per such parent so its open children stay nested. Keyed by parent ID;
	// a missing entry falls back to a bare ID-only container. refreshQueue
	// populates it with a bounded set of `bd show` lookups.
	closedParents map[string]beads.Issue
	// projectID is this queue's project, used to format a cross-project
	// resource holder as "project/bead" (matching the engine's
	// classifyAdmit) vs a same-project holder as just its bead id.
	projectID string
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

	// Partition top-level issues into genuine roots (no parent) and orphans
	// (parent set but absent from the open-issue set — a closed/filtered epic).
	// Orphans are regrouped under a synthesized container so the hierarchy
	// survives a parent epic closing mid-run. An issue whose parent IS present
	// is neither — it is reached recursively via childrenOf.
	var roots []beads.Issue
	orphansByParent := make(map[string][]beads.Issue)
	var orphanParents []string
	for _, iss := range in.allIssues {
		switch {
		case iss.ParentID == "":
			roots = append(roots, iss)
		case byID[iss.ParentID] == nil:
			if _, seen := orphansByParent[iss.ParentID]; !seen {
				orphanParents = append(orphanParents, iss.ParentID)
			}
			orphansByParent[iss.ParentID] = append(orphansByParent[iss.ParentID], iss)
		}
	}
	sortIssues(roots)
	sort.Strings(orphanParents) // deterministic order for synthesized containers

	totalCount := 0
	rootNodes := make([]QueueNode, 0, len(roots)+len(orphanParents))
	for _, root := range roots {
		node := buildQueueNode(root, childrenOf, in)
		totalCount += nodeCount(node)
		rootNodes = append(rootNodes, node)
	}
	// Synthesized closed-parent containers, appended after the real roots.
	for _, parentID := range orphanParents {
		node := buildOrphanContainer(parentID, orphansByParent[parentID], childrenOf, in)
		totalCount += nodeCount(node)
		rootNodes = append(rootNodes, node)
	}

	return QueueSnapshot{
		Roots:      rootNodes,
		NodeCount:  totalCount,
		ComputedAt: in.now,
	}
}

// buildOrphanContainer synthesizes a container QueueNode for a parent epic that
// is referenced by open children but absent from the open-issue set (closed or
// filtered). Its children are the orphaned issues (each expanded into its own
// subtree via buildQueueNode). Parent metadata (title/type/status) comes from
// in.closedParents when available; otherwise the container shows the bare
// parent ID. The node is always a container so it renders as a collapsible epic
// group — preserving the hierarchy the flat `bd list` output would otherwise
// drop.
func buildOrphanContainer(parentID string, orphans []beads.Issue, childrenOf map[string][]beads.Issue, in queueInput) QueueNode {
	sortIssues(orphans)
	childNodes := make([]QueueNode, 0, len(orphans))
	for _, o := range orphans {
		childNodes = append(childNodes, buildQueueNode(o, childrenOf, in))
	}

	parent, ok := in.closedParents[parentID]
	if !ok {
		// No metadata available — synthesize a minimal container from the ID.
		parent = beads.Issue{ID: parentID, Title: parentID, IssueType: "epic"}
	}
	reason := ""
	if parent.Status != "" && parent.Status != "open" && parent.Status != "in_progress" {
		// Surface why the parent isn't itself a live queue row.
		reason = "parent " + parent.Status
	} else {
		reason = "parent not in ready set"
	}
	return QueueNode{
		Issue:    parent,
		State:    QueueStateContainer,
		Reason:   reason,
		Children: childNodes,
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
	node := QueueNode{
		Issue:    iss,
		State:    state,
		Reason:   reason,
		Children: childNodes,
	}
	if state == QueueStateResourceDeferred {
		// Re-derive the structured kind/holder from the same reason string
		// deriveQueueState just built (formatResourceDeferralReason), rather
		// than plumbing a second return value through every deriveQueueState
		// case — keeps Reason the single source of truth for both the
		// human-readable text and the typed fields.
		if kind, holder, ok := parseResourceDeferralReason(reason); ok {
			node.ResourceKind = kind
			node.ResourceHolder = holder
		}
	}
	return node
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

	// 6. Resource-deferred: in the ready frontier but a declared res:<kind>
	//    label (sched.ResourcesFor) is at capacity across the live
	//    cross-project resource ledger (in.resources, from
	//    govern.Store.ResourcesStatus()). Checked before footprint-deferred,
	//    mirroring sched.BuildWave's resource-then-footprint ordering
	//    (design L4): resources protect the machine, footprints protect the
	//    merge.
	if in.readyIDs[iss.ID] {
		if kind, holder, blocked := resourceCapacityBlocker(iss, in); blocked {
			return QueueStateResourceDeferred, formatResourceDeferralReason(kind, holder)
		}
	}

	// 7. Footprint-deferred: in the ready frontier but footprint conflicts
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

	// 8. Ready frontier.
	if in.readyIDs[iss.ID] {
		return QueueStateReady, ""
	}

	// 9. Not in ready frontier, not dep-blocked by the dep graph — could be
	//    a footprint conflict that bd ready didn't surface, or an eligibility
	//    issue. Surface as ready for now; the engine's wave log has specifics.
	return QueueStateReady, ""
}

// resourceCapacityBlocker reports whether iss's declared res:<kind> labels
// (sched.ResourcesFor) would push any one of them over capacity, using the
// live cross-project resource ledger threaded through in.resources
// (govern.Store.ResourcesStatus(), koryph-4ql.1 L7/L4). Kinds are checked in
// ResourcesFor's sorted order for deterministic reporting, mirroring
// sched/wave.go's resourceBlocker. A kind absent from in.resources (no
// configured capacity override AND no live holder anywhere) is never
// reported blocked here even though it would default-bind to capacity 1 at
// dispatch (design L2) — zero holders can never be "at capacity". Fails open
// (no report) when in.resources is empty: an unavailable/old governor
// snapshot must not spuriously classify every declared-resource bead as
// deferred (I6).
func resourceCapacityBlocker(iss beads.Issue, in queueInput) (kind, holder string, blocked bool) {
	if len(in.resources) == 0 {
		return "", "", false
	}
	kinds := sched.ResourcesFor(iss)
	if len(kinds) == 0 {
		return "", "", false
	}
	byKind := make(map[string]ResourceSnapshot, len(in.resources))
	for _, rs := range in.resources {
		byKind[rs.Kind] = rs
	}
	for _, k := range kinds {
		rs, ok := byKind[k]
		if !ok || len(rs.Holders) == 0 {
			continue
		}
		capacity := rs.Capacity
		if capacity <= 0 {
			capacity = 1 // defensive; ResourcesStatus always resolves a positive capacity
		}
		if len(rs.Holders) < capacity {
			continue
		}
		h := rs.Holders[0]
		id := h.Bead
		if h.Project != "" && h.Project != in.projectID {
			id = h.Project + "/" + h.Bead
		}
		return k, id, true
	}
	return "", "", false
}

// formatResourceDeferralReason builds the resource-deferral reason string in
// the exact wording sched/wave.go's resourceBlocker and the engine's
// classifyAdmit both use (design docs/designs/2026-07-resource-governor.md
// L3/L4): "resource <kind> at capacity (held by <id>)".
// parseResourceDeferralReason is its inverse.
func formatResourceDeferralReason(kind, holder string) string {
	return "resource " + kind + " at capacity (held by " + holder + ")"
}

// parseResourceDeferralReason extracts (kind, holder) from a reason string
// built by formatResourceDeferralReason — used by buildQueueNode to populate
// QueueNode.ResourceKind/ResourceHolder from the same Reason text the TUI/IDE
// already display, so there is exactly one source of truth for the wording.
// ok is false for any string that does not match the expected shape (an
// empty kind or holder is treated as malformed, not a valid zero value).
func parseResourceDeferralReason(reason string) (kind, holder string, ok bool) {
	const prefix = "resource "
	const mid = " at capacity (held by "
	if !strings.HasPrefix(reason, prefix) {
		return "", "", false
	}
	rest := reason[len(prefix):]
	i := strings.Index(rest, mid)
	if i <= 0 {
		return "", "", false
	}
	kind = rest[:i]
	tail := rest[i+len(mid):]
	if len(tail) < 2 || !strings.HasSuffix(tail, ")") {
		return "", "", false
	}
	holder = tail[:len(tail)-1]
	if holder == "" {
		return "", "", false
	}
	return kind, holder, true
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
