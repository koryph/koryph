// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package sysmem

import (
	"errors"
	"testing"
	"time"
)

// TestAvailablePagesExcludesInactive is the koryph-3xs regression guard: the
// macOS availability estimate must sum ONLY the promptly reclaimable page
// classes (free + speculative + purgeable) and must NOT count "Pages inactive",
// which on macOS holds dirty/compressor-backed pages the kernel cannot hand out
// without writing them back first. Counting inactive over-reported headroom and
// admitted agents into a host that was already swapping.
func TestAvailablePagesExcludesInactive(t *testing.T) {
	pages := map[string]uint64{
		"Pages free":                   100,
		"Pages speculative":            20,
		"Pages purgeable":              5,
		"Pages inactive":               1000, // must be ignored
		"Pages active":                 9999, // never counted
		"Pages wired down":             9999, // never counted
		"Pages occupied by compressor": 9999, // never counted
	}
	if got := availablePages(pages); got != 125 {
		t.Errorf("availablePages = %d, want 125 (free+speculative+purgeable; inactive excluded)", got)
	}
}

// TestAvailablePagesMissingClassesAreZero proves a missing page class simply
// contributes zero (a conservative underestimate), never a panic.
func TestAvailablePagesMissingClassesAreZero(t *testing.T) {
	if got := availablePages(map[string]uint64{"Pages free": 42}); got != 42 {
		t.Errorf("availablePages with only free = %d, want 42", got)
	}
	if got := availablePages(map[string]uint64{}); got != 0 {
		t.Errorf("availablePages of empty = %d, want 0", got)
	}
}

// TestParseVMStatStripsTrailingPeriod covers the vm_stat line grammar the probe
// depends on: "Pages free:   6859." → 6859, with malformed lines skipped.
func TestParseVMStatStripsTrailingPeriod(t *testing.T) {
	out := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n" +
		"Pages free:                  6859.\n" +
		"Pages inactive:            123456.\n" +
		"Pages speculative:            200.\n" +
		"garbage line with no colon value\n" +
		"Pages purgeable:               10.\n"
	pages := parseVMStat(out)
	if pages["Pages free"] != 6859 || pages["Pages speculative"] != 200 || pages["Pages purgeable"] != 10 {
		t.Fatalf("parseVMStat = %v, want free=6859 speculative=200 purgeable=10", pages)
	}
	if got := availablePages(pages); got != 6859+200+10 {
		t.Errorf("availablePages = %d, want %d (inactive 123456 excluded)", got, 6859+200+10)
	}
}

func TestParseDarwinPressureBands(t *testing.T) {
	for _, tc := range []struct {
		raw  uint32
		want PressureBand
	}{
		{1, PressureNormal},
		{2, PressureWarning},
		{4, PressureCritical},
	} {
		got, err := parseDarwinPressure(tc.raw)
		if err != nil || got != tc.want {
			t.Errorf("parseDarwinPressure(%d) = (%s, %v), want (%s, nil)", tc.raw, got, err, tc.want)
		}
	}
	if _, err := parseDarwinPressure(0); err == nil {
		t.Fatal("unknown Darwin pressure value must fail closed")
	}
}

func TestParseSwapUsed(t *testing.T) {
	got, err := parseSwapUsed("total = 4.00G  used = 512.50M  free = 3.50G  (encrypted)")
	if err != nil {
		t.Fatal(err)
	}
	want := uint64(512.5 * 1024 * 1024)
	if got != want {
		t.Errorf("swap used = %d, want %d", got, want)
	}
	if _, err := parseSwapUsed("total = 0.00M free = 0.00M"); err == nil {
		t.Fatal("missing swap used field must be a probe error")
	}
}

func TestResolvePressureSampleFreshLastGoodThenDegraded(t *testing.T) {
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	fresh := Stat{
		Pressure:        PressureNormal,
		TotalBytes:      32 << 30,
		AvailableBytes:  128 << 20, // low conservative pages are still a valid sample
		SwapUsedBytes:   256 << 20,
		CompressedBytes: 512 << 20,
	}
	resolved, lastGood := ResolvePressureSample(now, fresh, nil, nil, 10*time.Second)
	if resolved.Origin != SampleFresh || lastGood == nil || lastGood.SampledAt != now {
		t.Fatalf("fresh resolution = %+v lastGood=%+v", resolved, lastGood)
	}

	resolved, retained := ResolvePressureSample(
		now.Add(9*time.Second), Stat{}, errors.New("temporary sysctl failure"), lastGood, 10*time.Second,
	)
	if resolved.Origin != SampleLastGood || resolved.Pressure != PressureNormal ||
		resolved.ProbeError == "" || retained == nil {
		t.Fatalf("last-good resolution = %+v retained=%+v", resolved, retained)
	}

	resolved, retained = ResolvePressureSample(
		now.Add(11*time.Second), Stat{}, errors.New("persistent sysctl failure"), lastGood, 10*time.Second,
	)
	if resolved.Origin != SampleDegraded || resolved.Pressure != PressureUnknown ||
		resolved.TotalBytes != 0 || retained == nil {
		t.Fatalf("degraded resolution = %+v retained=%+v", resolved, retained)
	}
}

func TestPressureTrendUsesSwapAndCompressorCounters(t *testing.T) {
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	older := Stat{
		SampledAt:       t0,
		SwapUsedBytes:   100 << 20,
		CompressedBytes: 500 << 20,
	}
	current := Stat{
		SampledAt:       t0.Add(time.Second),
		SwapUsedBytes:   164 << 20,
		CompressedBytes: 468 << 20,
	}
	got := current.TrendFrom(older)
	if got.SwapBytes != 64<<20 || got.CompressedBytes != -(32<<20) {
		t.Errorf("trend = %+v, want swap +64 MiB, compressor -32 MiB", got)
	}
}
