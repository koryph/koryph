// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/sched"
)

// TestDispatchModePrecedence exercises dispatchMode's three-way resolution
// order (koryph-2im.3, design L1): the --dispatch-mode run flag wins over the
// project config's dispatch_mode, which wins over the "rolling" default
// (koryph-2im.8: rolling became the default after the 2026-07-03 burn-in).
func TestDispatchModePrecedence(t *testing.T) {
	cases := []struct {
		name    string
		optMode string
		cfgMode string
		want    string
	}{
		{"flag overrides config", "rolling", "wave", "rolling"},
		{"config used when flag unset", "", "rolling", "rolling"},
		{"default when nothing set", "", "", "rolling"},
		{"flag wins even against an explicit wave config", "rolling", "wave", "rolling"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &runner{
				opts: Options{DispatchMode: tc.optMode},
				cfg:  &project.Config{DispatchMode: tc.cfgMode},
			}
			if got := r.dispatchMode(); got != tc.want {
				t.Errorf("dispatchMode() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestRunRejectsInvalidDispatchModeFlag proves Run() fails closed (ExitUsage)
// on a garbage --dispatch-mode value, before touching the registry/ledger —
// mirroring the invalid dispatch_mode config-load error (see
// project.TestConfig_DispatchModeValidation) at the flag layer.
func TestRunRejectsInvalidDispatchModeFlag(t *testing.T) {
	newFixture(t, fixOpts{})
	opts := baseOptions(nil)
	opts.DispatchMode = "continuous"

	got, err := Run(context.Background(), opts)
	if err == nil {
		t.Fatal("Run succeeded despite an invalid --dispatch-mode value")
	}
	if got.Code != ExitUsage {
		t.Errorf("Code = %d, want %d", got.Code, ExitUsage)
	}
	if !strings.Contains(err.Error(), "wave|rolling") {
		t.Errorf("error %q does not name the accepted values", err)
	}
}

// TestActiveFootprintsPrefersPersistedSlotFootprint proves the koryph-2im.3
// L2-persistence wiring: when a non-terminal slot carries a persisted
// Footprint, activeFootprints must use it AS-IS rather than recomputing from
// the bead's current labels — even when those labels have since changed (a
// relabel mid-run must not retroactively alter what a live slot is understood
// to conflict with).
func TestActiveFootprintsPrefersPersistedSlotFootprint(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	ctx := context.Background()

	// r.issues (checked before adapter.Show in issueFor) now disagrees with
	// the persisted footprint — if activeFootprints ever fell back to
	// recomputing, it would see fp:go:other here instead.
	r.issues["running-1"] = beads.Issue{ID: "running-1", Labels: []string{"fp:go:other"}}
	persisted := &sched.Footprint{Writes: []string{"go:engine"}}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "running-1", BeadID: "running-1", Status: ledger.SlotRunning,
		Footprint: persisted,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	active := r.activeIDs()
	got := r.activeFootprints(ctx, active)["running-1"]
	if len(got.Writes) != 1 || got.Writes[0] != "go:engine" {
		t.Fatalf("activeFootprints[running-1] = %+v, want the PERSISTED footprint [go:engine], not a recompute from current labels", got)
	}

	// And a slot with no persisted footprint (nil — the pre-koryph-2im.3 or
	// legacy-ledger case) still falls back to the recompute chain exactly as
	// before.
	r.issues["running-2"] = beads.Issue{ID: "running-2", Labels: []string{"fp:go:legacy"}}
	if err := r.store.SetSlot(r.run, &ledger.Slot{
		PhaseID: "running-2", BeadID: "running-2", Status: ledger.SlotRunning,
	}); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}
	active = r.activeIDs()
	got2 := r.activeFootprints(ctx, active)["running-2"]
	if len(got2.Writes) != 1 || got2.Writes[0] != "go:legacy" {
		t.Fatalf("activeFootprints[running-2] (no persisted footprint) = %+v, want recompute-from-labels [go:legacy]", got2)
	}
}
