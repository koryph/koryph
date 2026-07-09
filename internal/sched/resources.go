// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package sched

import (
	"github.com/koryph/koryph/internal/beads"
)

// ResourcesFor derives the external resource kinds an issue declares, one
// res:<kind> label per kind (design docs/designs/2026-07-resource-governor.md
// L1, koryph-4ql.2). It is a SIBLING of FootprintFor, not a contributor to
// it: res:* labels never feed FootprintFor's write-token derivation and
// FootprintFor never looks at res:* labels, so a bead whose only labels are
// res:* still falls through to the TokenUnknown catch-all footprint (pinned
// by TestResourcesForFootprintNonInteraction in wave_test.go). Resources are
// deliberately NOT nested under fp: (e.g. fp:res:kind-cluster would silently
// become the verbatim write token "res:kind-cluster" under FootprintFor's
// "any fp:<x> is a write token" rule) — counted capacity (design L2/L4) and
// binary RW exclusion (koryph-2im.1) are different admission dimensions on
// purpose. Mantra: footprints protect the merge; resources protect the
// machine.
//
// <kind> must match the charset [a-z0-9-]+ (lowercase letters, digits,
// hyphens — the same opaque-token shape area/fp tokens use). A malformed
// value (empty after the "res:" prefix, upper-case, or containing any other
// character) is silently dropped rather than surfaced as a parse error: a
// mistyped res: label is planning-time noise that the koryph-plan/
// koryph-replan guidance (L6) fixes at authoring time, not a correctness
// signal the scheduler must halt the wave on — failing open here matches
// I6's "defer, don't error" posture one level up in the design. Values are
// deduped and sorted, exactly like FootprintFor's tokens, so wave output and
// deferral reasons are deterministic across runs.
func ResourcesFor(issue beads.Issue) []string {
	var kinds []string
	for _, v := range issue.LabelValues("res:") {
		if !isResKind(v) {
			continue
		}
		kinds = append(kinds, v)
	}
	if len(kinds) == 0 {
		// Explicit nil (not dedupeSort's non-nil-but-empty result) so a bead
		// with no res:* labels — or only malformed ones — is indistinguishable
		// from the zero value, matching Opts' nil-map fast path (L1 bullet 5).
		return nil
	}
	return dedupeSort(kinds)
}

// isResKind reports whether v is a well-formed res:<kind> value: one or more
// of [a-z0-9-]. Anything else (uppercase, punctuation, empty) is malformed —
// see ResourcesFor's doc comment for why it is dropped rather than rejected.
func isResKind(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return false
		}
	}
	return true
}
