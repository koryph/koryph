// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sched

import (
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/project"
)

// TokenUnknown is the catch-all footprint token for issues whose blast radius
// cannot be inferred. It is always a write token, so it conflicts with every
// other unknown (and with itself), serializing them (koryph-2im.1).
const TokenUnknown = "domain:unknown"

// FootprintFor derives an issue's RW conflict footprint. Precedence
// (backward compatible with the pre-L4 write-only grammar):
//  1. fp:read:<token> labels — read tokens (NEW, koryph-2im.1);
//  2. fp:<token> labels of any other suffix — write tokens, used as-is
//     ("fp:" stripped) — existing labels keep their existing (write) meaning;
//     fp:* labels of either flavor win outright over area:* (unchanged
//     precedence: a bead with any fp:* label ignores its area:* labels);
//  3. else area:* labels resolved through cfg.AreaMap — write tokens
//     (unchanged);
//  4. else the catch-all TokenUnknown, as a write.
//
// A token declared in both sets collapses to write-only: a write already
// excludes readers, so keeping it in Reads too would just be misleading.
// Each set is de-duplicated and sorted independently.
func FootprintFor(issue beads.Issue, cfg *project.Config) Footprint {
	var reads, writes []string

	if fps := issue.LabelValues("fp:"); len(fps) > 0 {
		for _, v := range fps {
			if tok, ok := strings.CutPrefix(v, "read:"); ok {
				reads = append(reads, tok)
			} else {
				writes = append(writes, v)
			}
		}
	} else {
		for _, area := range issue.LabelValues("area:") {
			if mapped, ok := lookupArea(cfg, area); ok {
				writes = append(writes, mapped...)
			}
		}
	}

	if len(reads) == 0 && len(writes) == 0 {
		writes = []string{TokenUnknown}
	}
	return newFootprint(reads, writes)
}

// newFootprint dedupes+sorts each set and drops any read token also present
// as a write (write-wins collapse — see FootprintFor).
func newFootprint(reads, writes []string) Footprint {
	w := dedupeSort(writes)
	wset := make(map[string]bool, len(w))
	for _, t := range w {
		wset[t] = true
	}
	var r []string
	for _, t := range dedupeSort(reads) {
		if !wset[t] {
			r = append(r, t)
		}
	}
	return Footprint{Reads: r, Writes: w}
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

// Conflicts reports whether a and b may run simultaneously without risking a
// clobber (koryph-2im.1, design L4): true iff some token is shared AND at
// least one side holds it as a write. Two readers of the same token never
// conflict (RWMutex semantics: concurrent reads cannot collide on a merge, so
// invariant I1 still holds) — only a writer excludes.
func Conflicts(a, b Footprint) bool {
	bAll := tokenSet(b.Reads, b.Writes)
	for _, t := range a.Writes {
		if bAll[t] {
			return true
		}
	}
	aAll := tokenSet(a.Reads, a.Writes)
	for _, t := range b.Writes {
		if aAll[t] {
			return true
		}
	}
	return false
}

// tokenSet unions one or more token slices into a membership set.
func tokenSet(sets ...[]string) map[string]bool {
	m := map[string]bool{}
	for _, s := range sets {
		for _, t := range s {
			m[t] = true
		}
	}
	return m
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
