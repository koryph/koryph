// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// StreamReader — byte-offset incremental JSONL parse of a slot's stream.jsonl
// (claude `--output-format stream-json --include-partial-messages`), the same
// advance-by-offset approach `koryph tail --follow` uses (cmd/koryph/ops.go).
// One reader per open transcript (Decision 4 / §4).
//
// Contract:
//   - read() reads only bytes after the last offset, parses complete lines,
//     and holds an incomplete trailing fragment until its newline arrives.
//   - Unknown event `type`s are returned verbatim (raw) — the panel renders
//     them as collapsed JSON, forward-compatible with the pluggable-runtime
//     event envelope (koryph-v8u.1) noted in §4.
//   - Truncation / rotation (file shrank below the offset) resets to 0 so a
//     replaced stream re-reads from the top instead of returning garbage.
//
// No vscode dependency: pure fs, unit-testable against fixtures.

import * as fs from 'fs';
import * as fsp from 'fs/promises';
import { parseJSONL } from './json';
import { PathWatcher, WatchOptions } from './watcher';

/** A single parsed stream event (claude stream-json line, best-effort typed). */
export interface StreamEvent {
  /** The event type, when present (e.g. assistant, tool_use, result). */
  type?: string;
  /** The full raw record — always retained for forward-compatible rendering. */
  raw: unknown;
}

export class StreamReader {
  private offset = 0;
  private remainder = '';
  private watcher: PathWatcher | undefined;
  private readonly listeners = new Set<() => void>();

  constructor(private readonly file: string) {}

  /** The stream file this reader advances over. */
  get path(): string {
    return this.file;
  }

  /** Current byte offset (bytes already consumed). */
  get position(): number {
    return this.offset;
  }

  /**
   * Read events appended since the last call. Returns [] when nothing new (or
   * the file is absent). Detects truncation and restarts from offset 0.
   */
  async read(): Promise<StreamEvent[]> {
    let stat: fs.Stats;
    try {
      stat = await fsp.stat(this.file);
    } catch {
      return [];
    }
    if (stat.size < this.offset) {
      // File was replaced/truncated (new run reused the path) — restart.
      this.offset = 0;
      this.remainder = '';
    }
    if (stat.size === this.offset) {
      return [];
    }

    const handle = await fsp.open(this.file, 'r');
    try {
      const length = stat.size - this.offset;
      const buffer = Buffer.alloc(length);
      const { bytesRead } = await handle.read(buffer, 0, length, this.offset);
      this.offset += bytesRead;
      const text = this.remainder + buffer.subarray(0, bytesRead).toString('utf8');
      const { records, remainder } = parseJSONL(text);
      this.remainder = remainder;
      return records.map(toEvent);
    } finally {
      await handle.close();
    }
  }

  /** Read the entire stream from the top in one pass (non-incremental). */
  async readAll(): Promise<StreamEvent[]> {
    this.reset();
    return this.read();
  }

  /** Reset the offset so the next read() re-reads from the top. */
  reset(): void {
    this.offset = 0;
    this.remainder = '';
  }

  /**
   * Start watching the file and invoke `listener` when new bytes arrive.
   * The listener is responsible for calling read() to drain them.
   */
  watch(listener: () => void, watchOpts: WatchOptions = {}): { dispose: () => void } {
    this.listeners.add(listener);
    if (!this.watcher) {
      this.watcher = new PathWatcher(this.file, watchOpts);
      this.watcher.onChange(() => {
        for (const l of [...this.listeners]) {
          try {
            l();
          } catch {
            /* isolate subscriber faults */
          }
        }
      });
    }
    return { dispose: () => this.listeners.delete(listener) };
  }

  dispose(): void {
    this.listeners.clear();
    if (this.watcher) {
      this.watcher.dispose();
      this.watcher = undefined;
    }
  }
}

function toEvent(raw: unknown): StreamEvent {
  if (raw && typeof raw === 'object' && typeof (raw as { type?: unknown }).type === 'string') {
    return { type: (raw as { type: string }).type, raw };
  }
  return { raw };
}
