// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
)

// Epic validation — the in-loop trigger (design §2/§4/§4b,
// docs/designs/2026-07-epic-validation.md; koryph-wo0.4).
//
// After every engine-side bead close, the parent epic becomes a completion
// CANDIDATE. Each rolling tick drains at most one candidate: when the epic's
// children are all terminal, the engine either closes it (validation already
// passed — the docs bead was the last child), parks it (round cap), or spawns
// the frontier validator in a goroutine — never blocking the tick — and acts
// on the verdict deterministically via epicreview.Act on a later tick.
//
// The pending set is in-memory only: a crash between a close and its
// validation loses the candidate, and `koryph epic validate` is the
// documented recovery path (the health patrol's completed-unvalidated-epic
// sweep is a filed follow-up).

// epicValidationResult carries a finished validator run back to the tick loop.
type epicValidationResult struct {
	epicID  string
	round   int
	verdict epicreview.Verdict
}

// noteEpicCandidate marks the parent epic of a just-closed bead for a
// completion check. Best-effort: an unshowable bead or an orphan (no parent)
// is silently skipped.
func (r *runner) noteEpicCandidate(ctx context.Context, closedID string) {
	evcfg := r.cfg.EffectiveEpicValidation()
	if !*evcfg.Enabled {
		return
	}
	iss, err := r.adapter.Show(ctx, closedID)
	if err != nil || iss.ParentID == "" {
		return
	}
	if r.epicPending == nil {
		r.epicPending = map[string]bool{}
	}
	r.epicPending[iss.ParentID] = true
}

// drainEpicResults applies at most one finished validator verdict per tick.
// Runs on the tick goroutine so every bead mutation stays single-threaded.
func (r *runner) drainEpicResults(ctx context.Context) {
	if r.epicResults == nil {
		return
	}
	select {
	case res := <-r.epicResults:
		r.epicInFlight = ""
		bd, ok := r.adapter.(epicreview.BeadStore)
		if !ok {
			r.progress("epic %s: validation verdict dropped — work source lacks bead-store verbs", res.epicID)
			return
		}
		out, err := epicreview.Act(ctx, bd, epicreview.ActOpts{
			EpicID:   res.epicID,
			Round:    res.round,
			Config:   r.cfg.EffectiveEpicValidation(),
			Actor:    "engine epic-validation",
			Progress: r.progress,
		}, res.verdict)
		if err != nil {
			r.progress("epic %s: acting on validation verdict: %v", res.epicID, err)
		}
		logEpicValidation(r.run.RunID, r.opts.ProjectID, res.epicID, res.round, out.Outcome)
		// Gap follow-ups (and the docs bead) are ordinary children: when they
		// close, noteEpicCandidate re-queues the epic — no special casing.
	default:
	}
}

// maybeStartEpicValidation spawns at most one validator per project at a time,
// and only while the governor allows dispatch (drain/stop DEFERS validation —
// the pending set survives until the guard lifts; it is never skipped).
func (r *runner) maybeStartEpicValidation(ctx context.Context, allowDispatch bool) {
	if r.epicInFlight != "" || len(r.epicPending) == 0 || !allowDispatch {
		return
	}
	evcfg := r.cfg.EffectiveEpicValidation()
	if !*evcfg.Enabled {
		r.epicPending = nil
		return
	}

	// Deterministic order for reproducible tests and logs.
	ids := make([]string, 0, len(r.epicPending))
	for id := range r.epicPending {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, epicID := range ids {
		delete(r.epicPending, epicID)

		epic, err := r.adapter.Show(ctx, epicID)
		if err != nil {
			r.progress("epic %s: completion check: %v", epicID, err)
			continue
		}
		if epic.IssueType != "epic" || epic.Status == "closed" ||
			epic.HasLabel(epicreview.LabelNoValidate) || epic.HasLabel(epicreview.LabelParked) {
			continue
		}
		children, err := r.adapter.ListChildren(ctx, epicID)
		if err != nil {
			r.progress("epic %s: list children: %v", epicID, err)
			continue
		}
		if len(children) == 0 || anyOpenChild(children) {
			continue // not complete yet; a later close re-queues it
		}

		// Validation already passed → the docs bead was the last child: close
		// WITHOUT re-validating (§4b).
		if epic.HasLabel(epicreview.LabelPassed) {
			if err := r.adapter.Close(ctx, epicID, "validated; docs update merged"); err != nil {
				r.progress("epic %s: close after docs merge: %v", epicID, err)
				continue
			}
			r.progress("epic %s: docs update merged — epic closed", epicID)
			logEpicValidation(r.run.RunID, r.opts.ProjectID, epicID, 0, "closed-after-docs")
			continue
		}

		outDir := filepath.Join(r.rec.Root, ".koryph", "epic-reviews")
		round := epicreview.DetectNextRound(outDir, epicID)
		if round > evcfg.MaxRounds {
			r.parkEpic(ctx, epicID, round, evcfg.MaxRounds)
			continue
		}

		// Build validator opts on the tick goroutine (adapter reads), then
		// spawn: Validate shells out to the frontier agent for minutes.
		erChildren := make([]epicreview.Child, 0, len(children))
		for _, c := range children {
			erChildren = append(erChildren, epicreview.Child{
				ID:          c.ID,
				Title:       c.Title,
				Description: c.Description,
				CloseReason: c.Status,
				Labels:      c.Labels,
			})
		}
		opts := epicreview.Opts{
			EpicID:          epicID,
			EpicTitle:       epic.Title,
			EpicDescription: epic.Description,
			EpicNotes:       epic.Notes,
			Children:        erChildren,
			PriorVerdicts:   epicreview.LoadPriorVerdicts(outDir, epicID, round),
			Round:           round,
			RepoRoot:        r.rec.Root,
			Profile:         r.profile,
			Persona:         evcfg.Persona,
			Model:           evcfg.Model,
			TimeoutSec:      evcfg.TimeoutSeconds,
			OutDir:          outDir,
		}

		validate := r.epicValidateFn
		if validate == nil {
			validate = epicreview.Validate
		}
		if r.epicResults == nil {
			r.epicResults = make(chan epicValidationResult, 1)
		}
		r.epicInFlight = epicID
		r.progress("epic %s: all children closed — validation round %d starting (%s)",
			epicID, round, evcfg.Model)
		logEpicValidation(r.run.RunID, r.opts.ProjectID, epicID, round, "started")
		go func() {
			r.epicResults <- epicValidationResult{
				epicID:  epicID,
				round:   round,
				verdict: validate(ctx, opts),
			}
		}()
		return // one at a time per project
	}
}

// parkEpic applies the round-cap terminal state (§2): label + note, operator
// decides from here (`koryph epic validate` is the recovery verb).
func (r *runner) parkEpic(ctx context.Context, epicID string, round, maxRounds int) {
	bd, ok := r.adapter.(epicreview.BeadStore)
	note := fmt.Sprintf(
		"validation parked: round %d would exceed max_rounds=%d. Operator recovery: koryph epic validate %s --project %s",
		round, maxRounds, epicID, r.opts.ProjectID)
	if ok {
		if err := bd.AddLabel(ctx, epicID, epicreview.LabelParked); err != nil {
			r.progress("epic %s: add parked label: %v", epicID, err)
		}
		if err := bd.AppendNotes(ctx, epicID, note); err != nil {
			r.progress("epic %s: append parked note: %v", epicID, err)
		}
	}
	r.progress("epic %s: PARKED — %s", epicID, note)
	logEpicValidation(r.run.RunID, r.opts.ProjectID, epicID, round, "parked")
}

// anyOpenChild reports whether any child is non-terminal. Deferred counts as
// open: a deferred child is deliberate operator state, not completion.
func anyOpenChild(children []beads.Issue) bool {
	for _, c := range children {
		if c.Status != "closed" && c.Status != "done" {
			return true
		}
	}
	return false
}
