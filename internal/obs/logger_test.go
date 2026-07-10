// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRegistryFor verifies that For() returns the same *slog.Logger on
// repeated calls for the same component.
func TestRegistryFor(t *testing.T) {
	var buf bytes.Buffer
	root := NewTextHandler(&buf, slog.LevelDebug)
	cfg := defaultConfig()
	w := NewWatcher(cfg)
	reg := NewRegistry(NewRedactingHandler(root), w)

	a := reg.For("engine")
	b := reg.For("engine")
	if a != b {
		t.Error("For(engine) returned different logger on second call")
	}
	c := reg.For("sched")
	if a == c {
		t.Error("For(sched) should return a different logger than For(engine)")
	}
}

// TestRegistrySetLevel verifies per-component level gating.
func TestRegistrySetLevel(t *testing.T) {
	var buf bytes.Buffer
	root := NewTextHandler(&buf, slog.LevelDebug)
	cfg := defaultConfig()
	w := NewWatcher(cfg)
	reg := NewRegistry(NewRedactingHandler(root), w)

	// Set engine to WARN; debug should be suppressed.
	reg.SetLevel("engine", slog.LevelWarn)
	l := reg.For("engine")

	buf.Reset()
	l.Debug("should be suppressed")
	if buf.Len() != 0 {
		t.Errorf("debug log leaked through WARN gate: %q", buf.String())
	}

	buf.Reset()
	l.Warn("should appear")
	if buf.Len() == 0 {
		t.Error("warn log suppressed by WARN gate")
	}
	if !strings.Contains(buf.String(), "should appear") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

// TestRegistrySync verifies that Sync() propagates config changes to existing
// component loggers without recreating them.
func TestRegistrySync(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	t.Setenv("KORYPH_LOG_LEVEL", "")
	t.Setenv("KORYPH_LOG_FORMAT", "")
	t.Setenv("KORYPH_OTEL_ENDPOINT", "")

	var buf bytes.Buffer
	root := NewTextHandler(&buf, slog.LevelDebug)
	cfg, _ := LoadConfig() // info default
	w := NewWatcher(cfg)
	reg := NewRegistry(NewRedactingHandler(root), w)

	// First call at INFO — debug should be suppressed.
	eng := reg.For("engine")
	buf.Reset()
	eng.Debug("before sync")
	if buf.Len() != 0 {
		t.Errorf("debug before sync leaked: %q", buf.String())
	}

	// Push DEBUG via Reload.
	t.Setenv("KORYPH_LOG_LEVEL", "debug")
	if _, err := w.Reload(); err != nil {
		t.Fatal(err)
	}
	reg.Sync()

	buf.Reset()
	eng.Debug("after sync")
	if buf.Len() == 0 {
		t.Error("debug after sync suppressed")
	}
}

// TestReloadConfigPicksUpDiskChange locks the contract the engine's per-tick
// syncObsConfig relies on: the package-level ReloadConfig re-reads
// observability.json from disk and applies a new component level to a live
// logger, so a mid-run `koryph obs level <component> debug` takes effect on the
// next scheduler tick without a restart (the "no restart needed" promise).
func TestReloadConfigPicksUpDiskChange(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	// Scrub env-level overrides so the on-disk file is the sole level source.
	t.Setenv("KORYPH_LOG_LEVEL", "")

	var buf bytes.Buffer
	ReInit(defaultConfig(), NewTextHandler(&buf, LevelTrace))
	eng := For("engine")

	buf.Reset()
	eng.Debug("before reload") // default info → suppressed
	if buf.Len() != 0 {
		t.Fatalf("debug before reload leaked: %q", buf.String())
	}

	// Operator raises engine to debug on disk (what `koryph obs level` writes).
	obsPath := filepath.Join(home, "observability.json")
	if err := os.WriteFile(obsPath, []byte(`{"components":{"engine":"debug"}}`), 0o644); err != nil {
		t.Fatalf("write observability.json: %v", err)
	}
	if _, err := ReloadConfig(); err != nil {
		t.Fatalf("ReloadConfig: %v", err)
	}

	buf.Reset()
	eng.Debug("after reload")
	if buf.Len() == 0 {
		t.Error("debug suppressed after ReloadConfig — a mid-run obs level change would not take effect in the engine loop")
	}
}

// TestRedactingHandlerViaLogger verifies that secrets don't reach the handler.
func TestRedactingHandlerViaLogger(t *testing.T) {
	var buf bytes.Buffer
	root := NewJSONHandler(&buf, LevelTrace)
	cfg := defaultConfig()
	w := NewWatcher(cfg)
	rh := NewRedactingHandler(root)
	reg := NewRegistry(rh, w)
	reg.SetLevel("vault", LevelTrace)
	l := reg.For("vault")

	buf.Reset()
	// Build token at runtime so gitleaks does not flag this file.
	fakeToken := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ01234"
	l.Info("resolving key", slog.String("token", fakeToken))
	out := buf.String()
	if strings.Contains(out, "ghp_") {
		t.Errorf("token leaked to output: %s", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Errorf("REDACTED marker not found in output: %s", out)
	}
}

// TestTraceFunc verifies the package-level Trace() helper.
func TestTraceFunc(t *testing.T) {
	var buf bytes.Buffer
	root := NewTextHandler(&buf, LevelTrace)
	cfg := defaultConfig()
	cfg.DefaultLevel = "trace"

	// Use ReInit so the global registry is reset for this test.
	ReInit(cfg, root)

	buf.Reset()
	Trace("engine", "tick at trace level")
	if buf.Len() == 0 {
		t.Error("Trace() produced no output")
	}
	if !strings.Contains(buf.String(), "tick at trace level") {
		t.Errorf("unexpected trace output: %q", buf.String())
	}
}
