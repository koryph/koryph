// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/sysmem"
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
	if err := s.SetCap("", capN); err != nil {
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
	_, leases, _, err := s.Snapshot("")
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != capN {
		t.Errorf("active leases = %d, want %d", len(leases), capN)
	}
}

func TestReleaseFreesSlot(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 1)
	if ok, _ := s.Acquire(lease("p", "b1", 10)); !ok {
		t.Fatal("first acquire denied")
	}
	if ok, _ := s.Acquire(lease("p", "b2", 11)); ok {
		t.Fatal("second acquire granted over cap 1")
	}
	if err := s.Release("", "p", "b1"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Acquire(lease("p", "b2", 11)); !ok {
		t.Error("acquire denied after release freed the slot")
	}
}

// --- fair share across projects ------------------------------------------

func TestFairShareReservesOtherProjectsSlots(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 4)
	// Both projects demand → fair share 2 each.
	if err := s.RefreshDemand("", "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshDemand("", "b", 2); err != nil {
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
	_ = s.SetCap("", 4)
	_ = s.RefreshDemand("", "a", 1)
	_ = s.RefreshDemand("", "b", 2)

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
	if err := s.DropDemand("", "b"); err != nil {
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
	_ = s.SetCap("", 2)
	// Reserve under the engine pid (agent pid unknown pre-launch).
	if ok, _ := s.Acquire(Lease{Project: "p", Bead: "b1", EnginePID: 1}); !ok {
		t.Fatal("reserve denied")
	}
	// The reservation counts toward the cap even though PID is still 0.
	_, leases, _, err := s.Snapshot("")
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
	_, leases, _, err = s.Snapshot("")
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
	if _, leases, _, _ = s.Snapshot(""); len(leases) != 2 {
		t.Errorf("Hold without reserve did not count: %d leases, want 2", len(leases))
	}
}

// --- pruning --------------------------------------------------------------

func TestPruneReclaimsDeadLease(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 1)
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
	if err := s.RefreshDemand("", "stale", 7); err != nil {
		t.Fatal(err)
	}
	// Advance well past DemandTTL; the heartbeat should be pruned.
	s.Now = func() time.Time { return base.Add(s.DemandTTL + time.Minute) }
	if err := s.Prune(); err != nil {
		t.Fatal(err)
	}
	_, _, dem, err := s.Snapshot("")
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
	if got := s.Cap(""); got != DefaultMaxGlobalAgents {
		t.Errorf("default cap = %d, want %d", got, DefaultMaxGlobalAgents)
	}
	if err := s.SetCap("", 7); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap(""); got != 7 {
		t.Errorf("cap after set = %d, want 7", got)
	}
	if err := s.SetCap("", 0); err == nil {
		t.Error("SetCap(0) should error")
	}
}

// --- koryph-1o2.3: per-account seeded-default cap precedence --------------
//
// Precedence: (1) an explicit `governor set` cap for the pool always wins;
// (2) else Store.SeedCap(pool) (the engine's quota.Config.MaxThreads, wired in
// without govern importing quota); (3) else the "anthropic" pool's own cap,
// for migration continuity; (4) else DefaultMaxGlobalAgents.

func TestCapSeedWinsOverPackageDefault(t *testing.T) {
	s := newTestStore(t)
	s.SeedCap = func(pool string) int {
		if pool == "work" {
			return 12
		}
		return 0
	}
	if got := s.Cap("work"); got != 12 {
		t.Errorf("Cap(work) = %d, want seed 12", got)
	}
	// A pool the seed doesn't cover still falls through to the package default.
	if got := s.Cap("personal"); got != DefaultMaxGlobalAgents {
		t.Errorf("Cap(personal) = %d, want package default %d", got, DefaultMaxGlobalAgents)
	}
}

func TestCapExplicitOperatorCapWinsOverSeed(t *testing.T) {
	s := newTestStore(t)
	s.SeedCap = func(string) int { return 12 }
	if err := s.SetCap("work", 3); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap("work"); got != 3 {
		t.Errorf("Cap(work) = %d, want explicit operator cap 3 (seed must not win)", got)
	}
}

func TestCapFallsBackToAnthropicPoolForMigrationContinuity(t *testing.T) {
	s := newTestStore(t)
	// No seed configured; the pre-per-account-pools operator cap on the
	// anthropic pool must still govern a newly-named account.
	if err := s.SetCap(DefaultPool, 9); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap("newly-onboarded"); got != 9 {
		t.Errorf("Cap(newly-onboarded) = %d, want anthropic-pool continuity cap 9", got)
	}
	// The anthropic pool itself must never recurse into its own continuity
	// hop — it falls straight through to the package default when unset.
	s2 := newTestStore(t)
	if got := s2.Cap(DefaultPool); got != DefaultMaxGlobalAgents {
		t.Errorf("Cap(anthropic) with nothing configured = %d, want package default %d", got, DefaultMaxGlobalAgents)
	}
}

func TestCapSeedWinsOverAnthropicContinuity(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap(DefaultPool, 9); err != nil {
		t.Fatal(err)
	}
	s.SeedCap = func(pool string) int {
		if pool == "work" {
			return 4
		}
		return 0
	}
	if got := s.Cap("work"); got != 4 {
		t.Errorf("Cap(work) = %d, want seed 4 (must win over anthropic continuity cap 9)", got)
	}
}

func TestEffectiveCapAppliesSeedForNonAdaptivePool(t *testing.T) {
	s := newTestStore(t)
	s.SeedCap = func(pool string) int {
		if pool == "work" {
			return 6
		}
		return 0
	}
	if got := s.EffectiveCap("work"); got != 6 {
		t.Errorf("EffectiveCap(work) = %d, want seed 6", got)
	}
}

func TestEffectiveCapSeedNeverAppliesToAdaptivePool(t *testing.T) {
	s := newTestStore(t)
	s.SeedCap = func(string) int { return 99 } // would win if wrongly consulted
	if err := s.SetAdaptiveCap("work", 3, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.EffectiveCap("work"); got != 3 {
		t.Errorf("EffectiveCap(work) = %d, want the adaptive seed (3), not the SeedCap hook (99)", got)
	}
}

func TestAcquireAdmitsAgainstSeededCap(t *testing.T) {
	s := newTestStore(t)
	s.SeedCap = func(pool string) int {
		if pool == "work" {
			return 2
		}
		return 0
	}
	for i := 0; i < 2; i++ {
		ok, err := s.Acquire(Lease{Project: "p", Bead: fmt.Sprintf("b%d", i), PID: 100 + i, EnginePID: 1, Provider: "work"})
		if err != nil || !ok {
			t.Fatalf("acquire %d: ok=%v err=%v (want granted, under seeded cap 2)", i, ok, err)
		}
	}
	ok, err := s.Acquire(Lease{Project: "p", Bead: "b-over", PID: 200, EnginePID: 1, Provider: "work"})
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("3rd acquire against a seeded cap of 2 should be denied")
	}
}

// --- concurrency (independent acquirers must never exceed the cap) --------

func TestConcurrentAcquireNeverExceedsCap(t *testing.T) {
	s := newTestStore(t)
	capN := DefaultMaxGlobalAgents
	_ = s.SetCap("", capN)

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
	_, leases, _, err := s.Snapshot("")
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != capN {
		t.Errorf("leases on disk = %d, want %d", len(leases), capN)
	}
}

func pressureMem(
	band sysmem.PressureBand,
	origin sysmem.SampleOrigin,
	totalMB, availMB uint64,
) MemInput {
	const mib = uint64(1024 * 1024)
	return MemInput{
		FloorMB: 2048,
		Host: &HostInput{
			Sample: sysmem.PressureSample{
				Stat: sysmem.Stat{
					TotalBytes: totalMB * mib, AvailableBytes: availMB * mib,
					Pressure: band, SampledAt: time.Unix(0, 0).UTC(),
				},
				Origin: origin,
			},
			EffectivePressure: band,
			LiveRSSKnown:      true,
		},
	}
}

func TestPressureAwareNormalAdmitsDespiteLowConservativePages(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 4)
	mem := pressureMem(sysmem.PressureNormal, sysmem.SampleFresh, 16*1024, 128)
	mem.Host.LiveRSSMB = 3 * 1024
	mem.Host.ObservedEstimateMB = 1024

	res, err := s.AcquireEx(lease("p", "normal", 101), mem)
	if err != nil || !res.Granted || res.Outcome != AdmitGranted {
		t.Fatalf("normal-pressure admission = %+v, err=%v; low conservative pages must not deny", res, err)
	}
}

func TestPressureAwareWarningAndCriticalDenyIndependently(t *testing.T) {
	for _, band := range []sysmem.PressureBand{sysmem.PressureWarning, sysmem.PressureCritical} {
		t.Run(band.String(), func(t *testing.T) {
			s := newTestStore(t)
			mem := pressureMem(band, sysmem.SampleFresh, 16*1024, 8*1024)
			res, err := s.AcquireEx(lease("p", band.String(), 102), mem)
			if err != nil {
				t.Fatal(err)
			}
			if res.Granted || res.Outcome != AdmitDeniedPressure || res.Pressure != band {
				t.Fatalf("%s admission = %+v, want typed pressure denial", band, res)
			}
		})
	}
}

func TestPressureAwareRisingSwapDeniesNormalBand(t *testing.T) {
	s := newTestStore(t)
	mem := pressureMem(sysmem.PressureNormal, sysmem.SampleFresh, 16*1024, 8*1024)
	mem.Host.Trend = sysmem.Trend{SwapBytes: 64 << 20}

	res, err := s.AcquireEx(lease("p", "swap-rise", 103), mem)
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.Outcome != AdmitDeniedPressure || res.Pressure != sysmem.PressureWarning {
		t.Fatalf("rising-swap admission = %+v, want pressure warning denial", res)
	}
}

func TestDegradedProbeAdmitsAtMostOneMachineAgent(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 4)
	mem := pressureMem(sysmem.PressureUnknown, sysmem.SampleDegraded, 0, 0)

	first, err := s.AcquireEx(lease("p", "first", 104), mem)
	if err != nil || !first.Granted {
		t.Fatalf("first degraded admission = %+v, err=%v; want one active agent", first, err)
	}
	second, err := s.AcquireEx(lease("p", "second", 105), mem)
	if err != nil {
		t.Fatal(err)
	}
	if second.Granted || second.Outcome != AdmitDeniedPressure ||
		second.ProbeOrigin != sysmem.SampleDegraded {
		t.Fatalf("second degraded admission = %+v, want degraded pressure denial", second)
	}
}

func TestUnavailableLiveRSSDegradesToOneMachineAgent(t *testing.T) {
	s := newTestStore(t)
	_ = s.SetCap("", 4)
	mem := pressureMem(sysmem.PressureNormal, sysmem.SampleFresh, 16*1024, 8*1024)
	if first, err := s.AcquireEx(lease("p", "first-rss", 107), mem); err != nil || !first.Granted {
		t.Fatalf("first admission = %+v, err=%v", first, err)
	}
	mem.Host.LiveRSSKnown = false
	second, err := s.AcquireEx(lease("p", "unknown-rss", 108), mem)
	if err != nil {
		t.Fatal(err)
	}
	if second.Granted || second.Outcome != AdmitDeniedPressure {
		t.Fatalf("unknown RSS with active cohort = %+v, want pressure denial", second)
	}
}

func TestPressureAwareReservationUsesLiveRSSAndObservedEstimate(t *testing.T) {
	s := newTestStore(t)
	mem := pressureMem(sysmem.PressureNormal, sysmem.SampleFresh, 8*1024, 4*1024)
	mem.Host.LiveRSSMB = 5 * 1024
	mem.Host.ObservedEstimateMB = 2 * 1024

	res, err := s.AcquireEx(Lease{
		Project: "p", Bead: "large", PID: 106, EnginePID: 1,
		MemReserveMB: 512,
	}, mem)
	if err != nil {
		t.Fatal(err)
	}
	if res.Granted || res.Outcome != AdmitDeniedReservation || !res.CandidateTipped {
		t.Fatalf("reservation admission = %+v, want candidate-tipped denial", res)
	}
	if res.LiveRSSMB != 5*1024 || res.CandidateMemoryMB != 2*1024 ||
		res.MemoryBudgetMB != 6*1024 {
		t.Errorf("reservation evidence = %+v", res)
	}
}

func TestPressureHysteresisOpensAndRequiresAcknowledgedRelief(t *testing.T) {
	policy := PressurePolicy{
		CriticalFor: 10 * time.Second, CriticalSamples: 2, RecoverySamples: 2,
	}
	t0 := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	critical := sysmem.PressureSample{
		Stat:   sysmem.Stat{Pressure: sysmem.PressureCritical, SampledAt: t0},
		Origin: sysmem.SampleFresh,
	}
	first := AdvancePressure(PressureState{}, critical, sysmem.Trend{}, t0, policy)
	if first.OpenCircuit || first.State.CircuitOpen || first.Effective != sysmem.PressureCritical {
		t.Fatalf("first critical transition = %+v", first)
	}
	second := AdvancePressure(first.State, critical, sysmem.Trend{}, t0.Add(10*time.Second), policy)
	if !second.OpenCircuit || !second.State.CircuitOpen {
		t.Fatalf("persistent critical transition = %+v, want circuit edge", second)
	}

	normal := sysmem.PressureSample{
		Stat:   sysmem.Stat{Pressure: sysmem.PressureNormal, SampledAt: t0.Add(20 * time.Second)},
		Origin: sysmem.SampleFresh,
	}
	recovering := AdvancePressure(second.State, normal, sysmem.Trend{}, t0.Add(20*time.Second), policy)
	if recovering.ReliefReady || !recovering.State.CircuitOpen ||
		recovering.Effective != sysmem.PressureCritical {
		t.Fatalf("first normal transition = %+v, want hysteresis hold", recovering)
	}
	ready := AdvancePressure(recovering.State, normal, sysmem.Trend{}, t0.Add(21*time.Second), policy)
	if !ready.ReliefReady || !ready.State.CircuitOpen || ready.Effective != sysmem.PressureNormal {
		t.Fatalf("second normal transition = %+v, want relief ready but circuit held", ready)
	}
	closed := AcknowledgePressureRelief(ready.State, policy)
	if closed.CircuitOpen {
		t.Fatalf("acknowledged relief kept circuit open: %+v", closed)
	}
}

func TestPressureHysteresisLastGoodDoesNotManufactureRecovery(t *testing.T) {
	state := PressureState{
		Effective: sysmem.PressureCritical, CircuitOpen: true, NormalSamples: 0,
	}
	lastGoodNormal := sysmem.PressureSample{
		Stat:   sysmem.Stat{Pressure: sysmem.PressureNormal, SampledAt: time.Now()},
		Origin: sysmem.SampleLastGood,
	}
	for i := 0; i < DefaultRecoverySamples+2; i++ {
		got := AdvancePressure(state, lastGoodNormal, sysmem.Trend{}, time.Now(), PressurePolicy{})
		if got.State != state || got.ReliefReady || got.Effective != sysmem.PressureCritical {
			t.Fatalf("last-good sample advanced recovery: %+v", got)
		}
		state = got.State
	}
}

func TestPressureReliefClaimAllowsOneTargetPerEpisode(t *testing.T) {
	s := newTestStore(t)
	requests := []PressureReliefRequest{
		{
			OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
			OwnerProcessIdentity: "engine-birth-1",
			Target: PressureReliefTarget{
				Project: "p1", RunID: "r1", PhaseID: "a", BeadID: "a",
				PID: 201, ProcessIdentity: "birth-a", DispatchedAt: "2026-07-25T12:00:00Z",
			},
		},
		{
			OwnerProject: "p2", OwnerRunID: "r2", OwnerEnginePID: 102,
			OwnerProcessIdentity: "engine-birth-2",
			Target: PressureReliefTarget{
				Project: "p2", RunID: "r2", PhaseID: "b", BeadID: "b",
				PID: 202, ProcessIdentity: "birth-b", DispatchedAt: "2026-07-25T12:01:00Z",
			},
		},
	}
	type result struct {
		claim    PressureReliefClaim
		acquired bool
		err      error
	}
	start := make(chan struct{})
	results := make(chan result, len(requests))
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			claim, acquired, err := s.ClaimPressureRelief(request)
			results <- result{claim: claim, acquired: acquired, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	var winner result
	acquired := 0
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.acquired {
			acquired++
			winner = got
		}
	}
	if acquired != 1 {
		t.Fatalf("concurrent acquired claims = %d, want 1", acquired)
	}
	if err := s.AcknowledgePressureReliefClaim(
		winner.claim.ID,
		winner.claim.OwnerProject,
		winner.claim.OwnerRunID,
		winner.claim.OwnerEnginePID,
		winner.claim.OwnerProcessIdentity,
	); err != nil {
		t.Fatal(err)
	}
	loser := requests[0]
	if loser.OwnerRunID == winner.claim.OwnerRunID {
		loser = requests[1]
	}
	claim, got, err := s.ClaimPressureRelief(loser)
	if err != nil {
		t.Fatal(err)
	}
	if got || claim.ID != winner.claim.ID || claim.AcknowledgedAt == "" {
		t.Fatalf("acknowledged episode allowed second target: claim=%+v acquired=%t", claim, got)
	}
	if recovered, err := s.RecoverPressureRelief(winner.claim.ID); err != nil || !recovered {
		t.Fatalf("recover acknowledged claim = %t, %v", recovered, err)
	}
	if _, got, err := s.ClaimPressureRelief(loser); err != nil || !got {
		t.Fatalf("fresh episode claim = acquired %t err %v, want true/nil", got, err)
	}
}

func TestPressureReliefClaimTakeoverPreservesOriginalTarget(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.Alive = func(pid int) bool { return pid != 101 }
	first := PressureReliefRequest{
		OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
		OwnerProcessIdentity: "engine-birth-1",
		Target: PressureReliefTarget{
			Project: "p1", RunID: "r1", PhaseID: "old", BeadID: "old",
			PID: 201, ProcessIdentity: "birth-old", DispatchedAt: "2026-07-25T11:59:00Z",
		},
	}
	original, acquired, err := s.ClaimPressureRelief(first)
	if err != nil || !acquired {
		t.Fatalf("first claim = acquired %t err %v", acquired, err)
	}
	second := PressureReliefRequest{
		OwnerProject: "p2", OwnerRunID: "r2", OwnerEnginePID: 102,
		OwnerProcessIdentity: "engine-birth-2",
		Target: PressureReliefTarget{
			Project: "p2", RunID: "r2", PhaseID: "new", BeadID: "new",
			PID: 202, ProcessIdentity: "birth-new", DispatchedAt: "2026-07-25T12:01:00Z",
		},
	}
	now = now.Add(DefaultPressureReliefClaimTTL - time.Second)
	if _, got, err := s.ClaimPressureRelief(second); err != nil || got {
		t.Fatalf("premature takeover = acquired %t err %v", got, err)
	}
	now = now.Add(2 * time.Second)
	taken, got, err := s.ClaimPressureRelief(second)
	if err != nil || !got {
		t.Fatalf("expired takeover = acquired %t err %v", got, err)
	}
	if taken.ID != original.ID || taken.Target != original.Target {
		t.Fatalf("takeover changed episode target: original=%+v taken=%+v", original, taken)
	}
}

func TestPressureReliefRecoveryIsAcknowledgedClaimCAS(t *testing.T) {
	s := newTestStore(t)
	req := PressureReliefRequest{
		OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
		OwnerProcessIdentity: "engine-birth-1",
		Target: PressureReliefTarget{
			Project: "p1", RunID: "r1", PhaseID: "phase", BeadID: "bead",
			PID: 201, ProcessIdentity: "agent-birth",
			DispatchedAt: "2026-07-25T12:00:00Z",
		},
	}
	claim, acquired, err := s.ClaimPressureRelief(req)
	if err != nil || !acquired {
		t.Fatalf("claim = acquired %t err %v", acquired, err)
	}

	if recovered, err := s.RecoverPressureRelief(claim.ID); err != nil || recovered {
		t.Fatalf("unacknowledged recovery = %t, %v; want false, nil", recovered, err)
	}
	if current, exists, err := s.PressureReliefStatus(); err != nil || !exists ||
		current.ID != claim.ID || current.AcknowledgedAt != "" {
		t.Fatalf("unacknowledged claim changed: claim=%+v exists=%t err=%v",
			current, exists, err)
	}
	if err := s.AcknowledgePressureReliefClaim(
		claim.ID, req.OwnerProject, req.OwnerRunID, req.OwnerEnginePID,
		req.OwnerProcessIdentity,
	); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverPressureRelief("newer-claim"); err != nil || recovered {
		t.Fatalf("mismatched recovery = %t, %v; want false, nil", recovered, err)
	}
	if recovered, err := s.RecoverPressureRelief(claim.ID); err != nil || !recovered {
		t.Fatalf("acknowledged matching recovery = %t, %v; want true, nil", recovered, err)
	}
}

func TestPressureReliefPIDReuseAllowsImmediateImmutableTakeover(t *testing.T) {
	s := newTestStore(t)
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	s.Alive = func(int) bool { return true }
	first := PressureReliefRequest{
		OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
		OwnerProcessIdentity: "old-engine-birth",
		Target: PressureReliefTarget{
			Project: "p1", RunID: "r1", PhaseID: "fixed", BeadID: "fixed",
			PID: 201, ProcessIdentity: "agent-birth",
			DispatchedAt: "2026-07-25T11:59:00Z",
		},
	}
	original, acquired, err := s.ClaimPressureRelief(first)
	if err != nil || !acquired {
		t.Fatalf("first claim = acquired %t err %v", acquired, err)
	}
	second := PressureReliefRequest{
		OwnerProject: "p2", OwnerRunID: "r2", OwnerEnginePID: 102,
		OwnerProcessIdentity:         "new-engine-birth",
		ObservedOwnerProcessIdentity: "reused-owner-pid-birth",
		Target: PressureReliefTarget{
			Project: "p2", RunID: "r2", PhaseID: "must-not-win", BeadID: "must-not-win",
			PID: 202, ProcessIdentity: "other-agent-birth",
			DispatchedAt: "2026-07-25T12:01:00Z",
		},
	}
	taken, acquired, err := s.ClaimPressureRelief(second)
	if err != nil || !acquired {
		t.Fatalf("PID-reuse takeover = acquired %t err %v", acquired, err)
	}
	if taken.ID != original.ID || taken.Target != original.Target {
		t.Fatalf("PID-reuse takeover changed immutable target: original=%+v taken=%+v",
			original, taken)
	}
	if taken.OwnerProcessIdentity != second.OwnerProcessIdentity {
		t.Fatalf("takeover owner identity = %q, want %q",
			taken.OwnerProcessIdentity, second.OwnerProcessIdentity)
	}
}

func TestLegacyPressureClaimWithoutOwnerIdentityDoesNotImplyPIDReuse(t *testing.T) {
	s := newTestStore(t)
	legacy := PressureReliefClaim{
		Schema: pressureReliefClaimSchema, ID: "legacy-claim",
		OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
		Target: PressureReliefTarget{
			Project: "p1", RunID: "r1", PhaseID: "fixed", BeadID: "fixed",
			PID: 201, ProcessIdentity: "agent-birth",
			DispatchedAt: "2026-07-25T11:59:00Z",
		},
		ClaimedAt: "2026-07-25T11:59:00Z",
	}
	if err := s.writePressureReliefClaim(legacy); err != nil {
		t.Fatal(err)
	}
	claim, acquired, err := s.ClaimPressureRelief(PressureReliefRequest{
		OwnerProject: "p2", OwnerRunID: "r2", OwnerEnginePID: 102,
		OwnerProcessIdentity:         "new-engine-birth",
		ObservedOwnerProcessIdentity: "observed-live-legacy-owner",
		Target: PressureReliefTarget{
			Project: "p2", RunID: "r2", PhaseID: "other", BeadID: "other",
			PID: 202, ProcessIdentity: "other-agent-birth",
			DispatchedAt: "2026-07-25T12:01:00Z",
		},
	})
	if err != nil || acquired {
		t.Fatalf("live legacy-owner takeover = acquired %t err %v, want false/nil",
			acquired, err)
	}
	if claim.ID != legacy.ID || claim.Target != legacy.Target ||
		claim.OwnerProcessIdentity != "" {
		t.Fatalf("legacy claim changed without dead-owner proof: %+v", claim)
	}
}

func TestSharedPressureClaimIsAdmissionAuthoritativeAndUnreadableFailsClosed(t *testing.T) {
	t.Run("fresh runner observes claim", func(t *testing.T) {
		s := newTestStore(t)
		req := PressureReliefRequest{
			OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
			OwnerProcessIdentity: "engine-birth",
			Target: PressureReliefTarget{
				Project: "p1", RunID: "r1", PhaseID: "phase", BeadID: "bead",
				PID: 201, ProcessIdentity: "agent-birth",
				DispatchedAt: "2026-07-25T12:00:00Z",
			},
		}
		if _, acquired, err := s.ClaimPressureRelief(req); err != nil || !acquired {
			t.Fatalf("claim = acquired %t err %v", acquired, err)
		}
		got, err := s.AcquireEx(lease("fresh", "candidate", 301), MemInput{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != AdmitDeniedPressure || !got.CircuitOpen ||
			got.Detail != "shared-pressure-episode" {
			t.Fatalf("shared claim admission = %+v, want pressure denial", got)
		}
	})

	t.Run("corrupt claim fails closed", func(t *testing.T) {
		s := newTestStore(t)
		path := s.pressureReliefClaimPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := s.AcquireEx(lease("fresh", "candidate", 301), MemInput{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome != AdmitDeniedPressure || !got.CircuitOpen ||
			got.Detail != "shared-pressure-state-unreadable" {
			t.Fatalf("unreadable shared claim admission = %+v, want pressure denial", got)
		}
	})
}

func TestPressureEpisodeIsIndependentAdmissionAuthority(t *testing.T) {
	s := newTestStore(t)
	req := PressureEpisodeRequest{Project: "p1", RunID: "r1", EnginePID: 101}
	episode, opened, err := s.OpenPressureEpisode(req)
	if err != nil || !opened {
		t.Fatalf("open episode = opened %t err %v", opened, err)
	}
	if repeated, opened, err := s.OpenPressureEpisode(PressureEpisodeRequest{
		Project: "p2", RunID: "r2", EnginePID: 102,
	}); err != nil || opened || repeated.ID != episode.ID {
		t.Fatalf("repeat open = %+v opened=%t err=%v", repeated, opened, err)
	}
	got, err := s.AcquireEx(lease("fresh", "candidate", 301), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != AdmitDeniedPressure || !got.CircuitOpen ||
		got.Detail != "shared-pressure-episode" {
		t.Fatalf("targetless episode admission = %+v, want pressure denial", got)
	}
	if recovered, err := s.RecoverPressureEpisode("different", ""); err != nil || recovered {
		t.Fatalf("mismatched episode recovery = %t, %v; want false, nil", recovered, err)
	}
	if recovered, err := s.RecoverPressureEpisode(episode.ID, ""); err != nil || !recovered {
		t.Fatalf("matching targetless episode recovery = %t, %v; want true, nil", recovered, err)
	}
}

func TestPressureEpisodeRecoveryRequiresExactAcknowledgedClaim(t *testing.T) {
	s := newTestStore(t)
	episode, opened, err := s.OpenPressureEpisode(PressureEpisodeRequest{
		Project: "p1", RunID: "r1", EnginePID: 101,
	})
	if err != nil || !opened {
		t.Fatalf("open episode = opened %t err %v", opened, err)
	}
	req := PressureReliefRequest{
		OwnerProject: "p1", OwnerRunID: "r1", OwnerEnginePID: 101,
		OwnerProcessIdentity: "engine-birth",
		Target: PressureReliefTarget{
			Project: "p1", RunID: "r1", PhaseID: "phase", BeadID: "bead",
			PID: 201, ProcessIdentity: "agent-birth",
			DispatchedAt: "2026-07-25T12:00:00Z",
		},
	}
	claim, acquired, err := s.ClaimPressureRelief(req)
	if err != nil || !acquired {
		t.Fatalf("claim = acquired %t err %v", acquired, err)
	}
	if recovered, err := s.RecoverPressureEpisode(episode.ID, claim.ID); err != nil || recovered {
		t.Fatalf("unacknowledged episode recovery = %t, %v; want false, nil", recovered, err)
	}
	if err := s.AcknowledgePressureReliefClaim(
		claim.ID, req.OwnerProject, req.OwnerRunID, req.OwnerEnginePID,
		req.OwnerProcessIdentity,
	); err != nil {
		t.Fatal(err)
	}
	if recovered, err := s.RecoverPressureEpisode(episode.ID, "different"); err != nil || recovered {
		t.Fatalf("wrong-claim episode recovery = %t, %v; want false, nil", recovered, err)
	}
	if recovered, err := s.RecoverPressureEpisode(episode.ID, claim.ID); err != nil || !recovered {
		t.Fatalf("acknowledged episode recovery = %t, %v; want true, nil", recovered, err)
	}
	if _, exists, err := s.PressureReliefStatus(); err != nil || exists {
		t.Fatalf("claim remains after atomic recovery: exists=%t err=%v", exists, err)
	}
}

func TestUnreadablePressureEpisodeFailsAdmissionClosed(t *testing.T) {
	s := newTestStore(t)
	path := s.pressureEpisodePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := s.AcquireEx(lease("fresh", "candidate", 301), MemInput{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != AdmitDeniedPressure || !got.CircuitOpen ||
		got.Detail != "shared-pressure-state-unreadable" {
		t.Fatalf("unreadable episode admission = %+v, want fail-closed pressure denial", got)
	}
}
