// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/fsx"
)

// koryph-2im.11 (L5b): settle windows, the circuit breaker, and dispatch
// smoothing — all Adaptive-gated hardening on top of the koryph-2im.4 AIMD
// overlay tested in aimd_test.go.

// --- settle window -----------------------------------------------------------

// TestSettleFreezesAdditiveIncreaseDuringWindow proves the additive probe is
// frozen for the FULL settle window even when a stale LastProbeAt would
// otherwise suggest a probeInterval has elapsed — anchoring on SettleUntil
// (not the change itself) is what makes settle actually freeze growth.
func TestSettleFreezesAdditiveIncreaseDuringWindow(t *testing.T) {
	c := &Config{
		Adaptive:    true,
		DynamicCap:  4,
		HardMax:     16,
		LastProbeAt: epoch0.Add(-time.Hour).Format(time.RFC3339), // "long ago"
		SettleUntil: epoch0.Add(2 * time.Minute).Format(time.RFC3339),
	}
	if changed := applyProbe(c, epoch0.Add(time.Minute)); changed {
		t.Error("applyProbe should be frozen while inSettle")
	}
	if c.DynamicCap != 4 {
		t.Errorf("DynamicCap = %d, want unchanged 4", c.DynamicCap)
	}
	// Past settle expiry, growth resumes, measured from settle expiry.
	if !applyProbe(c, epoch0.Add(2*time.Minute+5*time.Minute)) {
		t.Error("applyProbe should grow once settle has expired and a full interval passed")
	}
	if c.DynamicCap != 5 {
		t.Errorf("DynamicCap = %d, want 5", c.DynamicCap)
	}
}

// TestProbeStepFreezesSettleToo proves an additive-increase step is itself a
// DynamicCap change and therefore starts its own fresh settle window (rule 1
// in applyRateLimit's doc comment applies symmetrically to applyProbe).
func TestProbeStepFreezesSettleToo(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 4, HardMax: 16, LastProbeAt: epoch0.Format(time.RFC3339)}
	if !applyProbe(c, epoch0.Add(5*time.Minute)) {
		t.Fatal("expected a probe step")
	}
	if !inSettle(*c, epoch0.Add(5*time.Minute+time.Second)) {
		t.Error("a probe step should itself start a fresh settle window")
	}
}

// --- burst-scaled decrease ---------------------------------------------------

func TestBurstScaledDecreaseFactor4ForDistinctSlots(t *testing.T) {
	c := &Config{
		Adaptive:   true,
		DynamicCap: 8,
		HardMax:    16,
		RecentRateLimitEvents: []RateLimitEvent{
			{At: epoch0.Add(-10 * time.Second).Format(time.RFC3339), Project: "p1", Bead: "b1"},
			{At: epoch0.Add(-5 * time.Second).Format(time.RFC3339), Project: "p2", Bead: "b2"},
		},
	}
	// A third DISTINCT (project,bead) within the 30s window makes 3 distinct
	// slots — a burst.
	decreased, opened := applyRateLimit(c, "p3", "b3", epoch0)
	if !decreased || opened {
		t.Fatalf("decreased=%v opened=%v, want decreased/not-opened", decreased, opened)
	}
	if c.DynamicCap != 2 { // 8/4, burst-scaled
		t.Errorf("DynamicCap = %d, want 2 (burst-scaled /4)", c.DynamicCap)
	}
}

func TestSameSlotRepeatedIsNotABurst(t *testing.T) {
	c := &Config{
		Adaptive:   true,
		DynamicCap: 8,
		HardMax:    16,
		RecentRateLimitEvents: []RateLimitEvent{
			{At: epoch0.Add(-10 * time.Second).Format(time.RFC3339), Project: "p1", Bead: "b1"},
			{At: epoch0.Add(-5 * time.Second).Format(time.RFC3339), Project: "p1", Bead: "b1"},
		},
	}
	// A third event from the SAME (project,bead) is still only 1 distinct slot.
	decreased, _ := applyRateLimit(c, "p1", "b1", epoch0)
	if !decreased {
		t.Fatal("expected a decrease")
	}
	if c.DynamicCap != 4 { // 8/2, normal factor — not a burst
		t.Errorf("DynamicCap = %d, want 4 (normal /2)", c.DynamicCap)
	}
}

func TestRecentRateLimitEventsWindowPruning(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 8, HardMax: 16}
	recordRateLimitEvent(c, "p1", "b1", epoch0)
	if got := len(c.RecentRateLimitEvents); got != 1 {
		t.Fatalf("len = %d, want 1", got)
	}
	// An event outside the 30s window prunes the earlier one.
	recordRateLimitEvent(c, "p2", "b2", epoch0.Add(31*time.Second))
	if got := len(c.RecentRateLimitEvents); got != 1 {
		t.Errorf("len = %d, want 1 (prior event pruned by window)", got)
	}
	if c.RecentRateLimitEvents[0].Project != "p2" {
		t.Errorf("surviving event = %+v, want p2's", c.RecentRateLimitEvents[0])
	}
}

func TestRecentRateLimitEventsBoundedSize(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 8, HardMax: 16}
	// Many distinct-slot events all within the 30s window: window-pruning
	// alone would keep them all, but maxRecentEvents defensively bounds size.
	for i := 0; i < maxRecentEvents+10; i++ {
		recordRateLimitEvent(c, "p", fmt.Sprintf("b%d", i), epoch0)
	}
	if got := len(c.RecentRateLimitEvents); got != maxRecentEvents {
		t.Errorf("len = %d, want bounded at %d", got, maxRecentEvents)
	}
}

// --- circuit breaker: pure trip/reopen/close mechanics -----------------------

func TestBreakerOpensOnRateLimitAtFloor(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 1, HardMax: 4}
	decreased, opened := applyRateLimit(c, "p", "b1", epoch0)
	if decreased {
		t.Error("a rate-limit at the floor should trip the breaker, not decrease")
	}
	if !opened {
		t.Fatal("expected the breaker to open")
	}
	if c.BreakerState != "open" {
		t.Errorf("BreakerState = %q, want open", c.BreakerState)
	}
	if c.BreakerBreakSeconds != DefaultBreakSeconds {
		t.Errorf("BreakerBreakSeconds = %d, want default %d", c.BreakerBreakSeconds, DefaultBreakSeconds)
	}
}

func TestBreakerOpensOnThreeDecreasesWithinTenMinutes(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 64, HardMax: 128}
	now := epoch0
	for i := 0; i < 2; i++ {
		decreased, opened := applyRateLimit(c, "p", fmt.Sprintf("b%d", i), now)
		if !decreased || opened {
			t.Fatalf("decrease %d: decreased=%v opened=%v, want true/false", i, decreased, opened)
		}
		now = now.Add(150 * time.Second) // past the 120s default settle window
	}
	decreased, opened := applyRateLimit(c, "p", "b2", now)
	if !decreased {
		t.Fatal("the third decrease should still apply")
	}
	if !opened {
		t.Fatal("3 decreases within 10 minutes should trip the breaker")
	}
	if c.BreakerState != "open" {
		t.Errorf("BreakerState = %q, want open", c.BreakerState)
	}
}

func TestBreakerReopenDurationDoublesAndCaps(t *testing.T) {
	c := &Config{Adaptive: true}
	openBreaker(c, epoch0, false)
	if c.BreakerBreakSeconds != DefaultBreakSeconds {
		t.Fatalf("initial open duration = %d, want %d", c.BreakerBreakSeconds, DefaultBreakSeconds)
	}
	wantSeq := []int{600, 1200, 2400, 3600, 3600}
	for i, want := range wantSeq {
		openBreaker(c, epoch0, true)
		if c.BreakerBreakSeconds != want {
			t.Errorf("reopen %d: BreakerBreakSeconds = %d, want %d", i+1, c.BreakerBreakSeconds, want)
		}
	}
}

func TestBreakerReopenCounterResetsOnClose(t *testing.T) {
	c := &Config{Adaptive: true}
	openBreaker(c, epoch0, false)
	openBreaker(c, epoch0, true) // simulate one failed probe cycle
	if c.BreakerReopenCount != 1 {
		t.Fatalf("setup: ReopenCount = %d, want 1", c.BreakerReopenCount)
	}
	closeBreaker(c, epoch0)
	if c.BreakerReopenCount != 0 {
		t.Errorf("ReopenCount after close = %d, want reset to 0", c.BreakerReopenCount)
	}
	if c.DynamicCap != 1 {
		t.Errorf("DynamicCap after close = %d, want reset to 1", c.DynamicCap)
	}
}

// --- circuit breaker: Store-level half-open admission ------------------------

// seedBreakerOpen writes an "open" breaker directly to disk (bypassing the
// decrease path, which this test suite exercises separately) and returns the
// resulting Config so callers can read BreakerBreakSeconds to advance the
// clock past it.
func seedBreakerOpen(t *testing.T, s *Store, now time.Time) Config {
	t.Helper()
	var c Config
	if err := fsx.ReadJSON(s.cfgPath, &c); err != nil {
		t.Fatal(err)
	}
	openBreaker(&c, now, false)
	if err := fsx.WriteJSONAtomic(s.cfgPath, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHalfOpenAdmitsExactlyOneProbe(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 } // this test is not about smoothing
	if err := s.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second) // break elapsed

	ok1, err := s.Acquire(lease("p1", "b1", 100))
	if err != nil || !ok1 {
		t.Fatalf("first acquire after break elapsed: ok=%v err=%v, want granted (the probe)", ok1, err)
	}
	ok2, err := s.Acquire(lease("p2", "b2", 200))
	if err != nil || ok2 {
		t.Errorf("second acquire while a probe is outstanding: ok=%v err=%v, want denied", ok2, err)
	}

	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "half-open" || status.ProbeProject != "p1" || status.ProbeBead != "b1" {
		t.Errorf("status = %+v, want half-open with probe p1/b1", status)
	}
}

// TestHalfOpenConcurrentAcquiresOnlyOneWins races real goroutines at the
// half-open transition: the flock must serialize them so exactly one becomes
// the probe.
func TestHalfOpenConcurrentAcquiresOnlyOneWins(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 }
	if err := s.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second)

	const workers = 16
	var granted int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.Acquire(lease("solo", fmt.Sprintf("b%d", i), 1000+i))
			if err == nil && ok {
				atomic.AddInt64(&granted, 1)
			}
		}(i)
	}
	wg.Wait()

	if granted != 1 {
		t.Errorf("granted = %d concurrent half-open acquires, want exactly 1", granted)
	}
	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "half-open" || status.ProbeProject == "" {
		t.Errorf("status = %+v, want half-open with a probe claimed", status)
	}
}

func TestHalfOpenCloseOnCleanRelease(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 }
	if err := s.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second)

	if ok, err := s.Acquire(lease("p1", "b1", 100)); err != nil || !ok {
		t.Fatalf("probe acquire: ok=%v err=%v", ok, err)
	}
	if err := s.Release("p1", "b1"); err != nil {
		t.Fatal(err)
	}

	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "" {
		t.Errorf("BreakerState = %q, want closed after a clean probe release", status.BreakerState)
	}
	if status.DynamicCap != 1 {
		t.Errorf("DynamicCap = %d, want reset to 1 on close", status.DynamicCap)
	}
	if status.ProbeProject != "" || status.ProbeBead != "" {
		t.Errorf("probe identity not cleared on close: %+v", status)
	}
}

func TestHalfOpenReopensOnProbeRateLimit(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 }
	if err := s.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second)

	if ok, err := s.Acquire(lease("p1", "b1", 100)); err != nil || !ok {
		t.Fatalf("probe acquire: ok=%v err=%v", ok, err)
	}
	if err := s.ReportRateLimit("p1", "b1", now); err != nil {
		t.Fatal(err)
	}

	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "open" {
		t.Errorf("BreakerState = %q, want re-opened after the probe rate-limited", status.BreakerState)
	}
	if status.BreakerReopenCount != 1 {
		t.Errorf("BreakerReopenCount = %d, want 1", status.BreakerReopenCount)
	}
	if status.BreakerBreakSeconds != DefaultBreakSeconds*2 {
		t.Errorf("BreakerBreakSeconds = %d, want doubled to %d", status.BreakerBreakSeconds, DefaultBreakSeconds*2)
	}
}

// TestCrashedProbeTimeoutReopensBreaker proves a probe whose agent pid is
// dead from the start (crash-on-launch, or its owning engine died) — so
// neither Release's clean-close nor ReportRateLimit's re-open ever fires —
// eventually resolves via pruneCrashedProbe's timeout fallback rather than
// wedging the breaker half-open forever.
func TestCrashedProbeTimeoutReopensBreaker(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return -1 }
	if err := s.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	c := seedBreakerOpen(t, s, now)
	now = now.Add(time.Duration(c.BreakerBreakSeconds) * time.Second)

	// The probe's agent pid is dead from the moment it is "admitted" —
	// simulating a launch that crashed immediately.
	s.Alive = func(int) bool { return false }

	ok, err := s.Acquire(lease("p1", "b1", 999))
	if err != nil || !ok {
		t.Fatalf("probe acquire: ok=%v err=%v", ok, err)
	}

	// Immediately after (well inside ProbeTimeout): the crashed lease has
	// already been pruned (Alive always false), but the breaker must NOT
	// resolve yet — it could still be a legitimate in-flight Release/report.
	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "half-open" {
		t.Fatalf("BreakerState = %q, want still half-open before ProbeTimeout", status.BreakerState)
	}

	now = now.Add(s.ProbeTimeout + time.Second)
	status, err = s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.BreakerState != "open" {
		t.Errorf("BreakerState = %q, want re-opened after the crashed-probe timeout", status.BreakerState)
	}
	if status.BreakerReopenCount != 1 {
		t.Errorf("BreakerReopenCount = %d, want 1 (treated conservatively as a failed probe)", status.BreakerReopenCount)
	}
}

// --- dispatch smoothing -------------------------------------------------------

func TestSmoothingDeniesSecondAcquireWithinInterval(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return 0 } // exactly the base 3s interval
	if err := s.SetAdaptiveCap(8, 16, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.Acquire(lease("p1", "b1", 100)); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	if ok, err := s.Acquire(lease("p2", "b2", 200)); err != nil || ok {
		t.Errorf("second acquire immediately after: ok=%v err=%v, want denied", ok, err)
	}

	now = now.Add(3 * time.Second)
	if ok, err := s.Acquire(lease("p2", "b2", 200)); err != nil || !ok {
		t.Errorf("acquire after the interval: ok=%v err=%v, want granted", ok, err)
	}
}

func TestSmoothingDeniesWithinJitteredBounds(t *testing.T) {
	// jitter=+0.5 widens the base 3s interval by 50% to 4.5s.
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return 0.5 }
	if err := s.SetAdaptiveCap(8, 16, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.Acquire(lease("p1", "b1", 100)); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	now = now.Add(4 * time.Second) // past the base 3s, inside the jittered 4.5s
	if ok, err := s.Acquire(lease("p2", "b2", 200)); err != nil || ok {
		t.Errorf("acquire at +4s with +50%% jitter: ok=%v err=%v, want denied (needs ~4.5s)", ok, err)
	}
	now = now.Add(time.Second) // +5s total, past 4.5s
	if ok, err := s.Acquire(lease("p2", "b2", 200)); err != nil || !ok {
		t.Errorf("acquire at +5s: ok=%v err=%v, want granted", ok, err)
	}
}

func TestSmoothingDenialDoesNotAdvanceTimestamp(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	s.Jitter = func() float64 { return 0 }
	if err := s.SetAdaptiveCap(8, 16, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.Acquire(lease("p1", "b1", 100)); err != nil || !ok {
		t.Fatalf("first acquire: ok=%v err=%v", ok, err)
	}
	before, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}

	// Two denied attempts in a row must not move LastAdmitAt at all.
	if ok, _ := s.Acquire(lease("p2", "b2", 200)); ok {
		t.Fatal("expected denial")
	}
	if ok, _ := s.Acquire(lease("p3", "b3", 300)); ok {
		t.Fatal("expected denial")
	}
	after, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if after.LastAdmitAt != before.LastAdmitAt {
		t.Errorf("LastAdmitAt moved on a denial: %q -> %q", before.LastAdmitAt, after.LastAdmitAt)
	}
}

// --- multi-engine coordination -----------------------------------------------

// TestMultiEngineStoresShareAdaptiveStateConsistently races two independent
// *Store handles (as two separate `koryph run` processes would use) over the
// same KORYPH_HOME: the shared flock must keep the dynamic cap authoritative
// regardless of which handle observes it.
func TestMultiEngineStoresShareAdaptiveStateConsistently(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	s1 := NewStore()
	s1.Now = func() time.Time { return epoch0 }
	s1.Alive = func(int) bool { return true }
	s1.Jitter = func() float64 { return -1 } // this test is not about smoothing
	s2 := NewStore()
	s2.Now = func() time.Time { return epoch0 }
	s2.Alive = func(int) bool { return true }
	s2.Jitter = func() float64 { return -1 }

	if err := s1.SetAdaptiveCap(4, 8, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	const workers = 16
	var granted int64
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		store := s1
		if i%2 == 0 {
			store = s2
		}
		go func(i int, s *Store) {
			defer wg.Done()
			ok, err := s.Acquire(lease("solo", fmt.Sprintf("b%d", i), 5000+i))
			if err == nil && ok {
				atomic.AddInt64(&granted, 1)
			}
		}(i, store)
	}
	wg.Wait()

	if int(granted) != 4 {
		t.Errorf("granted %d across two Store handles, want exactly the dynamic cap 4", granted)
	}
}

// --- adaptive-off parity ------------------------------------------------------

// TestAcquireAdaptiveOffIgnoresL5bFieldsEntirely proves byte-for-byte parity:
// even hand-set L5b field values that WOULD gate admission under Adaptive
// (an "open" breaker, a fresh LastAdmitAt) must be completely ignored when
// Adaptive is off — the pre-koryph-2im.11 static-cap path is untouched.
func TestAcquireAdaptiveOffIgnoresL5bFieldsEntirely(t *testing.T) {
	s := newTestStore(t)
	if err := fsx.WriteJSONAtomic(s.cfgPath, Config{
		MaxGlobalAgents: 4,
		BreakerState:    "open", // would deny everything if honored
		LastAdmitAt:     epoch0.Format(time.RFC3339),
	}); err != nil {
		t.Fatal(err)
	}
	ok, err := s.Acquire(lease("p1", "b1", 100))
	if err != nil || !ok {
		t.Errorf("Acquire with Adaptive=false: ok=%v err=%v, want granted (L5b fields must be ignored)", ok, err)
	}
}
