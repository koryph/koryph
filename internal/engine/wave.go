// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/promptc"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/sched"
	"github.com/koryph/koryph/internal/worktree"
)

// loop selects the effective dispatch loop and runs it to completion
// (koryph-2im.3, design L1): "rolling" with !Once runs the continuous refill
// loop (rollingLoop); every other combination — "wave" mode, or rolling with
// --once — runs today's wave semantics unchanged (waveLoop). This is a
// selection made ONCE at loop entry, not a per-iteration branch: --once's
// contract ("one dispatch pass, poll to idle, exit") is identical in both
// modes, so it is simplest and safest to keep it on the well-tested wave path
// rather than special-case it inside rollingLoop.
func (r *runner) loop(ctx context.Context) (Outcome, error) {
	if r.dispatchMode() == "rolling" && !r.opts.Once {
		return r.rollingLoop(ctx)
	}
	return r.waveLoop(ctx)
}

// govGate is the per-wave/per-refill governor decision shared by waveLoop and
// rollingLoop (koryph-2im.3): quota level + calibration + usage snapshot,
// billing selection (a side effect on r.billing/r.apiKey), the effective
// (possibly quota-scaled) width, the per-run budget-cap check, and the
// quota-stop-with-nothing-active immediate pause. Both loops call
// governorGate identically so they cannot drift on governor semantics
// (design L1/I4).
type govGate struct {
	allowDispatch bool
	level         quota.Level
	calibrated    bool
	advisory      bool
	usage         quota.Usage
	budgetHit     bool
	// uncalibratedBlock is set when --require-calibration (or the project's
	// require_calibration) refuses dispatch because the governor is uncalibrated
	// (koryph-grz). Distinct reason so the pause is not mislabeled quota-*.
	uncalibratedBlock bool
	width             int

	// paused is set when the gate itself already finalized the run — either
	// paused-quota (quota-stop with nothing active) or operator-drain-with-
	// nothing-active below (koryph-57v.1; the latter finalizes DRAINED, not
	// paused, but shares the same "return outcome immediately" shape): the
	// caller must return outcome immediately without scanning the frontier,
	// exactly as the pre-koryph-2im.3 wave loop did (an early exit is cheaper
	// than building a wave that will never dispatch, and — more importantly —
	// must not be reordered after a drained check: quota-stop always wins over
	// drained when nothing is running, unlike drain/budget below).
	paused  bool
	outcome Outcome

	// operatorDrain is set whenever `koryph drain`'s sentinel is present this
	// boundary (koryph-57v.1), regardless of whether it already short-circuited
	// via paused above. It lets both loops' "nothing active, can't dispatch"
	// fallback (below the frontier scan) report the operator-drain reason
	// instead of a quota-* one, on the off chance the sentinel is consumed by
	// something else between this gate and that check.
	operatorDrain bool
}

// requireCalibration reports whether this run hard-blocks dispatch while the
// quota governor is uncalibrated (koryph-grz): the --require-calibration run
// flag or the project's require_calibration config. Opt-in; default false
// preserves the fresh-install "advisory, don't deadlock" behavior.
func (r *runner) requireCalibration() bool {
	return r.opts.RequireCalibration || (r.cfg != nil && r.cfg.RequireCalibration)
}

// governorGate runs the governor/billing/budget checks once per wave or
// refill. See govGate's doc for why this is factored out rather than
// duplicated between waveLoop and rollingLoop.
func (r *runner) governorGate(ctx context.Context) govGate {
	level, calibrated, usage := r.governor(ctx)
	advisory, advisoryWhy := r.guardMode(calibrated)
	switch {
	case r.authMode == registry.AuthModeAPIKey:
		// First-class api-key account (koryph-i3b, design §3/§7): bills
		// api-key from wave 1, independent of governor level/advisory — this
		// is a declared pay-per-token account, not the legacy break-glass
		// fallback under a still-subscription-verified account (which stays
		// gated by level==stop, below, and only ever applies to a
		// subscription account). billingFor's own first branch makes this
		// choice; routing through it here (rather than duplicating the
		// check) is what "billingFor coexists with the new mode" means.
		r.billing, r.apiKey = r.billingFor(level)
	case advisory:
		// Advisory: measure + log, never block, never switch billing.
		r.billing, r.apiKey = account.BillingSubscription, ""
		if level != quota.LevelOK {
			r.progress("billing guard ADVISORY (%s): governor level %s — not blocking", advisoryWhy, level)
		}
	default:
		r.billing, r.apiKey = r.billingFor(level)
	}

	// Cache for the health patrol (koryph-gus): patrol uses these rather than
	// re-running the quota snapshot every patrol cycle.
	r.lastQuotaLevel = level
	r.lastQuotaUsage = usage

	g := govGate{allowDispatch: true, level: level, calibrated: calibrated, advisory: advisory, usage: usage}

	// Uncalibrated governor (koryph-grz): the fresh-install state where both
	// ceilings are 0, so the governor cannot enforce the 5h/weekly spend ladder
	// and passes advisory. This USED to be silent (the advisory branch above
	// only logs when level != OK, but an uncalibrated account reports OK) — so
	// an operator had no signal spend limits weren't enforced. Warn loudly once
	// per run, and hard-block when the operator opted into --require-calibration.
	if !calibrated {
		if r.requireCalibration() {
			g.allowDispatch = false
			g.uncalibratedBlock = true
			if !r.uncalibratedWarned {
				r.uncalibratedWarned = true
				r.progress("!!! quota governor UNCALIBRATED for account %q and --require-calibration is set — refusing to dispatch. Run `koryph quota calibrate` (or `koryph calibrate`) to set a ceiling.", r.quotaName())
			}
		} else if !r.uncalibratedWarned {
			r.uncalibratedWarned = true
			r.progress("WARNING: quota governor UNCALIBRATED for account %q — 5h/weekly spend limits are NOT enforced this run. Run `koryph quota calibrate` (or `koryph calibrate`) to enable enforcement, or pass --require-calibration to hard-block until then.", r.quotaName())
		}
	}

	if !r.opts.Manual && !advisory {
		// eff is the effective ladder for logging threshold annotations.
		eff := quota.Ladder{}.Effective()
		if r.quotaCfg != nil {
			eff = r.quotaCfg.Ladder.Effective()
		}
		// frac is the fraction of the enforcing ceiling this run is at, for
		// logging. subscription/oauth-token report the 5h window fraction
		// (unchanged); a first-class api-key account reports spent$/rolling-$
		// ceiling — the ladder it is actually enforced on (koryph-i3b.9).
		frac := r.govFraction(usage)
		switch level {
		case quota.LevelStop:
			// Hard stop: interrupt active agents (SIGTERM — checkpoints; worktrees
			// preserved for resume) and park the run immediately. Skip ONLY for the
			// legacy break-glass api-key fallback (a subscription account that
			// switched to per-token billing AT stop precisely to keep going past
			// the exhausted subscription window — parking would defeat the opt-in).
			// A first-class api-key account (authMode == AuthModeAPIKey) whose
			// rolling-$ ceiling reached hard-stop DOES park: that is the rolling-$
			// ladder's terminal rung (koryph-i3b.9, design §7).
			legacyFallback := r.billing == account.BillingAPIKey && r.authMode != registry.AuthModeAPIKey
			if !legacyFallback {
				r.interruptActiveSlots()
				r.run.Status = ledger.RunHardStopQuota
				_ = r.store.SaveRun(r.run)
				r.progress("governor hard stop: run %s parked at %.0f%% (hard-stop %.0f%%) — active agents sent SIGTERM, worktrees preserved for resume",
					r.run.RunID, frac*100, eff.HardStop*100)
				g.paused = true
				g.outcome = r.outcome(ExitOK, "quota-hard-stop", false)
				return g
			}
		case quota.LevelDrain:
			g.allowDispatch = false
			r.progress("governor graceful stop: no new dispatch (%.0f%% >= graceful-stop %.0f%%); finishing %d active slot(s)",
				frac*100, eff.GracefulStop*100, r.activeCount())
		case quota.LevelThrottle:
			r.progress("governor throttle: slot scaling active (%.0f%% >= throttle %.0f%%)",
				frac*100, eff.Throttle*100)
		case quota.LevelWarn:
			r.progress("governor warn: usage at %.0f%% (warn %.0f%%, throttle %.0f%%, graceful-stop %.0f%%, hard-stop %.0f%%)",
				frac*100, eff.Warn*100, eff.Throttle*100, eff.GracefulStop*100, eff.HardStop*100)
		}
	}

	// Operator drain (koryph-57v.1): a one-shot sentinel written by
	// `koryph drain`, re-read here so BOTH loops honor it for free at every
	// boundary. Unlike quota drain/stop, this is a deliberate operator
	// instruction rather than an account-spend signal, so a drain found with
	// nothing active does not merely PAUSE the run — it finalizes through the
	// same normal drained-exit shape as an empty frontier and consumes the
	// sentinel itself, so a fresh run afterwards starts clean (see
	// ledger.Store.ConsumeDrain and the stale-sentinel clear at run start in
	// run.go). With something still active, it behaves exactly like governor
	// drain: no new dispatch, active slots finish untouched.
	if r.store.DrainRequested() {
		g.operatorDrain = true
		// liveActiveCount, not activeCount: an operator drain during a resume with
		// a parked backlog (SlotQueued) must finish cleanly rather than force-run
		// un-started recovery work — the queued beads carry no live agent, stay
		// SlotQueued in the drained run, and a later --resume re-adopts them
		// (koryph-bzf). With no backlog the two counts are equal, so the operator
		// drain contract and its tests are unchanged.
		if r.liveActiveCount() == 0 {
			r.run.Status = ledger.RunDrained
			_ = r.store.FinalizeRun(r.run)
			r.store.ConsumeDrain()
			r.dropDemand() // withdraw from the fair-share denominator, same as any drained exit
			r.progress("operator drain: run %s finished (no active slots) — sentinel consumed", r.run.RunID)
			g.paused = true
			g.outcome = r.outcome(ExitDrained, "operator-drain", true)
			return g
		}
		if g.allowDispatch {
			r.progress("operator drain: no new dispatch; finishing %d active slot(s)", r.activeCount())
		}
		g.allowDispatch = false
	}

	width := r.width
	// Operator resize (koryph-57v.1): re-read every boundary; an active
	// override replaces the configured base width BEFORE quota scaling, so
	// `koryph resize` takes effect on the very next boundary without a
	// restart. Clamping to the project cap (unless --force) happens once, at
	// write time (cmd/koryph), not here.
	//
	// But a resize override PERSISTS across runs (ledger.Store.LoadResize), so a
	// leftover `koryph resize` from a prior loop would otherwise silently pin a
	// brand-new run to that width even when the operator passed an explicit
	// --max — the trap koryph-bzf closes. resizeApplies honors a live resize of
	// THIS run (set after it started) unconditionally, but lets an explicit
	// --max outrank a STALE one; a one-time warning names the ignored override
	// so `koryph resize --clear` is discoverable.
	if ov, ok := r.store.LoadResize(); ok {
		if r.resizeApplies(ov) {
			width = ov.Max
		} else if !r.staleResizeWarned {
			r.staleResizeWarned = true
			r.progress("ignoring a persisted resize override (--max %d, set %s) in favor of this run's explicit --max %d — run `koryph resize --clear` to remove the stale override",
				ov.Max, ov.SetAt, r.width)
		}
	}
	if !r.opts.Manual && calibrated && !advisory {
		if scaled := quota.ScaleSlotsForAuthMode(r.authMode, usage, r.projectedRunCostUSD(), r.quotaCfg, width); scaled < width {
			width = scaled
		}
	}
	g.width = width

	// Per-run budget ceiling: once projected run cost (settled + in-flight
	// estimates, koryph-u7q) reaches the cap, stop starting new agents; any
	// active slots still finish.
	if r.opts.BudgetUSD > 0 {
		if spent := r.projectedRunCostUSD(); spent >= r.opts.BudgetUSD {
			g.budgetHit = true
			if g.allowDispatch {
				r.progress("run budget reached: $%.2f projected >= $%.2f cap — no new dispatch", spent, r.opts.BudgetUSD)
			}
			g.allowDispatch = false
		}
	}
	return g
}

// resizeApplies decides whether a persisted resize override governs this
// boundary's width (koryph-bzf). A resize written (or changed) DURING this run is
// a live `koryph resize` of the running loop and always applies — the
// without-restart feature (koryph-57v.1). One that is unchanged since this run
// started was inherited from a PRIOR run (the override persists across runs), and
// is honored only when the run did not state its own width: an explicit `--max`
// outranks a stale override so a new loop is never silently pinned to a prior
// run's `koryph resize` value.
//
// "Live vs inherited" is decided by comparing the override's SetAt to the
// snapshot captured at run start (startupResizeSetAt), NOT by ordering it against
// the run's StartedAt: the stored timestamps are second-resolution, so a live
// resize done within the same second as run start would be indistinguishable
// from an inherited one by ordering. A snapshot comparison has no such ambiguity
// — a live resize writes a fresh SetAt that cannot equal a value captured before
// it existed.
func (r *runner) resizeApplies(ov ledger.ResizeOverride) bool {
	if ov.SetAt != r.startupResizeSetAt {
		return true // set or changed during this run — a live koryph resize wins even over --max
	}
	return r.opts.Max <= 0 // inherited from a prior run: an explicit --max outranks it
}

// waveLoop runs waves until drained, quota pause, ctx cancellation, or (Once)
// exactly one wave has fully settled. Unchanged behavior from before
// koryph-2im.3 — only the governor/billing/budget block was extracted into
// governorGate (shared with rollingLoop); every decision and its ordering is
// identical.
func (r *runner) waveLoop(ctx context.Context) (Outcome, error) {
	for {
		if ctx.Err() != nil {
			return r.interrupted()
		}
		syncObsConfig() // pick up `koryph obs level` changes without a restart
		// Heartbeat snapshot (koryph-lwnq) — see poll.go's identical call for why
		// this is safe from the loop goroutine. wave mode also polls to idle
		// inside pollUntilIdle below, which refreshes this every poll tick too.
		r.hb.setCounts(r.activeCount(), r.lastReadyCount, r.run.Wave)
		r.patrolIfDue(ctx)
		r.applyOperatorOverrides()
		r.run.Wave++
		_ = r.store.SaveRun(r.run)

		gate := r.governorGate(ctx)
		if gate.paused {
			return gate.outcome, nil
		}
		allowDispatch := gate.allowDispatch
		level, calibrated, usage, budgetHit := gate.level, gate.calibrated, gate.usage, gate.budgetHit
		width := gate.width

		// Promote resume-backlog beads (SlotQueued, parked by resume() when the
		// effective width could not admit them all) into live dispatches before
		// scanning the frontier, so recovered work takes priority and is capped
		// at the CURRENT width, not the stalled run's (koryph-bzf). A no-op on
		// every non-resume wave. A backlog it could not place this boundary is
		// detected below (liveActiveCount==0 with a backlog still present) for
		// the hot-spin guard.
		r.drainResumeBacklog(ctx, width, allowDispatch)

		// Boundary admission tally (koryph-4ql.3): dispatchedThisBoundary counts
		// beads actually admitted this wave; deniedThisBoundary records whether any
		// admission denial (skip OR break) occurred. Together they drive the
		// wave-mode pacing sleep below (dispatched nothing, nothing active, but a
		// denial happened → sleep one tick instead of hot-spinning).
		dispatchedThisBoundary := 0
		deniedThisBoundary := false

		// Frontier scan + wave build.
		issues, err := r.adapter.Ready(ctx, beads.ReadyOpts{Parent: r.opts.Parent})
		if err != nil {
			return r.outcome(ExitFatal, "bd ready failed", false), fmt.Errorf("engine: bd ready: %w", err)
		}
		// Operator-injected beads widen the frontier past the --parent scope
		// without a restart (D10) — merged before --only narrows, so an explicit
		// single-bead run is not overridden.
		issues = r.applyInjections(ctx, issues)
		// --only narrows the frontier to a single operator-chosen bead; once it
		// closes it drops out of `bd ready` and the run drains.
		if r.opts.Only != "" {
			issues = onlyBead(issues, r.opts.Only)
		}
		// Learned-model pass (koryph-qf6.6): stamp escalation-history
		// recommendations onto the frontier BEFORE the wave builds, so this
		// very wave routes on them.
		issues = r.applyLearnedModels(ctx, issues)
		issues, prerequisiteDeferred := r.applyPrerequisitePolicy(ctx, issues)
		issues, runtimeSkipped := r.applyRuntimePolicy(issues)
		active := r.activeIDs()
		// Cap the frontier at the FREE capacity, not the raw width (koryph-bzf).
		// Historically wave mode entered each iteration fully idle (pollUntilIdle
		// drained to zero active), so width and capacity were equal — but a resume
		// carries live reattached slots and a SlotQueued backlog into the first
		// iterations, and pollUntilIdle now returns while backlog remains, so
		// active can be > 0 here. Without this the frontier would dispatch `width`
		// MORE beads on top of the already-occupied slots, overshooting the cap
		// exactly as the old resume did. Build only when a slot is actually free —
		// BuildWave reads Max<=0 as UNLIMITED, so a zero-capacity boundary must
		// skip the build (empty wave) rather than pass 0, mirroring rollingLoop.
		// eligible is still computed below (independent of Max) for the drained
		// decision. Inert whenever active is empty (every steady-state wave).
		capacity := width - len(active)
		var w sched.Wave
		if capacity > 0 {
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
			w.Skipped = append(runtimeSkipped, w.Skipped...)
			w.Deferred = append(prerequisiteDeferred, w.Deferred...)
			r.captureFrontier(w)
		}

		eligible := 0
		for _, iss := range issues {
			if ok, _ := sched.Eligible(iss, active); ok {
				eligible++
			}
		}
		r.lastReadyCount = eligible

		// Drained: nothing eligible, nothing active, nothing batched.
		if eligible == 0 && len(active) == 0 && len(w.Items) == 0 {
			if len(prerequisiteDeferred) > 0 {
				r.reportWaveSkips(w)
				r.run.Status = ledger.RunDone
				_ = r.store.FinalizeRun(r.run)
				r.progress("no ready work has prerequisites present in %s (%d bead(s) deferred)",
					r.rec.DefaultBranch, len(prerequisiteDeferred))
				return r.outcome(ExitOK, "ready work has missing prerequisite commits", false), nil
			}
			if len(runtimeSkipped) > 0 {
				r.reportWaveSkips(w)
				r.run.Status = ledger.RunDone
				_ = r.store.FinalizeRun(r.run)
				r.progress("runtime-only %s: no ready work selected (%d bead(s) skipped)", r.opts.RuntimeOnly, len(runtimeSkipped))
				return r.outcome(ExitOK, "no ready work for runtime-only policy", false), nil
			}
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
		//
		// liveActiveCount (not len(active)) so a resume backlog that dispatch is
		// currently forbidden from placing (SlotQueued reserves a width slot but
		// runs no agent) parks the run rather than spinning: the queued beads
		// stay SlotQueued and a later --resume re-adopts them (koryph-bzf). With
		// no backlog the two counts are identical, so this is inert otherwise.
		if !allowDispatch && r.liveActiveCount() == 0 {
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

		// Preflight (loop mode only, calibrated + enforcing governor only).
		est := r.waveEstimate(w.Items)
		if allowDispatch && !r.opts.NoPreflight && !r.opts.Manual && calibrated && !gate.advisory && len(w.Items) > 0 {
			if ok, reason := quota.PreflightForAuthMode(r.authMode, usage, r.projectedRunCostUSD(), est, r.quotaCfg); !ok {
				allowDispatch = false
				r.progress("preflight refused wave: %s", reason)
				if len(active) == 0 {
					_ = r.store.FinalizeRun(r.run)
					return r.outcome(ExitOK, reason, false), nil
				}
			}
		}

		if allowDispatch {
			if err := r.validateEquivalentModels(w.Items); err != nil {
				return r.outcome(ExitFatal, "runtime equivalency failed", false), err
			}
			r.progress("wave %d: %d ready, dispatching %d%s",
				r.run.Wave, w.ReadyCount, len(w.Items), r.windowNote(calibrated, usage, est))
			r.reportWaveSkips(w)

			if r.opts.DryRun {
				for _, it := range w.Items {
					runtimeName, runtimeWhy := r.effectiveRuntimeFor(it.Issue)
					res, effort, err := r.resolveModel(dispatchReq{issue: it.Issue}, runtimeName)
					if err != nil {
						return r.outcome(ExitFatal, "runtime equivalency failed", false), err
					}
					r.progress("dry-run: would dispatch %s (%s) runtime %s (%s) model %s effort %s rationale %s footprint %s",
						it.Issue.ID, it.Issue.Title, runtimeName, runtimeWhy, res.Model, effort, res.Rationale, it.Footprint)
				}
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "dry-run", false), nil
			}

			// Nothing dispatchable and nothing running: report, don't spin.
			if len(w.Items) == 0 && len(active) == 0 {
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "no dispatchable work (all ready items deferred)", false), nil
			}

			logCoDispatch(r.run.RunID, r.opts.ProjectID, r.run.Wave, len(active), width)
			r.refreshDemand()
			r.warnIfOverFairShare()
			stagger := r.staggerDelay()
		dispatch:
			for i, it := range w.Items {
				// Per-run budget cap, re-checked per item (koryph-u7q): each
				// dispatch above added its estimate to projected spend (via the
				// now-running slot), so a single wide wave stops the moment
				// projected cost reaches the cap instead of dispatching the whole
				// batch and only settling past it later.
				if r.budgetExhausted() {
					r.progress("wave %d: run budget reached ($%.2f projected >= $%.2f cap) — deferring %d bead(s)",
						r.run.Wave, r.projectedRunCostUSD(), r.opts.BudgetUSD, len(w.Items)-i)
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
				// concurrency cap / memory floor (koryph-930) still batch-BREAKS —
				// same-project shares won't free up until a running agent finishes,
				// and neither will memory — but a per-bead resource-capacity or
				// candidate-tipped-memory denial SKIPS just this bead so the
				// lightweight beads behind it still dispatch. acquireGlobalSlot logs
				// the skip reason (naming the kind + holder); the break message
				// stays here, verbatim.
				kinds := it.Resources
				memReserveMB := r.resolveMemReserveMB(kinds)
				switch r.acquireGlobalSlot(it.Issue.ID, kinds, memReserveMB) {
				case admitSkip:
					deniedThisBoundary = true
					continue
				case admitBreak:
					deniedThisBoundary = true
					r.progress("wave %d: global governor cap or memory floor reached — deferring %d bead(s) to a later wave",
						r.run.Wave, len(w.Items)-i)
					break dispatch
				}
				r.issues[it.Issue.ID] = it.Issue
				fp := it.Footprint
				r.dispatchBead(ctx, dispatchReq{issue: it.Issue, epicID: it.EpicID, attempt: 1, footprint: &fp,
					resources: &dispatchResources{kinds: kinds, memReserveMB: memReserveMB}})
				dispatchedThisBoundary++
			}
			// Emit refill dispatched count for the structured log record.
			logRefillDispatched(r.run.RunID, r.opts.ProjectID, r.run.Wave, len(w.Items))
		}

		// Wave-mode boundary pacing (koryph-4ql.3, design L3/R3): a boundary that
		// dispatched nothing with nothing active AND saw at least one admission
		// denial would otherwise re-scan in a tight loop — tolerable for a cap
		// denial that clears in minutes, a hot-spin for a capacity-1 resource held
		// for hours by another project. Sleep one poll tick so I6's "retried at the
		// next boundary" has a defined cadence. Rolling mode already ticks
		// (waitTick), so this is wave-mode only. ctx cancellation is honored, like
		// the stagger select above.
		if dispatchedThisBoundary == 0 && len(active) == 0 && deniedThisBoundary {
			select {
			case <-ctx.Done():
				return r.interrupted()
			case <-time.After(r.pollInterval()):
			}
		}

		// Resume-backlog pacing (koryph-bzf): the same hot-spin guard for a
		// backlog the drain could not place this boundary (a pool-cap break OR a
		// per-bead resource skip) with no live slot to wait on — pollUntilIdle
		// returns immediately when nothing is live, so without this the outer loop
		// would re-scan tightly. Distinct from the tally above because SlotQueued
		// backlog keeps len(active) > 0 while liveActiveCount is 0. Reached only
		// with dispatch allowed (an unplaceable backlog under a dispatch ban
		// already paused via liveActiveCount above), so it always denotes a real
		// retry-later condition. Retries the drain at a defined cadence.
		if r.liveActiveCount() == 0 && len(r.queuedResumeIDs()) > 0 {
			select {
			case <-ctx.Done():
				return r.interrupted()
			case <-time.After(r.pollInterval()):
			}
		}

		// Poll this wave's slots (and any adopted ones) to a terminal state.
		if err := r.pollUntilIdle(ctx); err != nil {
			return r.interrupted()
		}
		if r.capabilityBlocked {
			return r.capabilityHandoff()
		}

		if r.opts.Once {
			_ = r.store.FinalizeRun(r.run)
			return r.outcome(ExitOK, "", false), nil
		}
	}
}

// governor loads the current quota config and measures usage. An account that
// has never been calibrated (both ceilings zero) short-circuits to an
// advisory LevelOK without measuring — quota.State would report the same
// verdict, and skipping the snapshot avoids a pointless (and possibly slow)
// ccusage/transcript probe.
func (r *runner) governor(ctx context.Context) (quota.Level, bool, quota.Usage) {
	if cfgQ, err := quota.LoadConfig(r.quotaName()); err == nil {
		r.quotaCfg = cfgQ
	}
	// First-class api-key account (koryph-i3b.9, design §7): the governor reads
	// the ladder off spent$/RollingCeilingUSD, not the 5h/weekly subscription
	// window (which does not apply). Short-circuit to advisory when no rolling-$
	// ceiling is configured — the api-key analogue of the uncalibrated escape
	// below — and skip the (subscription-only) usage snapshot entirely, since
	// StateForAuthMode ignores it for api-key. spent is the run's own tracked
	// pay-per-token cost (projectedRunCostUSD).
	if r.authMode == registry.AuthModeAPIKey {
		if !quota.RollingCalibrated(r.quotaCfg) {
			return quota.LevelOK, false, quota.Usage{Account: r.quotaCfg.Account}
		}
		u := quota.Usage{Account: r.quotaCfg.Account}
		level, calibrated := quota.StateForAuthMode(r.authMode, u, r.projectedRunCostUSD(), r.quotaCfg)
		return level, calibrated, u
	}
	if r.quotaCfg.WindowCeilingUSD <= 0 && r.quotaCfg.WeeklyCeilingUSD <= 0 {
		return quota.LevelOK, false, quota.Usage{Account: r.quotaCfg.Account}
	}
	u, _ := quota.Snapshot(ctx, r.profile, r.quotaCfg)
	quota.LogUsage(u, r.quotaCfg)
	level, calibrated := quota.StateForAuthMode(r.authMode, u, r.projectedRunCostUSD(), r.quotaCfg)
	return level, calibrated, u
}

// govFraction is the fraction of the enforcing ceiling this run is at, for
// governor log lines. subscription/oauth-token accounts report the worse of
// the 5h/weekly window fractions (the value the ladder is evaluated against,
// unchanged from before koryph-i3b.9); a first-class api-key account has no
// subscription window, so it reports spent$/RollingCeilingUSD instead — the
// same rolling-$ fraction StateForAuthMode/ScaleSlotsForAuthMode enforce on.
func (r *runner) govFraction(u quota.Usage) float64 {
	if r.authMode == registry.AuthModeAPIKey {
		return quota.RollingFraction(r.projectedRunCostUSD(), r.quotaCfg)
	}
	return u.Window5h.Fraction()
}

// guardMode resolves whether the billing guard's throttling constraints are
// advisory for this run, and why. Precedence: run flag > project registry
// setting > runtime usage-source capability > baseline (uncalibrated
// governor). Enforced is the default.
//
// The runtime-capability check (koryph-v8u.5) is the quota-gating half of
// this bead: a runtime whose Capabilities().UsageSource is false has no
// fail-closed usage measurement (see internal/quota's ccusage/transcript
// sources, which remain claude-only), so the governor's warn/drain/stop
// enforcement would otherwise block dispatch against an unmeasured account —
// ADVISORY is the only honest posture until that runtime has a real usage
// source. Claude reports UsageSource true, so this branch is a no-op for
// every project today; it only changes behavior for a future non-claude
// runtime.
func (r *runner) guardMode(calibrated bool) (advisory bool, why string) {
	if r.opts.NoBillingGuard {
		return true, "--no-billing-guard"
	}
	if r.rec.BillingGuard == "advisory" {
		return true, "project billing_guard=advisory"
	}
	// Live toggle: operator wrote guard advisory/off via `koryph quota guard`.
	// The config is re-read by governor() at every wave boundary, so this takes
	// effect on the very next wave without a restart. (koryph-i25)
	if r.quotaCfg != nil {
		if ok, reason := quota.ConfigGuardAdvisory(r.quotaCfg, time.Now()); ok {
			return true, reason
		}
	}
	if r.rt != nil && !r.rt.Capabilities().UsageSource {
		return true, fmt.Sprintf("runtime %q has no usage source (measured advisory only)", r.rt.Name())
	}
	if !calibrated {
		return true, "baseline: governor uncalibrated"
	}
	return false, ""
}

// billingFor selects the billing mode for this wave (koryph-i3b, design
// §3/§7). Two independent paths, checked in order:
//
//  1. First-class api-key account (r.authMode == AuthModeAPIKey): api-key
//     billing unconditionally, level-independent, from wave 1.
//  2. Legacy break-glass fallback, UNCHANGED: subscription always, EXCEPT
//     at governor stop when the operator explicitly opted into api-key
//     fallback (flag + registry policy + resolvable key). Logged loudly:
//     for a subscription account this remains the only path to per-token
//     spend.
func (r *runner) billingFor(level quota.Level) (account.BillingMode, string) {
	// First-class api-key account (koryph-i3b, design §3/§7): billing is
	// ALWAYS api-key, from wave 1, level-independent — a declared
	// pay-per-token account, not a break-glass fallback. r.credential was
	// resolved and verified once at Run() setup (account.VerifyAuth +
	// ResolveCredential); this coexists with, and is checked BEFORE, the
	// legacy triple-AND fallback below, which remains reachable only for a
	// subscription account (r.authMode == AuthModeSubscription, the
	// default) and is otherwise byte-for-byte unchanged.
	if r.authMode == registry.AuthModeAPIKey && r.credential != "" {
		return account.BillingAPIKey, r.credential
	}
	if level == quota.LevelStop && r.opts.AllowAPISpend &&
		r.rec.APIFallback == "explicit" && r.rec.APIKeyEnvVar != "" {
		if key := os.Getenv(r.rec.APIKeyEnvVar); key != "" {
			r.progress("!!! governor stop: switching to API-KEY billing from $%s (explicit opt-in) — per-token spend ahead", r.rec.APIKeyEnvVar)
			return account.BillingAPIKey, key
		}
	}
	return account.BillingSubscription, ""
}

// runCostUSD is the cumulative recorded (settled) cost of every slot in this
// run. It only reflects attempts that have COMPLETED — a running agent's cost
// lands in CostUSD at completeSlot — so on its own it reads $0 for a wave that
// is still in flight. Use projectedRunCostUSD for budget admission.
func (r *runner) runCostUSD() float64 {
	var total float64
	for _, sl := range r.run.Slots {
		if sl != nil {
			total += sl.CostUSD
		}
	}
	return total
}

// projectedRunCostUSD is the budget-admission figure (koryph-u7q): settled cost
// PLUS each still-live slot's dispatch-time estimate. Without the in-flight
// term, runCostUSD reads $0 until agents complete, so a whole wave (or a
// requeue) could be admitted after the cap was already committed and only settle
// past it later — the "budget sails past the cap" bug. EstimateUSD is the
// per-attempt estimate stamped at dispatch (bias-corrected when samples exist),
// and CostUSD already carries prior attempts' accumulated cost, so a live
// slot contributes prior-spend + this-attempt-estimate.
//
// "Live" is SlotRunning OR SlotStuck: pollSlot (poll.go) flips a running
// slot's status to SlotStuck when it goes quiet, but the process (and its
// spend) is still alive — koryph never interrupts a running agent on its
// own. A stuck-but-alive slot dropping out of this sum the moment it is
// flagged would let projected cost fall back below the cap while the agent
// keeps accruing real spend, breaching --budget silently.
func (r *runner) projectedRunCostUSD() float64 {
	var total float64
	for _, sl := range r.run.Slots {
		if sl == nil {
			continue
		}
		total += sl.CostUSD
		if sl.Status == ledger.SlotRunning || sl.Status == ledger.SlotStuck {
			total += sl.EstimateUSD
		}
	}
	return total
}

// budgetExhausted reports whether the per-run --budget ceiling is set and
// projected spend has reached it — the shared admission predicate for both
// fresh dispatch and requeue (koryph-u7q). Zero/absent BudgetUSD means no cap.
func (r *runner) budgetExhausted() bool {
	return r.opts.BudgetUSD > 0 && r.projectedRunCostUSD() >= r.opts.BudgetUSD
}

// onlyBead narrows a frontier to the single bead with id, or empty when it is
// not currently ready.
func onlyBead(issues []beads.Issue, id string) []beads.Issue {
	for _, iss := range issues {
		if iss.ID == id {
			return []beads.Issue{iss}
		}
	}
	return nil
}

// waveEstimate sums the per-item bias-corrected cost estimates for a
// candidate wave, pricing each item against ITS OWN resolved runtime
// (koryph-v8u.12) via the same bead `runtime:<name>` label / project
// default_runtime precedence dispatchBead itself applies
// (modelroute.ResolveRuntimeName) — so a wave mixing a runtime:<name> bead
// alongside claude beads estimates each against the right per-runtime base
// table instead of always assuming claude's. Also prices each item against
// ITS OWN holdout-arm assignment (koryph-3l1.3, registry.AgentProxy.ArmFor)
// so a wave estimate is never systematically off by the proxied/holdout
// split — the same arm computation dispatchBead itself will make for each
// of these items when it actually dispatches them.
//
// Bias correction (koryph-6bl): once enough observations accumulate for a
// (tier, size[, @proxyID]) bucket the corrected estimate replaces the raw
// base, so systematic under/over-estimation self-corrects instead of
// persisting.
// captureFrontier records the wave's per-candidate dispatch verdict to the run
// ledger so `koryph status --frontier` can explain the frontier (D7/D9). The
// scheduler already separates the ready beads it selected (Items) from those it
// deferred (footprint / resource / wave-full) and skipped (structurally
// non-dispatchable), each with a reason — this persists the full set rather than
// leaving it to truncate in the "deferred N beads, +26 more" progress line.
// Overwrites the previous wave's snapshot; best-effort, a save error never
// blocks the loop. bd-dependency-blocked beads are upstream of the ready
// frontier and so are not part of the wave.
func (r *runner) captureFrontier(w sched.Wave) {
	if r.run == nil {
		return
	}
	entries := make([]ledger.FrontierEntry, 0, len(w.Items)+len(w.Deferred)+len(w.Blocked)+len(w.Skipped))
	for _, it := range w.Items {
		entries = append(entries, ledger.FrontierEntry{BeadID: it.Issue.ID, Title: it.Issue.Title, Verdict: "dispatched"})
	}
	for _, d := range w.Deferred {
		entries = append(entries, ledger.FrontierEntry{BeadID: d.ID, Title: d.Title, Verdict: "deferred", Reason: d.Reason})
	}
	for _, b := range w.Blocked {
		entries = append(entries, ledger.FrontierEntry{BeadID: b.ID, Title: b.Title, Verdict: "blocked", Reason: b.Reason})
	}
	for _, s := range w.Skipped {
		entries = append(entries, ledger.FrontierEntry{BeadID: s.ID, Title: s.Title, Verdict: "skipped", Reason: s.Reason})
	}
	r.run.Frontier = &ledger.FrontierSnapshot{
		At:      time.Now().UTC().Format(time.RFC3339),
		Wave:    r.run.Wave,
		Entries: entries,
	}
	_ = r.store.SaveRun(r.run)
}

func (r *runner) waveEstimate(items []sched.Item) float64 {
	var est float64
	for _, it := range items {
		model := it.Model
		if model == "" {
			model = modelroute.TierSonnet
		}
		runtimeName, _ := r.effectiveRuntimeFor(it.Issue)
		// r.rec is nil in some estimator-only unit tests that build a bare
		// &runner{cfg:..., quotaCfg:...} (no registry record) — guard rather
		// than dereference, matching health.go's existing "r.rec != nil"
		// defensive precedent. A nil rec has no agent_proxy either way, so ""
		// (direct) is the correct fallback, not merely a crash-avoidance one.
		var proxyID string
		if r.rec != nil {
			proxyID, _ = r.rec.AgentProxy.ArmFor(it.Issue.ID)
		}
		corrected, _ := quota.EstimateItemCorrectedForRuntimeProxy(r.quotaCfg, runtimeName, model, quota.SizeOf(len(it.Issue.Description)), proxyID)
		est += corrected
	}
	return est
}

// itemEstimate returns the bias-corrected estimate for a single bead, using
// the same model/runtime/size logic as waveEstimate (koryph-6bl), segmented
// by proxyID (koryph-3l1.3) — the caller passes the SAME arm-assigned
// proxyID it is about to stamp on the ledger slot, so the estimate and the
// eventual actual (recorded via quota.RecordForProxy) land in the same
// calibration population. This is called at dispatch time so the estimate
// can be persisted on the ledger slot alongside the eventual actual, making
// estimator error observable.
func (r *runner) itemEstimate(iss beads.Issue, model, runtimeName, proxyID string) float64 {
	if model == "" {
		model = modelroute.TierSonnet
	}
	corrected, _ := quota.EstimateItemCorrectedForRuntimeProxy(r.quotaCfg, runtimeName, model, quota.SizeOf(len(iss.Description)), proxyID)
	return corrected
}

// windowNote renders the estimate/usage suffix for the wave progress line.
// When MAPE data is available for the dominant tier in the wave it appends
// a confidence hint so the operator can see how accurate the estimate
// historically is, e.g. "(est $1.65 +/-40% / window 3%)" (koryph-6bl).
func (r *runner) windowNote(calibrated bool, u quota.Usage, est float64) string {
	if !calibrated {
		return " (governor uncalibrated)"
	}
	mapeHint := r.waveMAPEHint()
	if mapeHint != "" {
		return fmt.Sprintf(" (est $%.2f %s/ window %.0f%%)", est, mapeHint, u.Window5h.Fraction()*100)
	}
	return fmt.Sprintf(" (est $%.2f / window %.0f%%)", est, u.Window5h.Fraction()*100)
}

// waveMAPEHint returns a "+/-X%" confidence string derived from the median
// MAPE across error-stat buckets that have enough observations, or "" when
// no data is available yet. The hint is intentionally coarse (rounded to the
// nearest 5%) to avoid false precision (koryph-6bl).
func (r *runner) waveMAPEHint() string {
	if r.quotaCfg == nil || len(r.quotaCfg.ErrorStats) == 0 {
		return ""
	}
	var total float64
	var count int
	for _, es := range r.quotaCfg.ErrorStats {
		if es != nil && es.N >= quota.BiasCorrectionThreshold {
			total += es.MAPE
			count++
		}
	}
	if count == 0 {
		return ""
	}
	mean := total / float64(count)
	// Round to nearest 5%.
	rounded := math.Round(mean/5) * 5
	if rounded < 5 {
		return ""
	}
	return fmt.Sprintf("+/-%.0f%% ", rounded)
}

// activeFootprints derives sched.BuildWave's in-flight footprint set from the
// currently non-terminal slots (koryph-2im.1, design L2). Each slot's
// footprint prefers the value persisted at dispatch time (koryph-2im.3,
// design L2 footprint persistence: ledger.Slot.Footprint) and only falls back
// to recomputing from the bead's current labels when nothing was persisted
// (a slot dispatched before koryph-2im.3, or one whose ledger predates the
// field — Slot.Footprint unmarshals to nil there, additive-compatible).
// Preferring the persisted value is what makes in-flight gating EXACT rather
// than approximate: a relabel after dispatch (or a requeue that refreshes
// r.issues from bd) must not retroactively change what a LIVE slot is
// understood to conflict with — the slot's footprint is fixed at the moment
// it was admitted, exactly like the RW lock it stands in for.
//
// r.issueFor already implements the fallback chain the design calls for
// (in-memory wave item → adapter.Show → a synthetic id-only issue with no
// labels) for exactly this "recover the bead behind a slot" purpose
// (internal/engine/recover.go). A synthetic no-label issue resolves to a
// write-only TokenUnknown footprint by construction (see FootprintFor), so
// reusing it here already yields the maximally-conservative fallback the
// spec asks for on any Show failure — no separate error path needed.
//
// This closes a latent gap (called out in the design doc): on --resume,
// adopted slots were previously excluded from a new wave only by id, so a
// freshly built wave could conflict with an adopted slot's real footprint.
// With Active wired in, the resume path is footprint-correct by construction.
func (r *runner) activeFootprints(ctx context.Context, activeIDs map[string]bool) map[string]sched.Footprint {
	if len(activeIDs) == 0 {
		return nil
	}
	out := make(map[string]sched.Footprint, len(activeIDs))
	for id := range activeIDs {
		sl := r.run.Slots[id]
		if sl == nil {
			continue
		}
		if sl.Footprint != nil {
			out[id] = *sl.Footprint
			continue
		}
		iss := r.issueFor(ctx, sl)
		out[id] = sched.FootprintFor(iss, r.cfg)
	}
	return out
}

// resolveDispatchResources returns the frozen resource kinds + memory
// reservation for a dispatch (koryph-4ql.3, design L2/L3). A requeue (or the
// fresh loop path) supplies q.resources, resolved once when the slot was first
// admitted and carried verbatim so a relabel/vocabulary edit mid-run cannot
// re-price a live slot (I8). Only a path that supplied none recomputes from the
// bead's live labels — the same asymmetry-free fallback footprint uses. This is
// the single seam the freeze test drives (mirrors resolveModel).
func (r *runner) resolveDispatchResources(q dispatchReq) (kinds []string, memReserveMB int) {
	if q.resources != nil {
		return q.resources.kinds, q.resources.memReserveMB
	}
	kinds = sched.ResourcesFor(q.issue)
	return kinds, r.resolveMemReserveMB(kinds)
}

// activeResources derives sched.BuildWave's in-flight resource holdings from the
// currently non-terminal slots (koryph-4ql.3, design L3), the resource mirror of
// activeFootprints. Each slot's kinds prefer the value PERSISTED at dispatch
// (ledger.Slot.Resources) and fall back to recomputing sched.ResourcesFor from
// the recovered issue only when nothing was persisted (a slot dispatched before
// this bead, or a ledger predating the field).
//
// NOTE the asymmetry with activeFootprints, and why persistence is LOAD-BEARING
// here rather than a fast path: activeFootprints' terminal fallback (an
// unrecoverable bead) degrades to the maximally-CONSERVATIVE domain:unknown
// (via issueFor's synthetic no-label issue → FootprintFor's TokenUnknown),
// whereas ResourcesFor of that same no-label issue yields the EMPTY set —
// maximally PERMISSIVE (L1's inverted default: undeclared means "agent +
// worktree only", not "unknown, serialize"). So a slot whose bead can no longer
// be recovered contributes NO holdings; only Slot.Resources keeps in-flight
// resource gating exact across --resume and requeue.
func (r *runner) activeResources(ctx context.Context, activeIDs map[string]bool) map[string][]string {
	if len(activeIDs) == 0 {
		return nil
	}
	out := make(map[string][]string, len(activeIDs))
	for id := range activeIDs {
		sl := r.run.Slots[id]
		if sl == nil {
			continue
		}
		if len(sl.Resources) > 0 {
			out[id] = sl.Resources
			continue
		}
		if kinds := sched.ResourcesFor(r.issueFor(ctx, sl)); len(kinds) > 0 {
			out[id] = kinds
		}
	}
	return out
}

// childLister adapts beads.ListChildren for sched.BuildWave. Adapter errors
// are treated as "no children" so a bd hiccup cannot wedge the wave.
func (r *runner) childLister(ctx context.Context) func(string) (bool, error) {
	return func(id string) (bool, error) {
		kids, err := r.adapter.ListChildren(ctx, id)
		if err != nil {
			return false, nil
		}
		for _, k := range kids {
			if k.Status != "closed" && k.Status != "done" {
				return true, nil
			}
		}
		return false, nil
	}
}

// dispatchResources is a slot's frozen external-resource claim (koryph-4ql.3,
// design L2/L3): the resolved kinds and the memory reservation summed over them.
// Threaded through dispatchReq so the value acquireGlobalSlot admits is the same
// value the ledger slot persists and every requeue re-attaches (I8).
type dispatchResources struct {
	kinds        []string
	memReserveMB int
}

// resourcesFromSlot rebuilds the frozen resource claim from a persisted slot for
// a requeue's dispatchReq (koryph-4ql.3), the resource sibling of the
// footprint-forwarding requeues use: the resolved kinds + reservation carried
// verbatim so the requeue re-attaches exactly what the slot was admitted with.
// Returns nil when the slot declared nothing (or predates the fields), which
// dispatchBead treats as "resolve from live labels" — harmless for a truly
// resource-free bead.
func resourcesFromSlot(sl *ledger.Slot) *dispatchResources {
	if len(sl.Resources) == 0 && sl.MemReserveMB == 0 {
		return nil
	}
	return &dispatchResources{kinds: sl.Resources, memReserveMB: sl.MemReserveMB}
}

// beadFeatures is the similarity feature vector persisted on the ledger slot
// (koryph-qf6.3): the bead's labels, size bucket, and issue type as they were
// at FIRST dispatch. Frozen exactly like footprint/resources — a relabel
// mid-run must not rewrite what a live slot is understood to look like, and
// the outcome learner (koryph-qf6.6) must join outcomes to the features the
// bead was ROUTED with.
type beadFeatures struct {
	labels    []string
	sizeClass string
	issueType string
}

// featuresFromSlot rebuilds the frozen feature vector from a persisted slot
// for a requeue's dispatchReq, the features sibling of resourcesFromSlot.
// Returns nil when the slot carries none (a ledger that predates the fields),
// which dispatchBead treats as "derive from the live issue".
func featuresFromSlot(sl *ledger.Slot) *beadFeatures {
	if len(sl.BeadLabels) == 0 && sl.SizeClass == "" && sl.IssueType == "" {
		return nil
	}
	return &beadFeatures{labels: sl.BeadLabels, sizeClass: sl.SizeClass, issueType: sl.IssueType}
}

// featuresFor resolves the feature vector for a dispatch: the frozen value a
// requeue threaded through (q.features), or — on a fresh dispatch — a snapshot
// of the live issue.
func featuresFor(q dispatchReq) *beadFeatures {
	if q.features != nil {
		return q.features
	}
	return &beadFeatures{
		labels:    q.issue.Labels,
		sizeClass: quota.SizeOf(len(q.issue.Description)),
		issueType: q.issue.IssueType,
	}
}

// dispatchReq describes one dispatch (fresh, requeue, or review bounce).
type dispatchReq struct {
	issue           beads.Issue
	epicID          string
	attempt         int
	resumeSHA       string
	resumeSessionID string
	reviewPath      string
	reviewIters     int
	// gateRequeues, mergeRequeues, conflictRequeues, and rateLimitRequeues
	// carry the requeue-budget counters forward across a requeue dispatch
	// (koryph-2im.6, koryph-2im.4, koryph-qf6.1): dispatchBead below builds a
	// fresh ledger.Slot rather than mutating the old one, so — the same way
	// reviewIters is threaded through — these must be passed explicitly or
	// each budget would reset to zero every requeue. Every requeue path must
	// thread ALL of them, not just the one it increments: requeue causes
	// interleave (a conflict requeue can be followed by a gate requeue), and
	// a path that drops the counters it doesn't own silently refills those
	// budgets (koryph-qf6.1: ConflictRequeues was never threaded at all, so
	// its budget could never bind across the slot replacement).
	gateRequeues      int
	mergeRequeues     int
	conflictRequeues  int
	rateLimitRequeues int
	// budgetKillRequeues carries the warm-resume budget-kill counter forward
	// (koryph-77r.10), the same way rateLimitRequeues does for rate-limit
	// deaths — see requeueBudgetKilled and ledger.Slot.BudgetKillRequeues.
	budgetKillRequeues int
	// turnExhaustedRequeues carries the fresh-restart turn-ceiling counter
	// forward (koryph-840), the same way budgetKillRequeues does for budget
	// deaths — see requeueTurnExhausted and ledger.Slot.TurnExhaustedRequeues.
	turnExhaustedRequeues int
	note                  string
	// wipSnapshotPath threads a captured WIP snapshot's path (koryph-77r.10,
	// worktree.PatchSnapshot via refreshWorktreeForRequeue) into the compiled
	// prompt's RESUMING block (promptc.Input.WIPSnapshotPath) so the agent can
	// restore uncommitted work from a prior attempt instead of orphaning it —
	// whether that attempt's worktree was preserved or rebuilt. Empty on a
	// fresh first-attempt dispatch and on any requeue with no WIP to capture.
	wipSnapshotPath string
	// footprint is the RW conflict footprint to persist on the ledger slot
	// (koryph-2im.3, design L2 footprint persistence): the batch item's
	// computed sched.Footprint on a fresh dispatch, or the prior slot's
	// already-persisted footprint carried forward on a requeue (see
	// requeueSlot/requeueRateLimited) — never recomputed here, so a relabel
	// mid-run cannot retroactively change what a live/resumed slot conflicts
	// with. nil (e.g. a synthetic/legacy path) leaves the new slot's
	// Footprint nil too, and activeFootprints falls back to its
	// recompute-from-labels chain exactly as before this bead.
	footprint *sched.Footprint
	// resources is the frozen external-resource claim to persist on the ledger
	// slot and re-attach to the govern lease (koryph-4ql.3, design L2/L3): the
	// resolved kinds + memory reservation, computed once on the first dispatch
	// and carried verbatim through every requeue — exactly like footprint above.
	// A relabel or a project/machine vocabulary edit mid-run must NOT re-price a
	// live slot (I8), so requeueSlot/requeueRateLimited/requeueBudgetKilled all
	// rebuild this from the persisted slot (resourcesFromSlot) rather than
	// re-deriving it. nil on a fresh dispatch means "resolve from the bead's live
	// labels" (dispatchBead falls back to sched.ResourcesFor); the wave/rolling
	// loops set it explicitly so the value acquireGlobalSlot admitted is the same
	// value the slot persists.
	resources *dispatchResources
	// features is the frozen similarity feature vector to persist on the
	// ledger slot (koryph-qf6.3): nil on a fresh dispatch (dispatchBead
	// snapshots the live issue via featuresFor), or the prior slot's persisted
	// vector carried forward on a requeue (featuresFromSlot) — never
	// recomputed, so a relabel mid-run cannot rewrite what a live slot is
	// understood to look like. Same freeze rationale as footprint/resources.
	features *beadFeatures
	// accumulatedCostUSD carries forward the total cost already spent on
	// previous attempts of this bead, so that CostUSD on the new slot
	// starts at the right baseline and completeSlot can ADD the new
	// attempt's cost rather than overwrite it (koryph-6bl). Zero on a fresh
	// first-attempt dispatch.
	accumulatedCostUSD float64
	// accumulatedTokens carries forward the token composition already spent
	// on previous attempts of this bead (koryph-77r.1), the same way
	// accumulatedCostUSD carries CostUSD forward: the new slot's token
	// fields start at this baseline and completeSlot's applyTokenUsage ADDs
	// each attempt's usage rather than overwriting it. Zero value on a fresh
	// first-attempt dispatch.
	accumulatedTokens dispatch.TokenUsage
	// frozenModel/frozenPersona/frozenModelWhy/frozenEffort carry the model
	// resolution forward from the first attempt so every requeue re-runs the
	// SAME model, persona, and effort the bead was originally dispatched with
	// (koryph-ehx). Mirrors the footprint field's freeze rationale exactly: a
	// requeue is the SAME bead attempt continuing, not a relabeled
	// re-evaluation, so a `model:*`/`runtime:*` relabel mid-run (or any
	// non-determinism in the persona-tier resolution chain) must NOT change
	// which model a retry runs — otherwise a bead dispatched on opus can
	// silently finish on haiku (or vice-versa). dispatchBead skips
	// modelroute.Resolve entirely when frozenModel != "". Empty frozenModel on
	// a fresh first-attempt dispatch means "resolve normally". The ONE
	// sanctioned mutation is requeueSlot's final-attempt escalation
	// (koryph-qf6.4): a recorded, allowlist-checked policy decision that
	// replaces the frozen tier with modelroute.EscalationTier's target and
	// says so in frozenModelWhy — never a re-resolution from labels.
	frozenModel    string
	frozenPersona  string
	frozenModelWhy string
	frozenEffort   string
}

// resolveModel decides which model, persona, and effort a dispatch runs under.
//
// On a requeue (q.frozenModel set) the first attempt's resolution is reused
// verbatim (koryph-ehx): a `model:*`/`runtime:*` relabel mid-run — or any
// non-determinism in the persona-tier resolution chain — must NOT change which
// model a retry runs, exactly as the persisted footprint cannot change what a
// retry conflicts with. On a fresh dispatch it resolves from the bead's live
// labels through the full modelroute precedence.
func (r *runner) resolveModel(q dispatchReq, runtimeName string) (modelroute.Resolution, string, error) {
	if q.frozenModel != "" {
		return modelroute.Resolution{
			Model:     q.frozenModel,
			Persona:   q.frozenPersona,
			Effort:    q.frozenEffort,
			Rationale: q.frozenModelWhy,
		}, q.frozenEffort, nil
	}

	res, err := r.resolveModelForRuntime(r.implementationStage(q.issue), q.issue, "", runtimeName)
	if err != nil {
		return modelroute.Resolution{}, "", err
	}
	// Persona metadata: the meta model/tier never override the resolved tier
	// here (Resolve already folded persona tier/model into res.Model above, per
	// koryph-v8u.10); only the effort hint is taken from this second read.
	effort := res.Effort
	if _, metaEffort, _, err := modelroute.PersonaMeta(r.rec.Root, res.Persona); err == nil && effort == "" {
		effort = metaEffort
	}
	return res, effort, nil
}

func mergeStringMaps(base, overlay map[string]string) map[string]string {
	if len(overlay) == 0 {
		return base
	}
	out := make(map[string]string, len(base)+len(overlay))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range overlay {
		out[k] = v
	}
	return out
}

// dispatchBead runs the full dispatch flow for one bead: model routing,
// worktree, prompt, backend launch, bd claim, ledger slot, manifest v2, audit.
// Failures block the slot and never fall through.
func (r *runner) dispatchBead(ctx context.Context, q dispatchReq) {
	beadID := q.issue.ID

	// Holdout-arm assignment (koryph-3l1.3, design §3 L6): computed once,
	// here, from beadID alone — BEFORE anything else in this function reads
	// r.rec.ProxyBaseURL()/AgentProxy.ID() directly — so every downstream use
	// (dispatch.Spec's env injection, the ledger slot stamp, the manifest
	// stamp, and the dispatch-time estimate below) agrees on the same arm for
	// this attempt. Because dispatchBead is the SOLE dispatch path shared by
	// wave.go's fresh dispatches, rolling.go's fresh dispatches, and every
	// requeue path in poll.go (requeueSlot/requeueRateLimited/
	// requeueBudgetKilled all funnel back through here with the same
	// q.issue.ID), and ArmFor hashes beadID alone, a requeued or resumed bead
	// is guaranteed to land in the same arm on every attempt without any of
	// those call sites needing to thread the arm forward themselves.
	proxyID, proxyBaseURL := r.rec.AgentProxy.ArmFor(beadID)
	proxyConfigured := r.rec.AgentProxy != nil && r.rec.AgentProxy.BaseURL != ""

	// Runtime selection normally follows bead runtime:<name> then project
	// default_runtime. --runtime-only may only reach a matching normal route;
	// --runtime-equivalent deliberately replaces it after preserving the source
	// decision for model equivalency translation.
	normalRuntime, normalWhy := r.normalRuntimeFor(q.issue)
	if r.opts.RuntimeOnly != "" && normalRuntime != r.opts.RuntimeOnly {
		r.blockSlot(beadID, q, fmt.Sprintf("runtime-only %s refuses normal runtime %s via %s", r.opts.RuntimeOnly, normalRuntime, normalWhy))
		return
	}
	runtimeName, runtimeWhy := r.effectiveRuntimeFor(q.issue)
	rt, ok := runtimeForName(runtimeName)
	if !ok {
		r.blockSlot(beadID, q, fmt.Sprintf(
			"runtime %s is not registered (resolved via %s)",
			runtimeName, runtimeWhy))
		return
	}
	if !runtimeEnabled(r.cfg, runtimeName) {
		r.blockSlot(beadID, q, fmt.Sprintf("runtime %s is not enabled for this project (resolved via %s)", runtimeName, runtimeWhy))
		return
	}
	if err := scopedSigningTransportError(rt, r.sshAuthSock); err != nil {
		r.blockSlot(beadID, q, err.Error())
		return
	}
	ra := r.rec.AccountFor(runtimeName)
	profile := account.Profile{Name: r.rec.AccountProfile, ConfigDir: ra.ConfigDir}
	expectedIdentity := ra.ExpectedIdentity
	if ra.EffectiveAuthMode() != registry.AuthModeSubscription && rt.Name() != "claude" {
		r.blockSlot(beadID, q, fmt.Sprintf("runtime %s does not support koryph-managed %s credentials; use its native login", runtimeName, ra.EffectiveAuthMode()))
		return
	}

	res, effort, err := r.resolveModel(q, runtimeName)
	if err != nil {
		r.blockSlot(beadID, q, "model resolution: "+err.Error())
		return
	}

	branch := worktree.BranchFor(beadID)
	wt, err := worktree.Ensure(ctx, worktree.EnsureOpts{
		RepoRoot:     r.rec.Root,
		WorktreeRoot: r.rec.WorktreeRoot,
		Branch:       branch,
		Base:         r.rec.DefaultBranch,
	})
	if err != nil {
		r.blockSlot(beadID, q, "worktree: "+err.Error())
		return
	}
	if wt.Created && len(r.cfg.Bootstrap) > 0 {
		if err := worktree.Bootstrap(ctx, wt.Path, r.cfg.Bootstrap, nil); err != nil {
			r.blockSlot(beadID, q, "bootstrap: "+err.Error())
			return
		}
	}

	phaseDir := r.store.PhaseDir(r.run.RunID, beadID)
	policy := r.mergePolicy(ctx, q.epicID)

	prompt := promptc.Compile(promptc.Input{
		EngineVersion:   EngineVersion,
		ProjectName:     r.rec.Name,
		Gate:            r.cfg.Gate,
		CommitStyle:     r.cfg.CommitStyle,
		CommitTemplate:  r.cfg.CommitTemplate,
		Bootstrap:       r.cfg.Bootstrap,
		Bead:            q.issue,
		ResumeSHA:       q.resumeSHA,
		WIPSnapshotPath: q.wipSnapshotPath,
		ReviewPath:      q.reviewPath,
		PhaseDir:        phaseDir,
		SummaryPath:     filepath.Join(phaseDir, "SUMMARY.md"),
		StatusPath:      filepath.Join(phaseDir, "status.json"),
		LogPath:         filepath.Join(phaseDir, "session.log"),
	})

	sessionID := newSessionID()
	sessionName := "koryph/" + r.opts.ProjectID + "/" + beadID + "/a" + strconv.Itoa(q.attempt)

	backend := r.backend
	if r.rt != nil && rt.Name() != r.rt.Name() {
		backend = &dispatch.CLIBackend{Runtime: rt}
	}
	maxBudgetUSD := r.quotaCfg.PerAgentMaxUSD
	if !rt.Capabilities().BudgetFlag {
		maxBudgetUSD = 0
	}
	resumeSessionID := q.resumeSessionID
	if !rt.Capabilities().Resume {
		resumeSessionID = ""
	}
	handle, err := backend.Dispatch(ctx, dispatch.Spec{
		ProjectID:        r.opts.ProjectID,
		RepoRoot:         r.rec.Root,
		RunID:            r.run.RunID,
		PhaseID:          beadID,
		PhaseDir:         phaseDir,
		Worktree:         wt.Path,
		Branch:           branch,
		Persona:          res.Persona,
		Model:            res.Model,
		Effort:           effort,
		Profile:          profile,
		ExpectedIdentity: expectedIdentity,
		Billing:          r.billing,
		APIKey:           r.apiKey,
		Credential:       r.credential,
		CredentialEnvVar: r.credentialEnvVar,
		MaxBudgetUSD:     maxBudgetUSD,
		Prompt:           prompt,
		SessionID:        sessionID,
		SessionName:      sessionName,
		ResumeSessionID:  resumeSessionID,
		BeadsDir:         r.beadsDir,
		Attempt:          q.attempt,
		SSHAuthSock:      r.sshAuthSock,
		EnvPassthrough:   r.rec.EnvPassthrough,
		ProxyBaseURL:     proxyBaseURL,
		StrictMCP:        r.rec.StrictMCP(),
	})
	if err != nil {
		r.blockSlot(beadID, q, "dispatch refused: "+err.Error())
		return
	}

	// Resolve the frozen resource claim (koryph-4ql.3, design L2/L3): use the
	// value threaded from the loop/requeue (q.resources) so the ledger slot
	// persists exactly what acquireGlobalSlot admitted and what a requeue froze;
	// only a path that supplied none (a synthetic/legacy dispatch) recomputes
	// from the bead's live labels here. The resolved tokens — not the labels —
	// are persisted and re-attached to the lease, so a relabel or vocabulary
	// edit mid-run cannot re-price a live slot (I8).
	resKinds, memReserveMB := r.resolveDispatchResources(q)

	_ = r.adapter.Claim(ctx, beadID) // best-effort
	r.holdGlobalSlot(beadID, handle.PID, res.Model, resKinds, memReserveMB)

	// Stamp the dispatch-time estimate (koryph-6bl). This is the per-attempt
	// estimate (bias-corrected when enough samples exist), NOT accumulated —
	// it is the prediction we are making for THIS attempt, used later by
	// completeSlot to compute estimator error and update ErrorStats.
	estimateUSD := r.itemEstimate(q.issue, res.Model, runtimeName, proxyID)

	// Similarity features (koryph-qf6.3): frozen from the requeue's persisted
	// slot when threaded, snapshotted from the live issue on a fresh dispatch.
	feat := featuresFor(q)

	now := time.Now().UTC().Format(time.RFC3339)
	sl := &ledger.Slot{
		PhaseID:               beadID,
		BeadID:                beadID,
		EpicID:                q.epicID,
		Branch:                branch,
		Worktree:              wt.Path,
		SessionID:             sessionID,
		SessionName:           sessionName,
		Agent:                 res.Persona,
		Model:                 res.Model,
		ModelWhy:              res.Rationale,
		Effort:                effort,
		Runtime:               runtimeName,
		AccountProfile:        r.rec.AccountProfile,
		ClaudeConfigDir:       r.rec.ClaudeConfigDir,
		VerifiedIdentity:      handle.VerifiedIdentity,
		VerifiedAt:            now,
		BillingMode:           string(r.billing),
		ProxyID:               proxyID,
		ProxyConfigured:       proxyConfigured,
		PID:                   handle.PID,
		Stream:                handle.StreamPath,
		StatusPath:            handle.StatusPath,
		LogPath:               filepath.Join(phaseDir, "session.log"),
		Status:                ledger.SlotRunning,
		Attempts:              q.attempt,
		ResumeSHA:             q.resumeSHA,
		ReviewIters:           q.reviewIters,
		GateRequeues:          q.gateRequeues,
		MergeRequeues:         q.mergeRequeues,
		ConflictRequeues:      q.conflictRequeues,
		DispatchedAt:          now,
		Note:                  q.note,
		RateLimitRequeues:     q.rateLimitRequeues,
		BudgetKillRequeues:    q.budgetKillRequeues,
		TurnExhaustedRequeues: q.turnExhaustedRequeues,
		Footprint:             q.footprint,
		Resources:             resKinds,
		MemReserveMB:          memReserveMB,
		BeadLabels:            feat.labels,
		SizeClass:             feat.sizeClass,
		IssueType:             feat.issueType,
		EstimateUSD:           estimateUSD,
		// CostUSD starts from accumulatedCostUSD so prior-attempt spend is
		// not lost when completeSlot ADDs the new attempt's cost (koryph-6bl).
		CostUSD: q.accumulatedCostUSD,
		// Token fields start from accumulatedTokens for the same reason
		// (koryph-77r.1): applyTokenUsage ADDs each attempt's usage.
		InputTokens:         q.accumulatedTokens.InputTokens,
		OutputTokens:        q.accumulatedTokens.OutputTokens,
		CacheReadTokens:     q.accumulatedTokens.CacheReadTokens,
		CacheCreationTokens: q.accumulatedTokens.CacheCreationTokens,
	}
	_ = r.store.SetSlot(r.run, sl)
	r.dispatched++

	_ = r.store.SaveManifest(r.run.RunID, beadID, &ledger.Manifest{
		ProjectID:       r.opts.ProjectID,
		BeadID:          beadID,
		EpicID:          q.epicID,
		AccountProfile:  r.rec.AccountProfile,
		ClaudeConfigDir: r.rec.ClaudeConfigDir,
		SessionID:       sessionID,
		SessionName:     sessionName,
		Model:           res.Model,
		ModelWhy:        res.Rationale,
		Runtime:         runtimeName,
		WorktreePath:    wt.Path,
		Branch:          branch,
		BaseCommit:      r.baseCommit(ctx),
		Attempt:         q.attempt,
		ExecutionState:  "running",
		RecoveryTier:    recoveryTier(q.issue, r.cfg),
		MergePolicy:     string(policy),
		AutoMerge:       r.opts.AutoMerge,
		BillingMode:     string(r.billing),
		ProxyID:         proxyID,
		BootstrapCmds:   r.cfg.Bootstrap,
		BatchAllowed:    r.rec.BatchPolicy == "explicit",
		ReviewStatus:    reviewStatus(q.reviewPath),
	})

	_ = r.reg.Audit(registry.Event{
		Kind:      "dispatch",
		ProjectID: r.opts.ProjectID,
		Actor:     r.owner,
		Detail: map[string]string{
			"bead":    beadID,
			"model":   res.Model,
			"account": r.rec.AccountProfile,
			"billing": string(r.billing),
		},
	})
	r.progress("bead %s: dispatched attempt %d (model %s — %s; pid %d)",
		beadID, q.attempt, res.Model, res.Rationale, handle.PID)
	logSlotDispatched(r.run.RunID, r.opts.ProjectID, beadID, q.attempt, res.Model, handle.PID)
}

// blockSlot records a slot that could not be dispatched. Blocked is terminal:
// it never falls through to a merge.
func (r *runner) blockSlot(beadID string, q dispatchReq, why string) {
	_ = r.store.UpdateSlot(r.run, beadID, func(s *ledger.Slot) {
		s.BeadID = beadID
		s.EpicID = q.epicID
		s.Status = ledger.SlotBlocked
		s.Attempts = q.attempt
		s.Note = why
	})
	r.releaseGlobalSlot(beadID) // terminal: free the reserved/held slot
	r.progress("bead %s: blocked (%s)", beadID, why)
	// frozenModel may be empty when the block precedes model resolution — the
	// outcome event then honestly reports "model unknown" (koryph-qf6.2).
	logSlotBlocked(beadID, why, q.frozenModel, "", q.attempt)
}

// mergePolicy resolves the effective merge policy: an epic merge:* label wins
// over the project config; Show errors fall back to the config.
// reportWaveSkips surfaces the scheduler's skip/deferral reasons so an operator
// can see WHY a ready bead did not dispatch (the reasons were previously
// computed and discarded, koryph-6g2.1). Structural skips (non-task types, gt:*
// gates) never dispatch as-is, so they are reported ONCE per run with a fix
// hint; deferrals (footprint conflict, wave full, container, no-dispatch) are
// transient and summarized per wave — or listed in full under --dry-run.
func (r *runner) reportWaveSkips(w sched.Wave) {
	if r.reportedSkips == nil {
		r.reportedSkips = map[string]bool{}
	}
	for _, s := range w.Skipped {
		if r.reportedSkips[s.ID] {
			continue
		}
		r.reportedSkips[s.ID] = true
		if strings.HasPrefix(s.Reason, "runtime-only ") {
			r.progress("skipped %s: %s — excluded by this run's runtime policy", s.ID, s.Reason)
			continue
		}
		r.progress("skipped %s: %s — not dispatchable as-is (file as task/bug/chore; area:* label; drop gt:*)", s.ID, s.Reason)
	}
	if len(w.Deferred) == 0 {
		return
	}
	// Emit structured deferral events for deferrals_by_token metric (Section O2).
	// The Reason field carries the human-readable conflict/wave-full/container
	// cause; it doubles as the token key since sched.Reason has no separate Token
	// field at this revision.
	for _, d := range w.Deferred {
		logDeferral(d.ID, d.Reason, d.Reason)
	}
	if r.opts.DryRun {
		for _, d := range w.Deferred {
			r.progress("dry-run: deferred %s (%s): %s", d.ID, d.Title, d.Reason)
		}
		return
	}
	r.progress("wave %d: deferred %d bead(s): %s", r.run.Wave, len(w.Deferred), summarizeReasons(w.Deferred, 3))
}

// summarizeReasons renders up to n "id(reason)" pairs, with a trailing "+k more".
func summarizeReasons(rs []sched.Reason, n int) string {
	var parts []string
	for i, d := range rs {
		if i >= n {
			parts = append(parts, fmt.Sprintf("+%d more", len(rs)-n))
			break
		}
		parts = append(parts, fmt.Sprintf("%s(%s)", d.ID, d.Reason))
	}
	return strings.Join(parts, ", ")
}

func (r *runner) mergePolicy(ctx context.Context, epicID string) project.Policy {
	if epicID != "" {
		if epic, err := r.adapter.Show(ctx, epicID); err == nil {
			for _, l := range epic.Labels {
				switch l {
				case "merge:auto":
					return project.PolicyAuto
				case "merge:manual":
					return project.PolicyManual
				case "merge:pr":
					return project.PolicyPR
				}
			}
		}
	}
	return r.cfg.MergePolicy
}

// recoveryTier resolves the recovery tier: an rt:<n> label wins over the
// project default.
func recoveryTier(issue beads.Issue, cfg *project.Config) int {
	for _, v := range issue.LabelValues("rt:") {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3 {
			return n
		}
	}
	return cfg.RiskTierDefault
}

// reviewStatus annotates a manifest for a review-bounce dispatch.
func reviewStatus(reviewPath string) string {
	if reviewPath == "" {
		return ""
	}
	return "bounced: blocking findings at " + reviewPath
}

// baseCommit resolves the default branch HEAD in the primary checkout
// (tolerating errors with an empty result).
func (r *runner) baseCommit(ctx context.Context) string {
	res, err := execx.Run(ctx, execx.Cmd{
		Dir: r.rec.Root, Name: "git", Args: []string{"rev-parse", r.rec.DefaultBranch},
	})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	return strings.TrimSpace(res.Stdout)
}

// interruptActiveSlots sends SIGTERM to every non-terminal slot's agent
// process GROUP (hard budget stop). Agents are launched Setsid (see
// dispatch.StopGraceful) so their subprocess children share the leader's
// pgid; signaling the leader pid alone orphans those children instead of
// stopping them, leaving them running (and spending) past the budget cap.
// Agents are designed to checkpoint on SIGTERM; their worktrees and branches
// are preserved. Never SIGKILLs — a dirty worktree is recoverable; a killed
// process in the middle of a commit is not.
func (r *runner) interruptActiveSlots() {
	for id, sl := range r.run.Slots {
		if sl == nil || ledger.Terminal(sl.Status) {
			continue
		}
		pid := sl.PID
		if pid <= 0 {
			r.progress("hard stop: slot %s has no PID (agent not yet attached or already gone)", id)
			continue
		}
		if err := dispatch.StopGraceful(pid); err != nil {
			r.progress("hard stop: could not signal pgid for pid %d (slot %s): %v — already gone?", pid, id, err)
		} else {
			r.progress("hard stop: sent SIGTERM to process group of pid %d (slot %s)", pid, id)
		}
	}
}
