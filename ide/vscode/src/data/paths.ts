// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Resolves koryph's machine-local state locations, mirroring
// internal/paths/paths.go. KORYPH_HOME overrides the default ~/.koryph (the
// same override the Go engine and test fixtures honour), so the extension and
// its tests can point at a fixture tree.

import * as os from 'os';
import * as path from 'path';

/**
 * The central state directory. Honours KORYPH_HOME exactly like
 * paths.KoryphHome(); falls back to ~/.koryph, or ".koryph" when the home dir
 * is unresolvable (matching the Go fallback).
 *
 * @param env process environment (injectable for tests).
 */
export function koryphHome(env: NodeJS.ProcessEnv = process.env): string {
  const override = env.KORYPH_HOME;
  if (override && override.length > 0) {
    return override;
  }
  const home = os.homedir();
  if (!home) {
    return '.koryph';
  }
  return path.join(home, '.koryph');
}

/** registry.d — one JSON record per managed project. */
export function registryDir(env?: NodeJS.ProcessEnv): string {
  return path.join(koryphHome(env), 'registry.d');
}

/** quota — per-account governor state. */
export function quotaDir(env?: NodeJS.ProcessEnv): string {
  return path.join(koryphHome(env), 'quota');
}

/** slots — the global concurrency governor's agent leases. */
export function slotsDir(env?: NodeJS.ProcessEnv): string {
  return path.join(koryphHome(env), 'slots');
}

/** slots/demand — per-project demand heartbeats. */
export function demandDir(env?: NodeJS.ProcessEnv): string {
  return path.join(slotsDir(env), 'demand');
}

/** governor.json — the machine-wide concurrency governor config file. */
export function governorConfig(env?: NodeJS.ProcessEnv): string {
  return path.join(koryphHome(env), 'governor.json');
}

/** audit.jsonl — the append-only account/dispatch audit trail. */
export function auditLog(env?: NodeJS.ProcessEnv): string {
  return path.join(koryphHome(env), 'audit.jsonl');
}

/** <repo>/.plan-logs — a project's run/log root. */
export function planLogs(repoRoot: string): string {
  return path.join(repoRoot, '.plan-logs');
}

/** <repo>/.plan-logs/koryph — a project's koryph run directory root. */
export function koryphRoot(repoRoot: string): string {
  return path.join(planLogs(repoRoot), 'koryph');
}

/**
 * The per-project ledger reached via the `latest` symlink:
 * <repo>/.plan-logs/koryph/latest/ledger.json. The engine maintains `latest`
 * as a relative symlink to the newest run id (see ledger/store.go).
 */
export function latestLedger(repoRoot: string): string {
  return path.join(koryphRoot(repoRoot), 'latest', 'ledger.json');
}

/** The phase directory for one slot within a resolved run. */
export function phaseDir(repoRoot: string, runID: string, phaseID: string): string {
  return path.join(koryphRoot(repoRoot), runID, phaseID);
}

/** The per-slot transcript stream (claude stream-json). */
export function streamFile(repoRoot: string, runID: string, phaseID: string): string {
  return path.join(phaseDir(repoRoot, runID, phaseID), 'stream.jsonl');
}
