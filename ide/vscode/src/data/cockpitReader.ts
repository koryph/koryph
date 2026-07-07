// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// CockpitReader — per-project cockpit data layer for the VS Code extension.
//
// Design constraint (koryph-5ew): the extension MUST consume all agent and
// project state exclusively through `koryph cockpit --json`. This class is the
// single place that makes that CLI call; the AgentThreadsProvider MUST use it
// instead of reading ledger/govern/quota files directly.
//
// Change triggering: CockpitReader watches the project's .plan-logs/koryph
// directory for filesystem changes (the same directory LedgerWatcher used to
// watch). On any change it emits an onChange event. Callers re-read by calling
// snapshot(), which invokes `koryph cockpit --json` to get fresh data assembled
// by cockpit.LedgerProvider.Refresh() — the same path the TUI uses.
//
// Caching: snapshot() caches the last successful result. If the CLI call fails
// the cached result (or undefined for a first-call failure) is returned so the
// tree degrades gracefully rather than clearing on a transient error.

import { CliAdapter } from './cli';
import { koryphRoot } from './paths';
import { CockpitSnapshot } from './schema';
import { PathWatcher, WatchOptions } from './watcher';

export class CockpitReader {
  private readonly watcher: PathWatcher;
  private readonly listeners = new Set<() => void>();
  private cached: CockpitSnapshot | undefined;
  private inflight: Promise<CockpitSnapshot | undefined> | undefined;

  constructor(
    private readonly projectId: string,
    private readonly repoRoot: string,
    private readonly cli: CliAdapter,
    watchOpts: WatchOptions = {},
  ) {
    // Watch the koryph run-log root recursively (same trigger as LedgerWatcher)
    // so we detect both the `latest` symlink flip and in-run ledger rewrites.
    this.watcher = new PathWatcher(koryphRoot(repoRoot), { recursive: true, ...watchOpts });
    this.watcher.onChange(() => this.emit());
  }

  onChange(listener: () => void): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /**
   * Return the latest cockpit snapshot for this project via
   * `koryph cockpit --json --project <id>`. Resolves to undefined on failure
   * (the last successful snapshot is returned as a fallback so the tree
   * degrades gracefully rather than clearing on a transient CLI error).
   *
   * Concurrent calls share the same in-flight request.
   */
  async snapshot(): Promise<CockpitSnapshot | undefined> {
    // Coalesce concurrent callers onto one in-flight request.
    if (this.inflight) {
      return this.inflight;
    }
    const req = this.cli.cockpit(this.projectId).then(
      (snap) => {
        this.cached = snap;
        return snap;
      },
      (_err) => {
        // CLI failed — return the last good snapshot (or undefined on cold start).
        return this.cached;
      },
    );
    this.inflight = req;
    try {
      return await req;
    } finally {
      this.inflight = undefined;
    }
  }

  /** The last successfully fetched snapshot, or undefined if none yet. */
  get last(): CockpitSnapshot | undefined {
    return this.cached;
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
