// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package sysmem

import "testing"

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
