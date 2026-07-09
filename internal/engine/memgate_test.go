// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/sysmem"
)

// memStatMB builds a sysmem.Stat from whole-megabyte totals for tests.
func memStatMB(totalMB, availMB uint64) sysmem.Stat {
	const mib = 1024 * 1024
	return sysmem.Stat{TotalBytes: totalMB * mib, AvailableBytes: availMB * mib}
}

// TestMemoryAdmits covers the koryph-930 memory admission gate. The gate is ON
// by default with a floor auto-sized to physical memory; an explicit setting
// overrides it, and a negative setting disables it. It fails open when no memory
// reading is available.
func TestMemoryAdmits(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f) // gov nil → configured floor is 0 (auto)

	tests := []struct {
		name    string
		env     string // KORYPH_MIN_FREE_MEMORY_MB; "" = unset
		total   uint64
		avail   uint64
		probeOK bool
		want    bool
	}{
		// Default (auto): floor = 1/8 of physical, so 16 GB → 2 GB floor.
		{"auto floor admits when ample", "", 16000, 8000, true, true},
		{"auto floor defers when low", "", 16000, 1000, true, false},
		// Small host: 4 GB → 500 MB, clamped up to the 1 GB minimum floor.
		{"auto floor clamps up on tiny host (admit)", "", 4000, 1500, true, true},
		{"auto floor clamps up on tiny host (defer)", "", 4000, 500, true, false},
		// Explicit override via env.
		{"explicit floor admits", "8000", 16000, 16000, true, true},
		{"explicit floor defers", "8000", 16000, 4000, true, false},
		// Negative disables the gate entirely.
		{"disabled admits even when starved", "-1", 16000, 1, true, true},
		// No usable reading → fail open.
		{"probe unavailable fails open", "", 0, 0, false, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", tc.env)
			r.memProbe = func() (sysmem.Stat, bool) {
				if !tc.probeOK {
					return sysmem.Stat{}, false
				}
				return memStatMB(tc.total, tc.avail), true
			}
			if got := r.memoryAdmits("tb1"); got != tc.want {
				t.Errorf("memoryAdmits(env=%q, total=%d, avail=%d, ok=%v) = %v, want %v",
					tc.env, tc.total, tc.avail, tc.probeOK, got, tc.want)
			}
		})
	}
}

// TestAcquireGlobalSlotMemoryGate proves the gate is wired ahead of the global
// governor: under memory pressure acquireGlobalSlot denies (deferring the wave)
// even though the governor itself is absent (nil → would otherwise always
// admit).
func TestAcquireGlobalSlotMemoryGate(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f) // gov nil: absent governor always admits
	t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", "8000")

	r.memProbe = func() (sysmem.Stat, bool) { return memStatMB(16000, 1000), true } // below floor
	// A candidate-agnostic floor breach is machine-wide → break (koryph-4ql.3).
	if got := r.acquireGlobalSlot("tb1", nil, 0); got != admitBreak {
		t.Errorf("acquireGlobalSlot under memory pressure = %v, want admitBreak (deferral)", got)
	}

	r.memProbe = func() (sysmem.Stat, bool) { return memStatMB(64000, 32000), true } // ample
	if got := r.acquireGlobalSlot("tb1", nil, 0); got != admitGranted {
		t.Errorf("acquireGlobalSlot with ample memory and no governor = %v, want admitGranted", got)
	}
}
