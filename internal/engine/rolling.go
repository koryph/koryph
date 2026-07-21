// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/sched"
)

// rollingLoop is the continuous-refill dispatch path (koryph-2im.3, design
// L1): unlike waveLoop, it never blocks until an entire batch is terminal —
// every tick it re-checks the governor, recomputes free capacity from the
// CURRENT active count, and tops off any freed slot from the frontier. This is
// what breaks the "wave barrier": a slot that frees up mid-batch is refilled
// on the very next tick rather than waiting for its siblings.
//
// Only entered when the effective dispatch mode is "rolling" AND !opts.Once
// (see loop's selection comment) — --once keeps today's wave semantics in
// both modes, so this function's caller guarantees Once is false.
//
// Governor/billing/budget checks are shared with waveLoop via governorGate
// (I4 cannot drift between the two loops). The frontier scan and
// sched.BuildWave call are the SAME primitives the wave loop uses — only the
// outer control flow differs: one tick of waitTick+pollPass per iteration
// instead of pollUntilIdle's block-to-idle.
func (r *runner) rollingLoop(ctx context.Context) (Outcome, error) {
	interval := r.pollInterval()
	wake := r.wakeCh
	if wake == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGCHLD)
		defer signal.Stop(ch)
		wake = ch
	}
	tick := 0

	for {
		if ctx.Err() != nil {
			return r.interrupted()
		}
		syncObsConfig() // pick up `koryph obs level` changes without a restart
		r.patrolIfDue(ctx)
		r.drainEpicResults(ctx)

		gate := r.governorGate(ctx)
		if gate.paused {
			return gate.outcome, nil
		}
		allowDispatch := gate.allowDispatch
		r.maybeStartEpicValidation(ctx, allowDispatch)
		level, calibrated, usage, budgetHit := gate.level, gate.calibrated, gate.usage, gate.budgetHit
		width := gate.width

		// Promote resume-backlog beads (SlotQueued, parked by resume() when the
		// effective width could not admit them all) into live dispatches before
		// scanning the frontier, so recovered work takes priority and is capped
		// at the CURRENT width rather than the stalled run's (koryph-bzf). A
		// no-op on every non-resume tick. Rolling already ticks via waitTick, so
		// a backlog held back by a machine-wide denial retries next tick with no
		// extra pacing needed here.
		r.drainResumeBacklog(ctx, width, allowDispatch)

		active := r.activeIDs()
		capacity := width - len(active)

		// Frontier scan + wave build (at most once per iteration). Scanned
		// whenever there is room to actually dispatch (capacity>0 &&
		// allowDispatch) OR whenever nothing is active — the latter is not an
		// optimization, it is required for a correct exit decision below:
		// with nothing running, only a real scan can tell "truly drained"
		// (eligible==0) apart from "paused with ready work waiting"
		// (eligible>0), exactly the distinction waveLoop's unconditional scan
		// makes. When active>0 and capacity<=0, none of the exit conditions
		// below apply (they all require active==0), so skipping the scan
		// there is a pure cost saving (design L1: "scan only when
		// capacity > 0, at most once per tick") with no semantic effect.
		var w sched.Wave
		eligible := 0
		scanned := false
		if (capacity > 0 && allowDispatch) || len(active) == 0 {
			issues, err := r.adapter.Ready(ctx, beads.ReadyOpts{Parent: r.opts.Parent})
			if err != nil {
				return r.outcome(ExitFatal, "bd ready failed", false), fmt.Errorf("engine: bd ready: %w", err)
			}
			// --only narrows the frontier to a single operator-chosen bead;
			// once it closes it drops out of `bd ready` and the run drains.
			if r.opts.Only != "" {
				issues = onlyBead(issues, r.opts.Only)
			}
			// Learned-model pass (koryph-qf6.6) — see waveLoop's identical
			// hook; the in-function throttle keeps rolling refills cheap.
			issues = r.applyLearnedModels(ctx, issues)
			w, err = sched.BuildWave(ctx, issues, r.cfg, sched.Opts{
				Max:              capacity,
				DefaultModel:     r.opts.DefaultModel,
				Parent:           r.opts.Parent,
				ActiveIDs:        active,
				Active:           r.activeFootprints(ctx, active),
				ActiveResources:  r.activeResources(ctx, active),
				ResourceCapacity: r.resourceCapacities(),
			}, r.childLister(ctx))
			if err != nil {
				return r.outcome(ExitFatal, "wave build failed", false), fmt.Errorf("engine: build wave: %w", err)
			}
			r.captureFrontier(w)
			for _, iss := range issues {
				if ok, _ := sched.Eligible(iss, active); ok {
					eligible++
				}
			}
			scanned = true
		}

		// Drained: nothing eligible, nothing active, nothing batched. Checked
		// FIRST (before the paused-quota check below) so a genuinely empty
		// frontier reports "drained" even while draining/stopped — mirrors
		// waveLoop, where drain (unlike stop) still scans and can find
		// nothing left to do.
		if scanned && eligible == 0 && len(active) == 0 && len(w.Items) == 0 &&
			r.epicInFlight == "" {
			r.run.Status = ledger.RunDrained
			_ = r.store.FinalizeRun(r.run)
			r.dropDemand() // withdraw from the fair-share denominator
			r.progress("drained: no ready work, no active slots")
			return r.outcome(ExitDrained, "drained", true), nil
		}

		// Gate with nothing active to finish: pause rather than spin. The
		// per-run budget cap and the quota governor both land here. (Operator
		// drain with nothing active already returned via gate.paused above; this
		// is defensive so the reason is never mislabeled quota-* if it weren't.)
		if !allowDispatch && r.liveActiveCount() == 0 && r.epicInFlight == "" {
			reason := "quota-" + string(level)
			if budgetHit {
				reason = "budget-cap"
			}
			if gate.uncalibratedBlock {
				reason = "governor-uncalibrated"
			}
			if gate.operatorDrain {
				reason = "operator-drain"
			}
			r.run.Status = ledger.RunPausedQuota
			_ = r.store.SaveRun(r.run)
			return r.outcome(ExitOK, reason, false), nil
		}

		// Preflight over the refill batch (loop mode only, calibrated +
		// enforcing governor only) — same primitive as waveLoop, just against
		// a (typically smaller) refill-sized batch, so a 1-bead refill can
		// proceed where an 8-bead wave estimate would have been refused.
		est := r.waveEstimate(w.Items)
		if allowDispatch && !r.opts.NoPreflight && !r.opts.Manual && calibrated && !gate.advisory && len(w.Items) > 0 {
			if ok, reason := quota.Preflight(usage, est, r.quotaCfg); !ok {
				allowDispatch = false
				r.progress("preflight refused refill: %s", reason)
				if len(active) == 0 {
					_ = r.store.FinalizeRun(r.run)
					return r.outcome(ExitOK, reason, false), nil
				}
			}
		}

		if allowDispatch {
			r.reportWaveSkips(w)

			if r.opts.DryRun {
				for _, it := range w.Items {
					model := it.Model
					if model == "" {
						model = "(stage default)"
					}
					r.progress("dry-run: would dispatch %s (%s) model %s footprint %s",
						it.Issue.ID, it.Issue.Title, model, it.Footprint)
				}
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "dry-run", false), nil
			}

			// Nothing dispatchable and nothing running: report, don't spin.
			// By construction this is only reached with eligible > 0 (the
			// drained check above already returned when eligible == 0), so
			// it means every ready item was deferred (footprint/container).
			if len(w.Items) == 0 && len(active) == 0 {
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "no dispatchable work (all ready items deferred)", false), nil
			}

			if len(w.Items) > 0 {
				logCoDispatch(r.run.RunID, r.opts.ProjectID, r.run.Wave, len(active), width)
				r.refreshDemand()
				r.warnIfOverFairShare()
				stagger := r.staggerDelay()
				dispatchedThisIter := 0
			dispatch:
				for i, it := range w.Items {
					// Per-run budget cap, re-checked per item (koryph-u7q): in-flight
					// estimates count toward projected spend, so a refill batch stops
					// the moment projected cost reaches the cap.
					if r.budgetExhausted() {
						r.progress("refill: run budget reached ($%.2f projected >= $%.2f cap) — deferring %d bead(s)",
							r.projectedRunCostUSD(), r.opts.BudgetUSD, len(w.Items)-i)
						break
					}
					if i > 0 && stagger > 0 {
						select {
						case <-ctx.Done():
							return r.interrupted()
						case <-time.After(stagger):
						}
					}
					// Machine admission (koryph-4ql.3, design L3): the global
					// concurrency cap / memory floor (koryph-930) still batch-BREAKS
					// the REST of this refill (the loop re-scans next tick), but a
					// per-bead resource-capacity or candidate-tipped-memory denial
					// SKIPS just this bead so the lightweight beads behind it still
					// refill. acquireGlobalSlot logs the skip reason (kind + holder);
					// the break message stays here. Rolling already ticks (waitTick),
					// so no wave-mode pacing sleep is needed here.
					kinds := it.Resources
					memReserveMB := r.resolveMemReserveMB(kinds)
					switch r.acquireGlobalSlot(it.Issue.ID, kinds, memReserveMB) {
					case admitSkip:
						continue
					case admitBreak:
						r.progress("refill: global governor cap or memory floor reached — deferring %d bead(s) to a later tick",
							len(w.Items)-i)
						break dispatch
					}
					r.issues[it.Issue.ID] = it.Issue
					fp := it.Footprint
					r.dispatchBead(ctx, dispatchReq{issue: it.Issue, epicID: it.EpicID, attempt: 1, footprint: &fp,
						resources: &dispatchResources{kinds: kinds, memReserveMB: memReserveMB}})
					dispatchedThisIter++
				}
				// run.Wave increments per refill that dispatches >= 1 bead
				// (design L1) — unlike waveLoop, which increments unconditionally
				// at the top of every iteration; rolling ticks with nothing to
				// dispatch are frequent (that is the point), so counting only
				// real refills keeps status/roster/IDE observability meaningful.
				if dispatchedThisIter > 0 {
					r.run.Wave++
					_ = r.store.SaveRun(r.run)
					r.progress("refill %d: %d ready, dispatched %d%s",
						r.run.Wave, w.ReadyCount, dispatchedThisIter, r.windowNote(calibrated, usage, est))
					logRefillDispatched(r.run.RunID, r.opts.ProjectID, r.run.Wave, dispatchedThisIter)
				}
			}
		}

		// One tick: wait for the poll timer or a SIGCHLD wake hint, then a
		// single poll pass — never block until the whole batch is idle (that
		// is the wave barrier this loop exists to remove).
		timerFired, err := r.waitTick(ctx, wake, interval)
		if err != nil {
			return r.interrupted()
		}
		probeProgress := false
		if timerFired {
			tick++
			probeProgress = progressProbeDue(tick)
		}
		r.pollPass(ctx, probeProgress)
	}
}
