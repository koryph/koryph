// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package metrics

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/registry"
)

// gitRepo creates a committed git repo usable as a Record.Root.
func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	return dir
}

// writeRun writes a ledger.json under <root>/.plan-logs/koryph/<runID>/.
func writeRun(t *testing.T, root, runID string, run *ledger.Run) {
	t.Helper()
	dir := filepath.Join(paths.KoryphRoot(root), runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteJSONAtomic(filepath.Join(dir, "ledger.json"), run); err != nil {
		t.Fatal(err)
	}
}

func TestCollectAndRender(t *testing.T) {
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

	// Run 1: finalized (done). Two slots.
	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "demo", Status: ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			"a": {PhaseID: "a", Model: "sonnet", Status: ledger.SlotMerged, Attempts: 1, CostUSD: 1.0, ReviewIters: 1},
			"b": {PhaseID: "b", Model: "opus", Status: ledger.SlotFailed, Attempts: 2, CostUSD: 2.0},
		},
	})
	// Run 2: unfinalized (running). One blocked slot.
	writeRun(t, root, "20260101-000002", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000002", ProjectID: "demo", Status: ledger.RunRunning,
		Slots: map[string]*ledger.Slot{
			"c": {PhaseID: "c", Model: "sonnet", Status: ledger.SlotBlocked, Attempts: 1, CostUSD: 0.5},
		},
	})
	// A `latest` symlink to run 2 must be skipped (no double count).
	_ = os.Symlink("20260101-000002", filepath.Join(paths.KoryphRoot(root), "latest"))

	rep, err := Collect(store, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rep.Projects) != 1 {
		t.Fatalf("projects = %d, want 1", len(rep.Projects))
	}
	p := rep.Projects[0]

	if p.Runs != 2 {
		t.Errorf("Runs = %d, want 2 (latest symlink must not double-count)", p.Runs)
	}
	if p.UnfinalizedRuns != 1 {
		t.Errorf("UnfinalizedRuns = %d, want 1", p.UnfinalizedRuns)
	}
	if p.Slots != 3 {
		t.Errorf("Slots = %d, want 3", p.Slots)
	}
	if p.Merged != 1 || p.Failed != 1 || p.Blocked != 1 {
		t.Errorf("merged/failed/blocked = %d/%d/%d, want 1/1/1", p.Merged, p.Failed, p.Blocked)
	}
	if p.Retries != 1 {
		t.Errorf("Retries = %d, want 1 (slot b has attempts=2)", p.Retries)
	}
	if p.ReviewBounces != 1 {
		t.Errorf("ReviewBounces = %d, want 1", p.ReviewBounces)
	}
	if p.CostUSD != 3.5 {
		t.Errorf("CostUSD = %v, want 3.5", p.CostUSD)
	}
	if rep.TotalUSD != 3.5 {
		t.Errorf("TotalUSD = %v, want 3.5", rep.TotalUSD)
	}

	sonnet := p.ByModel["sonnet"]
	if sonnet.Slots != 2 || sonnet.Merged != 1 || sonnet.CostUSD != 1.5 {
		t.Errorf("by-model sonnet = %+v, want slots=2 merged=1 cost=1.5", sonnet)
	}
	opus := p.ByModel["opus"]
	if opus.Slots != 1 || opus.Failed != 1 || opus.Retries != 1 || opus.CostUSD != 2.0 {
		t.Errorf("by-model opus = %+v, want slots=1 failed=1 retries=1 cost=2.0", opus)
	}
	if opus.MeanUSD != 2.0 {
		t.Errorf("opus MeanUSD = %v, want 2.0", opus.MeanUSD)
	}

	// No agent worktrees/branches → zeroes.
	if p.StaleWorktrees != 0 || p.OrphanBranches != 0 {
		t.Errorf("stale/orphan = %d/%d, want 0/0", p.StaleWorktrees, p.OrphanBranches)
	}

	// Render: human table only — no trailing JSON line.
	var buf bytes.Buffer
	Render(rep, &buf)
	rendered := buf.String()
	if !strings.Contains(rendered, "demo") {
		t.Errorf("Render missing project row:\n%s", rendered)
	}
	if strings.Contains(rendered, "JSON: ") {
		t.Errorf("Render must not emit JSON: line in human mode:\n%s", rendered)
	}
}

// TestCollectByModelActualAttribution locks in that the per-model cost/outcome
// breakdown is keyed on the model that ACTUALLY served (ModelActual), not the
// model dispatch requested (Model) — a mid-flight --fallback-model downgrade is
// charged to the tier that ran. A slot with no ModelActual falls back to Model.
func TestCollectByModelActualAttribution(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	root := gitRepo(t)

	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	rec := &registry.Record{
		ProjectID: "demo", Name: "demo", Root: root, DefaultBranch: "main",
		AccountProfile: registry.ProfilePersonal, ExpectedIdentity: "me@example.com",
	}
	if err := store.Add(context.Background(), rec); err != nil {
		t.Fatalf("add: %v", err)
	}

	writeRun(t, root, "20260101-000001", &ledger.Run{
		SchemaVersion: 2, RunID: "20260101-000001", ProjectID: "demo", Status: ledger.RunDone,
		Slots: map[string]*ledger.Slot{
			// Requested opus, downgraded to sonnet mid-flight: cost belongs to sonnet.
			"a": {PhaseID: "a", Model: "opus", ModelActual: "sonnet", Status: ledger.SlotMerged, Attempts: 1, CostUSD: 3.0},
			// No ModelActual (crash/old ledger): falls back to requested Model.
			"b": {PhaseID: "b", Model: "opus", Status: ledger.SlotFailed, Attempts: 1, CostUSD: 2.0},
		},
	})

	rep, err := Collect(store, "demo")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	p := rep.Projects[0]

	sonnet := p.ByModel["sonnet"]
	if sonnet.Slots != 1 || sonnet.Merged != 1 || sonnet.CostUSD != 3.0 {
		t.Errorf("sonnet (served) = %+v, want slots=1 merged=1 cost=3.0", sonnet)
	}
	opus := p.ByModel["opus"]
	if opus.Slots != 1 || opus.Failed != 1 || opus.CostUSD != 2.0 {
		t.Errorf("opus (fallback) = %+v, want slots=1 failed=1 cost=2.0", opus)
	}
}

func TestCollectFilterAndEmpty(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	store := registry.NewStore()
	if err := store.Init(context.Background()); err != nil {
		t.Fatalf("store init: %v", err)
	}
	// No records → empty report; Render is nil-safe.
	rep, err := Collect(store, "")
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if len(rep.Projects) != 0 || rep.TotalUSD != 0 {
		t.Errorf("empty report = %+v", rep)
	}
	var buf bytes.Buffer
	Render(rep, &buf)
	if strings.Contains(buf.String(), "JSON: ") {
		t.Errorf("Render must not emit JSON: line on empty report:\n%s", buf.String())
	}
	// Nil report is tolerated.
	Render(nil, &buf)
}
