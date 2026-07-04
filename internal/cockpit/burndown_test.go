// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
)

// --- modelTierOf -------------------------------------------------------------

func TestModelTierOf(t *testing.T) {
	cases := []struct {
		model string
		want  string
	}{
		{"claude-sonnet-4-5", "sonnet"},
		{"claude-opus-4", "opus"},
		{"claude-haiku-3-5", "haiku"},
		{"claude-fable-1", "fable"},
		{"claude-sonnet-4-5-20251020", "sonnet"},
		{"sonnet-4", "sonnet"},
		{"", "unknown"},
		{"custom-model", "custom"},
	}
	for _, tc := range cases {
		if got := modelTierOf(tc.model); got != tc.want {
			t.Errorf("modelTierOf(%q) = %q, want %q", tc.model, got, tc.want)
		}
	}
}

// --- durationPercentile ------------------------------------------------------

func TestDurationPercentile(t *testing.T) {
	durs := []time.Duration{
		10 * time.Minute,
		20 * time.Minute,
		30 * time.Minute,
		40 * time.Minute,
		50 * time.Minute,
	}

	if got := durationPercentile(nil, 50); got != 0 {
		t.Errorf("empty P50 = %v, want 0", got)
	}
	if got := durationPercentile(durs, 0); got != 10*time.Minute {
		t.Errorf("P0 = %v, want 10m", got)
	}
	if got := durationPercentile(durs, 100); got != 50*time.Minute {
		t.Errorf("P100 = %v, want 50m", got)
	}
	// P50 of 5 values (index 2) = 30m.
	if got := durationPercentile(durs, 50); got != 30*time.Minute {
		t.Errorf("P50 = %v, want 30m", got)
	}
}

// --- floatPercentile ---------------------------------------------------------

func TestFloatPercentile(t *testing.T) {
	vals := []float64{1, 2, 3, 4, 5}
	if got := floatPercentile(nil, 50); got != 0 {
		t.Errorf("empty P50 = %g, want 0", got)
	}
	if got := floatPercentile(vals, 0); got != 1 {
		t.Errorf("P0 = %g, want 1", got)
	}
	if got := floatPercentile(vals, 100); got != 5 {
		t.Errorf("P100 = %g, want 5", got)
	}
}

// --- velocityStats -----------------------------------------------------------

func TestVelocityStats(t *testing.T) {
	// All-zero series → mean=0, variance=0.
	zeros := make([]float64, 7)
	m, v := velocityStats(zeros)
	if m != 0 || v != 0 {
		t.Errorf("all-zero: mean=%g var=%g, want 0,0", m, v)
	}

	// Constant series [2,2,2,2,2,2,2] → mean=2, variance=0.
	consts := []float64{2, 2, 2, 2, 2, 2, 2}
	m, v = velocityStats(consts)
	if m != 2 || v != 0 {
		t.Errorf("constant: mean=%g var=%g, want 2,0", m, v)
	}
}

// --- sparklineFromCounts -----------------------------------------------------

func TestSparklineFromCounts(t *testing.T) {
	// All-zero → all spaces.
	s := sparklineFromCounts(make([]float64, 5), 5)
	for _, r := range s {
		if r != ' ' {
			t.Errorf("all-zero sparkline should be spaces, got %q", s)
			break
		}
	}

	// Non-zero: last char should be the full block.
	counts := []float64{0, 0, 0, 0, 10}
	s = sparklineFromCounts(counts, 5)
	runes := []rune(s)
	if len(runes) != 5 {
		t.Fatalf("sparkline length = %d, want 5", len(runes))
	}
	// Last position is max → full block (█ = index 8).
	wantLast := []rune(blockChars)[8]
	if runes[4] != wantLast {
		t.Errorf("last char = %q, want %q", string(runes[4]), string(wantLast))
	}
}

// --- maxConcurrency ----------------------------------------------------------

func TestMaxConcurrency(t *testing.T) {
	base := time.Date(2026, 7, 4, 10, 0, 0, 0, time.UTC)

	// Two overlapping slots → concurrency 2.
	run := &ledger.Run{
		Slots: map[string]*ledger.Slot{
			"a": {
				Status:       ledger.SlotMerged,
				DispatchedAt: base.Format(time.RFC3339),
				MergedAt:     base.Add(30 * time.Minute).Format(time.RFC3339),
			},
			"b": {
				Status:       ledger.SlotMerged,
				DispatchedAt: base.Add(10 * time.Minute).Format(time.RFC3339),
				MergedAt:     base.Add(45 * time.Minute).Format(time.RFC3339),
			},
		},
	}
	if got := maxConcurrency(run); got != 2 {
		t.Errorf("overlapping slots: maxConcurrency = %d, want 2", got)
	}

	// Two sequential slots → concurrency 1.
	seqRun := &ledger.Run{
		Slots: map[string]*ledger.Slot{
			"a": {
				Status:       ledger.SlotMerged,
				DispatchedAt: base.Format(time.RFC3339),
				MergedAt:     base.Add(20 * time.Minute).Format(time.RFC3339),
			},
			"b": {
				Status:       ledger.SlotMerged,
				DispatchedAt: base.Add(25 * time.Minute).Format(time.RFC3339),
				MergedAt:     base.Add(50 * time.Minute).Format(time.RFC3339),
			},
		},
	}
	if got := maxConcurrency(seqRun); got != 1 {
		t.Errorf("sequential slots: maxConcurrency = %d, want 1", got)
	}
}

// --- computeBurndown integration test ----------------------------------------

func TestComputeBurndown_Empty(t *testing.T) {
	snap := computeBurndown(burndownInput{
		now: time.Now(),
	})
	if !snap.ComputedAt.IsZero() && snap.Backlog.Ready != 0 {
		t.Errorf("empty input: Ready=%d, want 0", snap.Backlog.Ready)
	}
	if !snap.Backlog.InsufficientHistory {
		t.Error("empty input: want InsufficientHistory=true")
	}
	if len(snap.DurationStats) != 0 {
		t.Errorf("empty input: DurationStats len=%d, want 0", len(snap.DurationStats))
	}
}

func TestComputeBurndown_WithHistory(t *testing.T) {
	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	base := now.AddDate(0, 0, -7) // 7 days ago

	// Build 8 completed slots across two runs: 4 slots per run.
	mkSlot := func(id string, offset time.Duration, model string) *ledger.Slot {
		return &ledger.Slot{
			PhaseID:      id,
			Status:       ledger.SlotMerged,
			Model:        model,
			CostUSD:      2.5,
			DispatchedAt: base.Add(offset).Format(time.RFC3339),
			MergedAt:     base.Add(offset + 20*time.Minute).Format(time.RFC3339),
		}
	}

	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"a": mkSlot("a", 0, "claude-sonnet-4-5"),
				"b": mkSlot("b", 10*time.Minute, "claude-sonnet-4-5"),
				"c": mkSlot("c", 24*time.Hour, "claude-haiku-3-5"),
				"d": mkSlot("d", 24*time.Hour+15*time.Minute, "claude-haiku-3-5"),
			},
		},
		{
			RunID: "run2",
			Slots: map[string]*ledger.Slot{
				"e": mkSlot("e", 48*time.Hour, "claude-sonnet-4-5"),
				"f": mkSlot("f", 48*time.Hour+10*time.Minute, "claude-sonnet-4-5"),
				"g": mkSlot("g", 72*time.Hour, "claude-opus-4"),
				"h": mkSlot("h", 72*time.Hour+30*time.Minute, "claude-opus-4"),
			},
		},
	}

	readyIssues := []beads.Issue{
		{ID: "x-1", DependencyCount: 2},
		{ID: "x-2", DependencyCount: 1},
		{ID: "x-3", DependencyCount: 0},
	}

	snap := computeBurndown(burndownInput{
		runs:        runs,
		readyIssues: readyIssues,
		now:         now,
	})

	// Should have duration stats for sonnet, haiku, opus.
	tiers := map[string]bool{}
	for _, ds := range snap.DurationStats {
		tiers[ds.Tier] = true
	}
	for _, want := range []string{"sonnet", "haiku", "opus"} {
		if !tiers[want] {
			t.Errorf("DurationStats missing tier %q; tiers=%v", want, tiers)
		}
	}

	// Backlog should reflect 3 ready issues.
	if snap.Backlog.Ready != 3 {
		t.Errorf("Backlog.Ready = %d, want 3", snap.Backlog.Ready)
	}

	// Critical path = max(DependencyCount) + 1 = 2 + 1 = 3.
	if snap.Backlog.CriticalPathHops != 3 {
		t.Errorf("CriticalPathHops = %d, want 3", snap.Backlog.CriticalPathHops)
	}

	// Cost: 8 observations of $2.50 each.
	if snap.Cost.InsufficientHistory {
		t.Error("Cost.InsufficientHistory = true with 8 observations, want false")
	}
	if snap.Cost.AvgCostPerBead != 2.5 {
		t.Errorf("AvgCostPerBead = %g, want 2.5", snap.Cost.AvgCostPerBead)
	}
	if snap.Cost.ProjectedP50USD != 2.5*3 {
		t.Errorf("ProjectedP50USD = %g, want 7.5", snap.Cost.ProjectedP50USD)
	}
}

// --- FormatETARange ----------------------------------------------------------

func TestFormatETARange_InsufficientHistory(t *testing.T) {
	now := time.Now()
	s := FormatETARange(time.Time{}, time.Time{}, 2, now)
	if s != "insufficient history (n=2)" {
		t.Errorf("got %q, want \"insufficient history (n=2)\"", s)
	}
}

func TestFormatETARange_WithETA(t *testing.T) {
	now := time.Date(2026, 7, 4, 0, 0, 0, 0, time.UTC)
	p50 := now.Add(5 * 24 * time.Hour)  // Jul 9
	p90 := now.Add(10 * 24 * time.Hour) // Jul 14
	s := FormatETARange(p50, p90, 10, now)
	// Must contain "P50" and "P90".
	if len(s) == 0 {
		t.Fatal("FormatETARange returned empty string")
	}
	if s == "insufficient history (n=10)" {
		t.Error("should not return insufficient history with valid ETAs")
	}
}

// --- FitLabel ----------------------------------------------------------------

func TestFitLabel(t *testing.T) {
	cases := []struct {
		f    BurndownFit
		want string
	}{
		{FitUnknown, "? unknown"},
		{FitGreen, "✓ fits"},
		{FitAmber, "⚠ marginal"},
		{FitRed, "✗ over budget"},
	}
	for _, tc := range cases {
		if got := FitLabel(tc.f); got != tc.want {
			t.Errorf("FitLabel(%d) = %q, want %q", tc.f, got, tc.want)
		}
	}
}
