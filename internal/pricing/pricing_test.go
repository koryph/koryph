// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package pricing

import "testing"

// TestClaudeRateKnownTiers pins the canonical base rates every consumer
// derives from. These are the real published Anthropic list prices (USD /
// MTok); a change here is an intentional price update, not a refactor.
func TestClaudeRateKnownTiers(t *testing.T) {
	want := map[string]Rate{
		"opus":   {InPerMTok: 15, OutPerMTok: 75},
		"haiku":  {InPerMTok: 0.8, OutPerMTok: 4},
		"fable":  {InPerMTok: 25, OutPerMTok: 125},
		"sonnet": {InPerMTok: 3, OutPerMTok: 15},
	}
	for tier, w := range want {
		got, ok := ClaudeRate(tier)
		if !ok {
			t.Errorf("ClaudeRate(%q): not found", tier)
			continue
		}
		if got != w {
			t.Errorf("ClaudeRate(%q) = %+v, want %+v", tier, got, w)
		}
	}
	if len(Claude) != len(want) {
		t.Errorf("Claude has %d tiers, want %d — a new tier needs consumer coverage guards updated", len(Claude), len(want))
	}
}

// TestClaudeRateUnknown confirms an unknown tier is reported missing (never a
// silent zero Rate).
func TestClaudeRateUnknown(t *testing.T) {
	if _, ok := ClaudeRate("gpt-4"); ok {
		t.Error("ClaudeRate(gpt-4) reported found")
	}
}

// TestClaudeFallbackRate confirms the fallback tier resolves and matches
// sonnet's rate — the mid-tier default every pre-consolidation table used for
// an unrecognized model id.
func TestClaudeFallbackRate(t *testing.T) {
	if ClaudeFallbackTier != "sonnet" {
		t.Fatalf("ClaudeFallbackTier = %q, want sonnet", ClaudeFallbackTier)
	}
	if got := ClaudeFallbackRate(); got != (Rate{InPerMTok: 3, OutPerMTok: 15}) {
		t.Errorf("ClaudeFallbackRate() = %+v, want sonnet rate", got)
	}
}

// TestCacheMultipliers pins the two cache-TTL write tiers plus the read
// multiplier — the derivation constants internal/quota and internal/anthro
// apply to the shared base.
func TestCacheMultipliers(t *testing.T) {
	if CacheReadMultiplier != 0.1 {
		t.Errorf("CacheReadMultiplier = %v, want 0.1", CacheReadMultiplier)
	}
	if CacheWrite5MinMultiplier != 1.25 {
		t.Errorf("CacheWrite5MinMultiplier = %v, want 1.25", CacheWrite5MinMultiplier)
	}
	if CacheWrite1HourMultiplier != 2.0 {
		t.Errorf("CacheWrite1HourMultiplier = %v, want 2.0", CacheWrite1HourMultiplier)
	}
}
