// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package resmon

import (
	"math"
	"sort"
	"time"
)

// LiveUsage is one deduplicated machine view of the agent cohorts selected by
// root PID. It is the live-RSS input to pressure-aware admission.
type LiveUsage struct {
	RSSKB        int64
	RSSMB        int
	RSSKnown     bool
	Cohorts      int
	Processes    int
	MissingRoots int
}

// LiveCohortUsage sums the union of every selected cohort exactly once. A
// process may be reachable through both process-group and parent links (or
// through duplicate roots); the global PID set prevents double charging.
func LiveCohortUsage(table *ProcTable, roots []int) LiveUsage {
	var out LiveUsage
	if table == nil {
		out.MissingRoots = len(roots)
		return out
	}

	included := make(map[int]bool)
	seenRoots := make(map[int]bool)
	for _, rootPID := range roots {
		if rootPID <= 0 || seenRoots[rootPID] {
			continue
		}
		seenRoots[rootPID] = true
		root, ok := table.byPID[rootPID]
		if !ok {
			out.MissingRoots++
			continue
		}
		out.Cohorts++
		queue := make([]int, 0, 8)
		queued := make(map[int]bool)
		enqueue := func(pid int) {
			if _, known := table.byPID[pid]; known && !queued[pid] {
				queued[pid] = true
				queue = append(queue, pid)
			}
		}
		for _, pid := range table.pgroup[root.pgid] {
			enqueue(pid)
		}
		enqueue(rootPID)
		for i := 0; i < len(queue); i++ {
			for _, child := range table.children[queue[i]] {
				enqueue(child)
			}
		}
		for _, pid := range queue {
			included[pid] = true
		}
	}

	for pid := range included {
		rss := table.byPID[pid].rssKB
		if rss > 0 {
			out.RSSKB += rss
		}
		out.Processes++
	}
	if out.RSSKB > 0 {
		out.RSSMB = int((out.RSSKB + 1023) / 1024)
	}
	out.RSSKnown = out.MissingRoots == 0 && (out.Processes == 0 || out.RSSKB > 0)
	return out
}

// AttemptMemory is one completed attempt's calibration input.
type AttemptMemory struct {
	Runtime string
	PeakMB  int

	// Successful means the attempt produced an accepted terminal candidate.
	Successful bool
	// DuplicateBroadCommand marks an attempt whose RSS included overlapping
	// repository-wide validation. Such samples are never calibration evidence,
	// whether or not the attempt eventually succeeded.
	DuplicateBroadCommand bool

	FinishedAt time.Time
}

// EstimatePolicy configures conservative runtime-memory calibration.
type EstimatePolicy struct {
	MinSamples        int
	MaxSamples        int
	Percentile        float64
	MarginPercent     int
	HysteresisPercent int
	MinimumMB         int
}

const (
	DefaultEstimateMinSamples        = 3
	DefaultEstimateMaxSamples        = 32
	DefaultEstimatePercentile        = 0.90
	DefaultEstimateMarginPercent     = 20
	DefaultEstimateHysteresisPercent = 10
	DefaultEstimateMinimumMB         = 256
)

func (p EstimatePolicy) normalized() EstimatePolicy {
	if p.MinSamples <= 0 {
		p.MinSamples = DefaultEstimateMinSamples
	}
	if p.MaxSamples <= 0 {
		p.MaxSamples = DefaultEstimateMaxSamples
	}
	if p.MaxSamples < p.MinSamples {
		p.MaxSamples = p.MinSamples
	}
	if p.Percentile <= 0 || p.Percentile > 1 {
		p.Percentile = DefaultEstimatePercentile
	}
	if p.MarginPercent < 0 {
		p.MarginPercent = 0
	} else if p.MarginPercent == 0 {
		p.MarginPercent = DefaultEstimateMarginPercent
	}
	if p.HysteresisPercent < 0 {
		p.HysteresisPercent = 0
	} else if p.HysteresisPercent == 0 {
		p.HysteresisPercent = DefaultEstimateHysteresisPercent
	}
	if p.MinimumMB <= 0 {
		p.MinimumMB = DefaultEstimateMinimumMB
	}
	return p
}

// RuntimeEstimate is the auditable result of one calibration pass.
type RuntimeEstimate struct {
	Runtime           string
	EstimateMB        int
	ProposedMB        int
	EligibleSamples   int
	ExcludedFailed    int
	ExcludedDuplicate int
	Changed           bool
}

// CalibrateRuntimeEstimate derives a bounded conservative estimate from recent
// successful, duplicate-free attempts for runtime. It uses a nearest-rank
// percentile plus margin, then suppresses small changes through hysteresis.
// Failed attempts and attempts containing duplicate broad validation never
// recalibrate the estimate.
func CalibrateRuntimeEstimate(
	runtime string,
	currentMB int,
	samples []AttemptMemory,
	policy EstimatePolicy,
) RuntimeEstimate {
	policy = policy.normalized()
	result := RuntimeEstimate{Runtime: runtime, EstimateMB: currentMB}

	type eligible struct {
		peak int
		at   time.Time
		idx  int
	}
	var usable []eligible
	for i, sample := range samples {
		if sample.Runtime != runtime || sample.PeakMB <= 0 {
			continue
		}
		if !sample.Successful {
			result.ExcludedFailed++
			continue
		}
		if sample.DuplicateBroadCommand {
			result.ExcludedDuplicate++
			continue
		}
		usable = append(usable, eligible{peak: sample.PeakMB, at: sample.FinishedAt, idx: i})
	}
	sort.SliceStable(usable, func(i, j int) bool {
		if usable[i].at.Equal(usable[j].at) {
			return usable[i].idx < usable[j].idx
		}
		if usable[i].at.IsZero() {
			return true
		}
		if usable[j].at.IsZero() {
			return false
		}
		return usable[i].at.Before(usable[j].at)
	})
	if len(usable) > policy.MaxSamples {
		usable = usable[len(usable)-policy.MaxSamples:]
	}
	result.EligibleSamples = len(usable)
	if len(usable) < policy.MinSamples {
		return result
	}

	peaks := make([]int, 0, len(usable))
	for _, sample := range usable {
		peaks = append(peaks, sample.peak)
	}
	sort.Ints(peaks)
	rank := int(math.Ceil(policy.Percentile*float64(len(peaks)))) - 1
	if rank < 0 {
		rank = 0
	}
	if rank >= len(peaks) {
		rank = len(peaks) - 1
	}
	proposed := int(math.Ceil(float64(peaks[rank]) * float64(100+policy.MarginPercent) / 100))
	if proposed < policy.MinimumMB {
		proposed = policy.MinimumMB
	}
	result.ProposedMB = proposed

	if currentMB > 0 && policy.HysteresisPercent > 0 {
		delta := proposed - currentMB
		if delta < 0 {
			delta = -delta
		}
		if delta*100 < currentMB*policy.HysteresisPercent {
			return result
		}
	}
	result.EstimateMB = proposed
	result.Changed = proposed != currentMB
	return result
}
