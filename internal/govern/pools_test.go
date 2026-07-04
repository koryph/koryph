// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// koryph-v8u.11 (L5c): per-provider governor pools. The rest of this package's
// test suite (govern_test.go, aimd_test.go, settle_breaker_test.go) exercises
// a single pool (the default, "" -> DefaultPool) end to end; this file
// exercises the cross-pool contract specifically: independence, migration,
// fair share scoped to a pool, and normalization.

// poolLease is lease (govern_test.go) plus an explicit provider.
func poolLease(provider, project, bead string, pid int) Lease {
	return Lease{Project: project, Bead: bead, PID: pid, EnginePID: 1, Provider: provider}
}

// --- core assertion: independence across pools ----------------------------

// TestRateLimitInPoolAOnlyAffectsPoolA is the core assertion of koryph-v8u.11:
// a rate-limit event (and the resulting halve/settle/possible breaker trip)
// in one pool must not touch another pool's admission at all.
func TestRateLimitInPoolAOnlyAffectsPoolA(t *testing.T) {
	s := newTestStore(t)
	// Seed pool A (openai) already at the floor so a SINGLE rate-limit event
	// trips its breaker immediately (koryph-2im.11: a report at
	// EffectiveCap()<=1 trips independent of settle) — no elapsed time
	// needed, so pool B's own (independent, time-based) additive-probe clock
	// cannot muddy the "untouched" assertion below.
	if err := s.SetAdaptiveCap("openai", 1, 4, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.SetAdaptiveCap("anthropic", 8, 16, 0, 0, 0); err != nil {
		t.Fatal(err)
	}

	if err := s.ReportRateLimit("openai", "p", "b1", epoch0); err != nil {
		t.Fatal(err)
	}

	statusA, err := s.AIMDStatus("openai")
	if err != nil {
		t.Fatal(err)
	}
	if statusA.BreakerState != "open" {
		t.Fatalf("pool openai breaker = %q, want open after 3 decreases in 10m", statusA.BreakerState)
	}

	// Pool B (anthropic) must be completely untouched: still adaptive at its
	// original dynamic cap, breaker closed, zero rate-limit events, and
	// Acquire still admits normally.
	statusB, err := s.AIMDStatus("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if statusB.BreakerState != "" {
		t.Errorf("pool anthropic breaker = %q, want closed (unaffected by pool openai)", statusB.BreakerState)
	}
	if statusB.DynamicCap != 8 {
		t.Errorf("pool anthropic dynamic cap = %d, want unchanged 8", statusB.DynamicCap)
	}
	if statusB.RateLimitEvents != 0 {
		t.Errorf("pool anthropic rate-limit events = %d, want 0", statusB.RateLimitEvents)
	}

	// Admission: openai denies everything (breaker open); anthropic admits
	// normally — the two pools' admission is fully independent.
	if ok, err := s.Acquire(poolLease("openai", "solo", "x", 100)); err != nil || ok {
		t.Errorf("pool openai acquire: ok=%v err=%v, want denied (breaker open)", ok, err)
	}
	if ok, err := s.Acquire(poolLease("anthropic", "solo", "y", 200)); err != nil || !ok {
		t.Errorf("pool anthropic acquire: ok=%v err=%v, want granted (unaffected)", ok, err)
	}
}

// TestTwoPoolsSeparateCapsAndLeases proves the operator cap, lease counting,
// and Release are all pool-scoped: filling pool A's cap must not affect pool
// B's admission, and releasing a pool-A lease must not free a pool-B slot.
func TestTwoPoolsSeparateCapsAndLeases(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("anthropic", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCap("openai", 1); err != nil {
		t.Fatal(err)
	}

	if ok, err := s.Acquire(poolLease("anthropic", "p", "b1", 10)); err != nil || !ok {
		t.Fatalf("anthropic acquire 1: ok=%v err=%v", ok, err)
	}
	if ok, err := s.Acquire(poolLease("anthropic", "p", "b2", 11)); err != nil || ok {
		t.Errorf("anthropic acquire 2: ok=%v err=%v, want denied (pool A cap 1 full)", ok, err)
	}
	// Pool B's cap is untouched by pool A being full.
	if ok, err := s.Acquire(poolLease("openai", "p", "b1", 20)); err != nil || !ok {
		t.Fatalf("openai acquire 1: ok=%v err=%v, want granted (independent cap)", ok, err)
	}
	if ok, err := s.Acquire(poolLease("openai", "p", "b2", 21)); err != nil || ok {
		t.Errorf("openai acquire 2: ok=%v err=%v, want denied (pool B cap 1 full)", ok, err)
	}

	// Releasing pool A's lease does not free pool B's slot, and vice versa.
	if err := s.Release("anthropic", "p", "b1"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Acquire(poolLease("openai", "p", "b3", 22)); err != nil || ok {
		t.Errorf("openai acquire after releasing anthropic's lease: ok=%v err=%v, want still denied", ok, err)
	}
	if ok, err := s.Acquire(poolLease("anthropic", "p", "b4", 12)); err != nil || !ok {
		t.Errorf("anthropic acquire after its own release: ok=%v err=%v, want granted", ok, err)
	}
}

// --- migration --------------------------------------------------------------

// TestLegacyGovernorJSONMigratesIntoAnthropicPool proves a pre-koryph-v8u.11
// governor.json (any shape from koryph-1xk through koryph-2im.11 — a flat
// document with no "pools" key) loads transparently as the anthropic pool,
// preserving every AIMD/settle/breaker field, and round-trips thereafter in
// the new {"pools": {...}} shape.
func TestLegacyGovernorJSONMigratesIntoAnthropicPool(t *testing.T) {
	s := newTestStore(t)
	legacy := Config{
		MaxGlobalAgents: 6,
		Adaptive:        true,
		HardMax:         12,
		DynamicCap:      3,
		LastDecreaseAt:  epoch0.Format(time.RFC3339),
		RateLimitEvents: 4,
		SettleSeconds:   60,
		BreakSeconds:    600,
		BreakerState:    "half-open",
		ProbeProject:    "proj",
		ProbeBead:       "bead1",
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(s.cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.cfgPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	status, err := s.AIMDStatus("") // "" normalizes to DefaultPool (anthropic)
	if err != nil {
		t.Fatal(err)
	}
	if status.MaxGlobalAgents != 6 || status.HardMax != 12 || status.DynamicCap != 3 ||
		status.RateLimitEvents != 4 || status.SettleSeconds != 60 || status.BreakSeconds != 600 ||
		status.BreakerState != "half-open" || status.ProbeProject != "proj" || status.ProbeBead != "bead1" {
		t.Errorf("migrated anthropic pool = %+v, want every legacy field preserved", status)
	}

	// Round-trips thereafter in the new shape: re-reading the file directly
	// must show a "pools" envelope with the anthropic entry.
	var f File
	raw, err := os.ReadFile(s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.Pools[DefaultPool]; !ok {
		t.Fatalf("governor.json after a Store read did not round-trip into the new pools shape: %s", raw)
	}
}

// --- fair share within a pool only -----------------------------------------

// TestFairShareDoesNotCrossPools proves demand/fair-share denominators are
// scoped per pool: a project demanding in pool A must not affect another
// project's fair share in pool B, even though both projects and both pools
// share the same cap value.
func TestFairShareDoesNotCrossPools(t *testing.T) {
	s := newTestStore(t)
	if err := s.SetCap("anthropic", 4); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCap("openai", 4); err != nil {
		t.Fatal(err)
	}

	// Two projects demand in pool anthropic ⇒ fair share 2 each there.
	if err := s.RefreshDemand("anthropic", "a", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.RefreshDemand("anthropic", "b", 2); err != nil {
		t.Fatal(err)
	}
	// Only project "a" demands in pool openai ⇒ its fair share there is the
	// WHOLE cap (4), unaffected by "b" existing in the other pool.
	if err := s.RefreshDemand("openai", "a", 1); err != nil {
		t.Fatal(err)
	}

	fsAnthropic, err := s.FairShareFor("anthropic", "a")
	if err != nil {
		t.Fatal(err)
	}
	if fsAnthropic != 2 {
		t.Errorf("fair share for a in pool anthropic = %d, want 2 (shared with b)", fsAnthropic)
	}
	fsOpenAI, err := s.FairShareFor("openai", "a")
	if err != nil {
		t.Fatal(err)
	}
	if fsOpenAI != 4 {
		t.Errorf("fair share for a in pool openai = %d, want 4 (b does not demand here)", fsOpenAI)
	}
}

// --- empty-provider normalization -------------------------------------------

// TestEmptyProviderNormalizesToAnthropicEverywhere proves "" is treated as
// DefaultPool at every entry point that accepts a provider, and that a lease
// constructed with Provider=="" is stored with the resolved key (never an
// empty one).
func TestEmptyProviderNormalizesToAnthropicEverywhere(t *testing.T) {
	s := newTestStore(t)

	// SetCap/Cap.
	if err := s.SetCap("", 3); err != nil {
		t.Fatal(err)
	}
	if got := s.Cap(DefaultPool); got != 3 {
		t.Errorf("Cap(DefaultPool) after SetCap(\"\",3) = %d, want 3", got)
	}

	// Acquire/Hold: Provider=="" must be stored as DefaultPool, never "".
	if ok, err := s.Acquire(Lease{Project: "p", Bead: "b1", EnginePID: 1}); err != nil || !ok {
		t.Fatalf("acquire with empty provider: ok=%v err=%v", ok, err)
	}
	_, leases, _, err := s.Snapshot(DefaultPool)
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].Provider != DefaultPool {
		t.Fatalf("lease after empty-provider acquire = %+v, want stored Provider %q", leases, DefaultPool)
	}

	// RefreshDemand/DropDemand/FairShareFor with "".
	if err := s.RefreshDemand("", "p", 1); err != nil {
		t.Fatal(err)
	}
	share, err := s.FairShareFor("", "p")
	if err != nil {
		t.Fatal(err)
	}
	if share != 3 {
		t.Errorf("FairShareFor(\"\", ...) = %d, want 3 (DefaultPool's cap, sole demander)", share)
	}
	if err := s.DropDemand("", "p"); err != nil {
		t.Fatal(err)
	}

	// Release/ReportRateLimit/AIMDStatus/SetAdaptiveCap/EffectiveCap with "".
	if err := s.Release("", "p", "b1"); err != nil {
		t.Fatal(err)
	}
	if err := s.ReportRateLimit("", "p", "b1", s.Now()); err != nil {
		t.Fatal(err)
	}
	status, err := s.AIMDStatus("")
	if err != nil {
		t.Fatal(err)
	}
	if status.RateLimitEvents != 1 {
		t.Errorf("RateLimitEvents via empty-provider ReportRateLimit = %d, want 1", status.RateLimitEvents)
	}
	if err := s.SetAdaptiveCap("", 4, 0, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	if got := s.EffectiveCap(""); got != 4 {
		t.Errorf("EffectiveCap(\"\") after SetAdaptiveCap(\"\",...) = %d, want 4", got)
	}

	// The set of known pools must be exactly {anthropic} — "" never leaks in
	// as its own pool key.
	pools, err := s.Pools()
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 1 || pools[0] != DefaultPool {
		t.Errorf("Pools() = %v, want exactly [%q]", pools, DefaultPool)
	}
}

// --- concurrency across pools (proof-of-independence race coverage) --------

// TestConcurrentAcquireReleaseAcrossTwoPools races concurrent Acquire/Release
// against two independent pools sharing one store/lock: neither pool's cap
// is ever breached and neither pool observes the other's leases. Run with
// -race (required by koryph-v8u.11's test plan).
func TestConcurrentAcquireReleaseAcrossTwoPools(t *testing.T) {
	s := newTestStore(t)
	const capEach = 4
	if err := s.SetCap("anthropic", capEach); err != nil {
		t.Fatal(err)
	}
	if err := s.SetCap("openai", capEach); err != nil {
		t.Fatal(err)
	}

	const workersPerPool = 16
	var grantedAnthropic, grantedOpenAI int64
	var wg sync.WaitGroup
	for i := 0; i < workersPerPool; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.Acquire(poolLease("anthropic", "solo", fmt.Sprintf("a%d", i), 1000+i))
			if err == nil && ok {
				atomic.AddInt64(&grantedAnthropic, 1)
			}
		}(i)
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := s.Acquire(poolLease("openai", "solo", fmt.Sprintf("o%d", i), 2000+i))
			if err == nil && ok {
				atomic.AddInt64(&grantedOpenAI, 1)
			}
		}(i)
	}
	wg.Wait()

	if int(grantedAnthropic) != capEach {
		t.Errorf("pool anthropic granted %d under contention, want exactly %d", grantedAnthropic, capEach)
	}
	if int(grantedOpenAI) != capEach {
		t.Errorf("pool openai granted %d under contention, want exactly %d", grantedOpenAI, capEach)
	}

	_, anthropicLeases, _, err := s.Snapshot("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if len(anthropicLeases) != capEach {
		t.Errorf("anthropic leases on disk = %d, want %d", len(anthropicLeases), capEach)
	}
	_, openaiLeases, _, err := s.Snapshot("openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(openaiLeases) != capEach {
		t.Errorf("openai leases on disk = %d, want %d", len(openaiLeases), capEach)
	}

	// Release every lease concurrently, from both pools at once, and confirm
	// both pools end up empty (no cross-pool interference on release either).
	wg = sync.WaitGroup{}
	for _, l := range anthropicLeases {
		wg.Add(1)
		go func(bead string) {
			defer wg.Done()
			_ = s.Release("anthropic", "solo", bead)
		}(l.Bead)
	}
	for _, l := range openaiLeases {
		wg.Add(1)
		go func(bead string) {
			defer wg.Done()
			_ = s.Release("openai", "solo", bead)
		}(l.Bead)
	}
	wg.Wait()

	_, anthropicLeases, _, err = s.Snapshot("anthropic")
	if err != nil {
		t.Fatal(err)
	}
	if len(anthropicLeases) != 0 {
		t.Errorf("anthropic leases after releasing all = %d, want 0", len(anthropicLeases))
	}
	_, openaiLeases, _, err = s.Snapshot("openai")
	if err != nil {
		t.Fatal(err)
	}
	if len(openaiLeases) != 0 {
		t.Errorf("openai leases after releasing all = %d, want 0", len(openaiLeases))
	}
}
