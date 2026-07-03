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

	// First observation seeds the value.
	Record(cfg, "opus", "L", 10)
	if got := cfg.Calibration["opus:L"]; !approx(got, 10) {
		t.Fatalf("seed = %g, want 10", got)
	}

	// Second folds in via EWMA: 0.7*10 + 0.3*20 = 13.
	Record(cfg, "opus", "L", 20)
	if got := cfg.Calibration["opus:L"]; !approx(got, 13) {
		t.Fatalf("EWMA = %g, want 13", got)
	}
}
