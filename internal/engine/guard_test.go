// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/runtimetest"
)

// calibrateStopped writes a calibrated governor config whose usage source is
// unavailable in the fixture (no ccusage, KORYPH_NO_NPX=1, no transcript
// tree under the fake config dir), so the governor reads hard STOP.
func calibrateStopped(t *testing.T, account string) {
	t.Helper()
	cfg := quota.DefaultConfig(account)
	cfg.WindowCeilingUSD = 1
	cfg.WeeklyCeilingUSD = 1
	if err := quota.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
}

// Enforced (default): a calibrated governor at stop pauses the run before
// any dispatch.
func TestRunBillingGuardEnforcedStops(t *testing.T) {
	newFixture(t, fixOpts{})
	calibrateStopped(t, "work")

	var out bytes.Buffer
	oc, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if oc.Merged != 0 || oc.Dispatched != 0 {
		t.Fatalf("expected no dispatch under enforced stop, got %+v\n%s", oc, out.String())
	}
	if !strings.HasPrefix(oc.Reason, "quota-") {
		t.Fatalf("expected quota-* reason, got %q", oc.Reason)
	}
}

// --no-billing-guard: same stopped governor, run proceeds to a full merge on
// subscription billing.
func TestRunBillingGuardFlagDisablesThrottling(t *testing.T) {
	newFixture(t, fixOpts{})
	calibrateStopped(t, "work")

	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.NoBillingGuard = true
	oc, err := Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if oc.Merged != 1 {
		t.Fatalf("expected 1 merge with --no-billing-guard, got %+v\n%s", oc, out.String())
	}
	if !strings.Contains(out.String(), "billing guard ADVISORY (--no-billing-guard)") {
		t.Fatalf("expected advisory log line, got:\n%s", out.String())
	}
}

// Registry billing_guard=advisory: durable per-project disable, same effect
// without the run flag.
func TestRunBillingGuardRegistryAdvisory(t *testing.T) {
	f := newFixture(t, fixOpts{})
	calibrateStopped(t, "work")

	ctx := context.Background()
	st := registry.NewStoreAt(f.home)
	rec, err := st.Get("proj")
	if err != nil {
		t.Fatal(err)
	}
	rec.BillingGuard = "advisory"
	if err := st.Save(ctx, rec); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	oc, err := Run(ctx, baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if oc.Merged != 1 {
		t.Fatalf("expected 1 merge with billing_guard=advisory, got %+v\n%s", oc, out.String())
	}
	if !strings.Contains(out.String(), "billing guard ADVISORY (project billing_guard=advisory)") {
		t.Fatalf("expected registry advisory log line, got:\n%s", out.String())
	}
}

// TestGuardModeAdvisoryWhenRuntimeHasNoUsageSource is the koryph-v8u.5 quota-
// gating unit test: a runtime whose Capabilities().UsageSource is false must
// force the billing guard advisory — measured-if-possible, never blocking —
// even when the governor itself is calibrated, since there is no fail-closed
// usage source to enforce against. guardMode is exercised directly (no full
// Run()) so this stays a narrow, fast unit test of the capability gate.
func TestGuardModeAdvisoryWhenRuntimeHasNoUsageSource(t *testing.T) {
	r := &runner{
		rec: &registry.Record{},
		rt:  runtimetest.Stub{Caps: runtime.Capabilities{UsageSource: false}},
	}
	advisory, why := r.guardMode(true /* calibrated */)
	if !advisory {
		t.Fatal("guardMode advisory = false, want true when the runtime has no usage source")
	}
	if !strings.Contains(why, "usage source") {
		t.Errorf("why = %q, want it to mention the missing usage source", why)
	}
}

// TestGuardModeEnforcedWhenRuntimeHasUsageSource pins the no-op case: a
// runtime that DOES report a usage source (claude, always, today) must not
// trip the new capability gate — enforcement stays exactly as it was before
// koryph-v8u.5 once the governor is calibrated.
func TestGuardModeEnforcedWhenRuntimeHasUsageSource(t *testing.T) {
	r := &runner{
		rec: &registry.Record{},
		rt:  runtimetest.Stub{Caps: runtime.Capabilities{UsageSource: true}},
	}
	if advisory, why := r.guardMode(true /* calibrated */); advisory {
		t.Fatalf("guardMode advisory = true (%q), want false: calibrated + a runtime with a usage source must enforce", why)
	}
}

// TestRunBillingGuardConfigToggleAdvisory verifies the live billing-guard toggle
// (koryph-i25): writing GuardMode=advisory into the quota config — the same
// path `koryph quota guard --account A advisory` takes — lets a run proceed
// through a stopped governor without the --no-billing-guard flag. The engine
// re-reads the config at every wave boundary, so this is equivalent to
// flipping the switch while the loop is running.
func TestRunBillingGuardConfigToggleAdvisory(t *testing.T) {
	newFixture(t, fixOpts{})
	// Calibrate to hard-STOP so enforcement would normally block dispatch.
	calibrateStopped(t, "work")

	// Write the live toggle — advisory guard mode, no expiry.
	if _, err := quota.SetGuardMode("work", quota.GuardModeAdvisory, time.Time{}); err != nil {
		t.Fatalf("SetGuardMode: %v", err)
	}

	var out bytes.Buffer
	oc, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if oc.Merged != 1 {
		t.Fatalf("expected 1 merge with config guard advisory, got %+v\n%s", oc, out.String())
	}
	if !strings.Contains(out.String(), "billing guard ADVISORY") {
		t.Fatalf("expected advisory log line, got:\n%s", out.String())
	}
	// Confirm the log mentions the live-toggle, not a flag or registry setting.
	if !strings.Contains(out.String(), "live toggle") {
		t.Fatalf("expected 'live toggle' in advisory reason, got:\n%s", out.String())
	}
}

// TestRunBillingGuardConfigToggleExpired verifies that an expired --until
// override is treated as enforced: the engine sees GuardUntil in the past and
// falls back to enforcement, causing a stop like any other calibrated-stopped
// governor. (koryph-i25)
func TestRunBillingGuardConfigToggleExpired(t *testing.T) {
	newFixture(t, fixOpts{})
	calibrateStopped(t, "work")

	// Write the toggle with an already-expired until timestamp.
	expired := time.Now().Add(-1 * time.Hour)
	if _, err := quota.SetGuardMode("work", quota.GuardModeAdvisory, expired); err != nil {
		t.Fatalf("SetGuardMode: %v", err)
	}

	var out bytes.Buffer
	oc, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	// An expired toggle must NOT let the run through — back to enforced.
	if oc.Merged != 0 || oc.Dispatched != 0 {
		t.Fatalf("expected no dispatch with expired guard toggle, got %+v\n%s", oc, out.String())
	}
	if !strings.HasPrefix(oc.Reason, "quota-") {
		t.Fatalf("expected quota-* reason after expiry, got %q", oc.Reason)
	}
}

// TestGuardOffDoesNotAffectRateLimitGoverning is the koryph-i25 invariant
// regression test: disabling the billing guard (via NoBillingGuard flag — the
// same path as the live toggle) must NEVER suppress rate-limit governing.
// When a bead dies with a 429 the AIMD cap must still be halved and the
// RateLimitEvents counter must still increment, regardless of guard state.
//
// This pins the INVARIANT stated in the bead's operator notes:
//
//	"disabling the billing guard — by flag OR by the live toggle this bead
//	adds — must NEVER affect rate-limit governing (internal/govern AIMD
//	caps, 429 halving, settle windows, circuit breakers)."
func TestGuardOffDoesNotAffectRateLimitGoverning(t *testing.T) {
	f := newFixture(t, fixOpts{})
	// Point the fake claude at the rate-limited script.
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, rateLimitedClaudeScript, 0o755)

	// Calibrate to STOP so the guard is actually doing something when disabled.
	calibrateStopped(t, "work")

	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.NoBillingGuard = true // guard disabled — billing-guard advisory path
	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// With guard off the run proceeds, but the bead is rate-limited → blocked.
	if got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 1 blocked (rate-limited)", got)
	}

	// The AIMD governor must have seen the 429 events — halving and event
	// counting must be completely unaffected by the billing guard state.
	gs := govern.NewStore()
	status, err := gs.AIMDStatus("")
	if err != nil {
		t.Fatalf("AIMDStatus: %v", err)
	}
	wantEvents := rateLimitedRequeueBudget + 1 // initial + each requeue
	if status.RateLimitEvents != wantEvents {
		t.Errorf("governor RateLimitEvents = %d, want %d — "+
			"rate-limit governing must not be affected by the billing guard state",
			status.RateLimitEvents, wantEvents)
	}

	_ = f
}
