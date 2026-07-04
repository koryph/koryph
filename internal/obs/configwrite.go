// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SaveConfig writes cfg to the canonical observability.json path, creating
// the parent directory if necessary. The file is written atomically via a
// temp-file rename so a concurrent reader always sees a complete JSON object.
func SaveConfig(cfg Config) error {
	path := configFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("obs: create config dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("obs: marshal config: %w", err)
	}
	data = append(data, '\n')

	// Atomic write: write to a temp file in the same directory, then rename.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".observability-*.json.tmp")
	if err != nil {
		return fmt.Errorf("obs: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	if _, werr := tmp.Write(data); werr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("obs: write config: %w", werr)
	}
	if cerr := tmp.Close(); cerr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("obs: close temp file: %w", cerr)
	}
	if rerr := os.Rename(tmpName, path); rerr != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("obs: rename config: %w", rerr)
	}
	return nil
}

// SetComponentLevel loads the current config, sets the given component's
// level to levelStr, and saves it back. The level name is validated before
// writing; if the component map is nil it is created.
func SetComponentLevel(component, levelStr string) (Config, error) {
	if _, ok := ParseLevel(levelStr); !ok {
		return Config{}, fmt.Errorf("obs: unknown level %q (want trace|debug|info|warn|error)", levelStr)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	if cfg.Components == nil {
		cfg.Components = make(map[string]string)
	}
	cfg.Components[component] = levelStr
	return cfg, SaveConfig(cfg)
}

// SetDefaultLevel loads the current config, sets DefaultLevel, and saves.
func SetDefaultLevel(levelStr string) (Config, error) {
	if _, ok := ParseLevel(levelStr); !ok {
		return Config{}, fmt.Errorf("obs: unknown level %q (want trace|debug|info|warn|error)", levelStr)
	}
	cfg, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	cfg.DefaultLevel = levelStr
	return cfg, SaveConfig(cfg)
}

// EnableObs sets the default level to "info" if it is currently "error"
// (i.e. previously disabled), otherwise it is a no-op on the level. It always
// saves the config so callers can treat it as an idempotent enable.
func EnableObs() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	if cfg.DefaultLevel == "error" || cfg.DefaultLevel == "" {
		cfg.DefaultLevel = "info"
	}
	return cfg, SaveConfig(cfg)
}

// DisableObs sets the default level to "error" and all component levels to
// "error", effectively silencing all output. Live loops pick this up at the
// next tick (no restart needed).
func DisableObs() (Config, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return cfg, err
	}
	cfg.DefaultLevel = "error"
	for k := range cfg.Components {
		cfg.Components[k] = "error"
	}
	return cfg, SaveConfig(cfg)
}
