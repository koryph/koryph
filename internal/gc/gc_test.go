// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gc

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
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
		"gc_auto": true
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
