// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// QuotaReader — per-account governor config + calibration state from
// ~/.koryph/quota/<account>.json (internal/quota). This is the cheap read used
// between the slow `koryph quota show --json` snapshots (§5): it surfaces the
// calibrated ceilings so the status bar can show data age without invoking
// ccusage. Config files without schema_version are pre-versioning and load
// (backfilled), matching the Go loader.

import * as path from 'path';
import { listJSON, readJSON } from './json';
import { quotaDir } from './paths';
import { Parsed, QUOTA_CONFIG_SCHEMA_VERSION, QuotaConfig, guardSchemaVersion } from './schema';
import { PathWatcher, WatchOptions } from './watcher';

/** A per-account quota config with its schema-version guard result. */
export type ParsedQuotaConfig = Parsed<QuotaConfig>;

export class QuotaReader {
  private readonly watcher: PathWatcher;
  private readonly listeners = new Set<() => void>();

  constructor(
    private readonly env: NodeJS.ProcessEnv = process.env,
    watchOpts: WatchOptions = {},
  ) {
    this.watcher = new PathWatcher(quotaDir(env), { recursive: true, ...watchOpts });
    this.watcher.onChange(() => this.emit());
  }

  onChange(listener: () => void): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /** Load one account's cached config, or undefined if absent/unreadable. */
  async get(account: string): Promise<ParsedQuotaConfig | undefined> {
    const raw = await readJSON(path.join(quotaDir(this.env), `${account}.json`));
    if (raw === undefined || typeof raw !== 'object') {
      return undefined;
    }
    return guardSchemaVersion<QuotaConfig>(raw, raw as QuotaConfig, QUOTA_CONFIG_SCHEMA_VERSION, {
      assumeUnversioned: true,
    });
  }

  /** Load every cached account config, keyed by account name. */
  async list(): Promise<ParsedQuotaConfig[]> {
    const dir = quotaDir(this.env);
    const names = await listJSON(dir);
    const out: ParsedQuotaConfig[] = [];
    for (const name of names.sort()) {
      const raw = await readJSON(path.join(dir, name));
      if (raw === undefined || typeof raw !== 'object') {
        continue;
      }
      out.push(
        guardSchemaVersion<QuotaConfig>(raw, raw as QuotaConfig, QUOTA_CONFIG_SCHEMA_VERSION, {
          assumeUnversioned: true,
        }),
      );
    }
    return out;
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
