// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/account"
)

// writeTranscript writes a projects/x/y.jsonl fixture under root with 3 lines:
// two in the current window (opus + sonnet) and one well outside it.
func writeTranscript(t *testing.T, root string) {
	t.Helper()
	dir := filepath.Join(root, "projects", "x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().UTC().Add(-10 * time.Hour).Format(time.RFC3339)

	// opus (nested message shape): 1M each of in/out/cacheWrite/cacheRead.
	//   15 + 75 + 18.75 + 1.5 = 110.25
	line1 := fmt.Sprintf(`{"timestamp":%q,"message":{"model":"claude-opus-4-8","usage":{"input_tokens":1000000,"output_tokens":1000000,"cache_creation_input_tokens":1000000,"cache_read_input_tokens":1000000}}}`, now)
	// sonnet (top-level shape): 1M in + 1M out = 3 + 15 = 18
	line2 := fmt.Sprintf(`{"timestamp":%q,"model":"claude-sonnet-4-5","usage":{"input_tokens":1000000,"output_tokens":1000000}}`, now)
	// out-of-window opus, must be excluded.
	line3 := fmt.Sprintf(`{"timestamp":%q,"message":{"model":"claude-opus-4-8","usage":{"input_tokens":1000000,"output_tokens":1000000}}}`, old)

	body := strings.Join([]string{line1, line2, line3}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "y.jsonl"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLScan(t *testing.T) {
	root := t.TempDir()
	writeTranscript(t, root)

	got, err := JSONLScan(root, 5)
	if err != nil {
		t.Fatalf("JSONLScan: %v", err)
	}
	// 110.25 (opus) + 18 (sonnet); old line excluded.
	const want = 128.25
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("JSONLScan = %g, want %g", got, want)
	}
}

func TestJSONLScanNoFiles(t *testing.T) {
	if _, err := JSONLScan(t.TempDir(), 5); err == nil {
		t.Fatal("JSONLScan with no transcripts should error (→ unavailable)")
	}
}

func TestFiveHourWindowStart(t *testing.T) {
	epoch := time.Unix(0, 0).UTC()
	const grid = 5 * time.Hour

	now := time.Date(2026, 7, 2, 13, 42, 0, 0, time.UTC)
	got := fiveHourWindowStart(now)

	// Invariants of a fixed epoch-aligned 5h grid (24h isn't a 5h multiple, so
	// the boundary drifts across days — no fixed clock time to assert).
	if got.After(now) {
		t.Fatalf("window start %s is after now %s", got, now)
	}
	if d := now.Sub(got); d < 0 || d >= grid {
		t.Fatalf("now-start = %s, want in [0, 5h)", d)
	}
	if off := got.Sub(epoch) % grid; off != 0 {
		t.Fatalf("start %s is not on the 5h grid (offset %s)", got, off)
	}
	// The Unix epoch itself is a grid boundary.
	if s := fiveHourWindowStart(epoch); !s.Equal(epoch) {
		t.Fatalf("epoch window start = %s, want epoch", s)
	}
}

// fakeCcusage installs an executable `ccusage` on PATH that logs its
// CLAUDE_CONFIG_DIR and emits canned JSON. It returns the env-log path.
func fakeCcusage(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	envLog := filepath.Join(t.TempDir(), "env.log")

	// blocks --active → costUSD 12.5 ; daily → last-7 of 8 entries = 2..8 = 35
	script := "#!/bin/sh\n" +
		"echo \"CLAUDE_CONFIG_DIR=${CLAUDE_CONFIG_DIR}\" >> \"" + envLog + "\"\n" +
		"case \"$1\" in\n" +
		"  blocks) echo '{\"blocks\":[{\"costUSD\":12.5}]}' ;;\n" +
		"  daily) echo '{\"daily\":[{\"totalCost\":1},{\"totalCost\":2},{\"totalCost\":3},{\"totalCost\":4},{\"totalCost\":5},{\"totalCost\":6},{\"totalCost\":7},{\"totalCost\":8}]}' ;;\n" +
		"esac\n"
	if err := os.WriteFile(filepath.Join(bin, "ccusage"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return envLog
}

func TestSnapshotUsesCcusageAndCarriesConfigDir(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	envLog := fakeCcusage(t)

	workDir := t.TempDir()
	profile := account.Profile{Name: "work", ConfigDir: workDir}
	cfg := calibratedCfg()

	u, err := Snapshot(context.Background(), profile, cfg)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	if u.Window5h.Source != "ccusage" || u.Window5h.Approx {
		t.Fatalf("5h window = %+v, want ccusage/exact", u.Window5h)
	}
	if math.Abs(u.Window5h.SpentUSD-12.5) > 1e-9 {
		t.Fatalf("5h spent = %g, want 12.5", u.Window5h.SpentUSD)
	}
	if u.Weekly.Source != "ccusage" || math.Abs(u.Weekly.SpentUSD-35) > 1e-9 {
		t.Fatalf("weekly = %+v, want ccusage sum 35", u.Weekly)
	}
	if u.Window5h.CeilingUSD != cfg.WindowCeilingUSD {
		t.Fatalf("ceiling not copied from cfg: %g", u.Window5h.CeilingUSD)
	}

	// The child env must have carried the work profile's CLAUDE_CONFIG_DIR.
	logBytes, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatalf("read env log: %v", err)
	}
	if !strings.Contains(string(logBytes), "CLAUDE_CONFIG_DIR="+workDir) {
		t.Fatalf("child env missing CLAUDE_CONFIG_DIR=%s, log:\n%s", workDir, logBytes)
	}
}

func TestSnapshotFallsBackToJSONLScan(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	// No ccusage on PATH and npx disabled → forces the transcript scan.
	t.Setenv("PATH", "")
	t.Setenv("KORYPH_NO_NPX", "1")

	configDir := t.TempDir()
	writeTranscript(t, configDir)
	profile := account.Profile{Name: "work", ConfigDir: configDir}

	u, err := Snapshot(context.Background(), profile, calibratedCfg())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if u.Window5h.Source != "jsonl-scan" || !u.Window5h.Approx {
		t.Fatalf("5h window = %+v, want jsonl-scan/approx", u.Window5h)
	}
	if u.Window5h.SpentUSD <= 0 {
		t.Fatalf("jsonl-scan spent should be > 0, got %g", u.Window5h.SpentUSD)
	}
}

func TestSnapshotUnavailableFailsClosed(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	t.Setenv("PATH", "")
	t.Setenv("KORYPH_NO_NPX", "1")

	// Empty config dir → no ccusage, no transcripts → unavailable.
	profile := account.Profile{Name: "work", ConfigDir: t.TempDir()}
	u, err := Snapshot(context.Background(), profile, calibratedCfg())
	if err != nil {
		t.Fatalf("Snapshot should not error, got %v", err)
	}
	if u.Window5h.Source != "unavailable" {
		t.Fatalf("5h window source = %q, want unavailable", u.Window5h.Source)
	}
	// Fail closed: an unavailable window reports the account at ceiling.
	if f := u.Window5h.Fraction(); f != 1.0 {
		t.Fatalf("unavailable Fraction = %g, want 1.0", f)
	}
}

func TestCalibrate(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	cfg, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// ceiling = observed$ / (observed% / 100) = 50 / 0.40 = 125
	if err := Calibrate(cfg, 50, 40, "5h"); err != nil {
		t.Fatalf("Calibrate: %v", err)
	}
	if math.Abs(cfg.WindowCeilingUSD-125) > 1e-9 {
		t.Fatalf("window ceiling = %g, want 125", cfg.WindowCeilingUSD)
	}
	// Persisted.
	reloaded, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if math.Abs(reloaded.WindowCeilingUSD-125) > 1e-9 {
		t.Fatalf("persisted ceiling = %g, want 125", reloaded.WindowCeilingUSD)
	}
	if err := Calibrate(cfg, 10, 0, "5h"); err == nil {
		t.Fatal("Calibrate with 0%% should error")
	}
}
