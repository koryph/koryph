// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysmem

import (
	"errors"
	"testing"
)

// TestDefaultFloorMB covers the auto memory-floor sizing (koryph-930): 1/8 of
// physical, clamped to [1 GB, 8 GB], and 0 when there is no reading.
func TestDefaultFloorMB(t *testing.T) {
	tests := []struct {
		totalMB uint64
		want    int
	}{
		{0, 0},         // no reading → disabled/fail-open
		{4096, 1024},   // 512 clamped up to the 1 GB minimum
		{16384, 2048},  // 1/8 of 16 GB
		{24576, 3072},  // 1/8 of 24 GB (the common dev host)
		{131072, 8192}, // 16 GB uncapped would be silly → clamped to the 8 GB max
	}
	for _, tc := range tests {
		if got := DefaultFloorMB(tc.totalMB); got != tc.want {
			t.Errorf("DefaultFloorMB(%d) = %d, want %d", tc.totalMB, got, tc.want)
		}
	}
}

// TestAvailableLive exercises the real platform probe (Linux /proc/meminfo,
// macOS sysctl+vm_stat) on the host running the test — CI covers Linux, dev
// covers macOS. It only asserts invariants that must hold on any healthy host,
// so it is not flaky against fluctuating real memory.
func TestAvailableLive(t *testing.T) {
	s, err := Available()
	if errors.Is(err, ErrUnsupported) {
		t.Skip("no memory probe on this platform")
	}
	if err != nil {
		t.Fatalf("Available: %v", err)
	}
	if s.TotalBytes == 0 {
		t.Error("TotalBytes = 0, want the host's physical memory")
	}
	if s.AvailableBytes == 0 {
		t.Error("AvailableBytes = 0, want some reclaimable memory on a live host")
	}
	if s.AvailableBytes > s.TotalBytes {
		t.Errorf("AvailableBytes %d exceeds TotalBytes %d", s.AvailableBytes, s.TotalBytes)
	}
	if s.AvailableMB() == 0 {
		t.Error("AvailableMB() = 0, want a nonzero MB reading")
	}
}
