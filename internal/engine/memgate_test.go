// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import "testing"

// TestMemoryAdmits covers the koryph-930 memory admission gate: it defers a
// dispatch only when a floor is configured AND a usable reading shows available
// memory below it, and fails open in every other case (no floor, no probe).
func TestMemoryAdmits(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f) // gov is nil → floor comes only from the env override

	tests := []struct {
		name    string
		floor   string // KORYPH_MIN_FREE_MEMORY_MB; "" = unset (gate disabled)
		availMB uint64
		probeOK bool
		want    bool
	}{
		{"no floor configured admits", "", 100, true, true},
		{"ample memory admits", "8000", 16000, true, true},
		{"exactly at floor admits", "8000", 8000, true, true},
		{"below floor defers", "8000", 4000, true, false},
		{"probe unsupported fails open", "8000", 0, false, true},
		{"non-numeric floor ignored", "lots", 1, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("KORYPH_MIN_FREE_MEMORY_MB", tc.floor)
			r.memProbe = func() (uint64, bool) { return tc.availMB, tc.probeOK }
			if got := r.memoryAdmits("tb1"); got != tc.want {
				t.Errorf("memoryAdmits(floor=%q, avail=%d, ok=%v) = %v, want %v",
					tc.floor, tc.availMB, tc.probeOK, got, tc.want)
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

	r.memProbe = func() (uint64, bool) { return 1000, true } // below floor
	if r.acquireGlobalSlot("tb1") {
		t.Error("acquireGlobalSlot admitted under memory pressure; want deferral")
	}

	r.memProbe = func() (uint64, bool) { return 32000, true } // ample
	if !r.acquireGlobalSlot("tb1") {
		t.Error("acquireGlobalSlot deferred with ample memory and no governor; want admit")
	}
}
