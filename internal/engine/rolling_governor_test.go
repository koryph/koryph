// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// newRollingRunner builds a minimal api-key-mode *runner whose governor reads
// the rolling-$ ladder (koryph-i3b.9). The account's RollingCeilingUSD is
// persisted to the quota dir under KORYPH_HOME so governor()'s LoadConfig
// picks it up, exactly as a live run would after `koryph quota set-rolling`.
func newRollingRunner(t *testing.T, acct string, ceilingUSD float64, width int) *runner {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
	if ceilingUSD > 0 {
		if _, err := quota.SetRollingCeiling(acct, ceilingUSD); err != nil {
			t.Fatalf("SetRollingCeiling: %v", err)
		}
	}
	store := ledger.NewStore(t.TempDir())
	run, err := store.NewRun("proj", "bd", EngineVersion)
	if err != nil {
		t.Fatalf("NewRun: %v", err)
	}
	return &runner{
		opts:       Options{ProjectID: "proj", Out: &bytes.Buffer{}},
		rec:        &registry.Record{AccountProfile: acct},
		authMode:   registry.AuthModeAPIKey,
		credential: "sk-rolling-test",
		rt:         runtimetest.Stub{Caps: runtime.Capabilities{UsageSource: true}},
		store:      store,
		run:        run,
		width:      width,
	}
}

// setSpend pins the run's tracked pay-per-token spend to usd via a single
// settled slot — projectedRunCostUSD sums slot CostUSD, and a settled
// (SlotMerged) slot contributes no in-flight estimate, so spend == usd exactly.
func setSpend(r *runner, usd float64) {
	r.run.Slots = map[string]*ledger.Slot{
		"settled": {PhaseID: "settled", Status: ledger.SlotMerged, CostUSD: usd},
	}
}

// TestGovernorRollingLadderFiresForAPIKeyAccount is the koryph-i3b.9 acceptance
// test (design §7, AC6 clause 2): a first-class api-key account with
// RollingCeilingUSD set must drive the warn -> throttle -> graceful-stop ->
// hard-stop ladder off spent$/ceiling$ through the SAME governorGate the
// subscription window drives — proving StateForAuthMode/ScaleSlotsForAuthMode
// are actually wired in (they had no non-test caller before this bead).
//
// Default ladder (quota.Default*Fraction): warn 0.90, throttle 0.94,
// graceful-stop 0.97, hard-stop 0.99. Ceiling $100 → dollars map 1:1 to
// percent, so $91/$95/$98/$100 land one rung each.
func TestGovernorRollingLadderFiresForAPIKeyAccount(t *testing.T) {
	const ceiling = 100.0
	const width = 8

	t.Run("below warn stays OK and dispatches at full width", func(t *testing.T) {
		r := newRollingRunner(t, "rolling-ok", ceiling, width)
		setSpend(r, 50.0)
		g := r.governorGate(context.Background())
		if g.level != quota.LevelOK {
			t.Fatalf("level = %v, want %v ($50/$100 = 50%%)", g.level, quota.LevelOK)
		}
		if !g.calibrated {
			t.Fatalf("calibrated = false, want true (rolling ceiling configured)")
		}
		if !g.allowDispatch {
			t.Fatalf("allowDispatch = false, want true below warn")
		}
		if g.width != width {
			t.Fatalf("width = %d, want %d (no scaling below throttle)", g.width, width)
		}
		if r.billing != account.BillingAPIKey {
			t.Fatalf("billing = %q, want %q (api-key account bills per-token)", r.billing, account.BillingAPIKey)
		}
	})

	t.Run("warn rung", func(t *testing.T) {
		r := newRollingRunner(t, "rolling-warn", ceiling, width)
		setSpend(r, 91.0)
		g := r.governorGate(context.Background())
		if g.level != quota.LevelWarn {
			t.Fatalf("level = %v, want %v ($91/$100 = 91%%)", g.level, quota.LevelWarn)
		}
		if !g.allowDispatch || g.width != width {
			t.Fatalf("warn should not block or scale: allowDispatch=%v width=%d", g.allowDispatch, g.width)
		}
	})

	t.Run("throttle rung scales slots down", func(t *testing.T) {
		r := newRollingRunner(t, "rolling-throttle", ceiling, width)
		setSpend(r, 95.0)
		g := r.governorGate(context.Background())
		if g.level != quota.LevelThrottle {
			t.Fatalf("level = %v, want %v ($95/$100 = 95%%)", g.level, quota.LevelThrottle)
		}
		if !g.allowDispatch {
			t.Fatalf("throttle should still allow dispatch (scaled), got allowDispatch=false")
		}
		if g.width >= width {
			t.Fatalf("width = %d, want < %d (throttle scales slots off spent$/ceiling$)", g.width, width)
		}
	})

	t.Run("graceful-stop rung halts new dispatch", func(t *testing.T) {
		r := newRollingRunner(t, "rolling-drain", ceiling, width)
		setSpend(r, 98.0)
		g := r.governorGate(context.Background())
		if g.level != quota.LevelDrain {
			t.Fatalf("level = %v, want %v ($98/$100 = 98%%)", g.level, quota.LevelDrain)
		}
		if g.allowDispatch {
			t.Fatalf("graceful-stop must set allowDispatch=false")
		}
	})

	t.Run("hard-stop rung parks the run", func(t *testing.T) {
		r := newRollingRunner(t, "rolling-stop", ceiling, width)
		setSpend(r, 100.0)
		g := r.governorGate(context.Background())
		if g.level != quota.LevelStop {
			t.Fatalf("level = %v, want %v ($100/$100 = 100%%)", g.level, quota.LevelStop)
		}
		if !g.paused {
			t.Fatalf("hard-stop must park a first-class api-key run (g.paused=false)")
		}
		if r.run.Status != ledger.RunHardStopQuota {
			t.Fatalf("run.Status = %q, want %q (hard-stop parks the run)", r.run.Status, ledger.RunHardStopQuota)
		}
		if r.billing != account.BillingAPIKey {
			t.Fatalf("billing = %q, want %q — first-class api-key still bills per-token even at hard-stop", r.billing, account.BillingAPIKey)
		}
	})
}

// TestGovernorRollingUnconfiguredStaysAdvisory pins AC6's first clause: an
// api-key account with NO rolling-$ ceiling stays advisory — measured, never
// blocking — the api-key analogue of an uncalibrated subscription account.
func TestGovernorRollingUnconfiguredStaysAdvisory(t *testing.T) {
	r := newRollingRunner(t, "rolling-none", 0, 8)
	setSpend(r, 1_000_000.0) // absurd spend must NOT matter with no ceiling
	g := r.governorGate(context.Background())
	if g.calibrated {
		t.Fatalf("calibrated = true, want false (no rolling ceiling configured)")
	}
	if g.level != quota.LevelOK {
		t.Fatalf("level = %v, want %v (advisory: uncalibrated does not enforce)", g.level, quota.LevelOK)
	}
	if !g.allowDispatch {
		t.Fatalf("allowDispatch = false, want true (advisory must never block)")
	}
	if g.width != 8 {
		t.Fatalf("width = %d, want 8 (advisory must not scale)", g.width)
	}
	if r.billing != account.BillingAPIKey {
		t.Fatalf("billing = %q, want %q (first-class api-key bills per-token even when advisory)", r.billing, account.BillingAPIKey)
	}
}
