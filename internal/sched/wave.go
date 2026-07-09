// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sched

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

// Eligible reports whether an issue may be dispatched, and if not, a reason.
// Container-vs-open-children exclusion is not decided here (BuildWave supplies
// a child-lister); this covers the label/type/active rules.
func Eligible(issue beads.Issue, activeIDs map[string]bool) (bool, string) {
	switch issue.IssueType {
	case "epic", "feature", "decision", "merge-request":
		return false, "non-dispatch issue_type " + issue.IssueType
	}
	if issue.HasLabel("no-dispatch") {
		return false, "no-dispatch label"
	}
	if issue.HasLabel("refactor-core") {
		return false, "refactor-core label"
	}
	for _, l := range issue.Labels {
		if strings.HasPrefix(l, "gt:") {
			return false, "gate label " + l
		}
	}
	if activeIDs[issue.ID] {
		return false, "already active"
	}
	return true, ""
}

// silentSkip reports whether an ineligible issue is structural non-work
// (epics/features/decisions/merge-requests and gt:* gate beads) — recorded in
// the wave's Skipped list rather than Deferred, since it will never dispatch
// as-is.
func silentSkip(issue beads.Issue) bool {
	switch issue.IssueType {
	case "epic", "feature", "decision", "merge-request":
		return true
	}
	for _, l := range issue.Labels {
		if strings.HasPrefix(l, "gt:") {
			return true
		}
	}
	return false
}

// BuildWave selects a conflict-free set of at most opts.Max issues from a ready
// frontier. Ineligible structural issues (epics, gt:* gates) are recorded in
// Skipped (they will never dispatch as-is); no-dispatch/refactor-core/
// already-active and open-children containers are recorded in Deferred;
// footprint collisions and the width cap spill the remainder into Deferred.
//
// hasOpenChildren, when non-nil, reports whether an issue still has open
// children (a container bead that must not be worked directly).
func BuildWave(
	ctx context.Context,
	issues []beads.Issue,
	cfg *project.Config,
	opts Opts,
	hasOpenChildren func(id string) (bool, error),
) (Wave, error) {
	if err := ctx.Err(); err != nil {
		return Wave{}, err
	}

	w := Wave{
		Source:     "bd",
		Max:        opts.Max,
		ReadyCount: len(issues),
		Items:      []Item{},
	}
	if cfg != nil && cfg.WorkSource != "" {
		w.Source = cfg.WorkSource
	}

	// Filter to dispatch candidates.
	var candidates []beads.Issue
	for _, iss := range issues {
		if ok, reason := Eligible(iss, opts.ActiveIDs); !ok {
			if silentSkip(iss) {
				w.Skipped = append(w.Skipped, reasonFor(iss, reason))
			} else {
				w.Deferred = append(w.Deferred, reasonFor(iss, reason))
			}
			continue
		}
		if hasOpenChildren != nil {
			open, err := hasOpenChildren(iss.ID)
			if err != nil {
				return Wave{}, fmt.Errorf("check open children of %s: %w", iss.ID, err)
			}
			if open {
				w.Deferred = append(w.Deferred, reasonFor(iss, "container bead"))
				continue
			}
		}
		candidates = append(candidates, iss)
	}

	// Stable sort by priority ascending (P0 first); input order breaks ties.
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].Priority < candidates[j].Priority
	})

	// Sorted once up front so the in-flight check below always reports the
	// same blocker id for a given Active set, regardless of Go's randomized
	// map iteration order (koryph-2im.1).
	activeIDs := make([]string, 0, len(opts.Active))
	for id := range opts.Active {
		activeIDs = append(activeIDs, id)
	}
	sort.Strings(activeIDs)

	// Same determinism fix for resource holdings (design L4, koryph-4ql.2):
	// sorted once so a capacity breach always names the same first holder.
	activeResourceIDs := make([]string, 0, len(opts.ActiveResources))
	for id := range opts.ActiveResources {
		activeResourceIDs = append(activeResourceIDs, id)
	}
	sort.Strings(activeResourceIDs)

	// Greedy graph coloring: resource capacity (opts.ActiveResources ∪
	// already-selected items, design L4) is checked first, then in-flight
	// footprints (opts.Active) — a candidate conflicting/over-capacity with
	// running work is deferred before intra-batch coloring even considers it
	// (design L2, prerequisite for rolling/mid-wave dispatch: I1 must hold
	// against the live in-flight set, not just within this batch) — then
	// already-selected items in this wave. The resource check is per-item
	// (packing continues past a blocked bead, unlike "wave full"), so a
	// lower-priority resource-free bead can still dispatch behind a deferred
	// resource-heavy one, preserving priority order among the beads that
	// *can* run.
	for _, iss := range candidates {
		if opts.Max > 0 && len(w.Items) >= opts.Max {
			w.Deferred = append(w.Deferred, reasonFor(iss, "wave full"))
			continue
		}
		// Beads with no res:* labels bypass the resource check entirely —
		// kinds is nil, so the fast path costs nothing beyond the label scan
		// ResourcesFor already has to do (L1 bullet 5).
		kinds := ResourcesFor(iss)
		if len(kinds) > 0 {
			if kind, holder, blocked := resourceBlocker(kinds, opts.ActiveResources, activeResourceIDs, w.Items, opts.ResourceCapacity); blocked {
				w.Deferred = append(w.Deferred, reasonFor(iss, "resource "+kind+" at capacity (held by "+holder+")"))
				continue
			}
		}
		fp := FootprintFor(iss, cfg)
		if id, clash := firstActiveConflict(fp, opts.Active, activeIDs); clash {
			w.Deferred = append(w.Deferred, reasonFor(iss, "footprint conflict with "+id+" (in-flight)"))
			continue
		}
		if id, clash := firstConflict(fp, w.Items); clash {
			w.Deferred = append(w.Deferred, reasonFor(iss, "footprint conflict with "+id))
			continue
		}
		w.Items = append(w.Items, buildItem(iss, fp, kinds, opts))
	}

	return w, nil
}

// resourceBlocker reports whether admitting a candidate declaring kinds would
// push any one of them over its effective capacity, counting holders from
// in-flight resource holdings (active, in sortedActiveIDs order) unioned with
// already-selected items in this wave (design L4, koryph-4ql.2). Kinds are
// checked in ResourcesFor's sorted order, so the reported kind is
// deterministic when a candidate declares more than one and multiple are at
// capacity. Within a kind, the reported holder id is deterministic too:
// in-flight holders win (sorted bead-id order, mirroring
// firstActiveConflict) over in-batch holders (wave selection order) — an
// already-running claim is more concrete than one this same call is still
// assembling. A kind absent from capacity — including when the map itself is
// nil — defaults to 1 (the fail-safe-serial default, design L2).
func resourceBlocker(kinds []string, active map[string][]string, sortedActiveIDs []string, items []Item, capacity map[string]int) (kind, holder string, blocked bool) {
	for _, k := range kinds {
		kindCap := 1
		if c, ok := capacity[k]; ok {
			kindCap = c
		}
		count := 0
		firstHolder := ""
		for _, id := range sortedActiveIDs {
			if containsString(active[id], k) {
				count++
				if firstHolder == "" {
					firstHolder = id
				}
			}
		}
		for _, it := range items {
			if containsString(it.Resources, k) {
				count++
				if firstHolder == "" {
					firstHolder = it.Issue.ID
				}
			}
		}
		if count+1 > kindCap {
			return k, firstHolder, true
		}
	}
	return "", "", false
}

// containsString reports whether s is present in list.
func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// firstActiveConflict returns the id of the first in-flight footprint (in
// sortedIDs order, for deterministic reporting) that conflicts with fp.
func firstActiveConflict(fp Footprint, active map[string]Footprint, sortedIDs []string) (string, bool) {
	for _, id := range sortedIDs {
		if Conflicts(fp, active[id]) {
			return id, true
		}
	}
	return "", false
}

// firstConflict returns the ID of the first already-selected item whose
// footprint collides with fp.
func firstConflict(fp Footprint, items []Item) (string, bool) {
	for _, it := range items {
		if Conflicts(fp, it.Footprint) {
			return it.Issue.ID, true
		}
	}
	return "", false
}

// buildItem assembles a schedulable item, resolving its model. kinds is the
// already-parsed ResourcesFor result, threaded through rather than
// re-derived so the caller's single label scan is the only one (design L4).
// Persona and effort are left for the engine to resolve from stage/persona
// metadata.
func buildItem(iss beads.Issue, fp Footprint, kinds []string, opts Opts) Item {
	model, why := resolveModel(iss, opts)
	return Item{
		Issue:     iss,
		Footprint: fp,
		Resources: kinds,
		Model:     model,
		ModelWhy:  why,
		Persona:   "",
		EpicID:    iss.ParentID,
	}
}

// resolveModel picks a model tier for an implementation dispatch. Precedence:
// model:implement:<tier> > model:<tier> > opts.DefaultModel > "" (engine
// applies the stage default later). Every outcome carries a rationale.
func resolveModel(iss beads.Issue, opts Opts) (model, rationale string) {
	for _, l := range iss.Labels {
		if v, ok := strings.CutPrefix(l, "model:implement:"); ok && v != "" {
			return v, "label " + l
		}
	}
	for _, l := range iss.Labels {
		if v, ok := strings.CutPrefix(l, "model:"); ok {
			// Skip stage-scoped labels (model:<stage>:<tier>); only bare
			// model:<tier> applies here.
			if v == "" || strings.Contains(v, ":") {
				continue
			}
			return v, "label " + l
		}
	}
	if opts.DefaultModel != "" {
		return opts.DefaultModel, "run default"
	}
	return "", "stage default"
}

func reasonFor(iss beads.Issue, reason string) Reason {
	return Reason{ID: iss.ID, Title: iss.Title, Reason: reason}
}
