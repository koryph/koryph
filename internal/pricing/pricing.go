// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package pricing is the single source of truth for Anthropic Claude per-model
// list prices (koryph-fiv finding #5). Before it, three tables encoded Claude
// pricing independently — internal/anthro's pre-submit estimator, and
// internal/quota's usage-accounting and per-dispatch estimator seeds — and had
// silently drifted: anthro priced Opus at $5/$25 per MTok while quota (and the
// real published list) priced it at $15/$75. Every consumer now derives its
// base rates from this package, so an Anthropic price change is a one-line edit
// here, never a three-way reconciliation.
//
// Only the BASE input/output rates live here. Consumers still apply their own
// context-specific cache-TTL multiplier (see the CacheWrite* constants): the
// batch/message client (internal/anthro) writes 1-hour ephemeral cache (2x
// input), while transcript accounting (internal/quota) prices the 5-minute
// default (1.25x input). Those multipliers legitimately differ by context and
// are NOT part of the shared base — but they are named here so the two TTL
// tiers have one documented home too.
package pricing

// Rate is one Claude model's base list price in USD per million tokens (MTok),
// separate from any cache-read/-write multiplier a consumer layers on top.
type Rate struct {
	InPerMTok  float64
	OutPerMTok float64
}

// Tier pairs a canonical Claude tier name with its base Rate. The name doubles
// as the substring internal/quota matches against a concrete model id (e.g.
// "claude-opus-4-20250514" contains "opus").
type Tier struct {
	Name string
	Rate Rate
}

// Claude is the ordered canonical Claude price list. Order is significant for
// substring matching (internal/quota tries these in sequence, first match
// wins) and is preserved from that package's pre-consolidation rule order:
// opus, haiku, fable, sonnet. Values are the real published Anthropic list
// prices (USD / MTok).
var Claude = []Tier{
	{"opus", Rate{InPerMTok: 15, OutPerMTok: 75}},
	{"haiku", Rate{InPerMTok: 0.8, OutPerMTok: 4}},
	{"fable", Rate{InPerMTok: 25, OutPerMTok: 125}},
	{"sonnet", Rate{InPerMTok: 3, OutPerMTok: 15}},
}

// ClaudeFallbackTier is the tier whose rate prices a model id that matches no
// entry in Claude — sonnet, the mid-tier default every pre-consolidation table
// already fell back to.
const ClaudeFallbackTier = "sonnet"

// Cache-TTL multipliers, expressed as a fraction of the input rate. Reads are
// TTL-independent; writes are priced by their ephemeral TTL.
const (
	// CacheReadMultiplier prices a cache read at 0.1x the input rate.
	CacheReadMultiplier = 0.1
	// CacheWrite5MinMultiplier prices a 5-minute ephemeral cache write at 1.25x
	// the input rate (the Anthropic default TTL; what transcript accounting
	// sees).
	CacheWrite5MinMultiplier = 1.25
	// CacheWrite1HourMultiplier prices a 1-hour ephemeral cache write at 2x the
	// input rate (what internal/anthro's batch/message client requests).
	CacheWrite1HourMultiplier = 2.0
)

// ClaudeRate returns the base Rate for a canonical tier name (exact match, not
// substring), and whether it is known. The lookup is over Claude's small fixed
// list, so a linear scan is both simplest and fastest.
func ClaudeRate(tier string) (Rate, bool) {
	for _, t := range Claude {
		if t.Name == tier {
			return t.Rate, true
		}
	}
	return Rate{}, false
}

// ClaudeFallbackRate is the base Rate for ClaudeFallbackTier. It panics only if
// the fallback tier is ever removed from Claude — a programming error a test
// (and the first call) catches immediately.
func ClaudeFallbackRate() Rate {
	r, ok := ClaudeRate(ClaudeFallbackTier)
	if !ok {
		panic("pricing: fallback tier " + ClaudeFallbackTier + " missing from Claude table")
	}
	return r
}
