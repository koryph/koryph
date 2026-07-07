// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// TestDispatchHoldoutArmStampsDirect is the koryph-3l1.3 end-to-end
// acceptance test for the always-holdout edge case (Holdout=1): dispatchBead
// must stamp the ledger slot and manifest with ProxyID=="" (direct) even
// though agent_proxy is configured, and ProxyConfigured=true so the
// experiment report can still count this bead as belonging to the holdout
// arm rather than mistaking it for "no proxy configured at all."
func TestDispatchHoldoutArmStampsDirect(t *testing.T) {
	one := 1.0
	f := newFixture(t, fixOpts{
		agentProxy: &registry.AgentProxy{BaseURL: "http://127.0.0.1:8787", Pin: "v1", Holdout: &one},
	})
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 {
		t.Fatalf("Dispatched = %d, want 1", got.Dispatched)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	if sl.ProxyID != "" {
		t.Errorf("holdout arm: sl.ProxyID = %q, want \"\" (direct)", sl.ProxyID)
	}
	if !sl.ProxyConfigured {
		t.Error("holdout arm: sl.ProxyConfigured = false, want true (a proxy IS configured for this project)")
	}

	m, err := store.LoadManifest(run.RunID, "tb1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.ProxyID != "" {
		t.Errorf("holdout arm: manifest ProxyID = %q, want \"\"", m.ProxyID)
	}
}

// TestDispatchProxiedArmStampsProxyID is the koryph-3l1.3 end-to-end
// acceptance test for the always-proxied edge case (Holdout=0): dispatchBead
// must stamp the ledger slot and manifest with the CONFIGURED proxy identity.
func TestDispatchProxiedArmStampsProxyID(t *testing.T) {
	zero := 0.0
	proxy := &registry.AgentProxy{BaseURL: "http://127.0.0.1:8787", Pin: "v1", Holdout: &zero}
	f := newFixture(t, fixOpts{agentProxy: proxy})
	var out bytes.Buffer
	ctx := context.Background()

	got, err := Run(ctx, baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 {
		t.Fatalf("Dispatched = %d, want 1", got.Dispatched)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	wantProxyID := proxy.ID()
	if sl.ProxyID != wantProxyID {
		t.Errorf("proxied arm: sl.ProxyID = %q, want %q", sl.ProxyID, wantProxyID)
	}
	if !sl.ProxyConfigured {
		t.Error("proxied arm: sl.ProxyConfigured = false, want true")
	}

	m, err := store.LoadManifest(run.RunID, "tb1")
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if m.ProxyID != wantProxyID {
		t.Errorf("proxied arm: manifest ProxyID = %q, want %q", m.ProxyID, wantProxyID)
	}
}

// TestDispatchNoProxyConfiguredLeavesProxyConfiguredFalse is the control:
// with no agent_proxy at all (the default fixture), both ProxyID and
// ProxyConfigured stay zero-valued — the pre-koryph-3l1.3 behavior,
// unaffected by this bead's changes.
func TestDispatchNoProxyConfiguredLeavesProxyConfiguredFalse(t *testing.T) {
	f := newFixture(t, fixOpts{})
	var out bytes.Buffer
	ctx := context.Background()

	if _, err := Run(ctx, baseOptions(&out)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	if sl.ProxyID != "" || sl.ProxyConfigured {
		t.Errorf("no agent_proxy: sl = {ProxyID:%q ProxyConfigured:%v}, want {\"\" false}", sl.ProxyID, sl.ProxyConfigured)
	}
}
