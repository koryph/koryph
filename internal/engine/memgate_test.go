// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"errors"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/sysmem"
)

// memStatMB builds a sysmem.Stat from whole-megabyte totals for tests.
func memStatMB(totalMB, availMB uint64) sysmem.Stat {
	const mib = 1024 * 1024
	return sysmem.Stat{TotalBytes: totalMB * mib, AvailableBytes: availMB * mib}
}

func pressureStatMB(totalMB uint64, pressure sysmem.PressureBand, swapMB uint64) sysmem.Stat {
	stat := memStatMB(totalMB, totalMB/2)
	stat.Pressure = pressure
	stat.SwapUsedBytes = swapMB * 1024 * 1024
	return stat
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

func TestPressureAdmissionUsesOneResolvedSampleAndBoundsStaleProbe(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 25, 12, 0, 0, 0, time.UTC)
	r.pressureNow = func() time.Time { return now }

	memCalls := 0
	r.memProbe = func() (sysmem.Stat, bool) {
		memCalls++
		if memCalls == 1 {
			return pressureStatMB(16000, sysmem.PressureNormal, 10), true
		}
		return sysmem.Stat{}, false
	}
	resCalls := 0
	r.resProbe = func(context.Context) (*resmon.ProcTable, error) {
		resCalls++
		return &resmon.ProcTable{}, nil
	}

	if got := r.acquireGlobalSlot("fresh", nil, 0); got != admitGranted {
		t.Fatalf("fresh normal pressure = %v, want grant", got)
	}
	if r.pressureLastGood == nil || r.pressureLastGood.Pressure != sysmem.PressureNormal {
		t.Fatalf("last-good sample = %+v, want persisted normal sample", r.pressureLastGood)
	}

	// Beyond the bounded last-good TTL, the failed probe is degraded. The
	// already-active machine lease means exactly one active agent is allowed,
	// so a second bead must wait.
	now = now.Add(sysmem.DefaultLastGoodTTL + time.Second)
	if got := r.acquireGlobalSlot("stale", nil, 0); got != admitBreak {
		t.Fatalf("expired probe with one active lease = %v, want pressure break", got)
	}
	if memCalls != 2 || resCalls != 2 {
		t.Fatalf("admission probes = memory %d/process %d, want exactly 2/2", memCalls, resCalls)
	}
	if r.pressurePreviousFresh != nil {
		t.Fatal("degraded sample retained a trend baseline")
	}

	// Reconstructing a runner over the same run proves the checkpoint, not just
	// the in-memory fields, contains the last-good sample and pressure state.
	restarted := &runner{opts: r.opts, store: r.store, run: r.run}
	restarted.loadPressureState()
	if restarted.pressureLastGood == nil ||
		restarted.pressureLastGood.Pressure != sysmem.PressureNormal {
		t.Fatalf("reloaded last-good = %+v, want normal", restarted.pressureLastGood)
	}
}

func TestPressureTrendRequiresConsecutiveFreshSamples(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 7, 25, 13, 0, 0, 0, time.UTC)
	r.pressureNow = func() time.Time { return now }
	r.pressureSampleInterval = time.Nanosecond
	r.resProbe = func(context.Context) (*resmon.ProcTable, error) {
		return &resmon.ProcTable{}, nil
	}

	samples := []struct {
		stat sysmem.Stat
		ok   bool
	}{
		{pressureStatMB(16000, sysmem.PressureNormal, 10), true},
		{sysmem.Stat{}, false}, // breaks the consecutive-fresh trend chain
		{pressureStatMB(16000, sysmem.PressureNormal, 500), true},
		{pressureStatMB(16000, sysmem.PressureNormal, 600), true},
	}
	next := 0
	r.memProbe = func() (sysmem.Stat, bool) {
		sample := samples[next]
		next++
		return sample.stat, sample.ok
	}

	if got := r.acquireGlobalSlot("a", nil, 0); got != admitGranted {
		t.Fatalf("first fresh sample = %v, want grant", got)
	}
	now = now.Add(time.Second)
	if got := r.acquireGlobalSlot("b", nil, 0); got != admitGranted {
		t.Fatalf("bounded last-good sample = %v, want grant", got)
	}
	now = now.Add(time.Second)
	if got := r.acquireGlobalSlot("c", nil, 0); got != admitGranted {
		t.Fatalf("fresh recovery compared across blind interval = %v, want grant", got)
	}
	now = now.Add(time.Second)
	if got := r.acquireGlobalSlot("d", nil, 0); got != admitBreak {
		t.Fatalf("consecutive 100 MB swap rise = %v, want pressure break", got)
	}
}

func TestPersistentCriticalPressureGracefullyStopsOnlyNewestRecoverableCohort(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	selfIdentity, selfOK := table.ProcessIdentity(os.Getpid())
	parentIdentity, parentOK := table.ProcessIdentity(os.Getppid())
	if !selfOK || !parentOK {
		t.Skip("stable process identities unavailable")
	}
	r.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }

	base := time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC)
	r.pressureNow = func() time.Time { return base }
	r.gov.Now = func() time.Time { return base }
	r.pressureSampleInterval = time.Nanosecond
	band := sysmem.PressureCritical
	r.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, band, 10), true
	}
	r.run.Slots["older"] = &ledger.Slot{
		PhaseID: "older", BeadID: "older", Status: ledger.SlotRunning,
		PID: os.Getppid(), ProcessIdentity: parentIdentity, VerifiedIdentity: "agent@example.com",
		Worktree: f.repo, DispatchedAt: base.Add(-time.Minute).Format(time.RFC3339),
	}
	r.run.Slots["newest"] = &ledger.Slot{
		PhaseID: "newest", BeadID: "newest", Status: ledger.SlotRunning,
		PID: os.Getpid(), ProcessIdentity: selfIdentity, VerifiedIdentity: "agent@example.com",
		Worktree: f.repo, DispatchedAt: base.Format(time.RFC3339),
	}
	// This is the newest lease/slot but is not recoverable because its persisted
	// birth identity cannot be authenticated. Election must continue to the
	// next-newest recoverable cohort rather than giving up.
	r.run.Slots["unrecoverable-newest"] = &ledger.Slot{
		PhaseID: "unrecoverable-newest", BeadID: "unrecoverable-newest",
		Status: ledger.SlotRunning, PID: os.Getpid(), ProcessIdentity: "wrong-birth",
		VerifiedIdentity: "agent@example.com", Worktree: f.repo,
		DispatchedAt: base.Add(time.Minute).Format(time.RFC3339),
	}
	for _, sl := range r.run.Slots {
		if err := r.gov.Hold(govern.Lease{
			Project: r.opts.ProjectID, Bead: sl.BeadID, PID: sl.PID,
			EnginePID: os.Getpid(), Provider: fixtureAccount,
			AcquiredAt: sl.DispatchedAt,
		}); err != nil {
			t.Fatal(err)
		}
	}
	var stopped []int
	r.pressureStop = func(pid int) error {
		stopped = append(stopped, pid)
		return nil
	}
	var tripwires []SafetyTripwire
	r.opts.OnSafetyTripwire = func(event SafetyTripwire) {
		tripwires = append(tripwires, event)
	}

	if got := r.acquireGlobalSlot("candidate", nil, 0); got != admitBreak {
		t.Fatalf("first critical sample = %v, want pressure break", got)
	}
	if len(stopped) != 0 {
		t.Fatalf("non-persistent critical pressure stopped pids %v", stopped)
	}
	if len(tripwires) != 0 {
		t.Fatalf("non-persistent critical pressure emitted tripwires: %+v", tripwires)
	}
	base = base.Add(govern.DefaultCriticalFor + time.Second)
	if got := r.acquireGlobalSlot("candidate", nil, 0); got != admitBreak {
		t.Fatalf("persistent critical sample = %v, want pressure break", got)
	}
	if len(stopped) != 1 || stopped[0] != os.Getpid() {
		t.Fatalf("graceful stops = %v, want only newest pid %d", stopped, os.Getpid())
	}
	if len(tripwires) != 1 ||
		tripwires[0].Kind != SafetyTripwireHostMemoryPressure ||
		tripwires[0].Detail != "persistent critical host-memory pressure opened admission circuit" {
		t.Fatalf("persistent critical tripwires = %+v, want one host-memory event", tripwires)
	}
	base = base.Add(time.Second)
	if got := r.acquireGlobalSlot("candidate", nil, 0); got != admitBreak {
		t.Fatalf("open circuit = %v, want pressure break", got)
	}
	if len(stopped) != 1 {
		t.Fatalf("open circuit repeated graceful stop: %v", stopped)
	}
	if len(tripwires) != 1 {
		t.Fatalf("open circuit repeated safety tripwire: %+v", tripwires)
	}
	if !r.pressureState.CircuitOpen || !r.pressureReliefHandled ||
		r.pressureReliefPhaseID != "newest" {
		t.Fatalf("durable circuit state = %+v handled=%t phase=%q",
			r.pressureState, r.pressureReliefHandled, r.pressureReliefPhaseID)
	}

	// Three fresh normal samples satisfy recovery hysteresis. The circuit
	// closes only because the identity-checked graceful action above completed.
	band = sysmem.PressureNormal
	for i := 0; i < govern.DefaultRecoverySamples; i++ {
		base = base.Add(time.Second)
		r.acquireGlobalSlot("recovery-"+strconv.Itoa(i), nil, 0)
	}
	if r.pressureState.CircuitOpen {
		t.Fatalf("circuit remained open after handled relief and %d normal samples: %+v",
			govern.DefaultRecoverySamples, r.pressureState)
	}
	if len(stopped) != 1 {
		t.Fatalf("recovery repeated graceful stop: %v", stopped)
	}
}

func TestPressureReliefFailsClosedOnUnverifiedIdentity(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots["unsafe"] = &ledger.Slot{
		PhaseID: "unsafe", BeadID: "unsafe", Status: ledger.SlotRunning,
		PID: os.Getpid(), ProcessIdentity: "wrong", VerifiedIdentity: "agent@example.com",
		Worktree: f.repo, DispatchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	r.pressureStop = func(int) error {
		return errors.New("must not be called")
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	if ok, phase := r.coordinatePressureRelief(table, []govern.Lease{{
		Project: r.opts.ProjectID, Bead: "unsafe", PID: os.Getpid(),
		AcquiredAt: r.run.Slots["unsafe"].DispatchedAt,
	}}, true); ok || phase != "" {
		t.Fatalf("unverified cohort selected: phase=%q ok=%t", phase, ok)
	}
}

func TestNewestRecoverablePressureTargetScansMultipleLedgers(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	selfIdentity, selfOK := table.ProcessIdentity(os.Getpid())
	parentIdentity, parentOK := table.ProcessIdentity(os.Getppid())
	if !selfOK || !parentOK {
		t.Skip("stable process identities unavailable")
	}
	base := time.Date(2026, 7, 25, 15, 0, 0, 0, time.UTC)

	olderRun := *r.run
	olderRun.RunID = "20260725-145900"
	olderRun.Slots = map[string]*ledger.Slot{
		"older": {
			PhaseID: "older", BeadID: "older", Status: ledger.SlotRunning,
			PID: os.Getppid(), ProcessIdentity: parentIdentity,
			VerifiedIdentity: "agent@example.com", Worktree: f.repo,
			DispatchedAt: base.Format(time.RFC3339),
		},
	}
	r.store.RunDir(olderRun.RunID)
	if err := r.store.SaveRun(&olderRun); err != nil {
		t.Fatal(err)
	}

	// The newest lease belongs to the current ledger but its persisted birth
	// identity cannot be authenticated. Election must continue into a distinct
	// ledger and select the newest target that is actually recoverable.
	r.run.Slots = map[string]*ledger.Slot{
		"newer-unrecoverable": {
			PhaseID: "newer-unrecoverable", BeadID: "newer-unrecoverable",
			Status: ledger.SlotRunning, PID: os.Getpid(),
			ProcessIdentity:  selfIdentity + "-reused",
			VerifiedIdentity: "agent@example.com", Worktree: f.repo,
			DispatchedAt: base.Add(time.Minute).Format(time.RFC3339),
		},
	}
	target, ok := r.newestRecoverablePressureTarget(table, []govern.Lease{
		{
			Project: r.opts.ProjectID, Bead: "newer-unrecoverable",
			PID: os.Getpid(), AcquiredAt: base.Add(time.Minute).Format(time.RFC3339),
		},
		{
			Project: r.opts.ProjectID, Bead: "older",
			PID: os.Getppid(), AcquiredAt: base.Format(time.RFC3339),
		},
	})
	if !ok || target.RunID != olderRun.RunID || target.PhaseID != "older" ||
		target.ProcessIdentity != parentIdentity {
		t.Fatalf("multi-ledger target = %+v ok=%t, want older recoverable ledger", target, ok)
	}
}

func TestPausedPressureClaimBlocksFreshRunUntilOwnerAcknowledges(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r1 := runnerFromFixture(t, f)
	r1.gov = govern.NewStore()
	if err := r1.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	ownerIdentity, ok := table.ProcessIdentity(os.Getpid())
	if !ok {
		t.Skip("stable process identity unavailable")
	}
	now := time.Date(2026, 7, 25, 16, 0, 0, 0, time.UTC)
	claim, acquired, err := r1.gov.ClaimPressureRelief(govern.PressureReliefRequest{
		OwnerProject: r1.opts.ProjectID, OwnerRunID: r1.run.RunID,
		OwnerEnginePID: os.Getpid(), OwnerProcessIdentity: ownerIdentity,
		Target: govern.PressureReliefTarget{
			Project: r1.opts.ProjectID, RunID: r1.run.RunID,
			PhaseID: "paused", BeadID: "paused", PID: os.Getpid(),
			ProcessIdentity: ownerIdentity, DispatchedAt: now.Format(time.RFC3339),
		},
	})
	if err != nil || !acquired {
		t.Fatalf("paused claim = acquired %t err %v", acquired, err)
	}

	run2 := *r1.run
	run2.RunID += "-fresh"
	run2.Slots = map[string]*ledger.Slot{}
	r2 := &runner{
		opts:                   r1.opts,
		reg:                    r1.reg,
		rec:                    r1.rec,
		cfg:                    r1.cfg,
		store:                  r1.store,
		run:                    &run2,
		gov:                    govern.NewStore(),
		pressureSampleInterval: time.Nanosecond,
		pressureNow:            func() time.Time { return now },
	}
	r2.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureNormal, 0), true
	}
	r2.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }

	for i := 0; i < govern.DefaultRecoverySamples+1; i++ {
		now = now.Add(time.Second)
		if got := r2.acquireGlobalSlot("candidate", nil, 0); got != admitBreak {
			t.Fatalf("fresh run admission %d with paused claim = %v, want pressure break", i, got)
		}
	}
	if current, exists, err := r2.gov.PressureReliefStatus(); err != nil ||
		!exists || current.ID != claim.ID || current.AcknowledgedAt != "" {
		t.Fatalf("fresh run removed paused claim: claim=%+v exists=%t err=%v",
			current, exists, err)
	}

	if err := r1.gov.AcknowledgePressureReliefClaim(
		claim.ID, claim.OwnerProject, claim.OwnerRunID, claim.OwnerEnginePID,
		claim.OwnerProcessIdentity,
	); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if got := r2.acquireGlobalSlot("candidate", nil, 0); got != admitGranted {
		t.Fatalf("admission after owner acknowledgement = %v, want grant", got)
	}
	if current, exists, err := r2.gov.PressureReliefStatus(); err != nil || exists {
		t.Fatalf("acknowledged claim not CAS-recovered: claim=%+v exists=%t err=%v",
			current, exists, err)
	}
}

func TestTargetlessPressureEpisodeBlocksFreshRunUntilNormalRecovery(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r1 := runnerFromFixture(t, f)
	r1.gov = govern.NewStore()
	if err := r1.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	now := time.Date(2026, 7, 25, 17, 0, 0, 0, time.UTC)
	r1.pressureNow = func() time.Time { return now }
	r1.pressureSampleInterval = time.Nanosecond
	r1.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureCritical, 0), true
	}
	r1.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }

	if got := r1.acquireGlobalSlot("critical-1", nil, 0); got != admitBreak {
		t.Fatalf("first critical sample = %v, want pressure break", got)
	}
	now = now.Add(govern.DefaultCriticalFor + time.Second)
	if got := r1.acquireGlobalSlot("critical-2", nil, 0); got != admitBreak {
		t.Fatalf("persistent critical sample = %v, want pressure break", got)
	}
	episode, exists, err := r1.gov.PressureEpisodeStatus()
	if err != nil || !exists {
		t.Fatalf("targetless pressure episode = %+v exists=%t err=%v",
			episode, exists, err)
	}
	if claim, exists, err := r1.gov.PressureReliefStatus(); err != nil || exists {
		t.Fatalf("targetless pressure unexpectedly created relief claim: %+v exists=%t err=%v",
			claim, exists, err)
	}

	run2 := *r1.run
	run2.RunID += "-targetless-fresh"
	run2.Slots = map[string]*ledger.Slot{}
	r1.store.RunDir(run2.RunID)
	r2 := &runner{
		opts:                   r1.opts,
		reg:                    r1.reg,
		rec:                    r1.rec,
		cfg:                    r1.cfg,
		store:                  r1.store,
		run:                    &run2,
		gov:                    govern.NewStore(),
		pressureSampleInterval: time.Nanosecond,
		pressureNow:            func() time.Time { return now },
	}
	r2.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureNormal, 0), true
	}
	r2.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }

	for i := 0; i < govern.DefaultRecoverySamples; i++ {
		now = now.Add(time.Second)
		got := r2.acquireGlobalSlot("candidate", nil, 0)
		if i < govern.DefaultRecoverySamples-1 && got != admitBreak {
			t.Fatalf("fresh runner recovery sample %d = %v, want pressure break", i, got)
		}
		if i == govern.DefaultRecoverySamples-1 && got != admitGranted {
			t.Fatalf("fresh runner post-hysteresis admission = %v, want grant", got)
		}
	}
	if r2.pressureState.CircuitOpen {
		t.Fatalf("fresh runner retained recovered circuit: %+v", r2.pressureState)
	}
	if episode, exists, err := r2.gov.PressureEpisodeStatus(); err != nil || exists {
		t.Fatalf("normal recovery left targetless episode: %+v exists=%t err=%v",
			episode, exists, err)
	}
}

func TestInspectionFailureStillPublishesPressureEpisodeForFreshRun(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r1 := runnerFromFixture(t, f)
	r1.gov = govern.NewStore()
	if err := r1.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	selfIdentity, ok := table.ProcessIdentity(os.Getpid())
	if !ok {
		t.Skip("stable process identity unavailable")
	}
	now := time.Date(2026, 7, 25, 18, 0, 0, 0, time.UTC)
	r1.gov.Now = func() time.Time { return now }
	slot := &ledger.Slot{
		PhaseID: "active", BeadID: "active", Status: ledger.SlotRunning,
		PID: os.Getpid(), ProcessIdentity: selfIdentity,
		VerifiedIdentity: "agent@example.com", Worktree: f.repo,
		DispatchedAt: now.Format(time.RFC3339),
	}
	r1.run.Slots[slot.PhaseID] = slot
	if err := r1.gov.Hold(govern.Lease{
		Project: r1.opts.ProjectID, Bead: slot.BeadID, PID: slot.PID,
		EnginePID: os.Getpid(), Provider: fixtureAccount,
		AcquiredAt: slot.DispatchedAt,
	}); err != nil {
		t.Fatal(err)
	}
	r1.pressureNow = func() time.Time { return now }
	r1.pressureSampleInterval = time.Nanosecond
	r1.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureCritical, 0), true
	}
	inspectionFails := true
	r1.resProbe = func(context.Context) (*resmon.ProcTable, error) {
		if inspectionFails {
			return nil, errors.New("transient process inspection failure")
		}
		return table, nil
	}
	stops := 0
	r1.pressureStop = func(int) error {
		stops++
		return nil
	}

	r1.acquireGlobalSlot("critical-1", nil, 0)
	now = now.Add(govern.DefaultCriticalFor + time.Second)
	r1.acquireGlobalSlot("critical-2", nil, 0)
	if episode, exists, err := r1.gov.PressureEpisodeStatus(); err != nil || !exists {
		t.Fatalf("inspection failure did not publish episode: %+v exists=%t err=%v",
			episode, exists, err)
	}
	if claim, exists, err := r1.gov.PressureReliefStatus(); err != nil || exists {
		t.Fatalf("failed inspection created target claim: %+v exists=%t err=%v",
			claim, exists, err)
	}

	run2 := *r1.run
	run2.RunID += "-inspection-fresh"
	run2.Slots = map[string]*ledger.Slot{}
	r1.store.RunDir(run2.RunID)
	r2 := &runner{
		opts:                   r1.opts,
		reg:                    r1.reg,
		rec:                    r1.rec,
		cfg:                    r1.cfg,
		store:                  r1.store,
		run:                    &run2,
		gov:                    govern.NewStore(),
		pressureSampleInterval: time.Nanosecond,
		pressureNow:            func() time.Time { return now },
	}
	r2.gov.Now = func() time.Time { return now }
	r2.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureNormal, 0), true
	}
	r2.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }
	now = now.Add(time.Second)
	if got := r2.acquireGlobalSlot("fresh-candidate", nil, 0); got != admitBreak {
		t.Fatalf("fresh runner during inspection failure = %v, want pressure break", got)
	}
	if !r2.pressureState.CircuitOpen || r2.pressureState.NormalSamples != 1 {
		t.Fatalf("fresh runner did not inherit shared hysteresis: %+v", r2.pressureState)
	}

	// Once inspection recovers while critical pressure persists, the original
	// runner can attach and acknowledge the optional immutable relief target.
	inspectionFails = false
	now = now.Add(time.Second)
	r1.acquireGlobalSlot("critical-3", nil, 0)
	claim, exists, err := r1.gov.PressureReliefStatus()
	if err != nil || !exists || claim.AcknowledgedAt == "" {
		t.Fatalf("recovered inspection did not finish relief claim: %+v exists=%t err=%v",
			claim, exists, err)
	}
	if stops != 1 || claim.Target.PhaseID != slot.PhaseID {
		t.Fatalf("recovered inspection stops=%d claim=%+v, want one active target", stops, claim)
	}
}

func TestFreshNormalDoesNotRepublishExternallyClearedPersistedCriticalEpisode(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	selfIdentity, ok := table.ProcessIdentity(os.Getpid())
	if !ok {
		t.Skip("stable process identity unavailable")
	}
	now := time.Date(2026, 7, 25, 19, 0, 0, 0, time.UTC)
	r.gov.Now = func() time.Time { return now }
	slot := &ledger.Slot{
		PhaseID: "active", BeadID: "active", Status: ledger.SlotRunning,
		PID: os.Getpid(), ProcessIdentity: selfIdentity,
		VerifiedIdentity: "agent@example.com", Worktree: f.repo,
		DispatchedAt: now.Format(time.RFC3339),
	}
	r.run.Slots[slot.PhaseID] = slot
	if err := r.gov.Hold(govern.Lease{
		Project: r.opts.ProjectID, Bead: slot.BeadID, PID: slot.PID,
		EnginePID: os.Getpid(), Provider: fixtureAccount,
		AcquiredAt: slot.DispatchedAt,
	}); err != nil {
		t.Fatal(err)
	}

	// Persist exactly the stale local state a restarted runner inherits after
	// another runner has already observed normal recovery and cleared the
	// machine episode.
	r.pressureLoaded = true
	r.pressureState = govern.PressureState{
		Effective:       sysmem.PressureCritical,
		CriticalSince:   now.Add(-govern.DefaultCriticalFor).Format(time.RFC3339),
		CriticalSamples: 2,
		CircuitOpen:     true,
	}
	r.pressureReliefHandled = false
	r.savePressureState()
	episode, opened, err := r.gov.OpenPressureEpisode(govern.PressureEpisodeRequest{
		Project: r.opts.ProjectID, RunID: r.run.RunID, EnginePID: os.Getpid(),
	})
	if err != nil || !opened {
		t.Fatalf("open migration episode = opened %t err %v", opened, err)
	}
	otherRunnerStore := govern.NewStore()
	if recovered, err := otherRunnerStore.RecoverPressureEpisode(episode.ID, ""); err != nil || !recovered {
		t.Fatalf("other runner recovery = %t, %v", recovered, err)
	}

	// Simulate process restart over the persisted sidecar, then supply only
	// fresh-normal kernel observations. Hysteretic Effective remains critical
	// initially, but it is not authority to republish or signal.
	r.pressureLoaded = false
	r.pressureState = govern.PressureState{}
	r.pressureReliefHandled = false
	r.pressureReliefPhaseID = ""
	r.pressureSampleValid = false
	r.pressureSampleInterval = time.Nanosecond
	r.pressureNow = func() time.Time { return now }
	r.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(16000, sysmem.PressureNormal, 0), true
	}
	r.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }
	stops := 0
	r.pressureStop = func(int) error {
		stops++
		return nil
	}
	if handled, phaseID := r.coordinatePressureRelief(
		table,
		[]govern.Lease{{
			Project: r.opts.ProjectID, Bead: slot.BeadID, PID: slot.PID,
			AcquiredAt: slot.DispatchedAt,
		}},
		false,
	); handled || phaseID != "" {
		t.Fatalf("relief without fresh-critical proof = handled %t phase %q",
			handled, phaseID)
	}

	for i := 0; i < govern.DefaultRecoverySamples; i++ {
		now = now.Add(time.Second)
		r.acquireGlobalSlot("normal-"+strconv.Itoa(i), nil, 0)
		if episode, exists, err := r.gov.PressureEpisodeStatus(); err != nil || exists {
			t.Fatalf("fresh-normal sample %d republished episode: %+v exists=%t err=%v",
				i, episode, exists, err)
		}
		if claim, exists, err := r.gov.PressureReliefStatus(); err != nil || exists {
			t.Fatalf("fresh-normal sample %d created claim: %+v exists=%t err=%v",
				i, claim, exists, err)
		}
		if stops != 0 {
			t.Fatalf("fresh-normal sample %d sent %d stops, want zero", i, stops)
		}
	}
	if r.pressureState.CircuitOpen {
		t.Fatalf("fresh-normal hysteresis did not close stale local circuit: %+v",
			r.pressureState)
	}
}

func TestPressureAdmissionIncludesLiveCohortRSS(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.gov = govern.NewStore()
	if err := r.gov.SetCap(fixtureAccount, 8); err != nil {
		t.Fatal(err)
	}
	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	live := resmon.LiveCohortUsage(table, []int{os.Getpid()})
	if !live.RSSKnown || live.RSSMB <= 4 {
		t.Skipf("current cohort RSS is not usable for the test: %+v", live)
	}
	r.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }
	totalMB := uint64(live.RSSMB + 64)
	r.memProbe = func() (sysmem.Stat, bool) {
		return pressureStatMB(totalMB, sysmem.PressureNormal, 0), true
	}
	if err := r.gov.Hold(govern.Lease{
		Project: "other", Bead: "live", PID: os.Getpid(), EnginePID: os.Getpid(),
		Provider: fixtureAccount,
	}); err != nil {
		t.Fatal(err)
	}

	// The candidate's 65 MB declaration fits the host budget by itself. The
	// observed cohort leaves exactly 64 MB of headroom, so adding the candidate
	// tips the budget and proves engine admission handed RSS through to
	// AcquireEx.
	if got := r.acquireGlobalSlot("candidate", nil, 65); got != admitSkip {
		t.Fatalf("live-RSS-aware admission = %v, want reservation skip", got)
	}
}

func TestPressureReliefClaimPreventsConcurrentRunsStoppingMultipleCohorts(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r1 := runnerFromFixture(t, f)
	r1.gov = govern.NewStore()

	table, err := resmon.Snapshot(context.Background())
	if err != nil || table == nil {
		t.Skipf("resmon unavailable: %v", err)
	}
	identity, ok := table.ProcessIdentity(os.Getpid())
	if !ok {
		t.Skip("stable process identity unavailable")
	}
	dispatchedAt := time.Now().UTC().Format(time.RFC3339)
	slot := &ledger.Slot{
		PhaseID: "relief", BeadID: "relief", Status: ledger.SlotRunning,
		PID: os.Getpid(), ProcessIdentity: identity, VerifiedIdentity: "agent@example.com",
		Worktree: f.repo, DispatchedAt: dispatchedAt,
	}
	if err := r1.store.SetSlot(r1.run, slot); err != nil {
		t.Fatal(err)
	}
	lease := govern.Lease{
		Project: r1.opts.ProjectID, Bead: slot.BeadID, PID: slot.PID,
		EnginePID: os.Getpid(), Provider: fixtureAccount, AcquiredAt: dispatchedAt,
	}
	if err := r1.gov.Hold(lease); err != nil {
		t.Fatal(err)
	}

	var stops atomic.Int32
	r1.pressureStop = func(int) error {
		stops.Add(1)
		return nil
	}
	r2 := &runner{
		opts: r1.opts, reg: r1.reg, store: r1.store,
		run: &ledger.Run{
			RunID: r1.run.RunID + "-other",
			Slots: map[string]*ledger.Slot{},
		},
		gov: govern.NewStore(),
		pressureStop: func(int) error {
			stops.Add(1)
			return nil
		},
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, current := range []*runner{r1, r2} {
		current := current
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			current.coordinatePressureRelief(table, []govern.Lease{lease}, true)
		}()
	}
	close(start)
	wg.Wait()

	if got := stops.Load(); got != 1 {
		t.Fatalf("concurrent machine relief stops = %d, want exactly 1", got)
	}
	claim, exists, err := r1.gov.PressureReliefStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !exists || claim.AcknowledgedAt == "" || claim.Target.PhaseID != slot.PhaseID {
		t.Fatalf("global relief claim = %+v exists=%t, want acknowledged target %q",
			claim, exists, slot.PhaseID)
	}

	// The acknowledged tombstone remains authoritative: another run observing
	// the same persistent episode may acknowledge locally but cannot signal.
	if handled, phaseID := r2.coordinatePressureRelief(
		table, []govern.Lease{lease}, true,
	); !handled || phaseID != slot.PhaseID {
		t.Fatalf("second run observed claim = handled %t phase %q", handled, phaseID)
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("acknowledged episode triggered another stop: %d", got)
	}

	// A later run that never observed the circuit edge can still retire the
	// tombstone after the machine supplies the full fresh-normal hysteresis.
	r2.resProbe = func(context.Context) (*resmon.ProcTable, error) { return table, nil }
	for i := 0; i < govern.DefaultRecoverySamples; i++ {
		r2.pressureMemInput(
			pressureStatMB(16000, sysmem.PressureNormal, 0),
			true,
		)
	}
	if claim, exists, err := r2.gov.PressureReliefStatus(); err != nil || exists {
		t.Fatalf("fresh-normal recovery left claim: %+v exists=%t err=%v", claim, exists, err)
	}
}
