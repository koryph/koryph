// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// Config is the schema for ~/.koryph/observability.json.
// All fields are optional; zero values choose sensible defaults.
type Config struct {
	// DefaultLevel applies to any component not listed in Components.
	// One of: trace, debug, info, warn, error. Default: info.
	DefaultLevel string `json:"default_level,omitempty"`

	// Format selects the output format for the console/file handler:
	// "text" (human-readable, default) or "json".
	Format string `json:"format,omitempty"`

	// OTELEndpoint, when non-empty, enables OTLP export to the given gRPC
	// endpoint (e.g. "localhost:4317"). Overridden by KORYPH_OTEL_ENDPOINT.
	OTELEndpoint string `json:"otel_endpoint,omitempty"`

	// Components holds per-component level overrides. Keys are component
	// names (engine, sched, govern, quota, dispatch, merge, forge, bot, …).
	Components map[string]string `json:"components,omitempty"`

	// File, when non-empty, also writes a JSON log to the named path.
	// The path may use ~ for the home directory.
	File string `json:"file,omitempty"`

	// TelemetryMaxSizeMB is the maximum size in mebibytes of a single JSONL
	// telemetry file before it is rotated.  Values ≤ 0 default to 50 MiB.
	TelemetryMaxSizeMB int `json:"telemetry_max_size_mb,omitempty"`

	// TelemetryRetentionDays is the number of days to retain rotated JSONL
	// telemetry files.  Values ≤ 0 default to 30 days.
	TelemetryRetentionDays int `json:"telemetry_retention_days,omitempty"`
}

// defaultConfig returns the zero-value Config with documented defaults applied.
func defaultConfig() Config {
	return Config{
		DefaultLevel: "info",
		Format:       "text",
	}
}

// envOverride applies KORYPH_LOG_LEVEL, KORYPH_LOG_FORMAT, and
// KORYPH_OTEL_ENDPOINT environment variable overrides, following the design
// doc (§4): env > file > default.
func (c *Config) envOverride() {
	if v := os.Getenv("KORYPH_LOG_LEVEL"); v != "" {
		c.DefaultLevel = v
	}
	if v := os.Getenv("KORYPH_LOG_FORMAT"); v != "" {
		c.Format = v
	}
	if v := os.Getenv("KORYPH_OTEL_ENDPOINT"); v != "" {
		c.OTELEndpoint = v
	}
}

// defaultSlogLevel returns the parsed slog.Level for the config's DefaultLevel
// field, falling back to slog.LevelInfo on parse failure.
func (c Config) defaultSlogLevel() slog.Level {
	l, ok := ParseLevel(c.DefaultLevel)
	if !ok {
		return slog.LevelInfo
	}
	return l
}

// ComponentLevel returns the slog.Level for the named component, or the
// DefaultLevel if the component has no specific override.
func (c Config) ComponentLevel(name string) slog.Level {
	if c.Components != nil {
		if s, ok := c.Components[name]; ok {
			if l, ok := ParseLevel(s); ok {
				return l
			}
		}
	}
	return c.defaultSlogLevel()
}

// configFilePath returns the canonical path to observability.json.
// It checks KORYPH_HOME for test isolation.
func configFilePath() string {
	home := os.Getenv("KORYPH_HOME")
	if home == "" {
		h, err := os.UserHomeDir()
		if err == nil {
			home = filepath.Join(h, ".koryph")
		} else {
			home = ".koryph"
		}
	}
	return filepath.Join(home, "observability.json")
}

// ParseConfigBytes parses a Config from raw JSON bytes without reading any
// file. Env overrides are NOT applied. Used by the doctor package to validate
// a config file's contents without loading it through LoadConfig (which would
// apply env vars and obscure the file's actual state).
func ParseConfigBytes(data []byte) (Config, error) {
	cfg := defaultConfig()
	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), err
	}
	return cfg, nil
}

// LoadConfig reads observability.json from the koryph home directory.
// If the file does not exist, defaultConfig() is returned with no error.
// Env overrides are applied after the file is parsed.
func LoadConfig() (Config, error) {
	cfg := defaultConfig()
	path := configFilePath()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			cfg.envOverride()
			return cfg, nil
		}
		return cfg, err
	}

	if err := json.Unmarshal(data, &cfg); err != nil {
		return defaultConfig(), err
	}
	cfg.envOverride()
	return cfg, nil
}

// Watcher holds the latest Config and supports reload-in-place. The engine
// calls Reload() at each scheduler tick; no goroutine is spawned here.
type Watcher struct {
	mu  sync.RWMutex
	cfg Config
}

// NewWatcher creates a Watcher seeded with cfg.
func NewWatcher(cfg Config) *Watcher {
	return &Watcher{cfg: cfg}
}

// Config returns the current Config snapshot (safe for concurrent reads).
func (w *Watcher) Config() Config {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.cfg
}

// Reload re-reads observability.json and applies env overrides. It replaces
// the stored Config atomically. Returns the new Config and any parse error.
// On error the previous Config is retained unchanged.
func (w *Watcher) Reload() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return w.Config(), err
	}
	w.mu.Lock()
	w.cfg = cfg
	w.mu.Unlock()
	return cfg, nil
}

// ComponentLevel delegates to the current Config.ComponentLevel. Safe for
// concurrent use.
func (w *Watcher) ComponentLevel(name string) slog.Level {
	return w.Config().ComponentLevel(name)
}
