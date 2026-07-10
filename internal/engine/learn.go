// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/modellearn"
)

// learnApplyInterval throttles the wave-boundary learner pass: rolling mode
// refills on every freed slot, and re-scanning every run ledger plus
// re-deriving recommendations that often buys nothing — evidence only changes
// when a slot goes terminal, on the order of minutes.
const learnApplyInterval = time.Minute

// applyLearnedModels is the koryph-qf6.6 wave-boundary hook: gated on the
// project's adaptive_escalation config, it mines the run ledgers for
// escalation history and stamps learned `model:<tier>` labels (plus
// `model-learned:<date>` provenance) onto matching frontier beads — both in
// bd and on the in-memory slice it returns, so the wave being built right now
// already routes on them. Everything is best-effort: a ledger-scan or
// label-write failure degrades to the unlabeled frontier, never blocks
// dispatch.
func (r *runner) applyLearnedModels(ctx context.Context, issues []beads.Issue) []beads.Issue {
	ae := r.cfg.AdaptiveEscalation
	if ae == nil || !ae.Enabled || len(issues) == 0 {
		return issues
	}
	if !r.lastLearn.IsZero() && time.Since(r.lastLearn) < learnApplyInterval {
		return issues
	}
	r.lastLearn = time.Now()

	evs, err := modellearn.Collect(r.store)
	if err != nil {
		r.progress("model-learn: ledger scan failed (%v) — skipping this boundary", err)
		return issues
	}
	recs := modellearn.Recommend(evs, ae.MinEvidence)
	if len(recs) == 0 {
		return issues
	}
	date := time.Now().UTC().Format("2006-01-02")
	applied, updated, failed := modellearn.Apply(ctx, r.adapter, issues, recs, date)
	for _, a := range applied {
		r.progress("bead %s: learned model:%s applied (%s %s — %s escalation history)",
			a.BeadID, a.Tier, a.Area, a.Size, r.opts.ProjectID)
		logModelLearnApplied(a.BeadID, a.Tier, a.Area, a.Size)
	}
	if failed > 0 {
		r.progress("model-learn: %d label write(s) failed — those beads keep their defaults this boundary", failed)
	}
	return updated
}
