// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Burndown snapshot types and computation helpers for the TUI burndown tab
// (koryph-9af.7).
//
// Data sources:
//   - Ledger history (ListRuns + LoadRun) — velocity, duration stats,
//     observed parallelism; pure file reads.
//   - Beads adapter (bd ready / bd list --parent) — ready frontier count
//     and per-epic child totals; cached at burndownTTL cadence (5 s) to
//     avoid hammering bd on every 100 ms refresh tick.
//   - Quota config (LoadConfig) — per-tier estimator calibration for cost
//     projection (file read only; no ccusage subprocess in the TUI).
//
// Uncertainty contract: every projection surfaces P50 and P90 computed from
// the actual distribution of observed values. When fewer than MinSamples
// observations exist we set Sparse = true (or InsufficientHistory = true for
// the whole snapshot) and render "insufficient history (n=N)" in the TUI
// instead of extrapolating from noise.
package cockpit

import (
	"math"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

const (
	// burndownMaxRuns limits how many historical runs are scanned for stats.
	burndownMaxRuns = 30

	// burndownVelocityDays is the trailing window used to compute velocity.
	burndownVelocityDays = 14

	// MinSamples is the minimum observation count before we render stats
	// instead of "insufficient history". Exported for use by the TUI.
	MinSamples = 5

	// SparklineLen is the number of data-points in a rendered sparkline.
	// Exported for use by the TUI layer to size columns.
	SparklineLen = 12

	// blockChars encodes nine fill levels as Unicode block elements (index 0
	// = space for zero, indices 1–8 = ▁▂▃▄▅▆▇█).
	blockChars = " ▁▂▃▄▅▆▇█"
)

// BurndownFit is the cost-fits-in-window indicator colour.
type BurndownFit int

const (
	FitUnknown BurndownFit = iota // insufficient data
	FitGreen                      // projected P90 < 70 % of remaining window
	FitAmber                      // 70–100 % of remaining window
	FitRed                        // projected P90 > remaining window
)

// BurndownSnapshot holds trajectory projections for the burndown tab.
// The zero value is safe: every consumer checks InsufficientHistory / Sparse
// before rendering numbers.
type BurndownSnapshot struct {
	ComputedAt time.Time

	// Epics is per-epic burndown, sorted by Remaining desc.
	Epics           []EpicBurndown
	AllEpicsSummary EpicBurndown // cross-epic rollup (Title == "all epics")

	// Backlog is the whole-graph backlog drain view.
	Backlog BacklogBurndown

	// Cost is the cost-to-drain projection.
	Cost CostBurndown

	// DurationStats holds per-model-tier wall-time rolling stats.
	DurationStats []DurationStat
}

// EpicBurndown is one epic's burndown view.
type EpicBurndown struct {
	EpicID    string
	Title     string
	Total     int // all child beads
	Merged    int // merged/done children (from bd list --parent)
	Remaining int // = Total − Merged

	VelocityPerDay float64 // beads completed per day (P50 EWMA); 0 = unknown
	VelocityN      int     // distinct calendar days with ≥1 completion

	ETAP50 time.Time // zero = insufficient data
	ETAP90 time.Time

	// Sparkline is a fixed-width block-char chart of merged-per-day for the
	// most recent SparklineLen days.
	Sparkline string
}

// BacklogBurndown is the whole-graph drain view.
type BacklogBurndown struct {
	Ready          int // current ready-frontier size (from bd ready)
	TotalRemaining int // approximated as Ready when full list unavailable

	// CriticalPathHops is an approximation of the longest dep chain still
	// ahead (max DependencyCount in the ready frontier + 1).
	CriticalPathHops int

	// ObservedParallelism is the mean peak-concurrent-agents count across
	// recent completed runs.
	ObservedParallelism float64
	ParallelismN        int // runs sampled

	DrainETA_P50 time.Time
	DrainETA_P90 time.Time

	InsufficientHistory bool
	HistoryN            int // completed runs sampled

	Sparkline string // recent daily-completion trend
}

// CostBurndown is the cost-to-drain projection.
type CostBurndown struct {
	RemainingBeads      int
	AvgCostPerBead      float64 // P50 from observed slot costs
	ProjectedP50USD     float64
	ProjectedP90USD     float64
	WindowRemainingUSD  float64
	WindowCeilingUSD    float64
	WindowSpentUSD      float64
	Fit                 BurndownFit
	InsufficientHistory bool // fewer than MinSamples cost observations
}

// DurationStat is per-model-tier wall-time stats derived from
// DispatchedAt → MergedAt across all historical ledger runs.
type DurationStat struct {
	Tier   string        // model tier: "haiku" / "sonnet" / "opus" / "fable"
	N      int           // total observations
	Mean   time.Duration // arithmetic mean
	P50    time.Duration
	P90    time.Duration
	Sparse bool // true when N < MinSamples
}

// slotObs is one completed-slot observation extracted from the ledger.
type slotObs struct {
	mergedAt time.Time
	duration time.Duration
	tier     string
	epicID   string
	costUSD  float64
}

// burndownInput carries the raw data collected before computing the snapshot.
type burndownInput struct {
	runs         []*ledger.Run
	readyIssues  []beads.Issue
	epicChildren map[string][]beads.Issue // epicID → children
	// epicTitles maps epic id → display title (from the queue tree); nil ok.
	epicTitles map[string]string
	// openTasks is the count of ALL open workable issues (from the queue
	// tree), not just the ready frontier; 0 means unknown → fall back to the
	// ready count.
	openTasks  int
	quotaCfg   *quota.Config // may be nil
	quotaUsage *quota.Usage  // may be nil (no transcript scan available)
	now        time.Time
}

// computeBurndown assembles a BurndownSnapshot from raw inputs.
// All errors are soft — missing data produces InsufficientHistory/Sparse
// values rather than a hard error so the TUI always renders something.
func computeBurndown(inp burndownInput) BurndownSnapshot {
	snap := BurndownSnapshot{ComputedAt: inp.now}

	// Collect observations from ledger history.
	cutoff := inp.now.AddDate(0, 0, -burndownVelocityDays)
	var observations []slotObs
	var runParallelisms []float64

	for _, run := range inp.runs {
		if run == nil {
			continue
		}
		runObs := collectRunObs(run, cutoff)
		observations = append(observations, runObs...)
		if p := maxConcurrency(run); p > 0 {
			runParallelisms = append(runParallelisms, float64(p))
		}
	}

	snap.DurationStats = buildDurationStats(observations)
	snap.Backlog = buildBacklog(inp, observations, runParallelisms)
	snap.Epics, snap.AllEpicsSummary = buildEpicBurndowns(inp, observations)
	snap.Cost = buildCostBurndown(inp, observations)
	return snap
}

// collectRunObs returns per-slot observations from one run for slots merged
// within [cutoff, now).
func collectRunObs(run *ledger.Run, cutoff time.Time) []slotObs {
	var out []slotObs
	for _, sl := range run.Slots {
		if sl == nil || sl.MergedAt == "" || sl.DispatchedAt == "" {
			continue
		}
		merged, err1 := time.Parse(time.RFC3339, sl.MergedAt)
		dispatched, err2 := time.Parse(time.RFC3339, sl.DispatchedAt)
		if err1 != nil || err2 != nil {
			continue
		}
		// Only include terminal-success statuses.
		switch sl.Status {
		case ledger.SlotMerged, ledger.SlotDone, ledger.SlotPROpened:
			// ok
		default:
			continue
		}
		if merged.Before(cutoff) {
			continue
		}
		dur := merged.Sub(dispatched)
		if dur < 0 {
			dur = 0
		}
		out = append(out, slotObs{
			mergedAt: merged,
			duration: dur,
			tier:     modelTierOf(sl.Model),
			epicID:   sl.EpicID,
			costUSD:  sl.CostUSD,
		})
	}
	return out
}

// maxConcurrency returns the peak number of simultaneously-running slots
// in a single run, computed from (DispatchedAt, MergedAt) windows.
func maxConcurrency(run *ledger.Run) int {
	type window struct{ start, end time.Time }
	var windows []window
	for _, sl := range run.Slots {
		if sl == nil || sl.DispatchedAt == "" || sl.MergedAt == "" {
			continue
		}
		s, err1 := time.Parse(time.RFC3339, sl.DispatchedAt)
		e, err2 := time.Parse(time.RFC3339, sl.MergedAt)
		if err1 != nil || err2 != nil || e.Before(s) {
			continue
		}
		windows = append(windows, window{s, e})
	}
	if len(windows) == 0 {
		return 0
	}
	max := 1
	for i, a := range windows {
		c := 1
		for j, b := range windows {
			if i == j {
				continue
			}
			if b.start.Before(a.end) && b.end.After(a.start) {
				c++
			}
		}
		if c > max {
			max = c
		}
	}
	return max
}

// buildDurationStats groups observations by tier and computes N, Mean, P50, P90.
func buildDurationStats(obs []slotObs) []DurationStat {
	byTier := map[string][]time.Duration{}
	for _, o := range obs {
		byTier[o.tier] = append(byTier[o.tier], o.duration)
	}
	tiers := make([]string, 0, len(byTier))
	for t := range byTier {
		tiers = append(tiers, t)
	}
	sort.Strings(tiers)

	stats := make([]DurationStat, 0, len(tiers))
	for _, tier := range tiers {
		durs := byTier[tier]
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		n := len(durs)
		var total time.Duration
		for _, d := range durs {
			total += d
		}
		mean := time.Duration(0)
		if n > 0 {
			mean = total / time.Duration(n)
		}
		stats = append(stats, DurationStat{
			Tier:   tier,
			N:      n,
			Mean:   mean,
			P50:    durationPercentile(durs, 50),
			P90:    durationPercentile(durs, 90),
			Sparse: n < MinSamples,
		})
	}
	return stats
}

// buildBacklog assembles the BacklogBurndown.
func buildBacklog(inp burndownInput, obs []slotObs, runParallelisms []float64) BacklogBurndown {
	b := BacklogBurndown{
		Ready:          len(inp.readyIssues),
		TotalRemaining: len(inp.readyIssues), // fallback when the queue tree is unavailable
		HistoryN:       len(inp.runs),
	}
	// The queue tree's open-task count is the honest denominator: blocked and
	// deferred beads still have to drain too. The old ready-frontier
	// approximation could understate remaining work several-fold on a deeply
	// chained backlog, making the drain ETA look far better than reality.
	if inp.openTasks > b.TotalRemaining {
		b.TotalRemaining = inp.openTasks
	}
	if b.HistoryN < MinSamples {
		b.InsufficientHistory = true
	}

	// Critical-path hops: max DependencyCount in the ready frontier + 1.
	// Ready beads have had all deps satisfied; DependencyCount here reflects
	// how deep the chain was. This is a lower-bound approximation.
	for _, iss := range inp.readyIssues {
		if iss.DependencyCount > b.CriticalPathHops {
			b.CriticalPathHops = iss.DependencyCount
		}
	}
	b.CriticalPathHops++ // +1 for the ready bead itself

	// Observed parallelism.
	if len(runParallelisms) > 0 {
		sum := 0.0
		for _, p := range runParallelisms {
			sum += p
		}
		b.ObservedParallelism = sum / float64(len(runParallelisms))
		b.ParallelismN = len(runParallelisms)
	} else {
		b.ObservedParallelism = 1.0
	}

	// Velocity (beads/day).
	dailyCounts := dailyMergedCounts(obs, inp.now, burndownVelocityDays)
	vel, velVar := velocityStats(dailyCounts)
	b.Sparkline = sparklineFromCounts(dailyCounts, SparklineLen)

	// Drain ETA.
	if !b.InsufficientHistory && vel > 0 && b.TotalRemaining > 0 {
		// vel is OBSERVED fleet-wide merges/day — parallelism is already baked
		// into it (N concurrent agents each merging shows up as N× the daily
		// count). The previous code multiplied vel by ObservedParallelism
		// again, double-counting concurrency and making every drain ETA
		// optimistic by roughly that factor. ObservedParallelism stays a
		// reported metric; it is no longer a throughput multiplier.
		throughput := vel // beads/day, as measured
		daysP50 := float64(b.TotalRemaining) / throughput

		// P90: lower the throughput by 1.28σ (normal approximation).
		stddev := math.Sqrt(velVar)
		daysP90 := daysP50
		if stddev > 0 {
			reducedThroughput := math.Max(throughput-1.28*stddev, throughput*0.3)
			daysP90 = float64(b.TotalRemaining) / reducedThroughput
		}

		// Critical-path floor: cannot drain faster than the longest sequential
		// chain regardless of parallelism.
		var allDurs []time.Duration
		for _, o := range obs {
			allDurs = append(allDurs, o.duration)
		}
		sort.Slice(allDurs, func(i, j int) bool { return allDurs[i] < allDurs[j] })
		p50dur := durationPercentile(allDurs, 50)
		if p50dur > 0 {
			critPathDays := float64(b.CriticalPathHops) * p50dur.Hours() / 24.0
			if critPathDays > daysP50 {
				daysP50 = critPathDays
			}
			if critPathDays > daysP90 {
				daysP90 = critPathDays
			}
		}

		b.DrainETA_P50 = inp.now.Add(daysToDuration(daysP50))
		b.DrainETA_P90 = inp.now.Add(daysToDuration(daysP90))
	}

	return b
}

// buildEpicBurndowns assembles per-epic and summary burndown data.
func buildEpicBurndowns(inp burndownInput, obs []slotObs) ([]EpicBurndown, EpicBurndown) {
	epicObs := map[string][]slotObs{}
	for _, o := range obs {
		if o.epicID != "" {
			epicObs[o.epicID] = append(epicObs[o.epicID], o)
		}
	}

	var epics []EpicBurndown
	var totalMerged, totalChildren int

	for epicID, children := range inp.epicChildren {
		total := len(children)
		merged := 0
		for _, ch := range children {
			switch ch.Status {
			case "closed", "merged", "done":
				merged++
			}
		}
		remaining := total - merged
		if remaining < 0 {
			remaining = 0
		}

		eObs := epicObs[epicID]
		dailyCounts := dailyMergedCounts(eObs, inp.now, burndownVelocityDays)
		vel, velVar := velocityStats(dailyCounts)

		title := inp.epicTitles[epicID]
		if title == "" {
			title = epicID // no bd title available (e.g. epic already closed)
		}
		eb := EpicBurndown{
			EpicID:         epicID,
			Title:          title,
			Total:          total,
			Merged:         merged,
			Remaining:      remaining,
			VelocityPerDay: vel,
			VelocityN:      len(eObs),
			Sparkline:      sparklineFromCounts(dailyCounts, SparklineLen),
		}
		if vel > 0 && remaining > 0 {
			daysP50 := float64(remaining) / vel
			daysP90 := daysP50
			stddev := math.Sqrt(velVar)
			if stddev > 0 {
				daysP90 = float64(remaining) / math.Max(vel-1.28*stddev, vel*0.3)
			}
			eb.ETAP50 = inp.now.Add(daysToDuration(daysP50))
			eb.ETAP90 = inp.now.Add(daysToDuration(daysP90))
		}

		epics = append(epics, eb)
		totalMerged += merged
		totalChildren += total
	}

	sort.Slice(epics, func(i, j int) bool {
		return epics[i].Remaining > epics[j].Remaining
	})

	// Summary.
	allDaily := dailyMergedCounts(obs, inp.now, burndownVelocityDays)
	allVel, allVelVar := velocityStats(allDaily)
	summary := EpicBurndown{
		Title:          "all epics",
		Total:          totalChildren,
		Merged:         totalMerged,
		Remaining:      totalChildren - totalMerged,
		VelocityPerDay: allVel,
		VelocityN:      len(obs),
		Sparkline:      sparklineFromCounts(allDaily, SparklineLen),
	}
	if allVel > 0 && summary.Remaining > 0 {
		daysP50 := float64(summary.Remaining) / allVel
		daysP90 := daysP50
		stddev := math.Sqrt(allVelVar)
		if stddev > 0 {
			daysP90 = float64(summary.Remaining) / math.Max(allVel-1.28*stddev, allVel*0.3)
		}
		summary.ETAP50 = inp.now.Add(daysToDuration(daysP50))
		summary.ETAP90 = inp.now.Add(daysToDuration(daysP90))
	}

	return epics, summary
}

// buildCostBurndown assembles the cost-to-drain projection.
func buildCostBurndown(inp burndownInput, obs []slotObs) CostBurndown {
	cb := CostBurndown{
		RemainingBeads: len(inp.readyIssues),
	}

	var costs []float64
	for _, o := range obs {
		if o.costUSD > 0 {
			costs = append(costs, o.costUSD)
		}
	}
	cb.InsufficientHistory = len(costs) < MinSamples

	if len(costs) > 0 {
		sort.Float64s(costs)
		cb.AvgCostPerBead = floatPercentile(costs, 50)
		p90cost := floatPercentile(costs, 90)
		cb.ProjectedP50USD = cb.AvgCostPerBead * float64(cb.RemainingBeads)
		cb.ProjectedP90USD = p90cost * float64(cb.RemainingBeads)
	} else if inp.quotaCfg != nil {
		// Fall back to quota estimator (sonnet/M as a reasonable default).
		base := quota.EstimateItem(inp.quotaCfg, "sonnet", "M")
		cb.AvgCostPerBead = base
		cb.ProjectedP50USD = base * float64(cb.RemainingBeads)
		cb.ProjectedP90USD = base * 1.5 * float64(cb.RemainingBeads)
	}

	// Quota window (from Usage if available; otherwise window = 0 = unknown).
	if inp.quotaUsage != nil {
		w := inp.quotaUsage.Window5h
		cb.WindowCeilingUSD = w.CeilingUSD
		cb.WindowSpentUSD = w.SpentUSD
		cb.WindowRemainingUSD = w.CeilingUSD - w.SpentUSD
		if cb.WindowRemainingUSD < 0 {
			cb.WindowRemainingUSD = 0
		}
	}

	// Fit indicator.
	if cb.WindowRemainingUSD <= 0 || cb.ProjectedP90USD <= 0 {
		cb.Fit = FitUnknown
	} else {
		ratio := cb.ProjectedP90USD / cb.WindowRemainingUSD
		switch {
		case ratio < 0.70:
			cb.Fit = FitGreen
		case ratio < 1.00:
			cb.Fit = FitAmber
		default:
			cb.Fit = FitRed
		}
	}

	return cb
}

// --- helpers ----------------------------------------------------------------

// modelTierOf extracts a short tier name from a model string.
// "claude-opus-4-5" → "opus", "claude-sonnet-4" → "sonnet", etc.
func modelTierOf(model string) string {
	model = strings.ToLower(model)
	for _, prefix := range []string{"claude-", "claude.ai/"} {
		model = strings.TrimPrefix(model, prefix)
	}
	for _, tier := range []string{"haiku", "sonnet", "opus", "fable"} {
		if strings.HasPrefix(model, tier) {
			return tier
		}
	}
	if model == "" {
		return "unknown"
	}
	if i := strings.IndexByte(model, '-'); i > 0 {
		return model[:i]
	}
	return model
}

// dailyMergedCounts returns the number of completions per calendar day (UTC)
// for the last nDays days. Index 0 = oldest day, index nDays-1 = today.
func dailyMergedCounts(obs []slotObs, now time.Time, nDays int) []float64 {
	counts := make([]float64, nDays)
	todayUTC := now.UTC().Truncate(24 * time.Hour)
	for _, o := range obs {
		delta := int(todayUTC.Sub(o.mergedAt.UTC().Truncate(24*time.Hour)).Hours() / 24)
		if delta < 0 || delta >= nDays {
			continue
		}
		counts[nDays-1-delta]++
	}
	return counts
}

// velocityStats returns (mean_per_day, variance_per_day).
func velocityStats(dailyCounts []float64) (mean, variance float64) {
	if len(dailyCounts) == 0 {
		return 0, 0
	}
	var sum float64
	for _, c := range dailyCounts {
		sum += c
	}
	mean = sum / float64(len(dailyCounts))
	var varSum float64
	for _, c := range dailyCounts {
		d := c - mean
		varSum += d * d
	}
	variance = varSum / float64(len(dailyCounts))
	return mean, variance
}

// sparklineFromCounts renders a fixed-width block-char sparkline from a
// daily-counts series. n is the desired output width.
func sparklineFromCounts(dailyCounts []float64, n int) string {
	series := dailyCounts
	if len(series) > n {
		series = series[len(series)-n:]
	}
	// Pad with zeros on the left.
	padded := make([]float64, n)
	copy(padded[n-len(series):], series)

	maxVal := 0.0
	for _, v := range padded {
		if v > maxVal {
			maxVal = v
		}
	}

	runes := []rune(blockChars)
	nLevels := float64(len(runes) - 1) // 8 levels

	var sb strings.Builder
	for _, v := range padded {
		if maxVal == 0 {
			sb.WriteRune(' ')
			continue
		}
		idx := int(math.Round(v / maxVal * nLevels))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(runes) {
			idx = len(runes) - 1
		}
		sb.WriteRune(runes[idx])
	}
	return sb.String()
}

// durationPercentile returns the pth percentile (0–100) from a sorted slice.
func durationPercentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := (p / 100.0) * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo] + time.Duration(frac*float64(sorted[hi]-sorted[lo]))
}

// floatPercentile returns the pth percentile (0–100) from a sorted float64 slice.
func floatPercentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	if len(sorted) == 1 {
		return sorted[0]
	}
	idx := (p / 100.0) * float64(len(sorted)-1)
	lo := int(math.Floor(idx))
	hi := int(math.Ceil(idx))
	if lo == hi {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// daysToDuration converts a fractional day count to a time.Duration.
func daysToDuration(days float64) time.Duration {
	return time.Duration(days * 24 * float64(time.Hour))
}

// FormatETARange formats a P50/P90 ETA pair for display.
// Returns "P50 <date> / P90 <date>" or "insufficient history (n=N)".
func FormatETARange(p50, p90 time.Time, n int, now time.Time) string {
	if p50.IsZero() {
		return "insufficient history (n=" + burndownItoa(n) + ")"
	}
	p50s := etaStr(p50, now)
	p90s := etaStr(p90, now)
	if p90s == p50s {
		return "~" + p50s
	}
	return "P50 " + p50s + " / P90 " + p90s
}

// etaStr formats a time as "Jan 2 (+Nd)" relative to now.
func etaStr(t, now time.Time) string {
	days := int(math.Round(t.Sub(now).Hours() / 24))
	if days < 0 {
		days = 0
	}
	return t.Format("Jan 2") + " (+" + burndownItoa(days) + "d)"
}

// FitLabel returns a short label for a BurndownFit value.
func FitLabel(f BurndownFit) string {
	switch f {
	case FitGreen:
		return "✓ fits"
	case FitAmber:
		return "⚠ marginal"
	case FitRed:
		return "✗ over budget"
	default:
		return "? unknown"
	}
}

// burndownItoa converts an int to string without importing strconv in this file.
func burndownItoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
