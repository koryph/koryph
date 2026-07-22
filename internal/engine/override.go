// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

// applyOperatorOverrides folds any operator directives from the run's override
// sidecar into the in-memory ledger, so a manual state change — a bead landed
// or retired by hand via `koryph merge` — is adopted by the running engine
// instead of being reverted by its next single-writer rewrite of ledger.json
// (D5). Called at the top of every wave/rolling iteration, next to
// syncObsConfig and the health patrol.
//
// Only a terminal directive (merged/blocked/…) applied to a slot that is not
// already terminal takes effect, which makes it idempotent — once the slot has
// adopted the status, later cycles skip it — and prevents a stale directive from
// ever re-killing a slot that legitimately went back to work. Best-effort: a
// sidecar read error never blocks the loop.
func (r *runner) applyOperatorOverrides() {
	if r.run == nil {
		return
	}
	of, err := r.store.LoadOverrides(r.run.RunID)
	if err != nil || len(of.Overrides) == 0 {
		return
	}
	for _, ov := range of.Overrides {
		if ov.BeadID == "" || !ledger.Terminal(ov.Status) {
			continue // only terminal directives (mark landed/retired) are honored
		}
		sl := r.run.Slots[ov.BeadID]
		if sl == nil || ledger.Terminal(sl.Status) {
			continue // no such slot, or already terminal — nothing to adopt / re-apply
		}
		prev := sl.Status
		note := ov.Note
		_ = r.store.UpdateSlot(r.run, ov.BeadID, func(s *ledger.Slot) {
			s.Status = ov.Status
			if note != "" {
				s.Note = note
			}
		})
		r.releaseGlobalSlot(ov.BeadID) // terminal now: free the reserved slot
		r.progress("bead %s: operator override applied — %s → %s%s", ov.BeadID, prev, ov.Status, overrideNoteSuffix(note))
	}
}

func overrideNoteSuffix(note string) string {
	if note == "" {
		return ""
	}
	return " (" + note + ")"
}

// applyInjections merges operator-injected beads into the wave frontier so a
// specific bead can be added to a running --parent-scoped loop without a restart
// (D10). An injected bead is added only when it is genuinely READY (present in
// the unscoped bd frontier) and not already in scope or already dispatched, so a
// blocked or unknown injection can never be force-dispatched — the scheduler and
// governor then gate it exactly like any frontier bead. Best-effort: a sidecar
// or bd read error leaves the scoped frontier unchanged.
func (r *runner) applyInjections(ctx context.Context, scoped []beads.Issue) []beads.Issue {
	if r.run == nil {
		return scoped
	}
	of, err := r.store.LoadOverrides(r.run.RunID)
	if err != nil || len(of.Inject) == 0 {
		return scoped
	}
	inScope := make(map[string]bool, len(scoped))
	for _, iss := range scoped {
		inScope[iss.ID] = true
	}
	var wanted []string
	for _, id := range of.Inject {
		if id == "" || inScope[id] {
			continue
		}
		if _, dispatched := r.run.Slots[id]; dispatched {
			continue // injection already fulfilled — it has a slot
		}
		wanted = append(wanted, id)
	}
	if len(wanted) == 0 {
		return scoped
	}
	// Only add beads that are genuinely ready (whole-frontier query, no parent
	// scope), so an injection can widen scope but never force-dispatch a bead
	// that bd still considers blocked.
	ready, err := r.adapter.Ready(ctx, beads.ReadyOpts{})
	if err != nil {
		return scoped
	}
	readyByID := make(map[string]beads.Issue, len(ready))
	for _, iss := range ready {
		readyByID[iss.ID] = iss
	}
	for _, id := range wanted {
		if iss, ok := readyByID[id]; ok {
			scoped = append(scoped, iss)
			r.progress("bead %s: operator-injected into the frontier (outside the run's --parent scope)", id)
		}
	}
	return scoped
}
