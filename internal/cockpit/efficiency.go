// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Efficiency snapshot types and computation for the TUI Efficiency tab
// (koryph-9af.4, design §2.4).
//
// Data sources (all file reads; no subprocesses):
//   - Ledger runs (DispatchedAt timestamps) → dispatched-per-day sparkline.
//   - Active slots (Slot.Footprint) → held write-tokens for deferral ranking.
//   - govern.Store → per-pool cap/AIMD/settle/breaker detail.
//   - quota.Config → calibration (Calibration, ErrorStats) + window ceilings.
//   - quota.Usage → live spend fractions (nil in normal TUI path; shown as
//     "unavailable" with a calibrate/usage hint).
//
// Uncertainty contract: every field that requires live ccusage data is left
// zero/empty and QuotaSource is set to "unavailable" so the TUI renders a
// clear "run koryph quota usage" hint rather than silent zeros.
package cockpit

import (
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

const (
	// efficiencyTTL is how long the efficiency snapshot is cached.
	// Matches burndownTTL so both expensive derivations share one cadence.
	efficiencyTTL = 5 * time.Second

	// efficiencyMaxRuns limits how many historical runs are scanned for the
	// dispatch sparkline.
	efficiencyMaxRuns = 30

	// maxDeferralTokens caps the number of top-deferral-token rows displayed.
	maxDeferralTokens = 8
)

// efficiencyInput carries the raw data collected before computing the snapshot.
type efficiencyInput struct {
	runs        []*ledger.Run    // historical ledger runs (newest first)
	activeSlots []*ledger.Slot   // slots currently running/dispatching
	govStore    *govern.Store    // live governor (nil → pools from govSnap only)
	govSnap     GovernorSnapshot // already-fetched pool snapshot (fallback)
	quotaCfg    *quota.Config    // may be nil (uncalibrated)
	quotaUsage  *quota.Usage     // may be nil (ccusage not run in TUI path)
	now         time.Time
}

// computeEfficiency assembles an EfficiencySnapshot from raw inputs.
// All errors are soft — missing data yields zero / placeholder values.
func computeEfficiency(inp efficiencyInput) EfficiencySnapshot {
	snap := EfficiencySnapshot{ComputedAt: inp.now}

	// --- dispatch sparkline ---------------------------------------------------
	snap.DispatchSparkline = buildDispatchSparkline(inp.runs, inp.now)

	// --- concurrency ---------------------------------------------------------
	snap.AchievedConcurrency = countRunning(inp.activeSlots)
	snap.PermittedConcurrency = permittedCap(inp.govSnap)

	// --- deferral tokens (write-tokens held by active slots) -----------------
	snap.TopDeferralTokens = topDeferralTokens(inp.activeSlots)

	// --- governor pool detail ------------------------------------------------
	snap.GovernorPools = buildGovernorPools(inp.govStore, inp.govSnap, inp.now)

	// --- estimator table -----------------------------------------------------
	snap.EstimatorRows = buildEstimatorTable(inp.quotaCfg)

	// --- quota windows -------------------------------------------------------
	snap.QuotaSource, snap.QuotaWindow5hCeiling, snap.QuotaWindowWeeklyCeiling,
		snap.QuotaWindow5hSpent, snap.QuotaWindowWeeklySpent,
		snap.QuotaWindow5hFrac, snap.QuotaWindowWeeklyFrac =
		buildQuotaWindow(inp.quotaCfg, inp.quotaUsage)

	// --- token economy (koryph-77r.3, design §3 L1) ---------------------------
	snap.TokenRows, snap.FleetCacheHitRatio, snap.CacheHitTripwire,
		snap.TokensPerBeadTrend =
		buildTokenEconomy(inp.runs, inp.now)

	return snap
}

// maxTokenRows caps how many per-bead token rows are shown in the TUI.
const maxTokenRows = 12

// cacheHitWarnThreshold is the fleet-wide cache_read share below which the
// I7 tripwire fires. Matching design §2 I7: WARN when "cache_read share
// collapses mid-run".
const cacheHitWarnThreshold = 0.80

// buildTokenEconomy assembles the per-bead token table, fleet cache-hit ratio,
// tripwire state, and tokens-per-bead trend from historical ledger runs.
// All errors are soft; empty/zero values render gracefully in the TUI.
func buildTokenEconomy(runs []*ledger.Run, now time.Time) (
	rows []TokenCompositionRow,
	fleetCacheHitRatio float64,
	tripwire string,
	trendSeries []float64,
) {
	// Collect one row per completed slot that has non-zero token fields.
	// We use a slice to preserve ledger order (newest run first from caller).
	var allRows []TokenCompositionRow
	// Per-day totals for the trend sparkline (index 0 = oldest).
	tokensByDay := make([]float64, SparklineLen) // total tokens
	countsByDay := make([]float64, SparklineLen) // beads with data
	todayUTC := now.UTC().Truncate(24 * time.Hour)

	// Fleet-wide accumulators for the cache-hit ratio.
	var (
		fleetFresh    int64
		fleetRead     int64
		fleetCreation int64
	)

	for _, run := range runs {
		if run == nil {
			continue
		}
		// Sort slot keys for stable row ordering within a run.
		ids := make([]string, 0, len(run.Slots))
		for id := range run.Slots {
			ids = append(ids, id)
		}
		sort.Strings(ids)

		for _, id := range ids {
			sl := run.Slots[id]
			if sl == nil {
				continue
			}
			total := sl.InputTokens + sl.OutputTokens +
				sl.CacheReadTokens + sl.CacheCreationTokens
			if total == 0 {
				continue // ledger predates token fields or no data yet
			}

			// Compute per-slot cache-hit ratio.
			inputTotal := sl.InputTokens + sl.CacheReadTokens + sl.CacheCreationTokens
			ratio := 0.0
			if inputTotal > 0 {
				ratio = float64(sl.CacheReadTokens) / float64(inputTotal)
			}

			beadID := sl.BeadID
			if beadID == "" {
				beadID = sl.PhaseID
			}

			allRows = append(allRows, TokenCompositionRow{
				BeadID:        beadID,
				Title:         beadID, // no bd lookup in this path; caller may enrich
				TotalTokens:   total,
				InputFresh:    sl.InputTokens,
				CacheRead:     sl.CacheReadTokens,
				CacheCreation: sl.CacheCreationTokens,
				Output:        sl.OutputTokens,
				CacheHitRatio: ratio,
				CostUSD:       sl.CostUSD,
			})

			// Fleet-wide accumulation.
			fleetFresh += sl.InputTokens
			fleetRead += sl.CacheReadTokens
			fleetCreation += sl.CacheCreationTokens

			// Trend: bucket by dispatch day.
			if sl.DispatchedAt != "" {
				if t, err := time.Parse(time.RFC3339, sl.DispatchedAt); err == nil {
					delta := int(todayUTC.Sub(t.UTC().Truncate(24*time.Hour)).Hours() / 24)
					if delta >= 0 && delta < SparklineLen {
						idx := SparklineLen - 1 - delta
						tokensByDay[idx] += float64(total)
						countsByDay[idx]++
					}
				}
			}
		}
	}

	// Fleet cache-hit ratio.
	fleetDenom := fleetFresh + fleetRead + fleetCreation
	if fleetDenom > 0 {
		fleetCacheHitRatio = float64(fleetRead) / float64(fleetDenom)
	}

	// I7 cache-hit tripwire: WARN when ratio is below threshold and we have data.
	if fleetDenom > 0 && fleetCacheHitRatio < cacheHitWarnThreshold {
		tripwire = "warn"
	}

	// Tokens-per-bead trend: mean for each day bucket.
	trendSeries = make([]float64, SparklineLen)
	for i := range trendSeries {
		if countsByDay[i] > 0 {
			trendSeries[i] = tokensByDay[i] / countsByDay[i]
		}
	}

	// Trim allRows to the most recent maxTokenRows (the slice is already in
	// run order with newest runs first from the caller's load order).
	if len(allRows) > maxTokenRows {
		allRows = allRows[:maxTokenRows]
	}
	rows = allRows
	return
}

// buildDispatchSparkline counts slots dispatched per calendar day (UTC) for
// the last SparklineLen days across all historical runs.
func buildDispatchSparkline(runs []*ledger.Run, now time.Time) []float64 {
	counts := make([]float64, SparklineLen)
	todayUTC := now.UTC().Truncate(24 * time.Hour)
	for _, run := range runs {
		if run == nil {
			continue
		}
		for _, sl := range run.Slots {
			if sl == nil || sl.DispatchedAt == "" {
				continue
			}
			t, err := time.Parse(time.RFC3339, sl.DispatchedAt)
			if err != nil {
				continue
			}
			delta := int(todayUTC.Sub(t.UTC().Truncate(24*time.Hour)).Hours() / 24)
			if delta < 0 || delta >= SparklineLen {
				continue
			}
			counts[SparklineLen-1-delta]++
		}
	}
	return counts
}

// countRunning returns the number of slots in running or dispatching state.
func countRunning(slots []*ledger.Slot) int {
	n := 0
	for _, sl := range slots {
		if sl == nil {
			continue
		}
		switch sl.Status {
		case ledger.SlotRunning, ledger.SlotDispatching:
			n++
		}
	}
	return n
}

// permittedCap returns the effective AIMD cap from the default pool.
func permittedCap(gs GovernorSnapshot) int {
	if ps, ok := gs.Pools[govern.DefaultPool]; ok {
		if ps.Dynamic > 0 {
			return ps.Dynamic
		}
		if ps.Cap > 0 {
			return ps.Cap
		}
	}
	return govern.DefaultMaxGlobalAgents
}

// topDeferralTokens tallies write-tokens held by active slots and returns the
// top maxDeferralTokens entries sorted by hold-count descending.
// This surfaces which footprint areas are "locked up" most often —
// the coupling measurement the efficiency tab exists to show.
func topDeferralTokens(slots []*ledger.Slot) []DeferralToken {
	holdCount := map[string]int{}
	for _, sl := range slots {
		if sl == nil || sl.Footprint == nil {
			continue
		}
		switch sl.Status {
		case ledger.SlotRunning, ledger.SlotDispatching, ledger.SlotReview, ledger.SlotMergePending:
			// Slot is consuming footprint.
		default:
			continue
		}
		for _, tok := range sl.Footprint.Writes {
			holdCount[tok]++
		}
	}

	tokens := make([]DeferralToken, 0, len(holdCount))
	for tok, n := range holdCount {
		tokens = append(tokens, DeferralToken{Token: tok, HeldBy: n})
	}
	sort.Slice(tokens, func(i, j int) bool {
		if tokens[i].HeldBy != tokens[j].HeldBy {
			return tokens[i].HeldBy > tokens[j].HeldBy
		}
		return tokens[i].Token < tokens[j].Token
	})
	if len(tokens) > maxDeferralTokens {
		tokens = tokens[:maxDeferralTokens]
	}
	return tokens
}

// buildGovernorPools assembles per-pool detail from the govern store (preferred)
// with govSnap as fallback when the store is unavailable.
func buildGovernorPools(gs *govern.Store, snap GovernorSnapshot, now time.Time) []GovernorPoolDetail {
	if gs != nil {
		return buildGovernorPoolsFromStore(gs, now)
	}
	// Fallback: build from already-fetched snapshot (no settle/probe detail).
	pools := make([]GovernorPoolDetail, 0, len(snap.Pools))
	for _, ps := range snap.Pools {
		pools = append(pools, GovernorPoolDetail{
			Provider:     ps.Provider,
			Cap:          ps.Cap,
			Dynamic:      ps.Dynamic,
			Leases:       ps.Leases,
			Adaptive:     ps.Adaptive,
			BreakerState: ps.BreakerState,
		})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Provider < pools[j].Provider })
	return pools
}

// buildGovernorPoolsFromStore reads richer pool detail directly from the govern
// store (settle window, probe identity) for the efficiency dashboard.
func buildGovernorPoolsFromStore(gs *govern.Store, now time.Time) []GovernorPoolDetail {
	poolNames, err := gs.Pools()
	if err != nil {
		return nil
	}
	pools := make([]GovernorPoolDetail, 0, len(poolNames))
	for _, name := range poolNames {
		ps, err := gs.PoolStatus(name)
		if err != nil {
			continue
		}
		cfg := ps.AIMD
		dynamic := cfg.DynamicCap
		if dynamic <= 0 {
			dynamic = cfg.MaxGlobalAgents
		}
		if dynamic <= 0 {
			dynamic = govern.DefaultMaxGlobalAgents
		}

		settling := false
		if cfg.SettleUntil != "" {
			if t, err := time.Parse(time.RFC3339, cfg.SettleUntil); err == nil {
				settling = t.After(now)
			}
		}

		pools = append(pools, GovernorPoolDetail{
			Provider:     name,
			Cap:          cfg.MaxGlobalAgents,
			Dynamic:      dynamic,
			Leases:       len(ps.Leases),
			Adaptive:     cfg.Adaptive,
			BreakerState: cfg.BreakerState,
			Settling:     settling,
			SettleUntil:  cfg.SettleUntil,
			ProbeProject: cfg.ProbeProject,
			ProbeBead:    cfg.ProbeBead,
		})
	}
	sort.Slice(pools, func(i, j int) bool { return pools[i].Provider < pools[j].Provider })
	return pools
}

// buildEstimatorTable assembles the per-bucket estimator accuracy table from
// quota.Config.ErrorStats (koryph-6bl) and Calibration.
// Returns nil when cfg is nil or has no ErrorStats.
func buildEstimatorTable(cfg *quota.Config) []EstimatorRow {
	if cfg == nil {
		return nil
	}

	// Collect known buckets from both ErrorStats and Calibration.
	buckets := map[string]struct{}{}
	for k := range cfg.ErrorStats {
		buckets[k] = struct{}{}
	}
	for k := range cfg.Calibration {
		buckets[k] = struct{}{}
	}
	if len(buckets) == 0 {
		return nil
	}

	rows := make([]EstimatorRow, 0, len(buckets))
	for bucket := range buckets {
		tier, size := splitBucket(bucket)
		base := baseEstimate(cfg, tier, size)
		corrected := cfg.Calibration[bucket] // 0 if not calibrated

		row := EstimatorRow{
			Bucket:    bucket,
			Corrected: corrected,
			Base:      base,
		}
		if es, ok := cfg.ErrorStats[bucket]; ok && es != nil {
			row.N = es.N
			row.Bias = es.Bias
			row.MAPE = es.MAPE
		}
		rows = append(rows, row)
	}

	// Sort: by tier then size (S < M < L), then bucket string as fallback.
	sort.Slice(rows, func(i, j int) bool {
		return bucketLess(rows[i].Bucket, rows[j].Bucket)
	})
	return rows
}

// buildQuotaWindow extracts quota window data from cfg and usage.
// Returns (source, 5hCeiling, weeklyCeiling, 5hSpent, weeklySpent, 5hFrac, weeklyFrac).
func buildQuotaWindow(cfg *quota.Config, usage *quota.Usage) (
	source string,
	w5hCeiling, wWeeklyCeiling float64,
	w5hSpent, wWeeklySpent float64,
	w5hFrac, wWeeklyFrac float64,
) {
	if cfg == nil || (cfg.WindowCeilingUSD == 0 && cfg.WeeklyCeilingUSD == 0) {
		return "uncalibrated", 0, 0, 0, 0, -1, -1
	}
	w5hCeiling = cfg.WindowCeilingUSD
	wWeeklyCeiling = cfg.WeeklyCeilingUSD

	if usage != nil {
		w5hSpent = usage.Window5h.SpentUSD
		wWeeklySpent = usage.Weekly.SpentUSD
		if w5hCeiling > 0 {
			w5hFrac = w5hSpent / w5hCeiling
		}
		if wWeeklyCeiling > 0 {
			wWeeklyFrac = wWeeklySpent / wWeeklyCeiling
		}
		source = usage.Window5h.Source
		if source == "" {
			source = "unavailable"
		}
	} else {
		// No live usage available in this TUI path; mark fracs negative to
		// signal "not measured" to the renderer.
		w5hFrac = -1
		wWeeklyFrac = -1
		source = "unavailable"
	}
	return
}

// splitBucket splits a "<tier>:<size>" bucket key into (tier, size).
func splitBucket(bucket string) (tier, size string) {
	if idx := strings.Index(bucket, ":"); idx >= 0 {
		return bucket[:idx], bucket[idx+1:]
	}
	return bucket, "M"
}

// baseEstimate returns the uncalibrated base cost for (tier, size) from cfg.
func baseEstimate(cfg *quota.Config, tier, size string) float64 {
	tierBase := cfg.PerTierUSD[tier]
	if tierBase == 0 {
		tierBase = cfg.PerTierUSD["sonnet"] // fallback
		if tierBase == 0 {
			tierBase = 3.0
		}
	}
	sizeMultiplier := cfg.SizeMultiplier[size]
	if sizeMultiplier == 0 {
		sizeMultiplier = 1.0
	}
	margin := cfg.SafetyMargin
	if margin == 0 {
		margin = 1.5
	}
	return tierBase * sizeMultiplier * margin
}

// bucketLess defines the sort order for bucket keys:
// tier alphabetical then size S < M < L, fallback to full string.
func bucketLess(a, b string) bool {
	ta, sa := splitBucket(a)
	tb, sb := splitBucket(b)
	if ta != tb {
		return ta < tb
	}
	oa, ob := sizeOrder(sa), sizeOrder(sb)
	if oa != ob {
		return oa < ob
	}
	return a < b
}

// sizeOrder maps S/M/L to 0/1/2 for stable sort.
func sizeOrder(size string) int {
	switch size {
	case "S":
		return 0
	case "M":
		return 1
	case "L":
		return 2
	default:
		return 3
	}
}

// activeSlots returns the slots that are still consuming a footprint
// (running, dispatching, review, merge-pending).
func activeSlots(run *ledger.Run) []*ledger.Slot {
	if run == nil {
		return nil
	}
	var out []*ledger.Slot
	for _, sl := range run.Slots {
		if sl == nil {
			continue
		}
		switch sl.Status {
		case ledger.SlotRunning, ledger.SlotDispatching, ledger.SlotReview, ledger.SlotMergePending:
			out = append(out, sl)
		}
	}
	return out
}
