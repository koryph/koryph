// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/registry"
)

// TestCollectTokensEmpty verifies that CollectTokens on an empty store returns
// a well-formed report with no projects, and that RenderTokens handles it
// gracefully.
func TestCollectTokensEmpty(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}

	rep, err := CollectTokens(store, "")
	if err != nil {
		t.Fatalf("CollectTokens: %v", err)
	}
	if rep == nil {
		t.Fatal("CollectTokens returned nil report")
	}
	if len(rep.Projects) != 0 {
		t.Errorf("Projects = %d, want 0", len(rep.Projects))
	}

	var buf bytes.Buffer
	RenderTokens(rep, &buf)
	out := buf.String()
	if !strings.Contains(out, "no token data") {
		t.Errorf("RenderTokens empty: want 'no token data' message, got:\n%s", out)
	}
}

// TestCollectTokensNilReport verifies that RenderTokens is nil-safe.
func TestCollectTokensNilReport(t *testing.T) {
	var buf bytes.Buffer
	RenderTokens(nil, &buf) // must not panic
}

// TestCollectTokensNoTokenFields verifies that slots with all-zero token fields
// are skipped and produce no bead rows or tier rows.
func TestCollectTokensNoTokenFields(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	rec := &registry.Record{
		ProjectID:        "notoken",
		Name:             "notoken",
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(context.Background(), rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Slot with all-zero token fields (pre-L1 ledger).
	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "notoken", Status: ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			"a": {PhaseID: "a", Model: "sonnet", Status: ledger.SlotMerged, CostUSD: 1.0},
		},
	})

	rep, err := CollectTokens(store, "")
	if err != nil {
		t.Fatalf("CollectTokens: %v", err)
	}
	if len(rep.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(rep.Projects))
	}
	p := rep.Projects[0]
	if p.TotalBeads != 0 {
		t.Errorf("TotalBeads = %d, want 0 (no token fields)", p.TotalBeads)
	}
	if len(p.Beads) != 0 {
		t.Errorf("Beads = %d, want 0", len(p.Beads))
	}
	if len(p.ByTier) != 0 {
		t.Errorf("ByTier = %d, want 0", len(p.ByTier))
	}
	if p.Composition.Total != 0 {
		t.Errorf("Composition.Total = %d, want 0", p.Composition.Total)
	}
}

// TestCollectTokensBasic verifies per-bead and per-tier aggregation over a
// fixture ledger with populated token fields.
func TestCollectTokensBasic(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	rec := &registry.Record{
		ProjectID:        "demo",
		Name:             "demo",
		Root:             root,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(context.Background(), rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	// Run 1: two slots with token data.
	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "demo", Status: ledger.RunDone,
		StartedAt: "2026-01-01T00:00:01Z",
		Slots: map[string]*ledger.Slot{
			"a": {
				PhaseID: "a", BeadID: "koryph-abc", Model: "sonnet",
				Status:      ledger.SlotMerged,
				InputTokens: 1000, OutputTokens: 200,
				CacheReadTokens: 8000, CacheCreationTokens: 500,
				CostUSD: 1.0,
			},
			"b": {
				PhaseID: "b", BeadID: "koryph-def", Model: "opus",
				Status:      ledger.SlotFailed,
				InputTokens: 2000, OutputTokens: 300,
				CacheReadTokens: 5000, CacheCreationTokens: 0,
				CostUSD: 2.0,
			},
		},
	})
	// Run 2: one new slot.
	writeRun(t, root, "20260101-000002", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000002", ProjectID: "demo", Status: ledger.RunDone,
		StartedAt: "2026-01-01T00:00:02Z",
		Slots: map[string]*ledger.Slot{
			"c": {
				PhaseID: "c", Model: "sonnet",
				Status:      ledger.SlotMerged,
				InputTokens: 500, OutputTokens: 100,
				CacheReadTokens: 3000, CacheCreationTokens: 200,
				CostUSD: 0.5,
			},
		},
	})

	rep, err := CollectTokens(store, "demo")
	if err != nil {
		t.Fatalf("CollectTokens: %v", err)
	}
	if len(rep.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(rep.Projects))
	}
	p := rep.Projects[0]

	// 3 beads total (a, b, c).
	if p.TotalBeads != 3 {
		t.Errorf("TotalBeads = %d, want 3", p.TotalBeads)
	}
	if len(p.Beads) != 3 {
		t.Errorf("len(Beads) = %d, want 3", len(p.Beads))
	}

	// Total composition check.
	// a: 1000+200+8000+500 = 9700
	// b: 2000+300+5000+0   = 7300
	// c: 500+100+3000+200  = 3800
	// total = 20800
	if p.Composition.Total != 20800 {
		t.Errorf("Total = %d, want 20800", p.Composition.Total)
	}
	if p.Composition.Input != 3500 {
		t.Errorf("Input = %d, want 3500", p.Composition.Input)
	}
	if p.Composition.Output != 600 {
		t.Errorf("Output = %d, want 600", p.Composition.Output)
	}
	if p.Composition.CacheRead != 16000 {
		t.Errorf("CacheRead = %d, want 16000", p.Composition.CacheRead)
	}
	if p.Composition.CacheCreation != 700 {
		t.Errorf("CacheCreation = %d, want 700", p.Composition.CacheCreation)
	}

	// MeanPerBead = 20800 / 3 = 6933
	if p.MeanPerBead != 6933 {
		t.Errorf("MeanPerBead = %d, want 6933", p.MeanPerBead)
	}

	// CacheHitRatio = CacheRead / (CacheRead + Input) = 16000 / 19500 ≈ 0.8205
	if p.Composition.CacheHitRatio < 0.82 || p.Composition.CacheHitRatio > 0.83 {
		t.Errorf("CacheHitRatio = %.4f, want ~0.8205", p.Composition.CacheHitRatio)
	}

	// Two tiers: sonnet (a + c) and opus (b).
	if len(p.ByTier) != 2 {
		t.Errorf("ByTier keys = %d, want 2", len(p.ByTier))
	}
	sonnet := p.ByTier["sonnet"]
	if sonnet.Slots != 2 {
		t.Errorf("sonnet.Slots = %d, want 2", sonnet.Slots)
	}
	// sonnet: a(9700) + c(3800) = 13500
	if sonnet.Composition.Total != 13500 {
		t.Errorf("sonnet.Total = %d, want 13500", sonnet.Composition.Total)
	}
	if sonnet.MeanPerSlot != 6750 {
		t.Errorf("sonnet.MeanPerSlot = %d, want 6750", sonnet.MeanPerSlot)
	}

	opus := p.ByTier["opus"]
	if opus.Slots != 1 {
		t.Errorf("opus.Slots = %d, want 1", opus.Slots)
	}
	if opus.Composition.Total != 7300 {
		t.Errorf("opus.Total = %d, want 7300", opus.Composition.Total)
	}

	// Beads sorted descending by total: a(9700), b(7300), c(3800).
	if p.Beads[0].PhaseID != "a" {
		t.Errorf("Beads[0] = %q, want 'a' (9700 tokens)", p.Beads[0].PhaseID)
	}
	if p.Beads[1].PhaseID != "b" {
		t.Errorf("Beads[1] = %q, want 'b' (7300 tokens)", p.Beads[1].PhaseID)
	}
	if p.Beads[2].PhaseID != "c" {
		t.Errorf("Beads[2] = %q, want 'c' (3800 tokens)", p.Beads[2].PhaseID)
	}

	// BeadID is propagated from the slot.
	if p.Beads[0].BeadID != "koryph-abc" {
		t.Errorf("Beads[0].BeadID = %q, want 'koryph-abc'", p.Beads[0].BeadID)
	}

	// Run trend: 2 runs with token data.
	if len(p.RunTrend) != 2 {
		t.Errorf("RunTrend len = %d, want 2", len(p.RunTrend))
	}
	// Oldest run first.
	if p.RunTrend[0].RunID != "20260101-000001" {
		t.Errorf("RunTrend[0] = %q, want run 20260101-000001", p.RunTrend[0].RunID)
	}
	// Run 1 had 2 beads: 9700 + 7300 = 17000; mean = 8500.
	if p.RunTrend[0].Beads != 2 || p.RunTrend[0].TotalTokens != 17000 || p.RunTrend[0].MeanPerBead != 8500 {
		t.Errorf("RunTrend[0] = %+v, want beads=2 total=17000 mean=8500", p.RunTrend[0])
	}
}

// TestCollectTokensProjectFilter verifies that --project filtering works.
func TestCollectTokensProjectFilter(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	// Use separate roots so each project has its own ledger directory.
	rootAlpha := gitRepo(t)
	rootBeta := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}

	if err := store.Add(context.Background(), &registry.Record{
		ProjectID: "alpha", Name: "alpha", Root: rootAlpha,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}); err != nil {
		t.Fatalf("add alpha: %v", err)
	}
	if err := store.Add(context.Background(), &registry.Record{
		ProjectID: "beta", Name: "beta", Root: rootBeta,
		DefaultBranch:    "main",
		AccountProfile:   registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}); err != nil {
		t.Fatalf("add beta: %v", err)
	}

	writeRun(t, rootAlpha, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "alpha", Status: ledger.RunDone,
		StartedAt: "2026-01-01T00:00:01Z",
		Slots: map[string]*ledger.Slot{
			"x": {PhaseID: "x", Model: "sonnet", Status: ledger.SlotMerged,
				InputTokens: 100, OutputTokens: 10},
		},
	})

	// Filter to beta (no ledger files for it).
	rep, err := CollectTokens(store, "beta")
	if err != nil {
		t.Fatalf("CollectTokens: %v", err)
	}
	if len(rep.Projects) != 1 || rep.Projects[0].ProjectID != "beta" {
		t.Errorf("filter to beta: got %d projects", len(rep.Projects))
	}
	if rep.Projects[0].TotalBeads != 0 {
		t.Errorf("beta should have 0 beads, got %d", rep.Projects[0].TotalBeads)
	}

	// Filter to alpha: should have 1 bead.
	rep2, err := CollectTokens(store, "alpha")
	if err != nil {
		t.Fatalf("CollectTokens alpha: %v", err)
	}
	if len(rep2.Projects) != 1 || rep2.Projects[0].TotalBeads != 1 {
		t.Errorf("alpha: want 1 bead, got projects=%d beads=%d",
			len(rep2.Projects), rep2.Projects[0].TotalBeads)
	}
}

// TestRenderTokensOutput verifies that RenderTokens emits expected columns and
// does not emit a JSON line in human mode.
func TestRenderTokensOutput(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	if err := store.Add(context.Background(), &registry.Record{
		ProjectID: "render-test", Name: "render-test", Root: root,
		DefaultBranch: "main", AccountProfile: registry.ProfilePersonal,
		ExpectedIdentity: "me@example.com",
	}); err != nil {
		t.Fatalf("add: %v", err)
	}

	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "render-test",
		Status: ledger.RunDone, StartedAt: "2026-01-01T00:00:01Z",
		Slots: map[string]*ledger.Slot{
			"slot1": {
				PhaseID: "slot1", BeadID: "koryph-xyz", Model: "sonnet",
				Status:      ledger.SlotMerged,
				InputTokens: 5000, OutputTokens: 1000,
				CacheReadTokens: 40000, CacheCreationTokens: 2000,
			},
		},
	})

	rep, err := CollectTokens(store, "render-test")
	if err != nil {
		t.Fatalf("CollectTokens: %v", err)
	}

	var buf bytes.Buffer
	RenderTokens(rep, &buf)
	out := buf.String()

	// Must contain project name.
	if !strings.Contains(out, "render-test") {
		t.Errorf("output missing project name:\n%s", out)
	}
	// Must contain cache-hit percentage.
	if !strings.Contains(out, "cache-hit") {
		t.Errorf("output missing cache-hit:\n%s", out)
	}
	// Must contain tier table header.
	if !strings.Contains(out, "TIER") {
		t.Errorf("output missing TIER header:\n%s", out)
	}
	// Must contain bead table header.
	if !strings.Contains(out, "BEAD/PHASE") {
		t.Errorf("output missing BEAD/PHASE header:\n%s", out)
	}
	// Must contain trend table header.
	if !strings.Contains(out, "RUN") {
		t.Errorf("output missing trend RUN header:\n%s", out)
	}
	// Must not contain stray JSON line.
	if strings.Contains(out, `"generated_at"`) {
		t.Errorf("output must not contain raw JSON:\n%s", out)
	}
	// Must contain the bead ID.
	if !strings.Contains(out, "koryph-xyz") {
		t.Errorf("output missing bead ID 'koryph-xyz':\n%s", out)
	}
}

// TestMakeComposition unit-tests the derived fields of TokenComposition.
func TestMakeComposition(t *testing.T) {
	c := makeComposition(1000, 200, 8000, 500)
	if c.Total != 9700 {
		t.Errorf("Total = %d, want 9700", c.Total)
	}
	// CacheHitRatio = 8000 / (8000 + 1000) = 8000/9000 ≈ 0.8889
	if c.CacheHitRatio < 0.888 || c.CacheHitRatio > 0.890 {
		t.Errorf("CacheHitRatio = %.4f, want ~0.8889", c.CacheHitRatio)
	}

	// Zero cache and input → ratio = 0 (not NaN).
	c2 := makeComposition(0, 100, 0, 0)
	if c2.CacheHitRatio != 0 {
		t.Errorf("CacheHitRatio with zero input = %v, want 0", c2.CacheHitRatio)
	}
}

// TestFmtTokens verifies the human-readable token formatter.
func TestFmtTokens(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0K"},
		{1500, "1.5K"},
		{999999, "1000.0K"},
		{1000000, "1.0M"},
		{1234567, "1.2M"},
	}
	for _, tc := range cases {
		got := fmtTokens(tc.n)
		if got != tc.want {
			t.Errorf("fmtTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}
