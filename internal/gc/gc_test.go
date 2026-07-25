// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/fsx"
)

// TestConfigDefaults verifies that zero-value Config gets default values.
func TestConfigDefaults(t *testing.T) {
	cfg := Config{}.effective()
	if cfg.RunDirs.CompressAfterDays != 7 {
		t.Errorf("CompressAfterDays: got %d, want 7", cfg.RunDirs.CompressAfterDays)
	}
	if cfg.RunDirs.DeleteAfterDays != 90 {
		t.Errorf("DeleteAfterDays: got %d, want 90", cfg.RunDirs.DeleteAfterDays)
	}
	if cfg.AuditLog.RotateSizeMB != 10 {
		t.Errorf("AuditLog.RotateSizeMB: got %d, want 10", cfg.AuditLog.RotateSizeMB)
	}
	if cfg.RunsIndex.RotateSizeMB != 10 {
		t.Errorf("RunsIndex.RotateSizeMB: got %d, want 10", cfg.RunsIndex.RotateSizeMB)
	}
	if cfg.FootprintWarnGB != 1.0 {
		t.Errorf("FootprintWarnGB: got %f, want 1.0", cfg.FootprintWarnGB)
	}
	if cfg.ProjectBudget.SoftMB != 2048 || cfg.ProjectBudget.HardMB != 5120 {
		t.Errorf("ProjectBudget: got %+v, want 2048/5120 MiB", cfg.ProjectBudget)
	}
}

// TestConfigNeverUnmarshal verifies "never" string parses correctly.
func TestConfigNeverUnmarshal(t *testing.T) {
	raw := `{
		"run_dirs": {
			"compress_after_days": "never",
			"delete_after_days": "never"
		},
		"audit_log": {
			"retain_days": "never"
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !cfg.RunDirs.CompressAfterDaysNever {
		t.Error("RunDirs.CompressAfterDaysNever should be true")
	}
	if !cfg.RunDirs.DeleteAfterDaysNever {
		t.Error("RunDirs.DeleteAfterDaysNever should be true")
	}
	if !cfg.AuditLog.RetainDaysNever {
		t.Error("AuditLog.RetainDaysNever should be true")
	}
}

// TestConfigNumericUnmarshal verifies numeric values parse correctly.
func TestConfigNumericUnmarshal(t *testing.T) {
	raw := `{
		"run_dirs": {
			"compress_after_days": 14,
			"delete_after_days": 180
		},
		"audit_log": {
			"rotate_size_mb": 20,
			"retain_days": 365
		},
		"footprint_warn_gb": 2.5,
		"gc_auto": true,
		"project_budget": {
			"soft_mb": 1024,
			"hard_mb": 4096,
			"transcript_retain_days": 7,
			"failure_retain_days": 60,
			"log_tail_kb": 32
		}
	}`
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.RunDirs.CompressAfterDays != 14 {
		t.Errorf("CompressAfterDays: got %d, want 14", cfg.RunDirs.CompressAfterDays)
	}
	if cfg.RunDirs.DeleteAfterDays != 180 {
		t.Errorf("DeleteAfterDays: got %d, want 180", cfg.RunDirs.DeleteAfterDays)
	}
	if cfg.AuditLog.RotateSizeMB != 20 {
		t.Errorf("AuditLog.RotateSizeMB: got %d, want 20", cfg.AuditLog.RotateSizeMB)
	}
	if cfg.AuditLog.RetainDays != 365 {
		t.Errorf("AuditLog.RetainDays: got %d, want 365", cfg.AuditLog.RetainDays)
	}
	if cfg.FootprintWarnGB != 2.5 {
		t.Errorf("FootprintWarnGB: got %f, want 2.5", cfg.FootprintWarnGB)
	}
	if !cfg.GCAuto {
		t.Error("GCAuto should be true")
	}
	if cfg.ProjectBudget != (ProjectBudgetPolicy{
		SoftMB: 1024, HardMB: 4096, TranscriptRetainDays: 7,
		FailureRetainDays: 60, LogTailKB: 32,
	}) {
		t.Errorf("ProjectBudget = %+v", cfg.ProjectBudget)
	}
}

// TestLoadConfig verifies LoadConfig reads global + project configs.
func TestLoadConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	// Write global config.
	globalCfg := `{"run_dirs":{"compress_after_days":14}}`
	if err := os.WriteFile(filepath.Join(home, "retention.json"), []byte(globalCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// No project override.
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.RunDirs.CompressAfterDays != 14 {
		t.Errorf("CompressAfterDays: got %d, want 14", cfg.RunDirs.CompressAfterDays)
	}
	// Default from effective().
	if cfg.RunDirs.DeleteAfterDays != 90 {
		t.Errorf("DeleteAfterDays: got %d, want 90", cfg.RunDirs.DeleteAfterDays)
	}
}

// TestGCRunDirsDryRun verifies dry-run reports without deletion.
func TestGCRunDirsDryRun(t *testing.T) {
	repoRoot := t.TempDir()

	// Create a simulated koryphRoot with one old completed run.
	koryphRoot := filepath.Join(repoRoot, ".plan-logs", "koryph")
	if err := os.MkdirAll(koryphRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	// Create a run dir with ledger.json (all slots terminal).
	runID := "20260601-120000"
	runDir := filepath.Join(koryphRoot, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledgerData := `{"run_id":"20260601-120000","slots":{"bead1":{"phase_id":"bead1","status":"merged"}},"status":"done"}`
	if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), []byte(ledgerData), 0o644); err != nil {
		t.Fatal(err)
	}
	// Write a dummy file to give it some size.
	if err := os.WriteFile(filepath.Join(runDir, "session.log"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Travel 10 days into the future so the run is old enough.
	now := time.Now().Add(10 * 24 * time.Hour)
	// Set the mtime back so it appears old.
	oldTime := time.Now().Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(runDir, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KORYPH_HOME", t.TempDir())

	cfg := Config{
		RunDirs: RunDirPolicy{
			CompressAfterDays: 7,
			DeleteAfterDays:   90,
		},
		AuditLog:  RotatePolicy{RotateSizeMB: 10},
		RunsIndex: RotatePolicy{RotateSizeMB: 10},
	}
	cfg = cfg.effective()

	opts := Options{
		RepoRoot: repoRoot,
		DryRun:   true,
		Config:   &cfg,
		Now:      func() time.Time { return now },
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.DryRun {
		t.Error("expected DryRun=true in result")
	}
	// Find run-dirs class result.
	var rdc *ClassResult
	for i := range res.Classes {
		if res.Classes[i].Class == "run-dirs" {
			rdc = &res.Classes[i]
			break
		}
	}
	if rdc == nil {
		t.Fatal("no run-dirs class result")
	}
	if rdc.Compressed == 0 {
		t.Error("expected >=1 compressed in dry-run")
	}
	// Original dir must still exist (dry-run).
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("run dir should still exist in dry-run: %v", err)
	}
}

// TestGCRunDirsLiveSlotExempt verifies live-slot runs are skipped.
func TestGCRunDirsLiveSlotExempt(t *testing.T) {
	repoRoot := t.TempDir()
	koryphRoot := filepath.Join(repoRoot, ".plan-logs", "koryph")
	if err := os.MkdirAll(koryphRoot, 0o755); err != nil {
		t.Fatal(err)
	}

	runID := "20260601-120000"
	runDir := filepath.Join(koryphRoot, runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Ledger with a running slot.
	ledgerData := `{"run_id":"20260601-120000","slots":{"bead1":{"phase_id":"bead1","status":"running"}},"status":"running"}`
	if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), []byte(ledgerData), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(runDir, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{CompressAfterDays: 7, DeleteAfterDays: 90}}.effective()
	now := time.Now()
	opts := Options{RepoRoot: repoRoot, DryRun: false, Config: &cfg, Now: func() time.Time { return now }}
	res, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var rdc *ClassResult
	for i := range res.Classes {
		if res.Classes[i].Class == "run-dirs" {
			rdc = &res.Classes[i]
			break
		}
	}
	if rdc == nil {
		t.Fatal("no run-dirs class result")
	}
	if rdc.Skipped == 0 {
		t.Error("expected run with live slot to be skipped")
	}
	if rdc.Compressed > 0 {
		t.Error("expected no compression for run with live slot")
	}
	// Dir should still exist.
	if _, err := os.Stat(runDir); err != nil {
		t.Errorf("run dir with live slot should not be touched: %v", err)
	}
}

func TestGCPrunesTerminalSlotCachesWhileSiblingRemainsLive(t *testing.T) {
	repoRoot := t.TempDir()
	runID := "20260601-120000"
	runDir := filepath.Join(repoRoot, ".plan-logs", "koryph", runID)
	writeGCFile(t, filepath.Join(runDir, "ledger.json"), []byte(
		`{"run_id":"20260601-120000","slots":{`+
			`"done":{"phase_id":"done","status":"merged"},`+
			`"live":{"phase_id":"live","status":"running"}},"status":"running"}`,
	))
	for _, path := range []string{
		filepath.Join(runDir, "done", "cache", "item"),
		filepath.Join(runDir, "done", "go-telemetry", "item"),
		filepath.Join(runDir, "done", "runtime-cache", "item"),
		filepath.Join(runDir, "done", "gomodcache", "item"),
		filepath.Join(runDir, ".engine-evidence", "done", ".runtime-scratch", "item"),
		filepath.Join(runDir, "live", "cache", "item"),
	} {
		writeGCFile(t, path, []byte("scratch"))
	}

	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{
		CompressAfterDaysNever: true,
		DeleteAfterDaysNever:   true,
	}}.effective()
	if _, err := Run(Options{
		RepoRoot: repoRoot, ActiveRunID: runID, Config: &cfg,
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		filepath.Join(runDir, "done", "cache"),
		filepath.Join(runDir, "done", "go-telemetry"),
		filepath.Join(runDir, "done", "runtime-cache"),
		filepath.Join(runDir, "done", "gomodcache"),
		filepath.Join(runDir, ".engine-evidence", "done", ".runtime-scratch"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("terminal scratch survived at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(filepath.Join(runDir, "live", "cache", "item")); err != nil {
		t.Fatalf("live slot cache was touched: %v", err)
	}
}

func TestGCUnlinksKnownCacheSymlinksWithoutFollowing(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	outside := t.TempDir()
	sentinel := filepath.Join(outside, "sentinel")
	writeGCFile(t, sentinel, []byte("preserve"))
	phaseLink := filepath.Join(runDir, "bead1", "go-cache")
	if err := os.Symlink(outside, phaseLink); err != nil {
		t.Fatal(err)
	}
	reviewRoot := filepath.Join(runDir, ".engine-evidence", "bead1")
	if err := os.MkdirAll(reviewRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	reviewLink := filepath.Join(reviewRoot, ".runtime-scratch")
	if err := os.Symlink(outside, reviewLink); err != nil {
		t.Fatal(err)
	}
	runCacheGC(t, repoRoot, false)
	for _, link := range []string{phaseLink, reviewLink} {
		if _, err := os.Lstat(link); !os.IsNotExist(err) {
			t.Errorf("known disposable symlink survived at %s: %v", link, err)
		}
	}
	if raw, err := os.ReadFile(sentinel); err != nil || string(raw) != "preserve" {
		t.Fatalf("outside sentinel changed: %q, %v", raw, err)
	}
}

func TestGCRetainMarkerProtectsRunFromCleanupAndArchival(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	writeGCFile(t, filepath.Join(runDir, ".retain"), nil)
	cache := filepath.Join(runDir, "bead1", "go-cache", "entry")
	writeGCFile(t, cache, []byte("preserve"))
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{
		CompressAfterDays: 1,
		DeleteAfterDays:   2,
	}}.effective()
	if _, err := Run(Options{RepoRoot: repoRoot, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("explicitly retained run cache was touched: %v", err)
	}
	if _, err := os.Stat(runDir + ".tar.gz"); !os.IsNotExist(err) {
		t.Fatalf("explicitly retained run was archived: %v", err)
	}
}

func TestGCRetainedPhaseVetoesWholeRunArchival(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	phaseDir := filepath.Join(runDir, "bead1")
	writeGCFile(t, filepath.Join(phaseDir, ".retain"), nil)
	cache := filepath.Join(phaseDir, "go-cache", "entry")
	writeGCFile(t, cache, []byte("preserve"))
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{
		CompressAfterDays: 1,
		DeleteAfterDays:   2,
	}}.effective()
	if _, err := Run(Options{RepoRoot: repoRoot, Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cache); err != nil {
		t.Fatalf("retained phase cache was touched: %v", err)
	}
	if _, err := os.Stat(runDir); err != nil {
		t.Fatalf("run containing retained phase was removed: %v", err)
	}
	if _, err := os.Stat(runDir + ".tar.gz"); !os.IsNotExist(err) {
		t.Fatalf("run containing retained phase was archived: %v", err)
	}
}

func TestGCPrunesOnlyRecognizedCachesFromTerminalRuns(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	phaseDir := filepath.Join(runDir, "bead1")
	for _, name := range []string{"go-cache", "go-mod-cache", "go-build12345"} {
		cacheDir := filepath.Join(phaseDir, name, "nested")
		if err := os.MkdirAll(cacheDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cacheDir, "cache-data"), []byte("cache"), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	// Go's module cache can contain read-only directories as well as files.
	if err := os.Chmod(filepath.Join(phaseDir, "go-mod-cache", "nested"), 0o555); err != nil {
		t.Fatal(err)
	}
	unknownDir := filepath.Join(phaseDir, "go-build-not-a-number")
	if err := os.MkdirAll(unknownDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ledger.json", "manifest.json", "status.json", "stream.jsonl", "session.log", "SUMMARY.md"} {
		if err := os.WriteFile(filepath.Join(phaseDir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	res := runCacheGC(t, repoRoot, false)
	if got := classResult(t, res, "run-dirs").Deleted; got != 3 {
		t.Fatalf("deleted cache directories = %d, want 3", got)
	}
	for _, name := range []string{"go-cache", "go-mod-cache", "go-build12345"} {
		if _, err := os.Stat(filepath.Join(phaseDir, name)); !os.IsNotExist(err) {
			t.Errorf("recognized cache %s was not removed: %v", name, err)
		}
	}
	for _, name := range append([]string{"go-build-not-a-number"}, "ledger.json", "manifest.json", "status.json", "stream.jsonl", "session.log", "SUMMARY.md") {
		if _, err := os.Stat(filepath.Join(phaseDir, name)); err != nil {
			t.Errorf("diagnostic evidence %s was removed: %v", name, err)
		}
	}

	// A second invocation is a no-op after the caches are gone.
	if got := classResult(t, runCacheGC(t, repoRoot, false), "run-dirs").Deleted; got != 0 {
		t.Errorf("idempotent rerun deleted %d entries, want 0", got)
	}
}

func TestGCLatestTerminalRunPrunesTempButPreservesEvidence(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	koryphRoot := filepath.Dir(runDir)
	if err := os.Symlink(filepath.Base(runDir), filepath.Join(koryphRoot, "latest")); err != nil {
		t.Fatal(err)
	}
	tempDir := filepath.Join(runDir, "bead1", "go-tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(runDir, "bead1", "manifest.json")
	if err := os.WriteFile(evidence, []byte(`{"status":"complete"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	result := runCacheGC(t, repoRoot, false)
	if got := classResult(t, result, "run-dirs").Deleted; got != 1 {
		t.Fatalf("terminal temp deletions = %d, want 1", got)
	}
	if _, err := os.Stat(tempDir); !os.IsNotExist(err) {
		t.Fatalf("latest terminal temp survived: %v", err)
	}
	if _, err := os.Stat(evidence); err != nil {
		t.Fatalf("latest durable evidence was removed: %v", err)
	}
}

func TestGCPhaseCachesPreserveNonterminalRuns(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "running")
	cacheDir := filepath.Join(runDir, "bead1", "go-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}

	res := runCacheGC(t, repoRoot, false)
	if got := classResult(t, res, "run-dirs").Deleted; got != 0 {
		t.Errorf("deleted %d phase caches from nonterminal run", got)
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache in nonterminal run was removed: %v", err)
	}
}

func TestGCPhaseCachesPruneTerminalSlotsBeforeRunStatusSettles(t *testing.T) {
	for _, runStatus := range []string{"running", "paused-quota", "hard-stop-quota"} {
		t.Run(runStatus, func(t *testing.T) {
			repoRoot, runDir := gcCacheFixture(t, "merged")
			ledgerData := `{"run_id":"20260724-120000","slots":{"bead1":{"phase_id":"bead1","status":"merged"}},"status":"` + runStatus + `"}`
			if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), []byte(ledgerData), 0o644); err != nil {
				t.Fatal(err)
			}
			cacheDir := filepath.Join(runDir, "bead1", "go-cache")
			if err := os.MkdirAll(cacheDir, 0o755); err != nil {
				t.Fatal(err)
			}

			res := runCacheGC(t, repoRoot, false)
			if got := classResult(t, res, "run-dirs").Deleted; got != 1 {
				t.Errorf("deleted %d phase caches from %s run, want 1", got, runStatus)
			}
			if _, err := os.Stat(cacheDir); !os.IsNotExist(err) {
				t.Errorf("terminal slot cache in %s run survived: %v", runStatus, err)
			}
		})
	}
}

func TestGCPhaseCachesRejectUnsafePhaseNames(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	koryphRoot := filepath.Dir(runDir)
	cacheDir := filepath.Join(koryphRoot, "go-cache")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ledgerData := `{"run_id":"20260724-120000","slots":{"bead1":{"phase_id":"..","status":"merged"}},"status":"done"}`
	if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), []byte(ledgerData), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := classResult(t, runCacheGC(t, repoRoot, false), "run-dirs").Deleted; got != 0 {
		t.Errorf("deleted %d cache directories for unsafe phase name", got)
	}
	if _, err := os.Stat(cacheDir); err != nil {
		t.Errorf("cache outside run was removed: %v", err)
	}
}

func TestGCPhaseCachesDryRunReportsWithoutMutation(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "done")
	cacheFile := filepath.Join(runDir, "bead1", "go-cache", "cache-data")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}

	res := runCacheGC(t, repoRoot, true)
	cr := classResult(t, res, "run-dirs")
	if cr.Deleted != 1 || cr.ReclaimedMB <= 0 {
		t.Errorf("dry-run result = deleted %d, reclaimed %f MB; want one positive reclaim", cr.Deleted, cr.ReclaimedMB)
	}
	if _, err := os.Stat(cacheFile); err != nil {
		t.Errorf("dry-run mutated cache: %v", err)
	}
}

func TestGCPhaseCacheDryRunDoesNotDoubleCountCompression(t *testing.T) {
	repoRoot, runDir := gcCacheFixture(t, "merged")
	cacheFile := filepath.Join(runDir, "bead1", "go-cache", "cache-data")
	if err := os.MkdirAll(filepath.Dir(cacheFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cacheFile, make([]byte, 1024*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * 24 * time.Hour)
	if err := os.Chtimes(runDir, old, old); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{CompressAfterDays: 1, DeleteAfterDaysNever: true}}.effective()
	res, err := Run(Options{RepoRoot: repoRoot, DryRun: true, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	cr := classResult(t, res, "run-dirs")
	if cr.Compressed != 1 {
		t.Fatalf("compressed = %d, want 1", cr.Compressed)
	}
	if got, want := cr.ReclaimedMB, dirSizeMB(filepath.Dir(cacheFile)); got != want {
		t.Errorf("reclaimed = %f MB, want conservative known cache reclaim %f MB", got, want)
	}
}

func gcCacheFixture(t *testing.T, status string) (string, string) {
	t.Helper()
	repoRoot := t.TempDir()
	runDir := filepath.Join(repoRoot, ".plan-logs", "koryph", "20260724-120000")
	if err := os.MkdirAll(filepath.Join(runDir, "bead1"), 0o755); err != nil {
		t.Fatal(err)
	}
	ledgerData := `{"run_id":"20260724-120000","slots":{"bead1":{"phase_id":"bead1","status":"` + status + `"}},"status":"done"}`
	if err := os.WriteFile(filepath.Join(runDir, "ledger.json"), []byte(ledgerData), 0o644); err != nil {
		t.Fatal(err)
	}
	return repoRoot, runDir
}

func runCacheGC(t *testing.T, repoRoot string, dryRun bool) *Result {
	t.Helper()
	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{RunDirs: RunDirPolicy{CompressAfterDaysNever: true, DeleteAfterDaysNever: true}}.effective()
	res, err := Run(Options{RepoRoot: repoRoot, DryRun: dryRun, Config: &cfg})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func classResult(t *testing.T, res *Result, class string) *ClassResult {
	t.Helper()
	for i := range res.Classes {
		if res.Classes[i].Class == class {
			return &res.Classes[i]
		}
	}
	t.Fatalf("no %s result", class)
	return nil
}

// TestGCRotateLogDryRun verifies audit log rotation dry-run.
func TestGCRotateLogDryRun(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")

	// Write 15 MiB of data (above the 10 MiB default).
	data := make([]byte, 15*1024*1024)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(logPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{AuditLog: RotatePolicy{RotateSizeMB: 10}, RunsIndex: RotatePolicy{RotateSizeMB: 10}}.effective()
	now := time.Now()
	opts := Options{DryRun: true, Config: &cfg, Now: func() time.Time { return now }}
	cr := gcRotateLog(logPath, cfg.AuditLog, "audit-log", opts)

	if cr.Compressed == 0 {
		t.Error("expected >=1 compressed in dry-run for oversized log")
	}
	// Original file must still exist.
	if _, err := os.Stat(logPath); err != nil {
		t.Errorf("original log should still exist in dry-run: %v", err)
	}
}

// TestGCRotateLogLive verifies audit log is actually rotated.
func TestGCRotateLogLive(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")

	// Write 15 MiB of data.
	data := make([]byte, 15*1024*1024)
	for i := range data {
		data[i] = 'a'
	}
	if err := os.WriteFile(logPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg := Config{AuditLog: RotatePolicy{RotateSizeMB: 10}, RunsIndex: RotatePolicy{RotateSizeMB: 10}}.effective()
	now := time.Now()
	opts := Options{DryRun: false, Config: &cfg, Now: func() time.Time { return now }}
	cr := gcRotateLog(logPath, cfg.AuditLog, "audit-log", opts)

	if len(cr.Errors) > 0 {
		t.Errorf("unexpected errors: %v", cr.Errors)
	}
	if cr.Compressed == 0 {
		t.Error("expected >=1 compressed")
	}
	// Rotated .gz must exist.
	matches, _ := filepath.Glob(filepath.Join(dir, "audit-*.jsonl.gz"))
	if len(matches) == 0 {
		t.Error("expected rotated .gz file")
	}
	// Original file must still exist (truncated).
	fi, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("original log must still exist: %v", err)
	}
	if fi.Size() != 0 {
		t.Errorf("original log should be truncated: got size %d", fi.Size())
	}
}

// TestGCRotateLogRetention verifies old rotated files are pruned.
func TestGCRotateLogRetention(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")
	// Create empty base log.
	if err := os.WriteFile(logPath, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}

	// Create an old rotated file.
	oldRotated := filepath.Join(dir, "audit-20260101.jsonl.gz")
	if err := os.WriteFile(oldRotated, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-400 * 24 * time.Hour)
	if err := os.Chtimes(oldRotated, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	pol := RotatePolicy{RotateSizeMB: 10, RetainDays: 365}
	now := time.Now()
	opts := Options{DryRun: false, Config: &Config{}, Now: func() time.Time { return now }}
	cr := gcRotateLog(logPath, pol, "audit-log", opts)

	if cr.Deleted == 0 {
		t.Error("expected old rotated file to be deleted")
	}
	if _, err := os.Stat(oldRotated); !os.IsNotExist(err) {
		t.Error("old rotated file should have been deleted")
	}
}

func TestGCRotateLogSerializesConcurrentAppend(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.jsonl")
	if err := os.WriteFile(logPath, make([]byte, 1024*1024), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	previous := logArchiveCopy
	logArchiveCopy = func(dst io.Writer, src io.Reader) (int64, error) {
		close(entered)
		<-release
		return io.Copy(dst, src)
	}
	t.Cleanup(func() { logArchiveCopy = previous })

	rotateDone := make(chan ClassResult, 1)
	go func() {
		rotateDone <- gcRotateLog(logPath, RotatePolicy{RotateSizeMB: 1}, "audit-log", Options{})
	}()
	<-entered
	appendDone := make(chan error, 1)
	go func() {
		appendDone <- fsx.AppendLinePerm(logPath, []byte(`{"after":"rotation"}`), 0o600)
	}()
	select {
	case err := <-appendDone:
		t.Fatalf("append bypassed rotation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if result := <-rotateDone; len(result.Errors) != 0 {
		t.Fatalf("rotation errors: %v", result.Errors)
	}
	if err := <-appendDone; err != nil {
		t.Fatalf("append after rotation: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "{\"after\":\"rotation\"}\n" {
		t.Fatalf("active log after rotation = %q", raw)
	}
}
