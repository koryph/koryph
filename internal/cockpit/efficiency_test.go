// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package cockpit

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

// TestBuildTokenEconomy_Empty verifies that an empty run list yields zero values
// and no tripwire fire.
func TestBuildTokenEconomy_Empty(t *testing.T) {
	rows, ratio, tripwire, trend := buildTokenEconomy(nil, time.Now())
	if len(rows) != 0 {
		t.Errorf("empty input: len(rows) = %d, want 0", len(rows))
	}
	if ratio != 0 {
		t.Errorf("empty input: FleetCacheHitRatio = %g, want 0", ratio)
	}
	if tripwire != "" {
		t.Errorf("empty input: tripwire = %q, want \"\"", tripwire)
	}
	if len(trend) != SparklineLen {
		t.Errorf("empty input: trend len = %d, want %d", len(trend), SparklineLen)
	}
	for i, v := range trend {
		if v != 0 {
			t.Errorf("empty input: trend[%d] = %g, want 0", i, v)
		}
	}
}

// TestBuildTokenEconomy_ZeroTokenFields verifies that slots with zero token
// fields are skipped (ledger predates token fields).
func TestBuildTokenEconomy_ZeroTokenFields(t *testing.T) {
	now := time.Now()
	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"abc-1": {
					PhaseID:      "abc-1",
					BeadID:       "abc-1",
					Status:       ledger.SlotMerged,
					CostUSD:      1.5,
					DispatchedAt: now.Add(-time.Hour).Format(time.RFC3339),
					// No token fields — legacy ledger.
				},
			},
		},
	}
	rows, ratio, tripwire, _ := buildTokenEconomy(runs, now)
	if len(rows) != 0 {
		t.Errorf("zero-token slot should be skipped; got %d rows", len(rows))
	}
	if ratio != 0 {
		t.Errorf("zero-token: ratio = %g, want 0", ratio)
	}
	if tripwire != "" {
		t.Errorf("zero-token: tripwire = %q, want \"\"", tripwire)
	}
}

// TestBuildTokenEconomy_SingleSlotHealthy verifies a single slot with a high
// cache-hit ratio yields a populated row and no tripwire.
func TestBuildTokenEconomy_SingleSlotHealthy(t *testing.T) {
	// Fixed noon-UTC clock so the "1 hour ago" dispatch always lands in today's
	// UTC bucket — with time.Now() the slot crosses into yesterday's bucket when
	// the test runs in the first hour after UTC midnight, flaking the today-bucket
	// assertion below. Matches the deterministic clock in the sibling trend tests.
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	// 94.7% cache_read share — mirrors the measured fleet profile in the design doc.
	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"bead-1": {
					PhaseID:             "bead-1",
					BeadID:              "bead-1",
					Status:              ledger.SlotMerged,
					CostUSD:             2.0,
					InputTokens:         5_000,   // fresh input
					OutputTokens:        10_000,  // output
					CacheReadTokens:     900_000, // high cache_read
					CacheCreationTokens: 40_000,  // cache_creation
					DispatchedAt:        now.Add(-time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	rows, ratio, tripwire, trend := buildTokenEconomy(runs, now)

	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	r := rows[0]
	if r.BeadID != "bead-1" {
		t.Errorf("BeadID = %q, want \"bead-1\"", r.BeadID)
	}
	wantTotal := int64(5_000 + 10_000 + 900_000 + 40_000)
	if r.TotalTokens != wantTotal {
		t.Errorf("TotalTokens = %d, want %d", r.TotalTokens, wantTotal)
	}
	// Cache-hit ratio: 900_000 / (5_000 + 900_000 + 40_000) ≈ 0.9526.
	wantRatio := float64(900_000) / float64(5_000+900_000+40_000)
	if absf(r.CacheHitRatio-wantRatio) > 0.001 {
		t.Errorf("CacheHitRatio = %.4f, want ≈%.4f", r.CacheHitRatio, wantRatio)
	}
	// Fleet ratio should match since there's only one slot.
	if absf(ratio-wantRatio) > 0.001 {
		t.Errorf("FleetCacheHitRatio = %.4f, want ≈%.4f", ratio, wantRatio)
	}
	// No tripwire — ratio is above the 0.80 threshold.
	if tripwire != "" {
		t.Errorf("tripwire = %q, want \"\" (healthy ratio)", tripwire)
	}
	// Trend: today's bucket should have a non-zero value.
	if trend[len(trend)-1] == 0 {
		t.Errorf("today's trend bucket is 0, want > 0")
	}
}

// TestBuildTokenEconomy_TripwireFires verifies that a low cache-hit ratio
// triggers the I7 tripwire.
func TestBuildTokenEconomy_TripwireFires(t *testing.T) {
	now := time.Now()
	// Very low cache-hit (only 10% cache_read) — well below 0.80 threshold.
	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"bead-x": {
					PhaseID:             "bead-x",
					BeadID:              "bead-x",
					Status:              ledger.SlotMerged,
					InputTokens:         90_000, // large fresh-input share
					OutputTokens:        10_000,
					CacheReadTokens:     10_000, // only 10% cache hit
					CacheCreationTokens: 1_000,
					DispatchedAt:        now.Add(-time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	_, _, tripwire, _ := buildTokenEconomy(runs, now)
	if tripwire != "warn" {
		t.Errorf("tripwire = %q, want \"warn\" for low cache-hit ratio", tripwire)
	}
}

// TestBuildTokenEconomy_MaxRowsCap verifies that the row list is capped at
// maxTokenRows entries even with more slots in the ledger.
func TestBuildTokenEconomy_MaxRowsCap(t *testing.T) {
	now := time.Now()
	slots := make(map[string]*ledger.Slot, maxTokenRows+5)
	for i := 0; i < maxTokenRows+5; i++ {
		id := "bead-" + string(rune('a'+i))
		slots[id] = &ledger.Slot{
			PhaseID:         id,
			BeadID:          id,
			Status:          ledger.SlotMerged,
			InputTokens:     100,
			CacheReadTokens: 900,
			DispatchedAt:    now.Add(-time.Hour).Format(time.RFC3339),
		}
	}
	runs := []*ledger.Run{{RunID: "run1", Slots: slots}}
	rows, _, _, _ := buildTokenEconomy(runs, now)
	if len(rows) > maxTokenRows {
		t.Errorf("rows capped at %d, got %d", maxTokenRows, len(rows))
	}
}

// TestBuildTokenEconomy_TrendBuckets verifies that slots dispatched on
// different days land in the correct trend buckets.
func TestBuildTokenEconomy_TrendBuckets(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	yesterday := now.AddDate(0, 0, -1)
	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"today": {
					PhaseID:         "today",
					BeadID:          "today",
					Status:          ledger.SlotMerged,
					InputTokens:     1_000,
					CacheReadTokens: 9_000,
					DispatchedAt:    now.Format(time.RFC3339),
				},
				"yesterday": {
					PhaseID:         "yesterday",
					BeadID:          "yesterday",
					Status:          ledger.SlotMerged,
					InputTokens:     2_000,
					CacheReadTokens: 18_000,
					DispatchedAt:    yesterday.Format(time.RFC3339),
				},
			},
		},
	}
	_, _, _, trend := buildTokenEconomy(runs, now)

	if len(trend) != SparklineLen {
		t.Fatalf("trend len = %d, want %d", len(trend), SparklineLen)
	}
	// Last bucket = today.
	if trend[SparklineLen-1] == 0 {
		t.Errorf("today's trend bucket = 0, want > 0")
	}
	// Second-to-last bucket = yesterday.
	if trend[SparklineLen-2] == 0 {
		t.Errorf("yesterday's trend bucket = 0, want > 0")
	}
	// All earlier buckets = 0.
	for i := 0; i < SparklineLen-2; i++ {
		if trend[i] != 0 {
			t.Errorf("trend[%d] = %g, want 0 (no data before yesterday)", i, trend[i])
		}
	}
}

// TestComputeEfficiency_TokenFieldsPopulated is an integration test that
// verifies computeEfficiency() populates token fields when ledger has them.
func TestComputeEfficiency_TokenFieldsPopulated(t *testing.T) {
	now := time.Date(2026, 7, 7, 12, 0, 0, 0, time.UTC)
	runs := []*ledger.Run{
		{
			RunID: "run1",
			Slots: map[string]*ledger.Slot{
				"bead-t": {
					PhaseID:             "bead-t",
					BeadID:              "bead-t",
					Status:              ledger.SlotMerged,
					InputTokens:         5_000,
					OutputTokens:        10_000,
					CacheReadTokens:     900_000,
					CacheCreationTokens: 40_000,
					DispatchedAt:        now.Add(-time.Hour).Format(time.RFC3339),
				},
			},
		},
	}
	snap := computeEfficiency(efficiencyInput{
		runs: runs,
		now:  now,
	})
	if len(snap.TokenRows) == 0 {
		t.Error("TokenRows is empty, want at least one row")
	}
	if snap.FleetCacheHitRatio == 0 {
		t.Error("FleetCacheHitRatio = 0, want > 0")
	}
	// The ratio is healthy (>0.80), so tripwire should be empty.
	if snap.CacheHitTripwire != "" {
		t.Errorf("CacheHitTripwire = %q, want \"\" (healthy)", snap.CacheHitTripwire)
	}
	if len(snap.TokensPerBeadTrend) != SparklineLen {
		t.Errorf("TokensPerBeadTrend len = %d, want %d", len(snap.TokensPerBeadTrend), SparklineLen)
	}
}

// absf is a simple |x| helper used in float comparisons.
func absf(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// TestSplitBucketProxySuffix is the koryph-3l1.3 carried-contract regression
// test: a bucket key segmented by a proxyID built from a base_url with its
// own colons (e.g. "http://127.0.0.1:8787") must still split into the
// correct (tier, size) — a naive first-colon split (the prior
// implementation) returned size "M@http://127.0.0.1:8787#v1" instead of "M".
func TestSplitBucketProxySuffix(t *testing.T) {
	cases := []struct {
		bucket             string
		wantTier, wantSize string
	}{
		{"sonnet:M", "sonnet", "M"},
		{"sonnet:M@headroom-ai", "sonnet", "M"},
		{"opus:L@http://127.0.0.1:8787#v1", "opus", "L"},
		{"nocolon", "nocolon", "M"}, // no-colon default preserved
	}
	for _, tc := range cases {
		tier, size := splitBucket(tc.bucket)
		if tier != tc.wantTier || size != tc.wantSize {
			t.Errorf("splitBucket(%q) = (%q,%q), want (%q,%q)", tc.bucket, tier, size, tc.wantTier, tc.wantSize)
		}
	}
}

// TestBuildEstimatorTableProxySuffixDoesNotInflateBase proves
// buildEstimatorTable computes the SAME base estimate for a proxy-segmented
// bucket as for its direct counterpart (same tier/size) — before the
// koryph-3l1.3 fix, the corrupted size string missed the SizeMultiplier
// lookup and silently fell back to 1.0, producing a wrong (inflated, for
// multiplier < 1, or deflated, for multiplier > 1) base estimate for every
// proxy-segmented row.
func TestBuildEstimatorTableProxySuffixDoesNotInflateBase(t *testing.T) {
	cfg := quota.DefaultConfig("acct")
	cfg.SizeMultiplier = map[string]float64{"S": 0.5, "M": 1.0, "L": 2.0}
	cfg.ErrorStats = map[string]*quota.ErrorStat{
		"sonnet:L":                       {N: 5, Bias: 1.0, MAPE: 10},
		"sonnet:L@http://127.0.0.1:8787": {N: 5, Bias: 1.0, MAPE: 10},
	}

	rows := buildEstimatorTable(cfg)
	byBucket := map[string]EstimatorRow{}
	for _, r := range rows {
		byBucket[r.Bucket] = r
	}

	direct, ok := byBucket["sonnet:L"]
	if !ok {
		t.Fatal("missing direct sonnet:L row")
	}
	proxied, ok := byBucket["sonnet:L@http://127.0.0.1:8787"]
	if !ok {
		t.Fatal("missing proxied sonnet:L@... row")
	}
	if direct.Base != proxied.Base {
		t.Errorf("direct.Base = %v, proxied.Base = %v, want equal (same tier/size, size multiplier must not be lost to the proxy suffix)",
			direct.Base, proxied.Base)
	}
}
