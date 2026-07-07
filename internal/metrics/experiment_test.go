// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
)

// TestCollectExperimentEmpty verifies CollectExperiment on an empty store
// returns a well-formed, empty report, and RenderExperiment handles it
// gracefully.
func TestCollectExperimentEmpty(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}

	rep, err := CollectExperiment(store, "")
	if err != nil {
		t.Fatalf("CollectExperiment: %v", err)
	}
	if len(rep.Projects) != 0 {
		t.Errorf("Projects = %d, want 0", len(rep.Projects))
	}

	var buf bytes.Buffer
	RenderExperiment(rep, &buf)
	if !strings.Contains(buf.String(), "no experiment data") {
		t.Errorf("RenderExperiment empty: want 'no experiment data' message, got:\n%s", buf.String())
	}
}

// TestCollectExperimentNilReport verifies RenderExperiment is nil-safe.
func TestCollectExperimentNilReport(t *testing.T) {
	var buf bytes.Buffer
	RenderExperiment(nil, &buf) // must not panic
}

// TestCollectExperimentSkipsProjectsWithoutProxy proves a project with no
// agent_proxy configured is excluded entirely — there is no experiment
// running, so it must never appear as a (misleadingly) 100%-holdout project.
func TestCollectExperimentSkipsProjectsWithoutProxy(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	rec := &registry.Record{
		ProjectID:        "noproxy",
		Name:             "noproxy",
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(context.Background(), rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// A slot with ProxyConfigured=false — as every slot dispatched before
	// koryph-3l1.3, or for a project that has never configured agent_proxy,
	// unmarshals to.
	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "noproxy", Status: ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			"a": {PhaseID: "a", Model: "sonnet", Status: ledger.SlotMerged, CostUSD: 1.0,
				InputTokens: 100, OutputTokens: 50},
		},
	})

	rep, err := CollectExperiment(store, "")
	if err != nil {
		t.Fatalf("CollectExperiment: %v", err)
	}
	if len(rep.Projects) != 0 {
		t.Fatalf("Projects = %d, want 0 (no agent_proxy configured — nothing to compare)", len(rep.Projects))
	}
}

// TestCollectExperimentTwoArmSplit is the koryph-3l1.3 fixture-ledger
// acceptance test: a project with agent_proxy configured and slots stamped
// across both arms (ProxyConfigured=true, ProxyID either "" or the current
// proxy identity) produces a correct two-arm comparison — bead counts,
// tokens-per-bead, cache-hit ratio, requeue rate, blocking-review-finding
// rate, gate failures, and cost aggregates.
func TestCollectExperimentTwoArmSplit(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	holdout := 0.5
	rec := &registry.Record{
		ProjectID:        "exp",
		Name:             "exp",
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
		AgentProxy:       &registry.AgentProxy{BaseURL: "http://127.0.0.1:8787", Holdout: &holdout},
	}
	if err := store.Add(context.Background(), rec); err != nil {
		t.Fatalf("add: %v", err)
	}
	proxyID := rec.AgentProxy.ID()

	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "exp", Status: ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			// Proxied arm: two beads, one with a gate requeue and a blocking
			// review bounce.
			"p1": {
				PhaseID: "p1", Model: "sonnet", Status: ledger.SlotMerged,
				ProxyConfigured: true, ProxyID: proxyID,
				CostUSD: 1.0, InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 8000,
			},
			"p2": {
				PhaseID: "p2", Model: "sonnet", Status: ledger.SlotMerged,
				ProxyConfigured: true, ProxyID: proxyID,
				CostUSD: 2.0, InputTokens: 500, OutputTokens: 100, CacheReadTokens: 4000,
				GateRequeues: 1, ReviewIters: 1,
			},
			// Holdout arm: two beads, direct (ProxyID empty), one requeued
			// via rate-limit.
			"h1": {
				PhaseID: "h1", Model: "sonnet", Status: ledger.SlotMerged,
				ProxyConfigured: true, ProxyID: "",
				CostUSD: 1.5, InputTokens: 900, OutputTokens: 180, CacheReadTokens: 7000,
			},
			"h2": {
				PhaseID: "h2", Model: "opus", Status: ledger.SlotMerged,
				ProxyConfigured: true, ProxyID: "",
				CostUSD: 3.0, InputTokens: 600, OutputTokens: 150, CacheReadTokens: 3000,
				RateLimitRequeues: 1,
			},
			// Pre-experiment slot (ProxyConfigured=false): must be excluded
			// from both arms entirely.
			"legacy": {
				PhaseID: "legacy", Model: "sonnet", Status: ledger.SlotMerged,
				CostUSD: 9.0, InputTokens: 9000, OutputTokens: 900,
			},
			// Slot dispatched under a stale/rotated proxy identity: must be
			// excluded from both arms (belongs to neither the current
			// proxied population nor the holdout population).
			"stale": {
				PhaseID: "stale", Model: "sonnet", Status: ledger.SlotMerged,
				ProxyConfigured: true, ProxyID: "http://127.0.0.1:9999#old-pin",
				CostUSD: 4.0, InputTokens: 4000, OutputTokens: 400,
			},
		},
	})

	// Seed calibration for both arms so EstimatorBias/N surface too.
	cfg := quota.DefaultConfig(rec.AccountProfile)
	cfg.ErrorStats = map[string]*quota.ErrorStat{
		"sonnet:M":            {N: 4, Bias: 1.2, MAPE: 20},
		"sonnet:M@" + proxyID: {N: 2, Bias: 0.8, MAPE: 10},
	}
	if err := quota.SaveConfig(cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	rep, err := CollectExperiment(store, "")
	if err != nil {
		t.Fatalf("CollectExperiment: %v", err)
	}
	if len(rep.Projects) != 1 {
		t.Fatalf("Projects = %d, want 1", len(rep.Projects))
	}
	pe := rep.Projects[0]

	if pe.ProxyID != proxyID {
		t.Errorf("ProxyID = %q, want %q", pe.ProxyID, proxyID)
	}
	if pe.HoldoutFraction != 0.5 {
		t.Errorf("HoldoutFraction = %v, want 0.5", pe.HoldoutFraction)
	}
	if pe.CalibrationSlopeSeam == "" {
		t.Error("CalibrationSlopeSeam must document the unimplemented /usage seam, got empty")
	}

	// Proxied arm.
	if pe.Proxied.Beads != 2 {
		t.Errorf("Proxied.Beads = %d, want 2 (p1, p2; legacy/stale excluded)", pe.Proxied.Beads)
	}
	if pe.Proxied.CostUSD != 3.0 {
		t.Errorf("Proxied.CostUSD = %v, want 3.0 (1.0+2.0)", pe.Proxied.CostUSD)
	}
	if pe.Proxied.GateFailures != 1 {
		t.Errorf("Proxied.GateFailures = %d, want 1", pe.Proxied.GateFailures)
	}
	if pe.Proxied.BlockingReviewFindingRate != 0.5 {
		t.Errorf("Proxied.BlockingReviewFindingRate = %v, want 0.5 (1 of 2 beads)", pe.Proxied.BlockingReviewFindingRate)
	}
	if pe.Proxied.RequeueRate != 0.5 {
		t.Errorf("Proxied.RequeueRate = %v, want 0.5 (p2's gate requeue)", pe.Proxied.RequeueRate)
	}
	wantProxiedTotal := int64(1000+200+8000) + int64(500+100+4000)
	if pe.Proxied.Composition.Total != wantProxiedTotal {
		t.Errorf("Proxied.Composition.Total = %d, want %d", pe.Proxied.Composition.Total, wantProxiedTotal)
	}
	if pe.Proxied.EstimatorN != 2 || pe.Proxied.EstimatorBias != 0.8 {
		t.Errorf("Proxied estimator = N=%d Bias=%v, want N=2 Bias=0.8", pe.Proxied.EstimatorN, pe.Proxied.EstimatorBias)
	}

	// Holdout arm.
	if pe.Holdout.Beads != 2 {
		t.Errorf("Holdout.Beads = %d, want 2 (h1, h2)", pe.Holdout.Beads)
	}
	if pe.Holdout.CostUSD != 4.5 {
		t.Errorf("Holdout.CostUSD = %v, want 4.5 (1.5+3.0)", pe.Holdout.CostUSD)
	}
	if pe.Holdout.GateFailures != 0 {
		t.Errorf("Holdout.GateFailures = %d, want 0", pe.Holdout.GateFailures)
	}
	if pe.Holdout.RequeueRate != 0.5 {
		t.Errorf("Holdout.RequeueRate = %v, want 0.5 (h2's rate-limit requeue)", pe.Holdout.RequeueRate)
	}
	if len(pe.Holdout.ByTier) != 2 {
		t.Errorf("Holdout.ByTier = %d tiers, want 2 (sonnet, opus)", len(pe.Holdout.ByTier))
	}
	if pe.Holdout.EstimatorN != 4 || pe.Holdout.EstimatorBias != 1.2 {
		t.Errorf("Holdout estimator = N=%d Bias=%v, want N=4 Bias=1.2", pe.Holdout.EstimatorN, pe.Holdout.EstimatorBias)
	}

	// Render must not panic and should surface both arms plus the seam note.
	var buf bytes.Buffer
	RenderExperiment(rep, &buf)
	out := buf.String()
	for _, want := range []string{"proxied", "holdout", "calibration slope", proxyID} {
		if !strings.Contains(out, want) {
			t.Errorf("RenderExperiment output missing %q:\n%s", want, out)
		}
	}
}

// TestCollectExperimentProjectFilter proves the projectID argument scopes
// CollectExperiment the same way CollectTokens' does.
func TestCollectExperimentProjectFilter(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	rootA := gitRepo(t)
	rootB := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	for _, p := range []struct{ id, root string }{{"a", rootA}, {"b", rootB}} {
		rec := &registry.Record{
			ProjectID: p.id, Name: p.id, Root: p.root, DefaultBranch: "main",
			AccountProfile: registry.ProfilePersonal, ExpectedIdentity: "me@example.com",
			AgentProxy: &registry.AgentProxy{BaseURL: "http://127.0.0.1:8787"},
		}
		if err := store.Add(context.Background(), rec); err != nil {
			t.Fatalf("add %s: %v", p.id, err)
		}
	}

	rep, err := CollectExperiment(store, "a")
	if err != nil {
		t.Fatalf("CollectExperiment: %v", err)
	}
	if len(rep.Projects) != 1 || rep.Projects[0].ProjectID != "a" {
		t.Fatalf("Projects = %+v, want exactly project a", rep.Projects)
	}
}
