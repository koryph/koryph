// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

func normalizeAllowedIDs(ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, raw := range ids {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, fmt.Errorf("engine: AllowedIDs contains an empty bead id")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func allowedIDSet(ids []string) map[string]struct{} {
	if len(ids) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func safetyTripwireSet(kinds []SafetyTripwireKind) map[SafetyTripwireKind]struct{} {
	if len(kinds) == 0 {
		return nil
	}
	out := make(map[SafetyTripwireKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if strings.TrimSpace(string(kind)) != "" {
			out[kind] = struct{}{}
		}
	}
	return out
}

func (r *runner) hardStopEnabled(kind SafetyTripwireKind) bool {
	_, ok := r.hardStopKinds[kind]
	return ok
}

func (r *runner) idAllowed(id string) bool {
	if len(r.allowedIDs) == 0 {
		return true
	}
	_, ok := r.allowedIDs[id]
	return ok
}

func (r *runner) filterAllowedIssues(issues []beads.Issue) []beads.Issue {
	if len(r.allowedIDs) == 0 {
		return issues
	}
	out := make([]beads.Issue, 0, len(issues))
	for _, issue := range issues {
		if r.idAllowed(issue.ID) {
			out = append(out, issue)
		}
	}
	return out
}

// validateAllowedResume refuses to partially adopt a prior run. A resumable
// slot outside the fixed cohort cannot be ignored safely: its process, branch,
// claim, and shared run ledger would otherwise continue under ambiguous
// ownership. Terminal historical slots are inert and may remain as evidence.
func (r *runner) validateAllowedResume(run *ledger.Run) error {
	if len(r.allowedIDs) == 0 || run == nil {
		return nil
	}
	var outside []string
	for id, slot := range run.Slots {
		if slot != nil && !ledger.Terminal(slot.Status) && !r.idAllowed(id) {
			outside = append(outside, id)
		}
	}
	if len(outside) == 0 {
		return nil
	}
	sort.Strings(outside)
	return fmt.Errorf("fixed cohort refuses resumable bead(s) outside AllowedIDs: %s",
		strings.Join(outside, ", "))
}

func (r *runner) emitSafetyTripwire(kind SafetyTripwireKind, beadID, detail string) {
	if r.opts.NativeCanary {
		// The engine loop is the sole admission owner. Latch before publishing
		// so a zero-stagger batch cannot launch a sibling in the short interval
		// before the supervisor receives this event and cancels the context.
		r.safetyTripwireFired = true
	}
	if r.opts.OnSafetyTripwire == nil {
		return
	}
	runID := ""
	if r.run != nil {
		runID = r.run.RunID
	}
	r.opts.OnSafetyTripwire(SafetyTripwire{
		Kind: kind, RunID: runID, BeadID: beadID, Detail: detail,
	})
}
