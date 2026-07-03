// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
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
