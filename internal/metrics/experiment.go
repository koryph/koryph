// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
)

// ExperimentArm is one arm's (holdout or proxied) aggregate observations for
// the L6 standing-canary comparison (koryph-3l1.3, design
// docs/designs/2026-07-token-economy.md §3 L6, §2 I5): a claimed compression
// win from the proxied arm is only meaningful measured against what the SAME
// bead population would have cost/looked like dispatched directly, which is
// exactly what the holdout arm is for.
type ExperimentArm struct {
	// Name is "proxied" or "holdout" (the arm identity, not the underlying
	// registry.AgentProxy.ID() — see ProjectExperiment.ProxyID for that).
	Name string `json:"name"`
	// Beads is the number of distinct beads (ledger phase_ids) observed in
	// this arm, across every run this project has recorded.
	Beads int `json:"beads"`
	// Composition is this arm's aggregate token composition across every
	// observed bead — CacheHitRatio included (the I7 tripwire's signal, one
	// arm at a time).
	Composition TokenComposition `json:"composition"`
	// MeanTokensPerBead is Composition.Total / Beads (0 when Beads==0) — the
	// AC's "tokens-per-bead" headline number for this arm.
	MeanTokensPerBead int64 `json:"mean_tokens_per_bead"`
	// ByTier is the per-model-tier breakdown — the AC's "tokens-per-bead (by
	// class)" requirement, class meaning model tier.
	ByTier map[string]TierTokenStat `json:"by_tier"`
	// RequeueRate is the fraction of this arm's beads with at least one
	// requeue of any classified kind (gate, merge, rate-limit, budget-kill,
	// conflict) — the "requeue rate" quality tripwire (design §3 L6).
	RequeueRate float64 `json:"requeue_rate"`
	// BlockingReviewFindingRate is the fraction of beads that bounced back to
	// the implementer at least once for a blocking review finding
	// (ledger.Slot.ReviewIters > 0 — see engine's finishCandidate, which only
	// ever increments ReviewIters on review.Verdict.Blocking) — the
	// "blocking-review-finding rate" quality tripwire.
	BlockingReviewFindingRate float64 `json:"blocking_review_finding_rate"`
	// GateFailures is the total COUNT of gate-requeue events across this
	// arm's beads (sum of ledger.Slot.GateRequeues) — the AC's "gate
	// failures" tripwire. A count, not a rate: a bead that failed gate twice
	// contributes 2, not 1 — unlike RequeueRate/BlockingReviewFindingRate,
	// which are per-bead fractions.
	GateFailures int `json:"gate_failures"`
	// CostUSD is this arm's total observed spend across every bead.
	CostUSD float64 `json:"cost_usd"`
	// MeanCostPerBead is CostUSD / Beads (0 when Beads==0) — half of the
	// design §3 L6 calibration-slope check's raw material; see
	// ProjectExperiment.CalibrationSlopeSeam for the other half.
	MeanCostPerBead float64 `json:"mean_cost_usd_per_bead"`
	// EstimatorN/Bias/MAPE are this arm's segment of the account's
	// quota.Config.ErrorStats (calibKey's proxyID segmentation, koryph-77r.1/
	// koryph-3l1.3): an N-weighted rollup across every (tier,size) bucket
	// this arm has recorded observations for. All zero when the account has
	// no calibration data yet for this arm.
	EstimatorN    int     `json:"estimator_n"`
	EstimatorBias float64 `json:"estimator_bias"`
	EstimatorMAPE float64 `json:"estimator_mape"`
}

// ProjectExperiment is one project's two-arm standing-canary comparison
// (koryph-3l1.3, design §3 L6).
type ProjectExperiment struct {
	ProjectID string `json:"project_id"`
	Account   string `json:"account"`
	// ProxyID is the project's CURRENTLY configured agent_proxy identity
	// (registry.AgentProxy.ID()) — the proxied arm's identity. A bead
	// recorded under a since-rotated pin is not folded into either arm below
	// (see collectProjectExperiment's doc) — matching calibKey's own "no
	// migration, nothing orphaned, populations never blend" precedent.
	ProxyID string `json:"proxy_id"`
	// HoldoutFraction is the project's CURRENTLY configured holdout fraction
	// (registry.AgentProxy.EffectiveHoldout()) — informational context for
	// the table; the actual OBSERVED split is
	// Holdout.Beads / (Proxied.Beads + Holdout.Beads), which can differ from
	// this if the fraction was changed partway through the ledger window.
	HoldoutFraction float64       `json:"holdout_fraction"`
	Proxied         ExperimentArm `json:"proxied"`
	Holdout         ExperimentArm `json:"holdout"`
	// CalibrationSlopeSeam documents the design §3 L6 calibration-slope check
	// this report does NOT compute — see the const doc below.
	CalibrationSlopeSeam string `json:"calibration_slope_seam"`
}

// ExperimentReport is the overall result returned by CollectExperiment.
type ExperimentReport struct {
	GeneratedAt string              `json:"generated_at"`
	Projects    []ProjectExperiment `json:"projects"`
}

// calibrationSlopeSeamNote documents the one piece of design §3 L6's
// calibration-slope check ("ccusage-USD vs /usage-percent per arm") that
// ledger data alone cannot support: /usage-percent is an operator-read
// figure from the Claude Code /usage command (see `koryph quota calibrate`),
// never persisted per-dispatch, let alone per-arm. CostUSD/MeanCostPerBead/
// EstimatorBias on each ExperimentArm are the ledger-supported half of the
// same check (the ccusage-USD side); this string is the documented seam for
// the other half, so --json/table output surfaces the gap instead of
// silently omitting it.
const calibrationSlopeSeamNote = "not computed: the ccusage-USD vs /usage-percent slope check needs an operator-supplied /usage reading per arm, which no ledger field carries (design docs/designs/2026-07-token-economy.md §3 L6); cost_usd/mean_cost_usd_per_bead/estimator_bias above are the ledger-supported half — cross-reference them against `koryph quota calibrate` readings taken while dispatching predominantly in one arm"

// CollectExperiment builds the two-arm standing-canary comparison
// (koryph-3l1.3) from run ledgers across managed projects. projectID ""
// considers every registered project. A project is included only when it
// CURRENTLY has agent_proxy configured (rec.AgentProxy != nil, non-empty
// base_url) — with no proxy configured there is no experiment running and
// nothing to compare; see ledger.Slot.ProxyConfigured's doc for how
// historical (pre-experiment) slots are excluded even for a project that
// configured agent_proxy only after accumulating ledger history.
func CollectExperiment(store *registry.Store, projectID string) (*ExperimentReport, error) {
	recs, err := store.List()
	if err != nil {
		return nil, err
	}
	rep := &ExperimentReport{GeneratedAt: time.Now().UTC().Format(time.RFC3339)}
	for _, rec := range recs {
		if projectID != "" && rec.ProjectID != projectID {
			continue
		}
		if rec.AgentProxy == nil || rec.AgentProxy.BaseURL == "" {
			continue
		}
		pe, err := collectProjectExperiment(rec)
		if err != nil {
			return nil, err
		}
		rep.Projects = append(rep.Projects, pe)
	}
	return rep, nil
}

// beadArmAgg accumulates one bead's final observed state for the experiment
// report — the same "replace on each later run" cumulative-state pattern
// collectProjectTokens uses (a bead's ledger fields already accumulate
// within a single run across requeues; across separate run invocations the
// latest run's slot carries the full cumulative total, so later runs replace
// rather than add).
type beadArmAgg struct {
	tier            string
	in, out, cr, cc int64
	costUSD         float64
	requeued        bool
	blockingReview  bool
	gateFailures    int
}

// collectProjectExperiment aggregates one project's ledger history into its
// two-arm comparison. Only slots with ProxyConfigured==true are considered
// (koryph-3l1.3): a slot dispatched before agent_proxy was ever configured
// predates any experiment and must not inflate the holdout arm's bead count.
// Within that set, a slot is bucketed by ProxyID: "" → holdout arm, anything
// else → proxied arm. A slot recorded under a proxyID other than the
// project's CURRENT AgentProxy.ID() (a rotated pin's stale history) is
// excluded from both arms — it belongs to neither the current proxied
// population nor the holdout population, and folding it into either would
// blend populations calibKey deliberately keeps disjoint.
func collectProjectExperiment(rec *registry.Record) (ProjectExperiment, error) {
	pe := ProjectExperiment{
		ProjectID:            rec.ProjectID,
		Account:              rec.AccountProfile,
		ProxyID:              rec.AgentProxy.ID(),
		HoldoutFraction:      rec.AgentProxy.EffectiveHoldout(),
		CalibrationSlopeSeam: calibrationSlopeSeamNote,
	}

	proxiedBeads := map[string]*beadArmAgg{}
	holdoutBeads := map[string]*beadArmAgg{}

	koryphRoot := paths.KoryphRoot(rec.Root)
	entries, err := os.ReadDir(koryphRoot)
	if err != nil {
		pe.Proxied = finalizeArm("proxied", proxiedBeads)
		pe.Holdout = finalizeArm("holdout", holdoutBeads)
		return pe, nil
	}

	var runIDs []string
	seen := map[string]bool{}
	for _, e := range entries {
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		ledgerPath := filepath.Join(koryphRoot, e.Name(), "ledger.json")
		resolved := ledgerPath
		if r, rerr := filepath.EvalSymlinks(ledgerPath); rerr == nil {
			resolved = r
		}
		if seen[resolved] {
			continue
		}
		seen[resolved] = true
		if fsx.Exists(ledgerPath) {
			runIDs = append(runIDs, e.Name())
		}
	}
	sort.Strings(runIDs) // lexical = chronological (timestamp-prefixed IDs)

	for _, runID := range runIDs {
		ledgerPath := filepath.Join(koryphRoot, runID, "ledger.json")
		var run ledger.Run
		if err := fsx.ReadJSON(ledgerPath, &run); err != nil {
			continue
		}
		for _, sl := range run.Slots {
			if sl == nil || !sl.ProxyConfigured {
				continue
			}
			var bucket map[string]*beadArmAgg
			switch sl.ProxyID {
			case "":
				bucket = holdoutBeads
			case pe.ProxyID:
				bucket = proxiedBeads
			default:
				continue // stale/rotated pin's history — belongs to neither current arm
			}
			agg, ok := bucket[sl.PhaseID]
			if !ok {
				agg = &beadArmAgg{}
				bucket[sl.PhaseID] = agg
			}
			agg.tier = sl.Model
			agg.in = sl.InputTokens
			agg.out = sl.OutputTokens
			agg.cr = sl.CacheReadTokens
			agg.cc = sl.CacheCreationTokens
			agg.costUSD = sl.CostUSD
			agg.requeued = sl.GateRequeues > 0 || sl.MergeRequeues > 0 ||
				sl.RateLimitRequeues > 0 || sl.BudgetKillRequeues > 0 || sl.ConflictRequeues > 0
			agg.blockingReview = sl.ReviewIters > 0
			agg.gateFailures = sl.GateRequeues
		}
	}

	pe.Proxied = finalizeArm("proxied", proxiedBeads)
	pe.Holdout = finalizeArm("holdout", holdoutBeads)

	if cfg, err := quota.LoadConfig(quotaAccountFor(rec)); err == nil {
		applyEstimatorStats(&pe.Proxied, &pe.Holdout, cfg, pe.ProxyID)
	}

	return pe, nil
}

// quotaAccountFor mirrors internal/engine's runner.quotaName() /
// internal/registry's Store.Save proxy-flip staleness lookup: QuotaProfile
// wins when set, else AccountProfile.
func quotaAccountFor(rec *registry.Record) string {
	if rec.QuotaProfile != "" {
		return rec.QuotaProfile
	}
	return rec.AccountProfile
}

// finalizeArm turns a bead-aggregate map into the arm's rendered totals.
func finalizeArm(name string, beads map[string]*beadArmAgg) ExperimentArm {
	arm := ExperimentArm{Name: name, ByTier: map[string]TierTokenStat{}}
	if len(beads) == 0 {
		return arm
	}
	var totalIn, totalOut, totalCR, totalCC int64
	var requeuedCount, blockingCount int
	for _, agg := range beads {
		totalIn += agg.in
		totalOut += agg.out
		totalCR += agg.cr
		totalCC += agg.cc
		arm.CostUSD += agg.costUSD
		arm.GateFailures += agg.gateFailures
		if agg.requeued {
			requeuedCount++
		}
		if agg.blockingReview {
			blockingCount++
		}

		tier := agg.tier
		ts := arm.ByTier[tier]
		ts.Tier = tier
		ts.Slots++
		ts.Composition = makeComposition(
			ts.Composition.Input+agg.in,
			ts.Composition.Output+agg.out,
			ts.Composition.CacheRead+agg.cr,
			ts.Composition.CacheCreation+agg.cc,
		)
		arm.ByTier[tier] = ts
	}
	for k, ts := range arm.ByTier {
		if ts.Slots > 0 {
			ts.MeanPerSlot = ts.Composition.Total / int64(ts.Slots)
		}
		arm.ByTier[k] = ts
	}

	arm.Beads = len(beads)
	arm.Composition = makeComposition(totalIn, totalOut, totalCR, totalCC)
	arm.MeanTokensPerBead = arm.Composition.Total / int64(arm.Beads)
	arm.MeanCostPerBead = arm.CostUSD / float64(arm.Beads)
	arm.RequeueRate = float64(requeuedCount) / float64(arm.Beads)
	arm.BlockingReviewFindingRate = float64(blockingCount) / float64(arm.Beads)
	return arm
}

// applyEstimatorStats folds an account's quota.Config.ErrorStats into the
// matching arm, N-weighted across every (tier,size) bucket that arm owns
// (calibKey's proxyID segmentation: "" → holdout, currentProxyID → proxied;
// anything else is a stale/rotated identity and is skipped here too, for the
// same reason collectProjectExperiment skips it above).
func applyEstimatorStats(proxied, holdout *ExperimentArm, cfg *quota.Config, currentProxyID string) {
	for key, es := range cfg.ErrorStats {
		if es == nil || es.N == 0 {
			continue
		}
		_, _, proxyID := quota.ParseCalibKey(key)
		var arm *ExperimentArm
		switch proxyID {
		case "":
			arm = holdout
		case currentProxyID:
			arm = proxied
		default:
			continue
		}
		newN := arm.EstimatorN + es.N
		arm.EstimatorBias = (arm.EstimatorBias*float64(arm.EstimatorN) + es.Bias*float64(es.N)) / float64(newN)
		arm.EstimatorMAPE = (arm.EstimatorMAPE*float64(arm.EstimatorN) + es.MAPE*float64(es.N)) / float64(newN)
		arm.EstimatorN = newN
	}
}

// RenderExperiment writes a human-readable two-arm comparison table per
// project to w. For machine-readable output use --json
// (cmdMetricsTokens --experiment).
func RenderExperiment(r *ExperimentReport, w io.Writer) {
	if r == nil || len(r.Projects) == 0 {
		fmt.Fprintln(w, "no experiment data yet (configure agent_proxy and dispatch to accumulate it)")
		return
	}
	for _, p := range r.Projects {
		fmt.Fprintf(w, "project: %s  account: %s  proxy: %s  holdout: %.0f%%\n",
			p.ProjectID, p.Account, p.ProxyID, p.HoldoutFraction*100)

		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  ARM\tBEADS\tTOKENS/BEAD\tCACHE_HIT\tREQUEUE_RATE\tBLOCKING_REVIEW_RATE\tGATE_FAILURES\tCOST\tCOST/BEAD\tEST_BIAS(N)")
		for _, arm := range []ExperimentArm{p.Proxied, p.Holdout} {
			biasCol := "—"
			if arm.EstimatorN > 0 {
				biasCol = fmt.Sprintf("%.2f(%d)", arm.EstimatorBias, arm.EstimatorN)
			}
			fmt.Fprintf(tw, "  %s\t%d\t%s\t%.1f%%\t%.1f%%\t%.1f%%\t%d\t$%.2f\t$%.2f\t%s\n",
				arm.Name, arm.Beads, fmtTokens(arm.MeanTokensPerBead),
				arm.Composition.CacheHitRatio*100, arm.RequeueRate*100,
				arm.BlockingReviewFindingRate*100, arm.GateFailures,
				arm.CostUSD, arm.MeanCostPerBead, biasCol)
		}
		tw.Flush()
		fmt.Fprintf(w, "  calibration slope: %s\n\n", p.CalibrationSlopeSeam)
	}
}
