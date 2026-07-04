// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfigDefaults(t *testing.T) {
	// Point KORYPH_HOME at an empty temp dir so no file exists.
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	t.Setenv("KORYPH_LOG_LEVEL", "")
	t.Setenv("KORYPH_LOG_FORMAT", "")
	t.Setenv("KORYPH_OTEL_ENDPOINT", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultLevel != "info" {
		t.Errorf("DefaultLevel = %q, want info", cfg.DefaultLevel)
	}
	if cfg.Format != "text" {
		t.Errorf("Format = %q, want text", cfg.Format)
	}
}

func TestLoadConfigFromFile(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	t.Setenv("KORYPH_LOG_LEVEL", "")
	t.Setenv("KORYPH_LOG_FORMAT", "")
	t.Setenv("KORYPH_OTEL_ENDPOINT", "")

	data := map[string]any{
		"default_level": "debug",
		"format":        "json",
		"components": map[string]string{
			"engine": "trace",
			"sched":  "warn",
		},
	}
	b, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(tmp, "observability.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultLevel != "debug" {
		t.Errorf("DefaultLevel = %q, want debug", cfg.DefaultLevel)
	}
	if cfg.ComponentLevel("engine") != LevelTrace {
		t.Errorf("engine level = %v, want TRACE", cfg.ComponentLevel("engine"))
	}
	if cfg.ComponentLevel("sched") != slog.LevelWarn {
		t.Errorf("sched level = %v, want WARN", cfg.ComponentLevel("sched"))
	}
	// Component not listed → DefaultLevel (debug).
	if cfg.ComponentLevel("merge") != slog.LevelDebug {
		t.Errorf("merge level = %v, want DEBUG", cfg.ComponentLevel("merge"))
	}
}

func TestEnvOverride(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	t.Setenv("KORYPH_LOG_LEVEL", "warn")
	t.Setenv("KORYPH_LOG_FORMAT", "json")
	t.Setenv("KORYPH_OTEL_ENDPOINT", "localhost:4317")
	defer func() {
		os.Unsetenv("KORYPH_LOG_LEVEL")
		os.Unsetenv("KORYPH_LOG_FORMAT")
		os.Unsetenv("KORYPH_OTEL_ENDPOINT")
	}()

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.DefaultLevel != "warn" {
		t.Errorf("DefaultLevel = %q, want warn", cfg.DefaultLevel)
	}
	if cfg.Format != "json" {
		t.Errorf("Format = %q, want json", cfg.Format)
	}
	if cfg.OTELEndpoint != "localhost:4317" {
		t.Errorf("OTELEndpoint = %q, want localhost:4317", cfg.OTELEndpoint)
	}
}

func TestWatcherReload(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("KORYPH_HOME", tmp)
	t.Setenv("KORYPH_LOG_LEVEL", "")
	t.Setenv("KORYPH_LOG_FORMAT", "")
	t.Setenv("KORYPH_OTEL_ENDPOINT", "")

	cfg, _ := LoadConfig()
	w := NewWatcher(cfg)

	if w.ComponentLevel("engine") != slog.LevelInfo {
		t.Errorf("initial engine level = %v, want INFO", w.ComponentLevel("engine"))
	}

	// Write a new config and reload.
	data := map[string]any{
		"default_level": "warn",
		"components":    map[string]string{"engine": "debug"},
	}
	b, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(tmp, "observability.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}

	newCfg, err := w.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if newCfg.ComponentLevel("engine") != slog.LevelDebug {
		t.Errorf("after reload engine = %v, want DEBUG", newCfg.ComponentLevel("engine"))
	}
	if w.ComponentLevel("engine") != slog.LevelDebug {
		t.Errorf("watcher engine = %v, want DEBUG", w.ComponentLevel("engine"))
	}
}
