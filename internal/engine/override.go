// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import "github.com/koryph/koryph/internal/ledger"

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
