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

// silentSkip reports whether an ineligible issue should be dropped without a
// Deferred record (structural non-work: epics/features/decisions/merge-requests
// and gt:* gate beads) rather than reported.
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
// frontier. Ineligible structural issues (epics, gt:* gates) are dropped
// silently; no-dispatch/refactor-core/already-active and open-children
// containers are recorded in Deferred; footprint collisions and the width cap
// spill the remainder into Deferred as well.
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
			if !silentSkip(iss) {
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

	// Greedy graph coloring against already-selected footprints.
	for _, iss := range candidates {
		if opts.Max > 0 && len(w.Items) >= opts.Max {
			w.Deferred = append(w.Deferred, reasonFor(iss, "wave full"))
			continue
		}
		fp := FootprintFor(iss, cfg)
		if id, clash := firstConflict(fp, w.Items); clash {
			w.Deferred = append(w.Deferred, reasonFor(iss, "footprint conflict with "+id))
			continue
		}
		w.Items = append(w.Items, buildItem(iss, fp, opts))
	}

	return w, nil
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

// buildItem assembles a schedulable item, resolving its model. Persona and
// effort are left for the engine to resolve from stage/persona metadata.
func buildItem(iss beads.Issue, fp Footprint, opts Opts) Item {
	model, why := resolveModel(iss, opts)
	return Item{
		Issue:     iss,
		Footprint: fp,
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
