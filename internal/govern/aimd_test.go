// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

var epoch0 = time.Unix(0, 0).UTC()

// --- Config.EffectiveCap (pure) --------------------------------------------

func TestConfigEffectiveCapAdaptiveOff(t *testing.T) {
	// Adaptive off must reproduce today's static-cap behavior byte-for-byte —
	// including for a store written before these fields existed (a decoded
	// zero-value Config) and regardless of what garbage the (unused) dynamic
	// fields carry.
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{"zero-value config (old store, absent file)", Config{}, DefaultMaxGlobalAgents},
		{"max set, adaptive off", Config{MaxGlobalAgents: 5}, 5},
		{"max zero, adaptive off ⇒ default", Config{MaxGlobalAgents: 0}, DefaultMaxGlobalAgents},
		{"max negative, adaptive off ⇒ default", Config{MaxGlobalAgents: -1}, DefaultMaxGlobalAgents},
		{
			"adaptive fields populated but Adaptive=false ⇒ ignored",
			Config{MaxGlobalAgents: 5, DynamicCap: 99, HardMax: 1},
			5,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveCap(); got != tc.want {
				t.Errorf("EffectiveCap() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestConfigEffectiveCapAdaptiveOn(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want int
	}{
		{"within range", Config{Adaptive: true, DynamicCap: 5, HardMax: 10}, 5},
		{"floored at 1", Config{Adaptive: true, DynamicCap: 0, HardMax: 10}, 1},
		{"negative floored at 1", Config{Adaptive: true, DynamicCap: -3, HardMax: 10}, 1},
		{"clamped to hard max", Config{Adaptive: true, DynamicCap: 20, HardMax: 10}, 10},
		{"hard max unset ⇒ falls back to dynamic cap (no clamp)", Config{Adaptive: true, DynamicCap: 6, HardMax: 0}, 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.EffectiveCap(); got != tc.want {
				t.Errorf("EffectiveCap() = %d, want %d", got, tc.want)
			}
		})
	}
}

// --- applyRateLimit (pure) --------------------------------------------------

func TestApplyRateLimitHalvesFromVariousCaps(t *testing.T) {
	cases := []struct {
		name       string
		startCap   int
		hardMax    int
		wantHalved int
	}{
		{"8 -> 4", 8, 16, 4},
		{"5 -> 2 (integer division)", 5, 10, 2},
		{"2 -> 1", 2, 4, 1},
		// startCap<=1 (already at the floor) is NOT exercised here: per
		// koryph-2im.11 that trips the circuit breaker instead of halving —
		// see TestBreakerOpensOnRateLimitAtFloor.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Adaptive: true, DynamicCap: tc.startCap, HardMax: tc.hardMax}
			decreased, opened := applyRateLimit(c, "p", "b1", epoch0)
			if !decreased {
				t.Fatal("applyRateLimit() = false, want true (first event, not settling)")
			}
			if opened {
				t.Error("breaker should not open when the cap lands above the floor")
			}
			if c.DynamicCap != tc.wantHalved {
				t.Errorf("DynamicCap after halve = %d, want %d", c.DynamicCap, tc.wantHalved)
			}
			if c.LastDecreaseAt == "" {
				t.Error("LastDecreaseAt not stamped")
			}
			if c.RateLimitEvents != 1 {
				t.Errorf("RateLimitEvents = %d, want 1", c.RateLimitEvents)
			}
		})
	}
}

// TestApplyRateLimitSettleSuppressesDoubleHalve proves koryph-2im.11's settle
// window subsumes the old 60s decrease cooldown: a second event arriving
// inside the (now 120s default) freeze is counted but not re-applied, and a
// third event AFTER settle expires halves again.
func TestApplyRateLimitSettleSuppressesDoubleHalve(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 8, HardMax: 16}

	if decreased, _ := applyRateLimit(c, "p", "b1", epoch0); !decreased {
		t.Fatal("first event should decrease")
	}
	if c.DynamicCap != 4 {
		t.Fatalf("DynamicCap = %d, want 4", c.DynamicCap)
	}
	if !inSettle(*c, epoch0.Add(30*time.Second)) {
		t.Fatal("expected the store to be settling 30s after a decrease (default 120s settle)")
	}

	// A second event 30s later (inside the 120s settle window) is counted but
	// must NOT re-halve.
	if decreased, _ := applyRateLimit(c, "p", "b1", epoch0.Add(30*time.Second)); decreased {
		t.Error("event inside settle should not decrease")
	}
	if c.DynamicCap != 4 {
		t.Errorf("DynamicCap after settle-suppressed event = %d, want unchanged 4", c.DynamicCap)
	}
	if c.RateLimitEvents != 2 {
		t.Errorf("RateLimitEvents = %d, want 2 (both events counted)", c.RateLimitEvents)
	}

	// A third event after settle elapses halves again.
	if decreased, _ := applyRateLimit(c, "p", "b1", epoch0.Add(121*time.Second)); !decreased {
		t.Error("event after settle expiry should decrease")
	}
	if c.DynamicCap != 2 {
		t.Errorf("DynamicCap after second halve = %d, want 2", c.DynamicCap)
	}
	if c.RateLimitEvents != 3 {
		t.Errorf("RateLimitEvents = %d, want 3", c.RateLimitEvents)
	}
}

func TestApplyRateLimitCountsEvenWhenAdaptiveOff(t *testing.T) {
	// Adaptive off: the overlay is disabled, so DynamicCap must never move,
	// but the event counter is still useful observability (an operator can see
	// "you've been rate-limited N times" before ever turning adaptive on).
	c := &Config{MaxGlobalAgents: 5}
	if decreased, opened := applyRateLimit(c, "p", "b1", epoch0); decreased || opened {
		t.Error("applyRateLimit() with Adaptive=false should never decrease or open the breaker")
	}
	if c.DynamicCap != 0 {
		t.Errorf("DynamicCap = %d, want untouched 0", c.DynamicCap)
	}
	if c.RateLimitEvents != 1 || c.LastRateLimitAt == "" {
		t.Errorf("event not counted: RateLimitEvents=%d LastRateLimitAt=%q", c.RateLimitEvents, c.LastRateLimitAt)
	}
	// EffectiveCap is unaffected either way.
	if got := c.EffectiveCap(); got != 5 {
		t.Errorf("EffectiveCap() = %d, want 5 (operator cap, untouched)", got)
	}
}

// --- applyProbe (pure) ------------------------------------------------------

func TestApplyProbeAdditiveGrowthPastStartingCap(t *testing.T) {
	// The probe must climb PAST the operator's starting cap (4) up to HardMax
	// (8) — that is the whole point of L5's upward probing.
	c := &Config{Adaptive: true, MaxGlobalAgents: 4, DynamicCap: 4, HardMax: 8, LastProbeAt: epoch0.Format(time.RFC3339)}

	// One full interval elapsed ⇒ +1.
	if !applyProbe(c, epoch0.Add(5*time.Minute)) {
		t.Fatal("expected a probe step after 5 minutes")
	}
	if c.DynamicCap != 5 {
		t.Errorf("DynamicCap = %d, want 5 (past the starting cap of 4)", c.DynamicCap)
	}

	// Several more intervals from the (advanced) anchor climb further, clamped
	// at HardMax.
	if !applyProbe(c, parseTime(c.LastProbeAt).Add(30*time.Minute)) {
		t.Fatal("expected further probe steps")
	}
	if c.DynamicCap != 8 {
		t.Errorf("DynamicCap = %d, want clamped at hard max 8", c.DynamicCap)
	}
}

func TestApplyProbeFloorAtOneNeverDecreases(t *testing.T) {
	// The probe only ever adds; it must never lower the cap even if called
	// with DynamicCap already at (or somehow below) the floor.
	c := &Config{Adaptive: true, DynamicCap: 1, HardMax: 4, LastProbeAt: epoch0.Format(time.RFC3339)}
	applyProbe(c, epoch0.Add(time.Minute)) // < 1 interval: no-op
	if c.DynamicCap != 1 {
		t.Errorf("DynamicCap = %d, want unchanged 1 (sub-interval elapsed)", c.DynamicCap)
	}
}

func TestApplyProbeNoStepsBeforeInterval(t *testing.T) {
	c := &Config{Adaptive: true, DynamicCap: 4, HardMax: 8, LastProbeAt: epoch0.Format(time.RFC3339)}
	if changed := applyProbe(c, epoch0.Add(4*time.Minute)); changed {
		t.Error("applyProbe() before a full interval elapsed should be a no-op")
	}
	if c.DynamicCap != 4 {
		t.Errorf("DynamicCap = %d, want unchanged 4", c.DynamicCap)
	}
}

func TestApplyProbeAnchorsOnSettleExpiryNotTheChangeItself(t *testing.T) {
	// koryph-2im.11: the additive-increase quiet-clock starts at SETTLE
	// EXPIRY, not at the change (decrease) timestamp itself — a decrease at
	// epoch0+2min with a 120s settle freezes growth until epoch0+4min, not
	// epoch0+2min+5min as the pre-L5b (decrease-anchored) semantics would.
	settleUntil := epoch0.Add(2*time.Minute + 120*time.Second) // epoch0+4min
	c := &Config{
		Adaptive:       true,
		DynamicCap:     2,
		HardMax:        8,
		LastProbeAt:    epoch0.Format(time.RFC3339),
		LastDecreaseAt: epoch0.Add(2 * time.Minute).Format(time.RFC3339),
		SettleUntil:    settleUntil.Format(time.RFC3339),
	}
	// Still inside the settle window ⇒ frozen, no growth.
	if changed := applyProbe(c, epoch0.Add(3*time.Minute)); changed {
		t.Error("applyProbe() should not grow before settle expiry")
	}
	if c.DynamicCap != 2 {
		t.Errorf("DynamicCap = %d, want unchanged 2", c.DynamicCap)
	}
	// A full probeInterval past SETTLE EXPIRY (not the decrease itself) grows.
	if !applyProbe(c, settleUntil.Add(5*time.Minute)) {
		t.Error("applyProbe() should grow once 5 minutes have elapsed since settle expiry")
	}
	if c.DynamicCap != 3 {
		t.Errorf("DynamicCap = %d, want 3", c.DynamicCap)
	}
}

func TestApplyProbeSeedsAnchorWithoutCreditingElapsedTime(t *testing.T) {
	// No LastProbeAt/LastDecreaseAt yet (adaptive just enabled, or a
	// hand-edited store): seed the clock rather than crediting a huge
	// elapsed-since-epoch jump.
	c := &Config{Adaptive: true, DynamicCap: 2, HardMax: 8}
	if changed := applyProbe(c, epoch0.Add(24*time.Hour)); !changed {
		t.Error("applyProbe() should report changed=true when seeding the anchor")
	}
	if c.DynamicCap != 2 {
		t.Errorf("DynamicCap = %d, want unchanged 2 (seeding must not credit elapsed time)", c.DynamicCap)
	}
	if c.LastProbeAt == "" {
		t.Error("LastProbeAt not seeded")
	}
}

func TestApplyProbeNoopWhenAdaptiveOff(t *testing.T) {
	c := &Config{DynamicCap: 2, HardMax: 8}
	if changed := applyProbe(c, epoch0.Add(24*time.Hour)); changed {
		t.Error("applyProbe() with Adaptive=false must be a no-op")
	}
}

// --- Store integration -------------------------------------------------------

func TestStoreSetAdaptiveCapAndEffectiveCap(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }

	if err := s.SetAdaptiveCap(4, 0, 0, 0, 0); err != nil { // hardMax 0 ⇒ default 2x
		t.Fatal(err)
	}
	if got := s.EffectiveCap(); got != 4 {
		t.Errorf("EffectiveCap() right after enabling = %d, want 4 (seeded to max-global)", got)
	}

	// Probing past the starting cap: advance the clock past several 5-minute
	// intervals; EffectiveCap must climb, clamped at hardMax = 8 (2x4).
	now = epoch0.Add(30 * time.Minute)
	if got := s.EffectiveCap(); got != 8 {
		t.Errorf("EffectiveCap() after 30 minutes quiet = %d, want clamped at hard max 8", got)
	}
}

func TestStoreReportRateLimitHalvesSharedCap(t *testing.T) {
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	if err := s.SetAdaptiveCap(8, 16, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportRateLimit("p", "b1", now); err != nil {
		t.Fatal(err)
	}
	if got := s.EffectiveCap(); got != 4 {
		t.Errorf("EffectiveCap() after rate-limit = %d, want 4", got)
	}
	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.RateLimitEvents != 1 || status.LastDecreaseAt == "" {
		t.Errorf("AIMDStatus = %+v, want one recorded decrease", status)
	}
}

func TestStoreAcquireUsesEffectiveCapNotStaticCap(t *testing.T) {
	// The static operator cap (2) would refuse a 3rd/4th acquire; once the
	// probe has grown the dynamic cap past it, Acquire must admit more.
	s := newTestStore(t)
	now := epoch0
	s.Now = func() time.Time { return now }
	// This test is about fair-share/probe-cap growth, not dispatch smoothing
	// (koryph-2im.11) — neutralize the jittered spacing so back-to-back
	// Acquire calls at the same instant are not denied for spacing.
	s.Jitter = func() float64 { return -1 }
	if err := s.SetAdaptiveCap(2, 6, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if ok, err := s.Acquire(lease("solo", "b"+string(rune('0'+i)), 100+i)); err != nil || !ok {
			t.Fatalf("acquire %d: ok=%v err=%v, want granted within starting cap", i, ok, err)
		}
	}
	if ok, _ := s.Acquire(lease("solo", "b2", 102)); ok {
		t.Fatal("3rd acquire granted before any probe growth — static cap should still bind")
	}

	// Let two probe intervals elapse with no rate-limit events.
	now = epoch0.Add(10 * time.Minute)
	if ok, err := s.Acquire(lease("solo", "b2", 102)); err != nil || !ok {
		t.Errorf("acquire after probe growth: ok=%v err=%v, want granted (dynamic cap should now exceed 2)", ok, err)
	}
}

func TestStoreEffectiveCapCompatibleWithOldStore(t *testing.T) {
	// A governor.json written before the AIMD fields existed must still load
	// and behave exactly as it always has.
	s := newTestStore(t)
	old, err := json.Marshal(map[string]int{"max_global_agents": 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, old, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap(); got != 5 {
		t.Fatalf("Cap() = %d, want 5", got)
	}
	if got := s.EffectiveCap(); got != 5 {
		t.Errorf("EffectiveCap() on an old-style store = %d, want 5 (byte-for-byte compatible with Cap())", got)
	}
}

func TestSetAdaptiveCapRejectsNonPositive(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetAdaptiveCap(0, 0, 0, 0, 0); err == nil {
		t.Error("SetAdaptiveCap(0, ...) should error")
	}
}

// TestSetAdaptiveCapAppliesL5bDefaults proves koryph-2im.11's settle/break/
// smoothing knobs default when omitted (<=0) and persist when given.
func TestSetAdaptiveCapAppliesL5bDefaults(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetAdaptiveCap(4, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	status, err := s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SettleSeconds != DefaultSettleSeconds {
		t.Errorf("SettleSeconds = %d, want default %d", status.SettleSeconds, DefaultSettleSeconds)
	}
	if status.BreakSeconds != DefaultBreakSeconds {
		t.Errorf("BreakSeconds = %d, want default %d", status.BreakSeconds, DefaultBreakSeconds)
	}
	if status.MinDispatchIntervalSeconds != DefaultMinDispatchIntervalSeconds {
		t.Errorf("MinDispatchIntervalSeconds = %d, want default %d", status.MinDispatchIntervalSeconds, DefaultMinDispatchIntervalSeconds)
	}

	if err := s.SetAdaptiveCap(4, 0, 30, 60, 1); err != nil {
		t.Fatal(err)
	}
	status, err = s.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.SettleSeconds != 30 || status.BreakSeconds != 60 || status.MinDispatchIntervalSeconds != 1 {
		t.Errorf("explicit L5b knobs not persisted: %+v", status)
	}
}
