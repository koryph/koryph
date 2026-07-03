// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// A change-notifier over a path (file or directory) that prefers fs.watch and
// falls back to polling. koryph writes state files atomically (rename) and on
// networked / virtualised filesystems fs.watch can miss or coalesce events, so
// every watcher also polls on an interval; the two together give both low
// latency and correctness (Decision 2: `updated_at` is the change signal, but
// mtime/size drives the cheap poll).
//
// The watcher owns no parsing — subscribers re-read on `onChange`. It is a
// self-contained EventEmitter-free primitive (no vscode dependency) so the
// data layer is unit-testable as plain Node.

import * as fs from 'fs';

export type ChangeListener = () => void;

export interface WatchOptions {
  /** Poll interval in ms (fallback + safety net alongside fs.watch). */
  pollMs?: number;
  /** Force polling only (skip fs.watch); used by tests and flaky filesystems. */
  pollOnly?: boolean;
  /** Watch a directory's entries recursively-shallow (directory targets only). */
  recursive?: boolean;
}

const DEFAULT_POLL_MS = 1000;

/**
 * Watch `target` (a file or directory) and invoke listeners when it changes.
 * Safe to construct on a not-yet-existing path: it polls until the path
 * appears, then upgrades to fs.watch.
 */
export class PathWatcher {
  private readonly listeners = new Set<ChangeListener>();
  private fsWatcher: fs.FSWatcher | undefined;
  private timer: NodeJS.Timeout | undefined;
  private lastSig = '';
  private disposed = false;

  constructor(
    private readonly target: string,
    private readonly opts: WatchOptions = {},
  ) {
    this.lastSig = signature(target);
    this.start();
  }

  /** Register a change listener. Returns a disposer. */
  onChange(listener: ChangeListener): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /** Stop watching and release all resources. */
  dispose(): void {
    this.disposed = true;
    this.listeners.clear();
    if (this.fsWatcher) {
      this.fsWatcher.close();
      this.fsWatcher = undefined;
    }
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = undefined;
    }
  }

  private start(): void {
    const pollMs = this.opts.pollMs ?? DEFAULT_POLL_MS;
    this.timer = setInterval(() => this.poll(), pollMs);
    // Do not keep the event loop alive purely for polling (matters for tests
    // and headless hosts); the extension host holds its own references.
    if (typeof this.timer.unref === 'function') {
      this.timer.unref();
    }
    if (!this.opts.pollOnly) {
      this.tryFsWatch();
    }
  }

  private tryFsWatch(): void {
    if (this.disposed || this.fsWatcher) {
      return;
    }
    try {
      this.fsWatcher = fs.watch(
        this.target,
        { recursive: this.opts.recursive ?? false },
        () => this.poll(),
      );
      this.fsWatcher.on('error', () => this.demoteToPolling());
    } catch {
      // Path may not exist yet, or fs.watch unsupported here — polling covers
      // us and will re-attempt fs.watch once the path materialises.
      this.demoteToPolling();
    }
  }

  private demoteToPolling(): void {
    if (this.fsWatcher) {
      try {
        this.fsWatcher.close();
      } catch {
        /* ignore */
      }
      this.fsWatcher = undefined;
    }
  }

  private poll(): void {
    if (this.disposed) {
      return;
    }
    // If fs.watch never attached (path was absent at construction), keep trying.
    if (!this.fsWatcher && !this.opts.pollOnly) {
      this.tryFsWatch();
    }
    const sig = signature(this.target);
    if (sig !== this.lastSig) {
      this.lastSig = sig;
      this.emit();
    }
  }

  private emit(): void {
    for (const listener of [...this.listeners]) {
      try {
        listener();
      } catch {
        // A subscriber error must not kill the watcher or sibling listeners.
      }
    }
  }
}

/**
 * A cheap change signature for a path. For files: mtime+size. For directories:
 * a digest of child names + mtimes, so added/removed/rewritten entries are all
 * detected. Absent paths hash to a stable sentinel.
 */
function signature(target: string): string {
  let stat: fs.Stats;
  try {
    stat = fs.statSync(target);
  } catch {
    return 'absent';
  }
  if (stat.isDirectory()) {
    let entries: fs.Dirent[];
    try {
      entries = fs.readdirSync(target, { withFileTypes: true });
    } catch {
      return `dir:${stat.mtimeMs}`;
    }
    const parts = entries
      .map((e) => {
        let m = 0;
        try {
          m = fs.statSync(`${target}/${e.name}`).mtimeMs;
        } catch {
          /* entry vanished mid-scan */
        }
        return `${e.name}:${m}`;
      })
      .sort();
    return `dir:${parts.join('|')}`;
  }
  return `file:${stat.mtimeMs}:${stat.size}`;
}
