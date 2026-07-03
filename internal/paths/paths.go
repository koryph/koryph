// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package paths resolves koryph's machine-local state locations.
// All central state lives under KoryphHome (default ~/.koryph),
// which is itself a git repository so every registry/audit change is a commit.
package paths

import (
	"fmt"
	"hash/fnv"
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

// HooksDir holds the enforcement hook scripts (agent-boundary/worktree guards),
// installed OUTSIDE any agent's writable worktree so a dispatched agent cannot
// neuter its own guards. Referenced from .claude/settings.json via
// ${KORYPH_HOME:-$HOME/.koryph}/hooks/<name>.
func HooksDir() string { return filepath.Join(KoryphHome(), "hooks") }

// SigningDir holds koryph's scoped signing-agent socket. It lives under a short,
// per-user temp path (0700) rather than KoryphHome: a Unix domain socket path
// caps at ~104 chars, and the socket is ephemeral runtime IPC (recreated by
// `koryph signing enable`), not persistent state. Keyed by a hash of KoryphHome
// so distinct homes (and test fixtures) never collide.
func SigningDir() string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(KoryphHome()))
	dir := filepath.Join(os.TempDir(), fmt.Sprintf("koryph-%d-%08x", os.Getuid(), h.Sum32()))
	return dir
}

// SigningAgentSock is the koryph-managed ssh-agent socket that holds ONLY the
// commit-signing key, isolated from the operator's ambient SSH_AUTH_SOCK.
// Dispatched agents receive this socket so they can sign commits without gaining
// access to the operator's other keys.
func SigningAgentSock() string { return filepath.Join(SigningDir(), "signing.sock") }

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
