// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"math"
	"strings"
	"testing"
)

// win builds a measured Window.
func win(hours int, spent, ceiling float64, source string) Window {
	return Window{Hours: hours, SpentUSD: spent, CeilingUSD: ceiling, Source: source}
}

// usage assembles a Usage from a 5h and a weekly window.
func usage(w5, weekly Window) Usage {
	return Usage{Account: "acct", Window5h: w5, Weekly: weekly}
}

// calibrated is a config with real ceilings.
func calibratedCfg() *Config {
	c := DefaultConfig("acct")
	c.WindowCeilingUSD = 100
	c.WeeklyCeilingUSD = 1000
	return c
}

func TestConfigRoundtrip(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())

	// Missing file → uncalibrated default.
	got, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.WindowCeilingUSD != 0 || got.WeeklyCeilingUSD != 0 {
		t.Fatalf("fresh config should be uncalibrated, got %+v", got)
	}
	if got.PerAgentMaxUSD != 25 {
		t.Fatalf("default PerAgentMaxUSD = %g, want 25", got.PerAgentMaxUSD)
	}

	got.WindowCeilingUSD = 123.5
	got.Calibration = map[string]float64{"opus:L": 9.9}
	if err := SaveConfig(got); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got2, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig 2: %v", err)
	}
	if got2.WindowCeilingUSD != 123.5 {
		t.Fatalf("roundtrip ceiling = %g, want 123.5", got2.WindowCeilingUSD)
	}
	if got2.Calibration["opus:L"] != 9.9 {
		t.Fatalf("roundtrip calibration = %v", got2.Calibration)
	}
}

func TestState(t *testing.T) {
	cal := calibratedCfg()
	uncal := DefaultConfig("acct") // ceilings 0

	cases := []struct {
		name       string
		u          Usage
		cfg        *Config
		wantLevel  Level
		wantCalibd bool
	}{
		{
			name:       "uncalibrated is advisory ok",
			u:          usage(win(5, 0, 0, ""), win(168, 0, 0, "")),
			cfg:        uncal,
			wantLevel:  LevelOK,
			wantCalibd: false,
		},
		{
			name:       "healthy",
			u:          usage(win(5, 10, 100, "ccusage"), win(168, 50, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelOK,
			wantCalibd: true,
		},
		{
			name:       "warn at 0.80",
			u:          usage(win(5, 80, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelWarn,
			wantCalibd: true,
		},
		{
			name:       "drain at 0.90",
			u:          usage(win(5, 90, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelDrain,
			wantCalibd: true,
		},
		{
			name:       "stop at 0.95",
			u:          usage(win(5, 95, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelStop,
			wantCalibd: true,
		},
		{
			name:       "weekly dominates",
			u:          usage(win(5, 10, 100, "ccusage"), win(168, 920, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelDrain,
			wantCalibd: true,
		},
		{
			name:       "unavailable fails closed when calibrated",
			u:          usage(win(5, 0, 100, "unavailable"), win(168, 10, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelStop,
			wantCalibd: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, calibd := State(tc.u, tc.cfg)
			if lvl != tc.wantLevel || calibd != tc.wantCalibd {
				t.Fatalf("State = (%s, %v), want (%s, %v)", lvl, calibd, tc.wantLevel, tc.wantCalibd)
			}
		})
	}
}

func TestScaleSlots(t *testing.T) {
	const max = 9
	cases := []struct {
		frac float64
		want int
	}{
		{0.79, max}, // below warn → full
		{0.80, max}, // at warn → full
		{0.85, 5},   // midpoint of [warn,drain): 9 - 0.5*(9-1) = 5
		{0.895, 1},  // near drain → clamps to 1
		{0.90, 0},   // at drain → 0
		{0.95, 0},   // above drain → 0
	}
	for _, tc := range cases {
		// window carries the fraction; weekly is idle.
		u := usage(win(5, tc.frac*100, 100, "ccusage"), win(168, 0, 1000, "ccusage"))
		if got := ScaleSlots(u, max); got != tc.want {
			t.Fatalf("ScaleSlots(frac=%.3f) = %d, want %d", tc.frac, got, tc.want)
		}
	}
}

func TestPreflight(t *testing.T) {
	cal := calibratedCfg() // window ceiling 100
	base := usage(win(5, 50, 100, "ccusage"), win(168, 100, 1000, "ccusage"))

	if ok, reason := Preflight(base, 35, cal); !ok {
		t.Fatalf("wave to 85%% should pass, got not-ok: %s", reason)
	}
	ok, reason := Preflight(base, 45, cal) // (50+45)/100 = 0.95 >= drain
	if ok {
		t.Fatalf("wave crossing drain should not dispatch")
	}
	if !strings.Contains(reason, "drain") {
		t.Fatalf("reason should mention drain, got %q", reason)
	}

	// Uncalibrated → always ok, advisory.
	uncal := DefaultConfig("acct")
	uu := usage(win(5, 0, 0, ""), win(168, 0, 0, ""))
	ok, reason = Preflight(uu, 999, uncal)
	if !ok || !strings.Contains(reason, "uncalibrated") {
		t.Fatalf("uncalibrated Preflight = (%v, %q), want ok + uncalibrated", ok, reason)
	}

	// Unavailable window fails closed even when calibrated.
	un := usage(win(5, 0, 100, "unavailable"), win(168, 10, 1000, "ccusage"))
	if ok, _ := Preflight(un, 0, cal); ok {
		t.Fatalf("unavailable window should fail closed")
	}
}

func TestWindowFraction(t *testing.T) {
	if f := win(5, 10, 0, "ccusage").Fraction(); f != 1.0 {
		t.Fatalf("zero ceiling should be 1.0, got %g", f)
	}
	if f := win(5, 50, 100, "unavailable").Fraction(); f != 1.0 {
		t.Fatalf("unavailable should be 1.0, got %g", f)
	}
	if f := win(5, 25, 100, "ccusage").Fraction(); math.Abs(f-0.25) > 1e-9 {
		t.Fatalf("measured fraction = %g, want 0.25", f)
	}
}
