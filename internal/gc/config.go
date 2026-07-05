// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package gc implements data lifecycle management for koryph outputs.
// It provides configurable retention, compression, and archival policies
// for four artifact classes:
//
//   - run phase-dirs: compress whole run dir to tar.gz after N days, delete
//     after M days. manifest.json stays uncompressed beside the archive;
//     the 'latest' symlink and any ACTIVE run are always exempt.
//   - audit.jsonl + runs.jsonl: size-based rotation at R MB to
//     <name>-<date>.jsonl.gz; default retention FOREVER (audit trails).
//   - telemetry: managed by internal/obs (obs.PruneFromConfig).
//   - posture snapshots: EXEMPT by design (koryph-vud: never auto-deleted).
//
// The single config surface is ~/.koryph/retention.json (global) with
// per-project overrides in <repo>/.koryph/retention.json.
// "never" is supported everywhere as a retention value.
//
// gc refuses to touch any run whose ledger shows non-terminal slots.
package gc

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/paths"
)

// never is the sentinel string meaning "retain forever".
const never = "never"

// Config is the retention policy configuration.
// All fields are optional; zero/empty values use documented defaults.
type Config struct {
	// RunDirs configures retention for run phase-directories
	// (<repo>/.plan-logs/koryph/<run-id>/).
	RunDirs RunDirPolicy `json:"run_dirs,omitempty"`

	// AuditLog configures rotation for ~/.koryph/audit.jsonl.
	AuditLog RotatePolicy `json:"audit_log,omitempty"`

	// RunsIndex configures rotation for ~/.koryph/runs.jsonl.
	RunsIndex RotatePolicy `json:"runs_index,omitempty"`

	// FootprintWarnGB is the pending-GC footprint threshold (in GiB) above
	// which the health patrol emits a WARN. Values <= 0 default to 1 GiB.
	FootprintWarnGB float64 `json:"footprint_warn_gb,omitempty"`

	// GCAuto, when true, allows the health patrol to run gc opportunistically
	// on each patrol tick. Default FALSE -- automatic deletion is opt-in.
	GCAuto bool `json:"gc_auto,omitempty"`
}

// RunDirPolicy controls archival + deletion of run phase-directories.
type RunDirPolicy struct {
	// CompressAfterDays is the age in days after which a completed run dir
	// is compressed into a .tar.gz archive. 0 means "use default (7)".
	// Supports "never" in JSON to disable compression.
	CompressAfterDays int `json:"compress_after_days,omitempty"`

	// CompressAfterDaysNever, when true, disables compression regardless of
	// CompressAfterDays. Set via "compress_after_days": "never" in JSON.
	CompressAfterDaysNever bool `json:"-"`

	// DeleteAfterDays is the age in days after which a compressed (or plain)
	// run dir is deleted. 0 means "use default (90)".
	// Supports "never" in JSON to disable deletion.
	DeleteAfterDays int `json:"delete_after_days,omitempty"`

	// DeleteAfterDaysNever, when true, disables deletion.
	DeleteAfterDaysNever bool `json:"-"`
}

// RotatePolicy controls size-based rotation + retention of append-only JSONL logs.
type RotatePolicy struct {
	// RotateSizeMB is the file size in MiB at which the log is rotated to
	// <name>-<date>.jsonl.gz. 0 means "use default (10 MiB)".
	RotateSizeMB int `json:"rotate_size_mb,omitempty"`

	// RetainDays is the number of days to keep rotated files.
	// 0 means "never delete" (the default -- audit trails).
	// Supports "never" in JSON.
	RetainDays int `json:"retain_days,omitempty"`

	// RetainDaysNever, when true, forces "never delete" regardless of RetainDays.
	RetainDaysNever bool `json:"-"`
}

// runDirPolicyJSON is used for custom JSON unmarshalling to support the
// "never" string for CompressAfterDays / DeleteAfterDays.
type runDirPolicyJSON struct {
	CompressAfterDays json.RawMessage `json:"compress_after_days,omitempty"`
	DeleteAfterDays   json.RawMessage `json:"delete_after_days,omitempty"`
}

// rotatePolicyJSON is used for custom JSON unmarshalling to support the
// "never" string for RetainDays.
type rotatePolicyJSON struct {
	RotateSizeMB int             `json:"rotate_size_mb,omitempty"`
	RetainDays   json.RawMessage `json:"retain_days,omitempty"`
}

// configJSON is used for custom unmarshalling.
type configJSON struct {
	RunDirs         json.RawMessage `json:"run_dirs,omitempty"`
	AuditLog        json.RawMessage `json:"audit_log,omitempty"`
	RunsIndex       json.RawMessage `json:"runs_index,omitempty"`
	FootprintWarnGB float64         `json:"footprint_warn_gb,omitempty"`
	GCAuto          bool            `json:"gc_auto,omitempty"`
}

// parseIntOrNever parses a JSON value that may be an integer or the string
// "never". Returns (value, isNever, error).
func parseIntOrNever(raw json.RawMessage) (int, bool, error) {
	if len(raw) == 0 {
		return 0, false, nil
	}
	// Try string "never" first.
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		if s == never {
			return 0, true, nil
		}
	}
	// Try integer.
	var n int
	if err := json.Unmarshal(raw, &n); err == nil {
		return n, false, nil
	}
	return 0, false, nil
}

// UnmarshalJSON implements custom JSON parsing for Config to support
// "never" as a value for integer fields.
func (c *Config) UnmarshalJSON(data []byte) error {
	var raw configJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	c.FootprintWarnGB = raw.FootprintWarnGB
	c.GCAuto = raw.GCAuto

	if len(raw.RunDirs) > 0 {
		var rj runDirPolicyJSON
		if err := json.Unmarshal(raw.RunDirs, &rj); err != nil {
			return err
		}
		v, isNever, _ := parseIntOrNever(rj.CompressAfterDays)
		c.RunDirs.CompressAfterDays = v
		c.RunDirs.CompressAfterDaysNever = isNever

		v, isNever, _ = parseIntOrNever(rj.DeleteAfterDays)
		c.RunDirs.DeleteAfterDays = v
		c.RunDirs.DeleteAfterDaysNever = isNever
	}

	if len(raw.AuditLog) > 0 {
		var rj rotatePolicyJSON
		if err := json.Unmarshal(raw.AuditLog, &rj); err != nil {
			return err
		}
		c.AuditLog.RotateSizeMB = rj.RotateSizeMB
		v, isNever, _ := parseIntOrNever(rj.RetainDays)
		c.AuditLog.RetainDays = v
		c.AuditLog.RetainDaysNever = isNever
	}

	if len(raw.RunsIndex) > 0 {
		var rj rotatePolicyJSON
		if err := json.Unmarshal(raw.RunsIndex, &rj); err != nil {
			return err
		}
		c.RunsIndex.RotateSizeMB = rj.RotateSizeMB
		v, isNever, _ := parseIntOrNever(rj.RetainDays)
		c.RunsIndex.RetainDays = v
		c.RunsIndex.RetainDaysNever = isNever
	}

	return nil
}

// effective returns the Config with all zero values replaced by defaults.
func (c Config) effective() Config {
	if !c.RunDirs.CompressAfterDaysNever && c.RunDirs.CompressAfterDays <= 0 {
		c.RunDirs.CompressAfterDays = 7
	}
	if !c.RunDirs.DeleteAfterDaysNever && c.RunDirs.DeleteAfterDays <= 0 {
		c.RunDirs.DeleteAfterDays = 90
	}
	if c.AuditLog.RotateSizeMB <= 0 {
		c.AuditLog.RotateSizeMB = 10
	}
	// AuditLog RetainDays: 0 means "never" by default (audit trail).
	if c.RunsIndex.RotateSizeMB <= 0 {
		c.RunsIndex.RotateSizeMB = 10
	}
	// RunsIndex RetainDays: 0 means "never" by default.
	if c.FootprintWarnGB <= 0 {
		c.FootprintWarnGB = 1.0
	}
	return c
}

// globalConfigPath returns the path to the global retention config.
func globalConfigPath() string {
	return filepath.Join(paths.KoryphHome(), "retention.json")
}

// projectConfigPath returns the path to the per-project retention config.
func projectConfigPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".koryph", "retention.json")
}

// LoadConfig loads the retention config for the given repo root (may be "").
// If repoRoot is non-empty, a project-level config overlays the global config.
// Missing files are silently treated as empty (defaults apply).
func LoadConfig(repoRoot string) (Config, error) {
	cfg := loadOne(globalConfigPath())
	if repoRoot != "" {
		proj := loadOne(projectConfigPath(repoRoot))
		cfg = mergeConfigs(cfg, proj)
	}
	return cfg.effective(), nil
}

// loadOne reads a single retention.json, returning an empty Config on missing.
func loadOne(path string) Config {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}
	}
	return cfg
}

// mergeConfigs overlays src onto dst: non-zero fields in src win.
// "never" flags in src always override.
func mergeConfigs(dst, src Config) Config {
	// RunDirs
	if src.RunDirs.CompressAfterDaysNever {
		dst.RunDirs.CompressAfterDaysNever = true
		dst.RunDirs.CompressAfterDays = 0
	} else if src.RunDirs.CompressAfterDays > 0 {
		dst.RunDirs.CompressAfterDays = src.RunDirs.CompressAfterDays
		dst.RunDirs.CompressAfterDaysNever = false
	}
	if src.RunDirs.DeleteAfterDaysNever {
		dst.RunDirs.DeleteAfterDaysNever = true
		dst.RunDirs.DeleteAfterDays = 0
	} else if src.RunDirs.DeleteAfterDays > 0 {
		dst.RunDirs.DeleteAfterDays = src.RunDirs.DeleteAfterDays
		dst.RunDirs.DeleteAfterDaysNever = false
	}
	// AuditLog
	if src.AuditLog.RotateSizeMB > 0 {
		dst.AuditLog.RotateSizeMB = src.AuditLog.RotateSizeMB
	}
	if src.AuditLog.RetainDaysNever {
		dst.AuditLog.RetainDaysNever = true
		dst.AuditLog.RetainDays = 0
	} else if src.AuditLog.RetainDays > 0 {
		dst.AuditLog.RetainDays = src.AuditLog.RetainDays
		dst.AuditLog.RetainDaysNever = false
	}
	// RunsIndex
	if src.RunsIndex.RotateSizeMB > 0 {
		dst.RunsIndex.RotateSizeMB = src.RunsIndex.RotateSizeMB
	}
	if src.RunsIndex.RetainDaysNever {
		dst.RunsIndex.RetainDaysNever = true
		dst.RunsIndex.RetainDays = 0
	} else if src.RunsIndex.RetainDays > 0 {
		dst.RunsIndex.RetainDays = src.RunsIndex.RetainDays
		dst.RunsIndex.RetainDaysNever = false
	}
	// Scalars
	if src.FootprintWarnGB > 0 {
		dst.FootprintWarnGB = src.FootprintWarnGB
	}
	if src.GCAuto {
		dst.GCAuto = true
	}
	return dst
}
