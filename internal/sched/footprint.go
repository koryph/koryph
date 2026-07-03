// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sched

import (
	"sort"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

// TokenUnknown is the catch-all footprint token for issues whose blast radius
// cannot be inferred. It conflicts with every other unknown, serializing them.
const TokenUnknown = "domain:unknown"

// FootprintFor derives an issue's conflict footprint. Precedence:
//  1. explicit fp:* labels (used as-is, "fp:" stripped);
//  2. else area:* labels resolved through cfg.AreaMap;
//  3. else the catch-all TokenUnknown.
//
// Tokens are de-duplicated and sorted.
func FootprintFor(issue beads.Issue, cfg *project.Config) Footprint {
	var tokens []string

	if fps := issue.LabelValues("fp:"); len(fps) > 0 {
		tokens = append(tokens, fps...)
	} else {
		for _, area := range issue.LabelValues("area:") {
			if mapped, ok := lookupArea(cfg, area); ok {
				tokens = append(tokens, mapped...)
			}
		}
	}

	if len(tokens) == 0 {
		tokens = []string{TokenUnknown}
	}
	return Footprint{Tokens: dedupeSort(tokens)}
}

// lookupArea resolves an area token against cfg.AreaMap, accepting either the
// bare token ("api") or the fully-qualified label ("area:api") as the key.
func lookupArea(cfg *project.Config, area string) ([]string, bool) {
	if cfg == nil || cfg.AreaMap == nil {
		return nil, false
	}
	if toks, ok := cfg.AreaMap[area]; ok {
		return toks, true
	}
	if toks, ok := cfg.AreaMap["area:"+area]; ok {
		return toks, true
	}
	return nil, false
}

// Conflicts reports whether two footprints share any token.
func Conflicts(a, b Footprint) bool {
	set := make(map[string]bool, len(a.Tokens))
	for _, t := range a.Tokens {
		set[t] = true
	}
	for _, t := range b.Tokens {
		if set[t] {
			return true
		}
	}
	return false
}

// dedupeSort returns the unique, non-empty tokens of in, sorted.
func dedupeSort(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, t := range in {
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}
