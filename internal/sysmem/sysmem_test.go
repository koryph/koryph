// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sysmem

import (
	"errors"
	"testing"
)

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
