// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package resmon

import (
	"testing"
	"time"
)

func TestLiveCohortUsageDeduplicatesGroupsTreesAndRoots(t *testing.T) {
	table := newProcTable([]procInfo{
		{pid: 10, ppid: 1, pgid: 10, rssKB: 1024},
		{pid: 11, ppid: 10, pgid: 10, rssKB: 2048},
		// Reparented but still in cohort 10's process group.
		{pid: 12, ppid: 1, pgid: 10, rssKB: 3072},
		{pid: 20, ppid: 1, pgid: 20, rssKB: 4096},
	}, false)

	got := LiveCohortUsage(table, []int{10, 10, 20, 999})
	if got.RSSKB != 10*1024 || got.RSSMB != 10 || got.Cohorts != 2 ||
		got.Processes != 4 || got.MissingRoots != 1 || got.RSSKnown {
		t.Fatalf("live usage = %+v", got)
	}
	complete := LiveCohortUsage(table, []int{10, 20})
	if !complete.RSSKnown || complete.RSSMB != 10 {
		t.Fatalf("complete live usage = %+v", complete)
	}
}

func TestCalibrateRuntimeEstimatePercentileMarginAndExclusions(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	samples := []AttemptMemory{
		{Runtime: "codex", PeakMB: 500, Successful: true, FinishedAt: t0},
		{Runtime: "codex", PeakMB: 600, Successful: true, FinishedAt: t0.Add(time.Minute)},
		{Runtime: "codex", PeakMB: 700, Successful: true, FinishedAt: t0.Add(2 * time.Minute)},
		{Runtime: "codex", PeakMB: 800, Successful: true, FinishedAt: t0.Add(3 * time.Minute)},
		{Runtime: "codex", PeakMB: 1000, Successful: true, FinishedAt: t0.Add(4 * time.Minute)},
		{Runtime: "codex", PeakMB: 9000, Successful: false, FinishedAt: t0.Add(5 * time.Minute)},
		{
			Runtime: "codex", PeakMB: 8000, Successful: true,
			DuplicateBroadCommand: true, FinishedAt: t0.Add(6 * time.Minute),
		},
		{Runtime: "claude", PeakMB: 9999, Successful: true, FinishedAt: t0.Add(7 * time.Minute)},
	}

	got := CalibrateRuntimeEstimate("codex", 0, samples, EstimatePolicy{})
	if got.EstimateMB != 1200 || got.ProposedMB != 1200 || !got.Changed {
		t.Fatalf("estimate = %+v, want p90 1000 + 20%% = 1200", got)
	}
	if got.EligibleSamples != 5 || got.ExcludedFailed != 1 || got.ExcludedDuplicate != 1 {
		t.Errorf("calibration evidence = %+v", got)
	}
}

func TestCalibrateRuntimeEstimateUsesRecentBoundedWindow(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	samples := []AttemptMemory{
		{Runtime: "codex", PeakMB: 5000, Successful: true, FinishedAt: t0},
		{Runtime: "codex", PeakMB: 400, Successful: true, FinishedAt: t0.Add(time.Minute)},
		{Runtime: "codex", PeakMB: 500, Successful: true, FinishedAt: t0.Add(2 * time.Minute)},
		{Runtime: "codex", PeakMB: 600, Successful: true, FinishedAt: t0.Add(3 * time.Minute)},
	}
	got := CalibrateRuntimeEstimate("codex", 0, samples, EstimatePolicy{
		MinSamples: 3, MaxSamples: 3, Percentile: 1, MarginPercent: -1, HysteresisPercent: -1,
	})
	if got.EstimateMB != 600 {
		t.Fatalf("bounded estimate = %+v, want oldest 5000 MB sample excluded", got)
	}
}

func TestCalibrateRuntimeEstimateHysteresisAndMinimumEvidence(t *testing.T) {
	samples := []AttemptMemory{
		{Runtime: "codex", PeakMB: 900, Successful: true},
		{Runtime: "codex", PeakMB: 920, Successful: true},
	}
	insufficient := CalibrateRuntimeEstimate("codex", 1200, samples, EstimatePolicy{})
	if insufficient.EstimateMB != 1200 || insufficient.Changed || insufficient.ProposedMB != 0 {
		t.Fatalf("insufficient evidence changed estimate: %+v", insufficient)
	}

	samples = append(samples, AttemptMemory{Runtime: "codex", PeakMB: 950, Successful: true})
	stable := CalibrateRuntimeEstimate("codex", 1200, samples, EstimatePolicy{
		Percentile: 1, MarginPercent: 20, HysteresisPercent: 10,
	})
	// Proposed = 950*1.2 = 1140, only 5% below current, so keep 1200.
	if stable.ProposedMB != 1140 || stable.EstimateMB != 1200 || stable.Changed {
		t.Fatalf("hysteresis result = %+v", stable)
	}
}
