// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestStore returns a Store over a temp KORYPH_HOME with a fixed clock and
// an all-alive pid probe (so nothing is pruned unless a test says so).
func newTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
	s := NewStore()
	s.Now = func() time.Time { return time.Unix(0, 0).UTC() } // epoch 0
	s.Alive = func(int) bool { return true }
	return s
}

func lease(project, bead string, pid int) Lease {
	return Lease{Project: project, Bead: bead, PID: pid, EnginePID: 1}
}

// --- fair-share pure function --------------------------------------------

func TestFairShareSumsToCap(t *testing.T) {
	for cap := 1; cap <= 6; cap++ {
		for n := 1; n <= 6; n++ {
			demanders := make([]string, n)
			for i := range demanders {
				demanders[i] = fmt.Sprintf("p%02d", i)
			}
			for epoch := 0; epoch < 12; epoch++ {
				sum := 0
				for _, p := range demanders {
					sum += fairShare(cap, demanders, p, epoch)
				}
				if sum != cap {
					t.Fatalf("cap=%d n=%d epoch=%d: shares sum to %d, want %d", cap, n, epoch, sum, cap)
				}
			}
		}
	}
}

func TestFairShareRotationNoStarvation(t *testing.T) {
	// cap 1 over 3 demanders: exactly one gets the slot each epoch, and over 3
	// consecutive epochs every demander gets a turn.
	demanders := []string{"a", "b", "c"}
	got := map[string]int{}
	for epoch := 0; epoch < 3; epoch++ {
		winners := 0
		for _, p := range demanders {
			fs := fairShare(1, demanders, p, epoch)
			if fs == 1 {
				winners++
				got[p]++
			}
		}
		if winners != 1 {
			t.Errorf("epoch %d: %d winners, want exactly 1", epoch, winners)
		}
	}
	for _, p := range demanders {
		if got[p] != 1 {
			t.Errorf("demander %q got %d turns over 3 epochs, want 1 (no starvation)", p, got[p])
		}
	}
}

// --- cap enforcement ------------------------------------------------------

func TestAcquireEnforcesGlobalCap(t *testing.T) {
	s := newTestStore(t)
	capN := DefaultMaxGlobalAgents
	if err := s.SetCap(capN); err != nil {
		t.Fatal(err)
	}
	granted := 0
	for i := 0; i < capN+4; i++ { // demand exceeds the cap
		ok, err := s.Acquire(lease("solo", fmt.Sprintf("b%d", i), 1000+i))
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			granted++
		}
	}
	if granted != capN {
		t.Errorf("granted %d, want %d (cap)", granted, capN)
	}
	_, leases, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != capN {
		t.Errorf("active leases = %d, want %d", len(leases), capN)
	}
}

func TestReleaseFreesSlot(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap(1)
	if ok, _ := s.Acquire(lease("p", "b1", 10)); !ok {
		t.Fatal("first acquire denied")
	}
	if ok, _ := s.Acquire(lease("p", "b2", 11)); ok {
		t.Fatal("second acquire granted over cap 1")
	}
	if err := s.Release("p", "b1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Acquire(lease("p", "b2", 11)); !ok {
		t.Error("acquire denied after release freed the slot")
	}
}

// --- fair share across projects ------------------------------------------

func TestFairShareReservesOtherProjectsSlots(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap(4)
	// Both projects demand → fair share 2 each.
	if err := s.RefreshDemand("a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshDemand("b", 2); err != nil {
		t.Fatal(err)
	}
	// A takes its 2, then is denied its 3rd because B's share is reserved.
	for i := 0; i < 2; i++ {
		if ok, _ := s.Acquire(lease("a", fmt.Sprintf("a%d", i), 100+i)); !ok {
			t.Fatalf("A acquire %d denied, want granted (within fair share)", i)
		}
	}
	if ok, _ := s.Acquire(lease("a", "a2", 200)); ok {
		t.Error("A granted a 3rd slot while B (demanding) holds none — fair share breached")
	}
	// B can still claim its reserved share.
	if ok, _ := s.Acquire(lease("b", "b0", 300)); !ok {
		t.Error("B denied its reserved fair share")
	}
}

func TestDroppingDemandRaisesRemainingShare(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap(4)
	_ = s.RefreshDemand("a", 1)
	_ = s.RefreshDemand("b", 2)

	// A is capped at its fair share (2) while B still demands.
	for i := 0; i < 2; i++ {
		if ok, _ := s.Acquire(lease("a", fmt.Sprintf("a%d", i), 100+i)); !ok {
			t.Fatalf("A acquire %d denied within share", i)
		}
	}
	if ok, _ := s.Acquire(lease("a", "a2", 200)); ok {
		t.Fatal("A exceeded its fair share while B demanded")
	}

	// B finishes its frontier and drops demand → A alone now, share = cap.
	if err := s.DropDemand("b"); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < 4; i++ {
		if ok, _ := s.Acquire(lease("a", fmt.Sprintf("a%d", i), 100+i)); !ok {
			t.Errorf("A acquire %d denied after B dropped demand (idle capacity wasted)", i)
		}
	}
	// Cap still binds.
	if ok, _ := s.Acquire(lease("a", "a9", 999)); ok {
		t.Error("acquire granted beyond the cap")
	}
}

// --- two-phase reserve → hold --------------------------------------------

func TestReserveThenHold(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap(2)
	// Reserve under the engine pid (agent pid unknown pre-launch).
	if ok, _ := s.Acquire(Lease{Project: "p", Bead: "b1", EnginePID: 1}); !ok {
		t.Fatal("reserve denied")
	}
	// The reservation counts toward the cap even though PID is still 0.
	_, leases, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].PID != 0 {
		t.Fatalf("reservation = %+v, want one lease with PID 0", leases)
	}
	// Hold attaches the launched agent pid; the lease is updated, not duplicated.
	if err := s.Hold(Lease{Project: "p", Bead: "b1", PID: 4242, EnginePID: 1, Model: "sonnet"}); err != nil {
		t.Fatal(err)
	}
	_, leases, _, err = s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].PID != 4242 || leases[0].Model != "sonnet" {
		t.Errorf("after Hold = %+v, want single lease pid 4242 model sonnet", leases)
	}
	// Hold with no prior reservation still counts (requeue/resume after prune).
	if err := s.Hold(Lease{Project: "p", Bead: "b2", PID: 99, EnginePID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, leases, _, _ = s.Snapshot(); len(leases) != 2 {
		t.Errorf("Hold without reserve did not count: %d leases, want 2", len(leases))
	}
}

// --- pruning --------------------------------------------------------------

func TestPruneReclaimsDeadLease(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap(1)
	if ok, _ := s.Acquire(lease("p", "b1", 10)); !ok {
		t.Fatal("acquire denied")
	}
	// The holder dies; its slot must be reclaimable.
	s.Alive = func(pid int) bool { return pid != 10 }
	if ok, _ := s.Acquire(lease("p", "b2", 11)); !ok {
		t.Error("dead-pid lease not reclaimed on acquire")
	}
}

func TestPruneReclaimsStaleDemand(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(0, 0).UTC()
	s.Now = func() time.Time { return base }
	if err := s.RefreshDemand("stale", 7); err != nil {
		t.Fatal(err)
	}
	// Advance well past DemandTTL; the heartbeat should be pruned.
	s.Now = func() time.Time { return base.Add(s.DemandTTL + time.Minute) }
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	_, _, dem, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(dem) != 0 {
		t.Errorf("stale demand survived prune: %+v", dem)
	}
}

// --- cap config -----------------------------------------------------------

func TestCapDefaultAndSet(t *testing.T) {
	s := newTestStore(t)
	if got := s.Cap(); got != DefaultMaxGlobalAgents {
		t.Errorf("default cap = %d, want %d", got, DefaultMaxGlobalAgents)
	}
	if err := s.SetCap(7); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap(); got != 7 {
		t.Errorf("cap after set = %d, want 7", got)
	}
	if err := s.SetCap(0); err == nil {
		t.Error("SetCap(0) should error")
	}
}

// --- concurrency (independent acquirers must never exceed the cap) --------

func TestConcurrentAcquireNeverExceedsCap(t *testing.T) {
	s := newTestStore(t)
	capN := DefaultMaxGlobalAgents
	_ = s.SetCap(capN)

	const workers = 32
	var granted int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.Acquire(lease("solo", fmt.Sprintf("b%d", i), 5000+i))
			if err == nil && ok {
				atomic.AddInt64(&granted, 1)
			}
		}(i)
	}
	wg.Wait()

	if int(granted) != capN {
		t.Errorf("granted %d under contention, want exactly %d (cap)", granted, capN)
	}
	_, leases, _, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != capN {
		t.Errorf("leases on disk = %d, want %d", len(leases), capN)
	}
}
