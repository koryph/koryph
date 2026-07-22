// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"fmt"
	"sync"
	"testing"
)

// poolLease (pools_test.go) builds a lease with an explicit provider pool.

// TestMachineCeilingCapsAcrossPools proves the machine-wide ceiling
// (koryph-4rk6.2) bounds TOTAL concurrent leases across every pool, even when
// each pool has ample room under its own cap. Three pools with generous
// per-pool caps (16/8/8 = 32 possible) must still never collectively exceed
// the machine ceiling of 3.
func TestMachineCeilingCapsAcrossPools(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("personal", 16); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCap("anthropic", 8); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCap("work", 8); err != nil {
		t.Fatal(err)
	}
	const ceiling = 3
	if err := s.SetMachineCeiling(ceiling); err != nil {
		t.Fatal(err)
	}

	pools := []string{"personal", "anthropic", "work"}
	granted := 0
	// Round-robin across pools far past the ceiling: pool caps never bind, so
	// only the machine ceiling can stop admission.
	for i := 0; i < 12; i++ {
		pool := pools[i%len(pools)]
		res, err := s.AcquireEx(poolLease(pool, "proj-"+pool, fmt.Sprintf("b%d", i), 2000+i), MemInput{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Granted {
			granted++
		} else if res.Outcome != AdmitDeniedCap {
			t.Errorf("i=%d pool=%s denied with outcome %v, want AdmitDeniedCap (machine ceiling)", i, pool, res.Outcome)
		}
	}
	if granted != ceiling {
		t.Errorf("granted %d leases across 3 pools, want %d (machine ceiling)", granted, ceiling)
	}

	all, err := s.leases()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != ceiling {
		t.Errorf("total live leases across all pools = %d, want %d (machine ceiling never exceeded)", len(all), ceiling)
	}
}

// TestMachineCeilingReleaseFreesMachineSlot proves a release in ANY pool frees
// a machine-ceiling slot usable by any other pool (koryph-4rk6.2).
func TestMachineCeilingReleaseFreesMachineSlot(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("personal", 16)
	_ = s.SetCap("anthropic", 16)
	if err := s.SetMachineCeiling(2); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Acquire(poolLease("personal", "p", "b1", 10)); !ok {
		t.Fatal("first acquire denied under ceiling 2")
	}
	if ok, _ := s.Acquire(poolLease("anthropic", "a", "b1", 11)); !ok {
		t.Fatal("second acquire denied under ceiling 2")
	}
	if ok, _ := s.Acquire(poolLease("personal", "p", "b2", 12)); ok {
		t.Fatal("third acquire granted over machine ceiling 2")
	}
	// Free the anthropic slot; a personal bead must now fit.
	if err := s.Release("anthropic", "a", "b1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Acquire(poolLease("personal", "p", "b2", 12)); !ok {
		t.Error("acquire denied after a cross-pool release freed a machine slot")
	}
}

// TestMachineCeilingDefaultWhenUnset proves an unconfigured ceiling resolves to
// DefaultMaxMachineAgents (koryph-4rk6.2) — the default always binds, so a
// machine that never set one is still bounded rather than admitting the
// unbounded sum of its pool caps.
func TestMachineCeilingDefaultWhenUnset(t *testing.T) {
	s := newTestStore(t)
	if got := s.MachineCeiling(); got != DefaultMaxMachineAgents {
		t.Errorf("MachineCeiling with nothing configured = %d, want default %d", got, DefaultMaxMachineAgents)
	}
	// Give one pool a cap far above the default ceiling; admission must still
	// stop at DefaultMaxMachineAgents.
	_ = s.SetCap("personal", DefaultMaxMachineAgents+8)
	granted := 0
	for i := 0; i < DefaultMaxMachineAgents+4; i++ {
		if ok, _ := s.Acquire(poolLease("personal", "solo", fmt.Sprintf("b%d", i), 3000+i)); ok {
			granted++
		}
	}
	if granted != DefaultMaxMachineAgents {
		t.Errorf("granted %d, want default machine ceiling %d", granted, DefaultMaxMachineAgents)
	}
}

// TestMachineCeilingConcurrentNeverExceeds hammers Acquire from many goroutines
// across three pools and asserts the machine ceiling is never breached under
// flock contention (koryph-4rk6.2) — the cross-pool analogue of the per-pool
// cap contention test.
func TestMachineCeilingConcurrentNeverExceeds(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("personal", 16)
	_ = s.SetCap("anthropic", 16)
	_ = s.SetCap("work", 16)
	const ceiling = 4
	if err := s.SetMachineCeiling(ceiling); err != nil {
		t.Fatal(err)
	}
	pools := []string{"personal", "anthropic", "work"}
	var wg sync.WaitGroup
	var mu sync.Mutex
	granted := 0
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pool := pools[i%len(pools)]
			ok, err := s.Acquire(poolLease(pool, "proj-"+pool, fmt.Sprintf("b%d", i), 4000+i))
			if err != nil {
				t.Error(err)
				return
			}
			if ok {
				mu.Lock()
				granted++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if granted != ceiling {
		t.Errorf("granted %d under contention, want exactly %d (machine ceiling)", granted, ceiling)
	}
	all, err := s.leases()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) > ceiling {
		t.Errorf("machine ceiling breached under contention: %d live leases > %d", len(all), ceiling)
	}
}
