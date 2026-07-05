// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
)

// checkObs runs three sub-checks:
//  1. obs config valid — observability.json is parseable and all level strings
//     are recognised (skipped if the file does not exist).
//  2. telemetry dir writable — ~/.koryph/telemetry/ can be created / written
//     (a missing dir is only a warn, not an error — it is created lazily).
//  3. telemetry rotation healthy — no single JSONL file exceeds 50 MB, and no
//     files are older than 30 days (soft limits; values may be tightened once
//     rotation logic ships in §O6).
func checkObs(opts Options) []Finding {
	var out []Finding
	out = append(out, checkObsConfig(opts))
	out = append(out, checkObsTelemetryDir(opts))
	out = append(out, checkObsRotation(opts))
	return out
}

// checkObsConfig verifies that observability.json (if it exists) is valid JSON
// and that every level value is a recognised level string.
func checkObsConfig(opts Options) Finding {
	configPath := filepath.Join(opts.home(), "observability.json")
	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelOK,
			Message: "obs config: observability.json absent (defaults used)",
		}
	}
	if err != nil {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs config: cannot read observability.json: %v", err),
		}
	}

	cfg, err := obs.ParseConfigBytes(data)
	if err != nil {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs config: invalid JSON in observability.json: %v", err),
		}
	}

	// Validate all level strings.
	var badLevels []string
	if cfg.DefaultLevel != "" {
		if _, ok := obs.ParseLevel(cfg.DefaultLevel); !ok {
			badLevels = append(badLevels, fmt.Sprintf("default_level=%q", cfg.DefaultLevel))
		}
	}
	compNames := make([]string, 0, len(cfg.Components))
	for k := range cfg.Components {
		compNames = append(compNames, k)
	}
	sort.Strings(compNames)
	for _, name := range compNames {
		lvl := cfg.Components[name]
		if _, ok := obs.ParseLevel(lvl); !ok {
			badLevels = append(badLevels, fmt.Sprintf("components.%s=%q", name, lvl))
		}
	}
	if len(badLevels) > 0 {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs config: unknown level(s): %s (want trace|debug|info|warn|error)", strings.Join(badLevels, ", ")),
		}
	}

	return Finding{
		Check:   checkNameObs,
		Level:   LevelOK,
		Message: fmt.Sprintf("obs config: valid (default_level=%s)", cfg.DefaultLevel),
	}
}

// checkObsTelemetryDir verifies that the telemetry directory exists and is
// writable. A missing directory is a WARN (created lazily on first write).
func checkObsTelemetryDir(opts Options) Finding {
	telDir := filepath.Join(opts.home(), "telemetry")

	fi, err := os.Stat(telDir)
	if os.IsNotExist(err) {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelWarn,
			Message: fmt.Sprintf("obs telemetry: dir %s does not exist (will be created lazily; run `koryph init` to create now)", paths.TelemetryDir()),
		}
	}
	if err != nil {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs telemetry: stat %s: %v", telDir, err),
		}
	}
	if !fi.IsDir() {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs telemetry: %s exists but is not a directory", telDir),
		}
	}

	// Probe writability by creating and immediately removing a temp file.
	tmp, werr := os.CreateTemp(telDir, ".doctor-probe-*.tmp")
	if werr != nil {
		return Finding{
			Check:   checkNameObs,
			Level:   LevelError,
			Message: fmt.Sprintf("obs telemetry: dir %s is not writable: %v", telDir, werr),
		}
	}
	_ = tmp.Close()
	_ = os.Remove(tmp.Name())

	return Finding{
		Check:   checkNameObs,
		Level:   LevelOK,
		Message: fmt.Sprintf("obs telemetry: %s writable", telDir),
	}
}

// checkObsRotation checks for oversized or stale JSONL telemetry files.
// Thresholds match the defaults used by the fileWriter (50 MB / 30 days).
// Users can tune them via telemetry_max_size_mb / telemetry_retention_days in
// observability.json; run `koryph obs prune` to remove stale files on demand.
const (
	obsRotationMaxBytes = 50 * 1024 * 1024 // 50 MB
	obsRotationMaxDays  = 30
)

func checkObsRotation(opts Options) Finding {
	telDir := filepath.Join(opts.home(), "telemetry")
	entries, err := os.ReadDir(telDir)
	if os.IsNotExist(err) {
		// No dir yet — rotation check is moot (covered by telemetry-dir check).
		return Finding{Check: checkNameObs, Level: LevelOK, Message: "obs rotation: no telemetry yet"}
	}
	if err != nil {
		return Finding{Check: checkNameObs, Level: LevelWarn,
			Message: fmt.Sprintf("obs rotation: cannot read telemetry dir: %v", err)}
	}

	now := opts.now()
	var oversized, stale []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		path := filepath.Join(telDir, e.Name())
		fi, serr := os.Stat(path)
		if serr != nil {
			continue
		}
		if fi.Size() > obsRotationMaxBytes {
			oversized = append(oversized, fmt.Sprintf("%s (%.1f MB)", e.Name(), float64(fi.Size())/1024/1024))
		}
		age := now.Sub(fi.ModTime())
		if age.Hours() > float64(obsRotationMaxDays)*24 {
			stale = append(stale, fmt.Sprintf("%s (%.0f days)", e.Name(), age.Hours()/24))
		}
	}

	switch {
	case len(oversized) > 0 && len(stale) > 0:
		return Finding{Check: checkNameObs, Level: LevelWarn,
			Message: fmt.Sprintf("obs rotation: oversized: [%s]; stale: [%s] — run `koryph obs prune` to clean up",
				strings.Join(oversized, ", "), strings.Join(stale, ", "))}
	case len(oversized) > 0:
		return Finding{Check: checkNameObs, Level: LevelWarn,
			Message: fmt.Sprintf("obs rotation: oversized files: %s — run `koryph obs prune` to clean up", strings.Join(oversized, ", "))}
	case len(stale) > 0:
		return Finding{Check: checkNameObs, Level: LevelWarn,
			Message: fmt.Sprintf("obs rotation: stale files: %s — run `koryph obs prune` to clean up", strings.Join(stale, ", "))}
	default:
		return Finding{Check: checkNameObs, Level: LevelOK, Message: "obs rotation: healthy"}
	}
}
