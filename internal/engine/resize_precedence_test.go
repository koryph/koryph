// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// koryph-bzf follow-up: a persisted resize override survives across runs, so a
// leftover `koryph resize` must not silently pin a NEW run to that width when the
// operator passed an explicit --max. resizeApplies distinguishes a live resize of
// the current run (SetAt differs from the run-start snapshot) from one inherited
// across runs (SetAt unchanged) — the live one wins unconditionally, the
// inherited one yields to an explicit --max.
func TestResizeApplies(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	const startupSetAt = "2026-07-13T10:00:00Z" // override present when the run started
	const laterSetAt = "2026-07-13T11:00:00Z"   // a fresh `koryph resize` during the run

	inherited := ledger.ResizeOverride{Max: 6, SetAt: startupSetAt}
	live := ledger.ResizeOverride{Max: 6, SetAt: laterSetAt}

	cases := []struct {
		name       string
		startupSet string
		optsMax    int
		ov         ledger.ResizeOverride
		want       bool
	}{
		{"explicit --max ignores an inherited (prior-run) resize", startupSetAt, 2, inherited, false},
		{"no explicit --max: inherited resize persists across runs", startupSetAt, 0, inherited, true},
		{"explicit --max still yields to a resize changed during this run", startupSetAt, 2, live, true},
		{"a resize appearing mid-run (none at start) is live", "", 2, live, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r.startupResizeSetAt = tc.startupSet
			r.opts.Max = tc.optsMax
			if got := r.resizeApplies(tc.ov); got != tc.want {
				t.Errorf("resizeApplies(startup=%q, optsMax=%d, setAt=%q) = %v, want %v",
					tc.startupSet, tc.optsMax, tc.ov.SetAt, got, tc.want)
			}
		})
	}
}
