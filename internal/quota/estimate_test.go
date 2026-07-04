// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"math"
	"testing"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestSizeOf(t *testing.T) {
	cases := []struct {
		n    int
		want string
	}{
		{0, "S"}, {399, "S"}, {400, "M"}, {1999, "M"}, {2000, "L"}, {10000, "L"},
	}
	for _, tc := range cases {
		if got := SizeOf(tc.n); got != tc.want {
			t.Fatalf("SizeOf(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestEstimateItem(t *testing.T) {
	cfg := DefaultConfig("acct") // sonnet=3, M=1.0, margin=1.5

	// Static: sonnet base * M mult * safety margin.
	if got := EstimateItem(cfg, "sonnet", "M"); !approx(got, 4.5) {
		t.Fatalf("static estimate = %g, want 4.5", got)
	}
	// opus L: 9 * 2 * 1.5 = 27.
	if got := EstimateItem(cfg, "opus", "L"); !approx(got, 27) {
		t.Fatalf("opus L estimate = %g, want 27", got)
	}
	// Unknown tier falls back to sonnet base.
	if got := EstimateItem(cfg, "mystery", "M"); !approx(got, 4.5) {
		t.Fatalf("unknown-tier estimate = %g, want 4.5 (sonnet base)", got)
	}

	// Calibration beats the static estimate.
	cfg.Calibration = map[string]float64{"sonnet:M": 2.0}
	if got := EstimateItem(cfg, "sonnet", "M"); !approx(got, 2.0) {
		t.Fatalf("calibrated estimate = %g, want 2.0 (calibration wins)", got)
	}
}

func TestEstimateWave(t *testing.T) {
	cfg := DefaultConfig("acct")
	items := []struct{ Tier, Size string }{
		{"sonnet", "M"}, // 4.5
		{"opus", "L"},   // 27.0
	}
	if got := EstimateWave(cfg, items); !approx(got, 31.5) {
		t.Fatalf("EstimateWave = %g, want 31.5", got)
	}
}

func TestRecordEWMA(t *testing.T) {
	cfg := DefaultConfig("acct")

	// First observation seeds the value (no estimate → error stats skipped).
	Record(cfg, "opus", "L", 10, 0)
	if got := cfg.Calibration["opus:L"]; !approx(got, 10) {
		t.Fatalf("seed = %g, want 10", got)
	}
	if cfg.ErrorStats != nil {
		t.Fatalf("ErrorStats should be nil when estimateUSD=0")
	}

	// Second folds in via EWMA: 0.7*10 + 0.3*20 = 13.
	Record(cfg, "opus", "L", 20, 0)
	if got := cfg.Calibration["opus:L"]; !approx(got, 13) {
		t.Fatalf("EWMA = %g, want 13", got)
	}
}

func TestRecordErrorStats(t *testing.T) {
	cfg := DefaultConfig("acct")

	// First observation with estimate: actual=10, estimate=8 → ratio=1.25, APE=25%.
	Record(cfg, "sonnet", "M", 10, 8)
	es := cfg.ErrorStats["sonnet:M"]
	if es == nil {
		t.Fatal("ErrorStats not created")
	}
	if es.N != 1 {
		t.Fatalf("N = %d, want 1", es.N)
	}
	if !approx(es.Bias, 1.25) {
		t.Fatalf("Bias = %g, want 1.25", es.Bias)
	}
	wantMAPE := math.Abs(10-8) / 8 * 100 // 25
	if !approx(es.MAPE, wantMAPE) {
		t.Fatalf("MAPE = %g, want %g", es.MAPE, wantMAPE)
	}

	// Second observation: actual=4, estimate=8 → ratio=0.5, APE=50%.
	// EWMA bias: 0.7*1.25 + 0.3*0.5 = 1.025. EWMA MAPE: 0.7*25 + 0.3*50 = 32.5.
	Record(cfg, "sonnet", "M", 4, 8)
	if es.N != 2 {
		t.Fatalf("N = %d, want 2", es.N)
	}
	wantBias := 0.7*1.25 + 0.3*0.5
	if !approx(es.Bias, wantBias) {
		t.Fatalf("Bias = %g, want %g", es.Bias, wantBias)
	}
	wantMAPE2 := 0.7*25 + 0.3*50
	if !approx(es.MAPE, wantMAPE2) {
		t.Fatalf("MAPE = %g, want %g", es.MAPE, wantMAPE2)
	}
}

func TestEstimateItemCorrected_BelowThreshold(t *testing.T) {
	cfg := DefaultConfig("acct")
	// Seed 4 observations (below BiasCorrectionThreshold=5).
	for i := 0; i < 4; i++ {
		Record(cfg, "sonnet", "M", 9.0, 4.5) // estimate=4.5, actual=9 → bias=2
	}
	corrected, base := EstimateItemCorrected(cfg, "sonnet", "M")
	// Below threshold: corrected == base.
	if !approx(corrected, base) {
		t.Fatalf("below threshold: corrected=%g != base=%g", corrected, base)
	}
}

func TestEstimateItemCorrected_AboveThreshold(t *testing.T) {
	cfg := DefaultConfig("acct")
	// Seed 5 observations (at BiasCorrectionThreshold); bias converges toward 2.
	for i := 0; i < BiasCorrectionThreshold; i++ {
		Record(cfg, "sonnet", "M", 9.0, 4.5) // actual=9, estimate=4.5 → ratio=2
	}
	corrected, base := EstimateItemCorrected(cfg, "sonnet", "M")
	// Above threshold: corrected = base * bias. Bias is approximately 2.
	bias := cfg.ErrorStats["sonnet:M"].Bias
	wantCorrected := base * bias
	if !approx(corrected, wantCorrected) {
		t.Fatalf("corrected=%g, want base(%g)*bias(%g)=%g", corrected, base, bias, wantCorrected)
	}
	if approx(corrected, base) {
		t.Fatalf("corrected should differ from base when bias correction is active")
	}
}

// TestEstimateItemForRuntimeClaudeParity proves EstimateItemForRuntime(cfg,
// "claude", tier, size) is byte-for-byte EstimateItem(cfg, tier, size) —
// including the unknown-tier fallback path — across every existing fixture
// (koryph-v8u.12's "claude parity" requirement).
func TestEstimateItemForRuntimeClaudeParity(t *testing.T) {
	cfg := DefaultConfig("acct")
	cases := []struct{ tier, size string }{
		{"haiku", "S"}, {"sonnet", "M"}, {"opus", "L"}, {"fable", "M"}, {"mystery", "M"},
	}
	for _, tc := range cases {
		want := EstimateItem(cfg, tc.tier, tc.size)
		got := EstimateItemForRuntime(cfg, "claude", tc.tier, tc.size)
		if !approx(got, want) {
			t.Errorf("EstimateItemForRuntime(claude, %s, %s) = %g, want %g (EstimateItem parity)",
				tc.tier, tc.size, got, want)
		}
	}

	// Calibration wins identically regardless of the runtime argument: it is
	// NOT runtime-namespaced (koryph-v8u.12 back-compat decision).
	cfg.Calibration = map[string]float64{"sonnet:M": 2.0}
	if got := EstimateItemForRuntime(cfg, "claude", "sonnet", "M"); !approx(got, 2.0) {
		t.Errorf("calibrated EstimateItemForRuntime(claude, ...) = %g, want 2.0", got)
	}
}

// TestEstimateItemForRuntimeStubTable proves the estimator base table is
// genuinely namespaced by runtime: a runtime whose config carries no
// PerTierUSD entry for a tier falls back to THAT RUNTIME's own default base,
// not claude's sonnet rate.
func TestEstimateItemForRuntimeStubTable(t *testing.T) {
	const name = "quota-estimate-test-stub-runtime"
	tierUSDTables[name] = tierUSDTable{
		perTier:  map[string]float64{"big": 100},
		fallback: 1,
	}
	t.Cleanup(func() { delete(tierUSDTables, name) })

	cfg := DefaultConfigForRuntime("acct", name)
	if got, want := cfg.PerTierUSD["big"], 100.0; got != want {
		t.Errorf("DefaultConfigForRuntime(%s).PerTierUSD[big] = %g, want %g", name, got, want)
	}

	// A tier the stub's own PerTierUSD carries resolves directly.
	if got, want := EstimateItemForRuntime(cfg, name, "big", "M"), 100*1.0*1.5; !approx(got, want) {
		t.Errorf("EstimateItemForRuntime(%s, big, M) = %g, want %g", name, got, want)
	}
	// An unrecognized tier falls back to the STUB's own fallback (1), not
	// claude's sonnet rate (3).
	if got, want := EstimateItemForRuntime(cfg, name, "unknown-tier", "M"), 1*1.0*1.5; !approx(got, want) {
		t.Errorf("EstimateItemForRuntime(%s, unknown-tier, M) = %g, want %g (stub's own fallback)", name, got, want)
	}

	// Claude's own estimate must be unaffected by the stub table's existence.
	claudeCfg := DefaultConfig("acct")
	if got, want := EstimateItemForRuntime(claudeCfg, "claude", "mystery", "M"), 4.5; !approx(got, want) {
		t.Errorf("claude estimate changed after registering a stub table: got %g, want %g", got, want)
	}
}

// TestEstimateItemForRuntimeUnknownRuntimeFallsBackToClaude proves an
// unregistered runtime name degrades gracefully to claude's table (a cost
// ESTIMATE is advisory governor input, never a dispatch gate — unlike
// modelroute.Resolve's deliberately fail-closed unknown-runtime error).
func TestEstimateItemForRuntimeUnknownRuntimeFallsBackToClaude(t *testing.T) {
	cfg := DefaultConfig("acct")
	got := EstimateItemForRuntime(cfg, "no-such-runtime", "mystery", "M")
	want := EstimateItemForRuntime(cfg, "claude", "mystery", "M")
	if !approx(got, want) {
		t.Errorf("EstimateItemForRuntime(no-such-runtime, ...) = %g, want claude fallback %g", got, want)
	}
}

// TestEstimateWaveForRuntimeClaudeParity proves EstimateWaveForRuntime(cfg,
// "claude", items) matches EstimateWave(cfg, items).
func TestEstimateWaveForRuntimeClaudeParity(t *testing.T) {
	cfg := DefaultConfig("acct")
	items := []struct{ Tier, Size string }{
		{"sonnet", "M"},
		{"opus", "L"},
	}
	want := EstimateWave(cfg, items)
	got := EstimateWaveForRuntime(cfg, "claude", items)
	if !approx(got, want) {
		t.Errorf("EstimateWaveForRuntime(claude, ...) = %g, want %g (EstimateWave parity)", got, want)
	}
}

// TestDefaultConfigForRuntimeClaudeParity proves DefaultConfigForRuntime(acct,
// "claude") reproduces DefaultConfig's exact PerTierUSD literal.
func TestDefaultConfigForRuntimeClaudeParity(t *testing.T) {
	want := DefaultConfig("acct")
	got := DefaultConfigForRuntime("acct", "claude")
	if len(got.PerTierUSD) != len(want.PerTierUSD) {
		t.Fatalf("PerTierUSD length = %d, want %d", len(got.PerTierUSD), len(want.PerTierUSD))
	}
	for k, v := range want.PerTierUSD {
		if got.PerTierUSD[k] != v {
			t.Errorf("PerTierUSD[%s] = %g, want %g", k, got.PerTierUSD[k], v)
		}
	}
	if got.SafetyMargin != want.SafetyMargin || got.PerAgentMaxUSD != want.PerAgentMaxUSD {
		t.Errorf("DefaultConfigForRuntime(claude) = %+v, want match with DefaultConfig %+v", got, want)
	}
}
