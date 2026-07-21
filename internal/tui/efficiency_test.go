// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

// Tests for the Efficiency tab token economy section (koryph-77r.3, §L1).

import (
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/cockpit"
)

// effSnap returns an EfficiencySnapshot for testing the token section.
func effSnap(rows []cockpit.TokenCompositionRow, ratio float64, tripwire string, trend []float64) cockpit.EfficiencySnapshot {
	if trend == nil {
		trend = make([]float64, cockpit.SparklineLen)
	}
	return cockpit.EfficiencySnapshot{
		ComputedAt:         time.Now(),
		FleetCacheHitRatio: ratio,
		CacheHitTripwire:   tripwire,
		TokenRows:          rows,
		TokensPerBeadTrend: trend,
	}
}

// TestRenderTokenSection_EmptyState verifies that the empty state renders a
// "no token data" hint (not a crash or blank).
func TestRenderTokenSection_EmptyState(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 100
	snap := effSnap(nil, 0, "", nil)
	out := m.renderTokenSection(snap)

	if !strings.Contains(out, "Token Economy") {
		t.Errorf("output missing section title; got: %q", out[:min(len(out), 80)])
	}
	if !strings.Contains(out, "no token data") {
		t.Errorf("empty state: expected 'no token data' hint; got: %q", out[:min(len(out), 200)])
	}
}

// TestRenderTokenSection_HealthyRatio verifies that a healthy ratio (>=0.90)
// renders without a tripwire warning.
func TestRenderTokenSection_HealthyRatio(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 120

	row := cockpit.TokenCompositionRow{
		BeadID:        "abc-1",
		Title:         "abc-1",
		TotalTokens:   955_000,
		InputFresh:    5_000,
		CacheRead:     900_000,
		CacheCreation: 40_000,
		Output:        10_000,
		CacheHitRatio: 0.9526,
		CostUSD:       2.0,
	}
	trend := make([]float64, cockpit.SparklineLen)
	trend[cockpit.SparklineLen-1] = 955_000 // today

	snap := effSnap([]cockpit.TokenCompositionRow{row}, 0.9526, "", trend)
	out := m.renderTokenSection(snap)

	if !strings.Contains(out, "95.3%") {
		t.Errorf("expected cache-hit ratio '95.3%%' in output; got excerpt: %q",
			stripANSI(out)[:min(len(stripANSI(out)), 200)])
	}
	// No tripwire warning should appear.
	if strings.Contains(stripANSI(out), "cache_read share collapsed") {
		t.Errorf("healthy ratio should not show the tripwire warning; got: %q", stripANSI(out))
	}
	// The bead ID should appear in the table.
	if !strings.Contains(stripANSI(out), "abc-1") {
		t.Errorf("bead 'abc-1' not found in table; got: %q", stripANSI(out))
	}
	// Trend sparkline should be present.
	if !strings.Contains(out, "tokens/bead trend") {
		t.Errorf("missing tokens/bead trend line; got: %q", stripANSI(out)[:min(len(stripANSI(out)), 300)])
	}
}

// TestRenderTokenSection_TripwireWarn verifies that the I7 tripwire renders when
// CacheHitTripwire is "warn".
func TestRenderTokenSection_TripwireWarn(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 120

	row := cockpit.TokenCompositionRow{
		BeadID:        "bead-x",
		TotalTokens:   101_000,
		InputFresh:    90_000,
		CacheRead:     10_000,
		CacheCreation: 1_000,
		Output:        10_000,
		CacheHitRatio: 0.099,
		CostUSD:       0.5,
	}
	snap := effSnap([]cockpit.TokenCompositionRow{row}, 0.099, "warn", nil)
	out := m.renderTokenSection(snap)

	if !strings.Contains(stripANSI(out), "cache_read share collapsed") {
		t.Errorf("expected tripwire warning in output for low cache-hit ratio; got: %q",
			stripANSI(out)[:min(len(stripANSI(out)), 300)])
	}
}

// TestRenderTokenSection_LargeTokenCount verifies K/M suffix formatting.
func TestRenderTokenSection_LargeTokenCount(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 120

	row := cockpit.TokenCompositionRow{
		BeadID:        "big-bead",
		TotalTokens:   1_255_000_000, // 1.255B as in the design doc
		InputFresh:    6_250_000,
		CacheRead:     1_188_850_000,
		CacheCreation: 47_740_000,
		Output:        12_160_000,
		CacheHitRatio: 0.947,
		CostUSD:       100.0,
	}
	snap := effSnap([]cockpit.TokenCompositionRow{row}, 0.947, "", nil)
	out := stripANSI(m.renderTokenSection(snap))

	// Should see "M" or "G" suffix for the large numbers.
	if !strings.Contains(out, "K") && !strings.Contains(out, "M") {
		t.Errorf("large counts should use K/M suffix; got excerpt: %q", out[:min(len(out), 200)])
	}
}

// TestFormatTokenCount verifies the K/M formatting helper.
func TestFormatTokenCount(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1_000, "1.0K"},
		{1_500, "1.5K"},
		{999_999, "1000.0K"},
		{1_000_000, "1.0M"},
		{1_255_000, "1.3M"},
	}
	for _, tc := range cases {
		got := formatTokenCount(tc.in)
		if got != tc.want {
			t.Errorf("formatTokenCount(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// min returns the smaller of a and b.
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// --- Resources section (koryph-4ql.1 L7, koryph-4ql.10) --------------------

// TestRenderResourcesSection_Empty verifies the empty state (old snapshot, or
// a governor with no res:* activity) renders a hint, not a crash or blank.
func TestRenderResourcesSection_Empty(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 100
	out := m.renderResourcesSection(cockpit.GovernorSnapshot{})

	if !strings.Contains(out, "Resources") {
		t.Errorf("output missing section title; got: %q", out[:min(len(out), 80)])
	}
	if !strings.Contains(out, "no declared resource kinds") {
		t.Errorf("empty state hint missing; got: %q", stripANSI(out))
	}
}

// TestRenderResourcesSection_AtCapacity verifies a kind at capacity renders
// its holder (project/bead) and the reserved/materialized MB split.
func TestRenderResourcesSection_AtCapacity(t *testing.T) {
	m := newEfficiencyModel(DefaultTheme())
	m.width = 120

	gov := cockpit.GovernorSnapshot{
		Resources: []cockpit.ResourceSnapshot{
			{
				Kind:           "kind-cluster",
				Capacity:       1,
				MemMB:          6144,
				ReservedMB:     6144,
				MaterializedMB: 0,
				Holders: []cockpit.ResourceHolderSnapshot{
					{Project: "proj", Bead: "koryph-abc", MemReserveMB: 6144, Ramping: true},
				},
			},
		},
	}
	out := stripANSI(m.renderResourcesSection(gov))

	if !strings.Contains(out, "kind-cluster") {
		t.Errorf("kind name missing; got: %q", out)
	}
	if !strings.Contains(out, "cap:1/1") {
		t.Errorf("capacity 1/1 missing; got: %q", out)
	}
	if !strings.Contains(out, "reserved:6144MB") || !strings.Contains(out, "materialized:0MB") {
		t.Errorf("reserved/materialized split missing; got: %q", out)
	}
	if !strings.Contains(out, "proj/koryph-abc") {
		t.Errorf("holder proj/koryph-abc missing; got: %q", out)
	}
	if !strings.Contains(out, "ramping") {
		t.Errorf("ramping annotation missing; got: %q", out)
	}
}
