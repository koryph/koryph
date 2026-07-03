// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package paths resolves koryph's machine-local state locations.
// All central state lives under KoryphHome (default ~/.koryph),
// which is itself a git repository so every registry/audit change is a commit.
package paths

import (
	"os"
	"path/filepath"
)

// KoryphHome returns the central state directory. Override with
// KORYPH_HOME (used by tests and fixtures).
func KoryphHome() string {
	if v := os.Getenv("KORYPH_HOME"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".koryph"
	}
	return filepath.Join(home, ".koryph")
}

// RegistryDir holds one JSON record per managed project.
func RegistryDir() string { return filepath.Join(KoryphHome(), "registry.d") }

// QuotaDir holds per-account governor state.
func QuotaDir() string { return filepath.Join(KoryphHome(), "quota") }

// SlotsDir holds the global concurrency governor's agent leases (and, under
// demand/, per-project demand heartbeats).
func SlotsDir() string { return filepath.Join(KoryphHome(), "slots") }

// DemandDir holds per-project demand heartbeats for fair-share allocation.
func DemandDir() string { return filepath.Join(SlotsDir(), "demand") }

// GovernorConfig is the machine-wide concurrency governor config file.
func GovernorConfig() string { return filepath.Join(KoryphHome(), "governor.json") }

// AuditLog is the append-only account/dispatch audit trail.
func AuditLog() string { return filepath.Join(KoryphHome(), "audit.jsonl") }

// RunsIndex is the cross-project index of koryph runs.
func RunsIndex() string { return filepath.Join(KoryphHome(), "runs.jsonl") }

// PlanLogs returns a project's run/log root (repo-local; checkpoints live
// with the work they checkpoint).
func PlanLogs(repoRoot string) string { return filepath.Join(repoRoot, ".plan-logs") }

// KoryphRoot returns a project's koryph run directory root.
func KoryphRoot(repoRoot string) string { return filepath.Join(PlanLogs(repoRoot), "koryph") }
