// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// LedgerWatcher — one per visible project. Reads and watches the latest run's
// ledger at <repo>/.plan-logs/koryph/latest/ledger.json (via the `latest`
// symlink the engine maintains). Parses ledger.Run schema v2; unknown/newer
// versions degrade to raw JSON (Decision 2, §1). The engine is the only writer.
//
// The `latest` symlink is repointed to a new run id at each run start, so the
// watcher targets the koryph run root directory (not the resolved file): a
// directory watch catches both the symlink flip and in-run ledger rewrites.

import { readJSON } from './json';
import { koryphRoot, latestLedger } from './paths';
import { LEDGER_SCHEMA_VERSION, Parsed, Run, guardSchemaVersion } from './schema';
import { PathWatcher, WatchOptions } from './watcher';

/** A run ledger with its schema-version guard result. */
export type ParsedRun = Parsed<Run>;

export class LedgerWatcher {
  private readonly watcher: PathWatcher;
  private readonly listeners = new Set<() => void>();

  constructor(
    private readonly repoRoot: string,
    watchOpts: WatchOptions = {},
  ) {
    // Watch the run root recursively so both the `latest` flip and the nested
    // ledger.json rewrites are observed.
    this.watcher = new PathWatcher(koryphRoot(repoRoot), { recursive: true, ...watchOpts });
    this.watcher.onChange(() => this.emit());
  }

  /** The ledger file path resolved through the `latest` symlink. */
  get ledgerPath(): string {
    return latestLedger(this.repoRoot);
  }

  onChange(listener: () => void): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /**
   * Load the latest run ledger, or undefined when the project has no run yet
   * (or the ledger is unreadable). Newer schema versions degrade rather than
   * throw; callers inspect `.known` to decide raw vs typed display.
   */
  async load(): Promise<ParsedRun | undefined> {
    const raw = await readJSON(latestLedger(this.repoRoot));
    if (raw === undefined || typeof raw !== 'object') {
      return undefined;
    }
    return guardSchemaVersion<Run>(raw, raw as Run, LEDGER_SCHEMA_VERSION);
  }

  dispose(): void {
    this.listeners.clear();
    this.watcher.dispose();
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
