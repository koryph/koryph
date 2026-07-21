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
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

const (
	// efficiencyMaxRuns limits how many historical runs are scanned for the
	// dispatch sparkline.
	efficiencyMaxRuns = 30

	// maxDeferralTokens caps the number of top-deferral-token rows displayed.
	maxDeferralTokens = 8
)

// efficiencyInput carries the raw data collected before computing the snapshot.
type efficiencyInput struct {
	runs        []*ledger.Run      // historical ledger runs (newest first)
	activeSlots []*ledger.Slot     // slots currently running/dispatching
	govObs      govern.Observation // read-only governor observation (may be zero)
	govSnap     GovernorSnapshot   // already-fetched pool snapshot (fallback)
	quotaCfg    *quota.Config      // may be nil (uncalibrated)
	quotaUsage  *quota.Usage       // may be nil (no transcript scan available)
	titles      map[string]string  // bead id → display title (may be nil)
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
	snap.GovernorPools = buildGovernorPools(inp.govObs, inp.govSnap, inp.now)

	// --- estimator table -----------------------------------------------------
	snap.EstimatorRows = buildEstimatorTable(inp.quotaCfg)

	// --- quota windows (per provider) -----------------------------------------
	snap.ProviderQuotas = buildQuotaWindows(inp.quotaCfg, inp.quotaUsage)

	// --- token economy (koryph-77r.3, design §3 L1) ---------------------------
	te := buildTokenEconomy(inp.runs, inp.titles, inp.now)
	snap.TokenRows = te.rows
	snap.ModelRows = te.modelRows
	snap.FleetCacheHitRatio = te.fleetRatio
	snap.FleetCacheHit24h = te.recentRatio
	snap.CacheHitTripwire = te.tripwire
	snap.TokensPerBeadTrend = te.trend

	return snap
}

// maxTokenRows caps how many per-bead token rows are shown in the TUI.
const maxTokenRows = 12

// cacheHitWarnThreshold is the fleet-wide cache_read share below which the
// I7 tripwire fires. Matching design §2 I7: WARN when "cache_read share
// collapses mid-run".
const cacheHitWarnThreshold = 0.80

// tokenEconomy is buildTokenEconomy's result bundle.
type tokenEconomy struct {
	rows       []TokenCompositionRow
	modelRows  []ModelTokenRow
	fleetRatio float64 // all-history cache_read share
	// recentRatio is the cache_read share over slots dispatched in the last
	// 24 h; negative when no slot in that window carries token data. This is
	// the actionable number — an all-history ratio buries a fresh prompt or
	// cache regression under weeks of healthy data.
	recentRatio float64
	tripwire    string
	trend       []float64
}

// buildTokenEconomy assembles the per-bead token table, per-model rollup,
// fleet cache-hit ratios, tripwire state, and tokens-per-bead trend from
// historical ledger runs. titles maps bead id → display title (may be nil).
// All errors are soft; empty/zero values render gracefully in the TUI.
func buildTokenEconomy(runs []*ledger.Run, titles map[string]string, now time.Time) tokenEconomy {
	te := tokenEconomy{recentRatio: -1}

	// One row per slot with non-zero token fields, tagged with its dispatch
	// time so the table can show the most RECENT beads (the previous code
	// sorted by slot id within each run, so the "recent work" table was
	// actually alphabetical).
	type timedRow struct {
		row        TokenCompositionRow
		dispatched time.Time
	}
	var all []timedRow
	tokensByDay := make([]float64, SparklineLen)
	countsByDay := make([]float64, SparklineLen)
	todayUTC := now.UTC().Truncate(24 * time.Hour)

	var fleetFresh, fleetRead, fleetCreation int64
	var recentFresh, recentRead, recentCreation int64
	modelAgg := map[string]*ModelTokenRow{}

	for _, run := range runs {
		if run == nil {
			continue
		}
		for _, sl := range run.Slots {
			if sl == nil {
				continue
			}
			total := sl.InputTokens + sl.OutputTokens +
				sl.CacheReadTokens + sl.CacheCreationTokens
			if total == 0 {
				continue // ledger predates token fields or no data yet
			}

			inputTotal := sl.InputTokens + sl.CacheReadTokens + sl.CacheCreationTokens
			ratio := 0.0
			if inputTotal > 0 {
				ratio = float64(sl.CacheReadTokens) / float64(inputTotal)
			}

			beadID := sl.BeadID
			if beadID == "" {
				beadID = sl.PhaseID
			}
			title := titles[beadID]
			if title == "" {
				title = beadID
			}

			var dispatched time.Time
			if sl.DispatchedAt != "" {
				dispatched, _ = time.Parse(time.RFC3339, sl.DispatchedAt)
			}

			all = append(all, timedRow{
				row: TokenCompositionRow{
					BeadID:        beadID,
					Title:         title,
					TotalTokens:   total,
					InputFresh:    sl.InputTokens,
					CacheRead:     sl.CacheReadTokens,
					CacheCreation: sl.CacheCreationTokens,
					Output:        sl.OutputTokens,
					CacheHitRatio: ratio,
					CostUSD:       sl.CostUSD,
				},
				dispatched: dispatched,
			})

			fleetFresh += sl.InputTokens
			fleetRead += sl.CacheReadTokens
			fleetCreation += sl.CacheCreationTokens
			if !dispatched.IsZero() && now.Sub(dispatched) <= 24*time.Hour {
				recentFresh += sl.InputTokens
				recentRead += sl.CacheReadTokens
				recentCreation += sl.CacheCreationTokens
			}

			// Per-model rollup, keyed on the model that ACTUALLY served
			// (ModelActual) with the requested model as fallback — this is the
			// "how many tokens am I burning on which model" table that drives
			// tier-change decisions.
			model := sl.ModelActual
			if model == "" {
				model = sl.Model
			}
			if model == "" {
				model = "unknown"
			}
			mr := modelAgg[model]
			if mr == nil {
				mr = &ModelTokenRow{Model: model}
				modelAgg[model] = mr
			}
			mr.Beads++
			mr.TotalTokens += total
			mr.InputFresh += sl.InputTokens
			mr.CacheRead += sl.CacheReadTokens
			mr.CacheCreation += sl.CacheCreationTokens
			mr.Output += sl.OutputTokens
			mr.CostUSD += sl.CostUSD

			// Trend: bucket by dispatch day.
			if !dispatched.IsZero() {
				delta := int(todayUTC.Sub(dispatched.UTC().Truncate(24*time.Hour)).Hours() / 24)
				if delta >= 0 && delta < SparklineLen {
					idx := SparklineLen - 1 - delta
					tokensByDay[idx] += float64(total)
					countsByDay[idx]++
				}
			}
		}
	}

	if denom := fleetFresh + fleetRead + fleetCreation; denom > 0 {
		te.fleetRatio = float64(fleetRead) / float64(denom)
	}
	if denom := recentFresh + recentRead + recentCreation; denom > 0 {
		te.recentRatio = float64(recentRead) / float64(denom)
	}

	// I7 cache-hit tripwire on the RECENT window when one exists (falling
	// back to all-history when nothing dispatched in 24 h): warn only about
	// a live regression, not archaeology.
	switch {
	case te.recentRatio >= 0 && te.recentRatio < cacheHitWarnThreshold:
		te.tripwire = "warn"
	case te.recentRatio < 0 && te.fleetRatio > 0 && te.fleetRatio < cacheHitWarnThreshold:
		te.tripwire = "warn"
	}

	te.trend = make([]float64, SparklineLen)
	for i := range te.trend {
		if countsByDay[i] > 0 {
			te.trend[i] = tokensByDay[i] / countsByDay[i]
		}
	}

	// Most recent dispatches first; rows without a timestamp sink to the end.
	sort.SliceStable(all, func(i, j int) bool {
		return all[i].dispatched.After(all[j].dispatched)
	})
	if len(all) > maxTokenRows {
		all = all[:maxTokenRows]
	}
	te.rows = make([]TokenCompositionRow, len(all))
	for i, tr := range all {
		te.rows[i] = tr.row
	}

	te.modelRows = make([]ModelTokenRow, 0, len(modelAgg))
	for _, mr := range modelAgg {
		te.modelRows = append(te.modelRows, *mr)
	}
	sort.Slice(te.modelRows, func(i, j int) bool {
		return te.modelRows[i].TotalTokens > te.modelRows[j].TotalTokens
	})
	return te
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

// permittedCap returns the effective AIMD cap from the primary (default,
// else alphabetically-first) pool — deterministic, never map iteration.
func permittedCap(gs GovernorSnapshot) int {
	if ps, ok := gs.PrimaryPool(); ok {
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

// buildGovernorPools assembles per-pool detail from the read-only governor
// observation (preferred; carries settle/probe detail) with govSnap as
// fallback when the observation is empty. Neither path touches the store —
// the days of the efficiency tab re-scanning (and pruning!) the lease
// directory on its own are over; there is exactly one governor read per
// refresh (Observe) and both consumers share it.
func buildGovernorPools(obs govern.Observation, snap GovernorSnapshot, now time.Time) []GovernorPoolDetail {
	if len(obs.Pools) > 0 {
		pools := make([]GovernorPoolDetail, 0, len(obs.Pools))
		for name, ps := range obs.Pools {
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

// buildQuotaWindows extracts quota window data from cfg and usage, one entry
// per AI provider (ProviderQuotaSnapshot). Only "claude" is ever populated
// today — internal/quota's usage sources (ccusage, the transcript JSONL scan)
// are Claude-specific, and no other runtime yet reports
// Capabilities().UsageSource == true — but the slice return shape is the
// provider-keyed join point a future runtime's quota reader appends to,
// rather than a flat pair of fields that would need a second flat pair
// bolted on beside it once a second provider exists.
func buildQuotaWindows(cfg *quota.Config, usage *quota.Usage) []ProviderQuotaSnapshot {
	pq := ProviderQuotaSnapshot{Runtime: "claude", Provider: govern.DefaultPool}

	if cfg == nil || (cfg.WindowCeilingUSD == 0 && cfg.WeeklyCeilingUSD == 0) {
		pq.Source = "uncalibrated"
		pq.Window5hFrac = -1
		pq.WeeklyFrac = -1
		return []ProviderQuotaSnapshot{pq}
	}
	pq.Window5hCeiling = cfg.WindowCeilingUSD
	pq.WeeklyCeiling = cfg.WeeklyCeilingUSD

	if usage != nil {
		pq.Window5hSpent = usage.Window5h.SpentUSD
		pq.WeeklySpent = usage.Weekly.SpentUSD
		if pq.Window5hCeiling > 0 {
			pq.Window5hFrac = pq.Window5hSpent / pq.Window5hCeiling
		}
		if pq.WeeklyCeiling > 0 {
			pq.WeeklyFrac = pq.WeeklySpent / pq.WeeklyCeiling
		}
		pq.Source = usage.Window5h.Source
		if pq.Source == "" {
			pq.Source = "unavailable"
		}
	} else {
		// No live usage available in this TUI path; mark fracs negative to
		// signal "not measured" to the renderer.
		pq.Window5hFrac = -1
		pq.WeeklyFrac = -1
		pq.Source = "unavailable"
	}
	return []ProviderQuotaSnapshot{pq}
}

// splitBucket splits a "<tier>:<size>" or "<tier>:<size>@<proxyID>" bucket
// key into (tier, size), discarding any proxy segment (koryph-3l1.3 carried
// contract from koryph-3l1.1's operator notes): this used to split on the
// bucket's first ':' directly, which corrupts once RecordForProxy starts
// writing "@proxyID" suffixes, because size would come back as
// "M@<proxyID>" — an unrecognized SizeMultiplier key (silently defaulting to
// 1.0) and an unrecognized sizeOrder bucket (silently sorting last). Delegates
// to quota.ParseCalibKey, which strips the proxy segment FIRST — see its doc
// for why order matters (a proxyID like "http://127.0.0.1:8787" has colons
// of its own). "M" is this function's own no-colon-found default (unchanged
// from before this bead); ParseCalibKey itself has no opinion on defaults.
func splitBucket(bucket string) (tier, size string) {
	tier, size, _ = quota.ParseCalibKey(bucket)
	if size == "" {
		size = "M"
	}
	return tier, size
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
