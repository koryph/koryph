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
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/epicreview"
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
	width         int

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

// governorGate runs the governor/billing/budget checks once per wave or
// refill. See govGate's doc for why this is factored out rather than
// duplicated between waveLoop and rollingLoop.
func (r *runner) governorGate(ctx context.Context) govGate {
	level, calibrated, usage := r.governor(ctx)
	advisory, advisoryWhy := r.guardMode(calibrated)
	if advisory {
		// Advisory: measure + log, never block, never switch billing.
		r.billing, r.apiKey = account.BillingSubscription, ""
		if level != quota.LevelOK {
			r.progress("billing guard ADVISORY (%s): governor level %s — not blocking", advisoryWhy, level)
		}
	} else {
		r.billing, r.apiKey = r.billingFor(level)
	}

	// Cache for the health patrol (koryph-gus): patrol uses these rather than
	// re-running the quota snapshot every patrol cycle.
	r.lastQuotaLevel = level
	r.lastQuotaUsage = usage

	g := govGate{allowDispatch: true, level: level, calibrated: calibrated, advisory: advisory, usage: usage}

	if !r.opts.Manual && !advisory {
		// eff is the effective ladder for logging threshold annotations.
		eff := quota.Ladder{}.Effective()
		if r.quotaCfg != nil {
			eff = r.quotaCfg.Ladder.Effective()
		}
		switch level {
		case quota.LevelStop:
			// Hard stop: interrupt active agents (SIGTERM — checkpoints; worktrees
			// preserved for resume) and park the run immediately.
			if r.billing != account.BillingAPIKey {
				r.interruptActiveSlots()
				r.run.Status = ledger.RunHardStopQuota
				_ = r.store.SaveRun(r.run)
				r.progress("governor hard stop: run %s parked at %.0f%% (hard-stop %.0f%%) — active agents sent SIGTERM, worktrees preserved for resume",
					r.run.RunID, usage.Window5h.Fraction()*100, eff.HardStop*100)
				g.paused = true
				g.outcome = r.outcome(ExitOK, "quota-hard-stop", false)
				return g
			}
		case quota.LevelDrain:
			g.allowDispatch = false
			r.progress("governor graceful stop: no new dispatch (%.0f%% >= graceful-stop %.0f%%); finishing %d active slot(s)",
				usage.Window5h.Fraction()*100, eff.GracefulStop*100, r.activeCount())
		case quota.LevelThrottle:
			r.progress("governor throttle: slot scaling active (%.0f%% >= throttle %.0f%%)",
				usage.Window5h.Fraction()*100, eff.Throttle*100)
		case quota.LevelWarn:
			r.progress("governor warn: usage at %.0f%% (warn %.0f%%, throttle %.0f%%, graceful-stop %.0f%%, hard-stop %.0f%%)",
				usage.Window5h.Fraction()*100, eff.Warn*100, eff.Throttle*100, eff.GracefulStop*100, eff.HardStop*100)
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
		if r.activeCount() == 0 {
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
	if ov, ok := r.store.LoadResize(); ok {
		width = ov.Max
	}
	if !r.opts.Manual && calibrated && !advisory {
		if scaled := quota.ScaleSlots(usage, r.quotaCfg, width); scaled < width {
			width = scaled
		}
	}
	g.width = width

	// Per-run budget ceiling: once cumulative run cost reaches the cap, stop
	// starting new agents; any active slots still finish.
	if r.opts.BudgetUSD > 0 {
		if spent := r.runCostUSD(); spent >= r.opts.BudgetUSD {
			g.budgetHit = true
			if g.allowDispatch {
				r.progress("run budget reached: $%.2f spent >= $%.2f cap — no new dispatch", spent, r.opts.BudgetUSD)
			}
			g.allowDispatch = false
		}
	}
	return g
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
		r.patrolIfDue(ctx)
		r.run.Wave++
		_ = r.store.SaveRun(r.run)

		gate := r.governorGate(ctx)
		if gate.paused {
			return gate.outcome, nil
		}
		allowDispatch := gate.allowDispatch
		level, calibrated, usage, budgetHit := gate.level, gate.calibrated, gate.usage, gate.budgetHit
		width := gate.width

		// Frontier scan + wave build.
		issues, err := r.adapter.Ready(ctx, beads.ReadyOpts{Parent: r.opts.Parent})
		if err != nil {
			return r.outcome(ExitFatal, "bd ready failed", false), fmt.Errorf("engine: bd ready: %w", err)
		}
		// --only narrows the frontier to a single operator-chosen bead; once it
		// closes it drops out of `bd ready` and the run drains.
		if r.opts.Only != "" {
			issues = onlyBead(issues, r.opts.Only)
		}
		active := r.activeIDs()
		w, err := sched.BuildWave(ctx, issues, r.cfg, sched.Opts{
			Max:          width,
			DefaultModel: r.opts.DefaultModel,
			Parent:       r.opts.Parent,
			ActiveIDs:    active,
			Active:       r.activeFootprints(ctx, active),
		}, r.childLister(ctx))
		if err != nil {
			return r.outcome(ExitFatal, "wave build failed", false), fmt.Errorf("engine: build wave: %w", err)
		}

		eligible := 0
		for _, iss := range issues {
			if ok, _ := sched.Eligible(iss, active); ok {
				eligible++
			}
		}

		// Drained: nothing eligible, nothing active, nothing batched.
		if eligible == 0 && len(active) == 0 && len(w.Items) == 0 {
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
		if !allowDispatch && len(active) == 0 {
			reason := "quota-" + string(level)
			if budgetHit {
				reason = "budget-cap"
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
			if ok, reason := quota.Preflight(usage, est, r.quotaCfg); !ok {
				allowDispatch = false
				r.progress("preflight refused wave: %s", reason)
				if len(active) == 0 {
					_ = r.store.FinalizeRun(r.run)
					return r.outcome(ExitOK, reason, false), nil
				}
			}
		}

		if allowDispatch {
			r.progress("wave %d: %d ready, dispatching %d%s",
				r.run.Wave, w.ReadyCount, len(w.Items), r.windowNote(calibrated, usage, est))
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
			if len(w.Items) == 0 && len(active) == 0 {
				_ = r.store.FinalizeRun(r.run)
				return r.outcome(ExitOK, "no dispatchable work (all ready items deferred)", false), nil
			}

			logCoDispatch(r.run.RunID, r.opts.ProjectID, r.run.Wave, len(active), width)
			r.refreshDemand()
			r.warnIfOverFairShare()
			stagger := r.staggerDelay()
			for i, it := range w.Items {
				if i > 0 && stagger > 0 {
					select {
					case <-ctx.Done():
						return r.interrupted()
					case <-time.After(stagger):
					}
				}
				// Global concurrency cap (across all projects). A denial defers
				// the rest of this wave — same-project shares won't free up until
				// a running agent finishes — so break and re-scan next wave.
				if !r.acquireGlobalSlot(it.Issue.ID) {
					r.progress("wave %d: global governor cap reached — deferring %d bead(s) to a later wave",
						r.run.Wave, len(w.Items)-i)
					break
				}
				r.issues[it.Issue.ID] = it.Issue
				fp := it.Footprint
				r.dispatchBead(ctx, dispatchReq{issue: it.Issue, epicID: it.EpicID, attempt: 1, footprint: &fp})
			}
			// Emit refill dispatched count for the structured log record.
			logRefillDispatched(r.run.RunID, r.opts.ProjectID, r.run.Wave, len(w.Items))
		}

		// Poll this wave's slots (and any adopted ones) to a terminal state.
		if err := r.pollUntilIdle(ctx); err != nil {
			return r.interrupted()
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
	if r.quotaCfg.WindowCeilingUSD <= 0 && r.quotaCfg.WeeklyCeilingUSD <= 0 {
		return quota.LevelOK, false, quota.Usage{Account: r.quotaCfg.Account}
	}
	u, _ := quota.Snapshot(ctx, r.profile, r.quotaCfg)
	level, calibrated := quota.State(u, r.quotaCfg)
	return level, calibrated, u
}

// billingFor selects the billing mode for this wave. Subscription always,
// EXCEPT at governor stop when the operator explicitly opted into api-key
// fallback (flag + registry policy + resolvable key). Logged loudly: this is
// the only path to per-token spend.
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

func (r *runner) billingFor(level quota.Level) (account.BillingMode, string) {
	if level == quota.LevelStop && r.opts.AllowAPISpend &&
		r.rec.APIFallback == "explicit" && r.rec.APIKeyEnvVar != "" {
		if key := os.Getenv(r.rec.APIKeyEnvVar); key != "" {
			r.progress("!!! governor stop: switching to API-KEY billing from $%s (explicit opt-in) — per-token spend ahead", r.rec.APIKeyEnvVar)
			return account.BillingAPIKey, key
		}
	}
	return account.BillingSubscription, ""
}

// runCostUSD is the cumulative recorded cost of every slot in this run — the
// figure the per-run --budget ceiling is measured against.
func (r *runner) runCostUSD() float64 {
	var total float64
	for _, sl := range r.run.Slots {
		if sl != nil {
			total += sl.CostUSD
		}
	}
	return total
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
func (r *runner) waveEstimate(items []sched.Item) float64 {
	var est float64
	for _, it := range items {
		model := it.Model
		if model == "" {
			model = modelroute.TierSonnet
		}
		runtimeName, _ := modelroute.ResolveRuntimeName(it.Issue.Labels, r.cfg.DefaultRuntime)
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

// dispatchReq describes one dispatch (fresh, requeue, or review bounce).
type dispatchReq struct {
	issue           beads.Issue
	epicID          string
	attempt         int
	resumeSHA       string
	resumeSessionID string
	reviewPath      string
	reviewIters     int
	// gateRequeues, mergeRequeues, and rateLimitRequeues carry the
	// requeue-budget counters forward across a requeue dispatch
	// (koryph-2im.6, koryph-2im.4): dispatchBead below builds a fresh
	// ledger.Slot rather than mutating the old one, so — the same way
	// reviewIters is threaded through — these must be passed explicitly or
	// each budget would reset to zero every requeue.
	gateRequeues      int
	mergeRequeues     int
	rateLimitRequeues int
	// budgetKillRequeues carries the warm-resume budget-kill counter forward
	// (koryph-77r.10), the same way rateLimitRequeues does for rate-limit
	// deaths — see requeueBudgetKilled and ledger.Slot.BudgetKillRequeues.
	budgetKillRequeues int
	note               string
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

	// Runtime selection (koryph-v8u.3): bead `runtime:<name>` label > project
	// default_runtime > "claude" (modelroute.ResolveRuntimeName). Dispatch
	// itself only ever drives the claude CLI today — no other runtime's
	// worktree/backend/ledger wiring exists yet — so anything other than
	// "claude" blocks the slot right here, before any worktree or backend
	// work happens, rather than silently falling back to claude (which would
	// dispatch a bead under an identity/account/model policy the operator
	// never asked for).
	runtimeName, runtimeWhy := modelroute.ResolveRuntimeName(q.issue.Labels, r.cfg.DefaultRuntime)
	if runtimeName != "claude" {
		r.blockSlot(beadID, q, fmt.Sprintf(
			"runtime %s not available (dispatch only supports claude today; resolved via %s)",
			runtimeName, runtimeWhy))
		return
	}

	// Auto-filed docs-update beads (epic validation §4b, label validation:docs)
	// dispatch as the docs stage: PersonaFor(StageDocs) routes them to the
	// docs-author persona instead of the implementer. Everything else stays
	// implement-stage.
	stage := modelroute.StageImplement
	if q.issue.HasLabel(epicreview.LabelDocs) {
		stage = modelroute.StageDocs
	}
	res, err := modelroute.Resolve(modelroute.Req{
		Stage:         stage,
		Labels:        q.issue.Labels,
		RunDefault:    r.opts.DefaultModel,
		AllowedModels: r.rec.AllowedModels,
		Stages:        r.cfg.Stages,
		// RepoRoot/ModelMap enable the koryph-v8u.10 persona-tier resolution
		// step inside Resolve: a bead model:<tier> label (above) still wins
		// unchanged; absent that, the implement-stage persona's `tier`
		// frontmatter resolves through the selected runtime's ModelMap
		// (overlaid with the project's ModelMap override) before falling
		// back to the persona's legacy `model` pin and finally the
		// runtime-namespaced hardcoded stage default (koryph-v8u.3).
		RepoRoot: r.rec.Root,
		ModelMap: r.cfg.ModelMap,
		Runtime:  runtimeName,
	})
	if err != nil {
		r.blockSlot(beadID, q, "model resolution: "+err.Error())
		return
	}
	// Persona metadata: the meta model/tier never override the resolved
	// tier here (Resolve already folded persona tier/model into res.Model
	// above, per koryph-v8u.10); only the effort hint is taken from this
	// second read.
	effort := res.Effort
	if _, metaEffort, _, err := modelroute.PersonaMeta(r.rec.Root, res.Persona); err == nil && effort == "" {
		effort = metaEffort
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

	handle, err := r.backend.Dispatch(ctx, dispatch.Spec{
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
		Profile:          r.profile,
		ExpectedIdentity: r.expectedIdentity,
		Billing:          r.billing,
		APIKey:           r.apiKey,
		MaxBudgetUSD:     r.quotaCfg.PerAgentMaxUSD,
		Prompt:           prompt,
		SessionID:        sessionID,
		SessionName:      sessionName,
		ResumeSessionID:  q.resumeSessionID,
		BeadsDir:         r.beadsDir,
		Attempt:          q.attempt,
		SSHAuthSock:      r.sshAuthSock,
		EnvPassthrough:   r.rec.EnvPassthrough,
		ProxyBaseURL:     proxyBaseURL,
	})
	if err != nil {
		r.blockSlot(beadID, q, "dispatch refused: "+err.Error())
		return
	}

	_ = r.adapter.Claim(ctx, beadID) // best-effort
	r.holdGlobalSlot(beadID, handle.PID, res.Model)

	// Stamp the dispatch-time estimate (koryph-6bl). This is the per-attempt
	// estimate (bias-corrected when enough samples exist), NOT accumulated —
	// it is the prediction we are making for THIS attempt, used later by
	// completeSlot to compute estimator error and update ErrorStats.
	estimateUSD := r.itemEstimate(q.issue, res.Model, runtimeName, proxyID)

	now := time.Now().UTC().Format(time.RFC3339)
	sl := &ledger.Slot{
		PhaseID:            beadID,
		BeadID:             beadID,
		EpicID:             q.epicID,
		Branch:             branch,
		Worktree:           wt.Path,
		SessionID:          sessionID,
		SessionName:        sessionName,
		Agent:              res.Persona,
		Model:              res.Model,
		ModelWhy:           res.Rationale,
		Effort:             effort,
		Runtime:            runtimeName,
		AccountProfile:     r.rec.AccountProfile,
		ClaudeConfigDir:    r.rec.ClaudeConfigDir,
		VerifiedIdentity:   handle.VerifiedIdentity,
		VerifiedAt:         now,
		BillingMode:        string(r.billing),
		ProxyID:            proxyID,
		ProxyConfigured:    proxyConfigured,
		PID:                handle.PID,
		Stream:             handle.StreamPath,
		StatusPath:         handle.StatusPath,
		LogPath:            filepath.Join(phaseDir, "session.log"),
		Status:             ledger.SlotRunning,
		Attempts:           q.attempt,
		ResumeSHA:          q.resumeSHA,
		ReviewIters:        q.reviewIters,
		GateRequeues:       q.gateRequeues,
		MergeRequeues:      q.mergeRequeues,
		DispatchedAt:       now,
		Note:               q.note,
		RateLimitRequeues:  q.rateLimitRequeues,
		BudgetKillRequeues: q.budgetKillRequeues,
		Footprint:          q.footprint,
		EstimateUSD:        estimateUSD,
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
		PromptCache:     r.rec.PromptCachePolicy,
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
	logSlotBlocked(beadID, why)
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

// interruptActiveSlots sends SIGTERM to every non-terminal slot's agent PID
// (hard budget stop). Agents are designed to checkpoint on SIGTERM; their
// worktrees and branches are preserved. Never SIGKILLs — a dirty worktree is
// recoverable; a killed process in the middle of a commit is not.
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
		if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
			r.progress("hard stop: could not signal pid %d (slot %s): %v — already gone?", pid, id, err)
		} else {
			r.progress("hard stop: sent SIGTERM to pid %d (slot %s)", pid, id)
		}
	}
}
