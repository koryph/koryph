// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import "testing"

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
