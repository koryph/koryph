// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"math"
	"testing"

	"github.com/koryph/koryph/internal/pricing"
)

// TestPriceForRuntimeClaudeParity proves priceFor(model) is exactly
// priceForRuntime("claude", model) (koryph-v8u.3 item 4): the table-driven
// rewrite must reproduce the pre-existing hardcoded opus/haiku/fable/sonnet
// switch byte-for-byte, for every branch and the fallback.
func TestPriceForRuntimeClaudeParity(t *testing.T) {
	models := []string{
		"claude-opus-4-8", "claude-haiku-4-8", "fable-preview",
		"claude-sonnet-4-5", "some-unrecognized-model-id", "",
	}
	for _, m := range models {
		want := priceFor(m)
		got := priceForRuntime("claude", m)
		if got != want {
			t.Errorf("priceForRuntime(claude, %q) = %+v, want %+v (priceFor parity)", m, got, want)
		}
	}
}

// TestPriceForRuntimeExactValues locks in the claude table's actual per-MTok
// numbers (koryph-v8u.3 item 4: "claude table = today's values") so a future
// refactor cannot silently drift them.
func TestPriceForRuntimeExactValues(t *testing.T) {
	cases := []struct {
		model string
		want  modelPrice
	}{
		{"claude-opus-4-8", modelPrice{in: 15, out: 75, cacheWrite: 18.75, cacheRead: 1.5}},
		{"claude-haiku-4-8", modelPrice{in: 0.8, out: 4, cacheWrite: 1, cacheRead: 0.08}},
		{"fable-preview", modelPrice{in: 25, out: 125, cacheWrite: 31.25, cacheRead: 2.5}},
		{"claude-sonnet-4-5", modelPrice{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}},
		{"unrecognized-model", modelPrice{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}},
	}
	for _, tc := range cases {
		if got := priceForRuntime("claude", tc.model); got != tc.want {
			t.Errorf("priceForRuntime(claude, %q) = %+v, want %+v", tc.model, got, tc.want)
		}
	}
}

// TestPriceForRuntimeStubTable proves the mechanism is genuinely table-driven
// keyed by runtime: a second table, registered only for the duration of this
// test, is consulted for its own name and does not leak into (or borrow
// from) claude's table.
func TestPriceForRuntimeStubTable(t *testing.T) {
	const name = "quota-test-stub-runtime"
	pricingTables[name] = runtimePriceTable{
		rules: []priceRule{
			{"big", modelPrice{in: 100, out: 200, cacheWrite: 10, cacheRead: 1}},
		},
		fallback: modelPrice{in: 1, out: 2, cacheWrite: 0.1, cacheRead: 0.01},
	}
	t.Cleanup(func() { delete(pricingTables, name) })

	if got, want := priceForRuntime(name, "stub-big-model"),
		(modelPrice{in: 100, out: 200, cacheWrite: 10, cacheRead: 1}); got != want {
		t.Errorf("priceForRuntime(%s, stub-big-model) = %+v, want %+v", name, got, want)
	}
	if got, want := priceForRuntime(name, "stub-small-model"),
		(modelPrice{in: 1, out: 2, cacheWrite: 0.1, cacheRead: 0.01}); got != want {
		t.Errorf("priceForRuntime(%s, stub-small-model) = %+v, want the stub's own fallback, not claude's", name, got)
	}
	// The stub table must not have altered claude's own pricing.
	if got, want := priceForRuntime("claude", "claude-sonnet-4-5"),
		(modelPrice{in: 3, out: 15, cacheWrite: 3.75, cacheRead: 0.3}); got != want {
		t.Errorf("claude pricing changed after registering a stub table: got %+v, want %+v", got, want)
	}
}

// TestPriceForRuntimeUnknownRuntimeFallsBackToClaude proves an unrecognized
// runtime name degrades gracefully to claude's table (usage pricing is
// advisory governor input, never a dispatch gate — unlike
// modelroute.Resolve's deliberately fail-closed unknown-runtime error).
func TestPriceForRuntimeUnknownRuntimeFallsBackToClaude(t *testing.T) {
	got := priceForRuntime("no-such-runtime", "claude-opus-4-8")
	want := priceForRuntime("claude", "claude-opus-4-8")
	if got != want {
		t.Errorf("priceForRuntime(no-such-runtime, ...) = %+v, want claude fallback %+v", got, want)
	}
}

// TestClaudePriceTableMatchesCanonical proves claudePriceTable is a faithful
// projection of pricing.Claude (koryph-fiv finding #5): base in/out come
// straight from the canonical rate, cache from the 5-minute-TTL multipliers.
// This is the guarantee usage.go's doc names — the table is derived, never a
// second hand-maintained copy that can drift.
func TestClaudePriceTableMatchesCanonical(t *testing.T) {
	round6 := func(v float64) float64 { return math.Round(v*1e6) / 1e6 }
	for _, tier := range pricing.Claude {
		got := priceForRuntime("claude", tier.Name)
		want := modelPrice{
			in:         tier.Rate.InPerMTok,
			out:        tier.Rate.OutPerMTok,
			cacheWrite: round6(tier.Rate.InPerMTok * pricing.CacheWrite5MinMultiplier),
			cacheRead:  round6(tier.Rate.InPerMTok * pricing.CacheReadMultiplier),
		}
		if got != want {
			t.Errorf("claude price for %q = %+v, want canonical-derived %+v", tier.Name, got, want)
		}
	}
}

// TestTierUSDTableCoversCanonicalTiers guards the deliberate NON-derivation in
// estimate.go: its coarse $/dispatch seed is a different granularity than
// pricing.Claude's per-MTok rates, so it is not derived — but it MUST still
// name every canonical Claude tier, so a tier added to pricing.Claude cannot be
// silently forgotten by the governor's estimator seed (koryph-fiv finding #5).
func TestTierUSDTableCoversCanonicalTiers(t *testing.T) {
	claude := tierUSDTables["claude"].perTier
	for _, tier := range pricing.Claude {
		if _, ok := claude[tier.Name]; !ok {
			t.Errorf("tierUSDTables[claude] missing canonical tier %q — add its $/dispatch seed", tier.Name)
		}
	}
}
