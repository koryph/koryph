// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// GovernorReader — the live global slot picture from the machine-wide
// concurrency governor (internal/govern). Reads:
//   - ~/.koryph/governor.json         → cap {max_global_agents} (default 4)
//   - ~/.koryph/slots/<lease>.json     → one lease per running agent
//   - ~/.koryph/slots/demand/<proj>.json → per-project demand heartbeats
//
// Lease files are the cheapest, most truthful "who is live right now" signal
// (§2 badges) because they are keyed to the real agent PID and freed only when
// that process dies. Read-only; the CLI/engine own these files.

import * as path from 'path';
import { listJSON, readJSON } from './json';
import { demandDir, governorConfig, slotsDir } from './paths';
import { DEFAULT_MAX_GLOBAL_AGENTS, Demand, GovernorConfig, Lease } from './schema';
import { PathWatcher, WatchOptions } from './watcher';

/** A snapshot of the global governor state. */
export interface GovernorSnapshot {
  /** Machine-wide cap (governor.json, or the default when absent). */
  maxGlobalAgents: number;
  /** True when governor.json was present (vs. defaulted). */
  configPresent: boolean;
  /** Active leases (one per running agent), sorted by acquired_at. */
  leases: Lease[];
  /** Live per-project demand heartbeats. */
  demands: Demand[];
}

export class GovernorReader {
  private readonly slotsWatcher: PathWatcher;
  private readonly configWatcher: PathWatcher;
  private readonly listeners = new Set<() => void>();

  constructor(
    private readonly env: NodeJS.ProcessEnv = process.env,
    watchOpts: WatchOptions = {},
  ) {
    // A recursive slots watch covers both leases and demand/ heartbeats.
    this.slotsWatcher = new PathWatcher(slotsDir(env), { recursive: true, ...watchOpts });
    this.configWatcher = new PathWatcher(governorConfig(env), watchOpts);
    this.slotsWatcher.onChange(() => this.emit());
    this.configWatcher.onChange(() => this.emit());
  }

  onChange(listener: () => void): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /** Read the machine-wide cap (default when governor.json is absent). */
  async cap(): Promise<{ max: number; present: boolean }> {
    const raw = await readJSON(governorConfig(this.env));
    if (raw && typeof raw === 'object') {
      const cfg = raw as GovernorConfig;
      if (typeof cfg.max_global_agents === 'number' && cfg.max_global_agents > 0) {
        return { max: cfg.max_global_agents, present: true };
      }
    }
    return { max: DEFAULT_MAX_GLOBAL_AGENTS, present: false };
  }

  /** All active leases (skips demand/ and malformed files). */
  async leases(): Promise<Lease[]> {
    const dir = slotsDir(this.env);
    const names = await listJSON(dir);
    const out: Lease[] = [];
    for (const name of names) {
      const raw = await readJSON(path.join(dir, name));
      if (isLease(raw)) {
        out.push(raw);
      }
    }
    return out.sort((a, b) => a.acquired_at.localeCompare(b.acquired_at));
  }

  /** All live per-project demand heartbeats. */
  async demands(): Promise<Demand[]> {
    const dir = demandDir(this.env);
    const names = await listJSON(dir);
    const out: Demand[] = [];
    for (const name of names) {
      const raw = await readJSON(path.join(dir, name));
      if (isDemand(raw)) {
        out.push(raw);
      }
    }
    return out.sort((a, b) => a.project.localeCompare(b.project));
  }

  /** A complete snapshot in one call (cap + leases + demands). */
  async snapshot(): Promise<GovernorSnapshot> {
    const [{ max, present }, leases, demands] = await Promise.all([
      this.cap(),
      this.leases(),
      this.demands(),
    ]);
    return { maxGlobalAgents: max, configPresent: present, leases, demands };
  }

  /** Count of live agents for a project (or across all projects). */
  async liveCount(projectID?: string): Promise<number> {
    const leases = await this.leases();
    if (!projectID) {
      return leases.length;
    }
    return leases.filter((l) => l.project === projectID).length;
  }

  dispose(): void {
    this.listeners.clear();
    this.slotsWatcher.dispose();
    this.configWatcher.dispose();
  }

  private emit(): void {
    for (const listener of [...this.listeners]) {
      try {
        listener();
      } catch {
        /* isolate subscriber faults */
      }
    }
  }
}

function isLease(raw: unknown): raw is Lease {
  return (
    !!raw &&
    typeof raw === 'object' &&
    typeof (raw as Lease).project === 'string' &&
    typeof (raw as Lease).pid === 'number' &&
    typeof (raw as Lease).acquired_at === 'string'
  );
}

function isDemand(raw: unknown): raw is Demand {
  return (
    !!raw &&
    typeof raw === 'object' &&
    typeof (raw as Demand).project === 'string' &&
    typeof (raw as Demand).engine_pid === 'number' &&
    typeof (raw as Demand).updated_at === 'string'
  );
}
