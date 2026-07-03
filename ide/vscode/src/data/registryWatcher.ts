// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// RegistryWatcher — reads and watches ~/.koryph/registry.d/*.json, one
// registry.Record per managed project (see internal/registry). Read-only:
// registry files are git-committed by the store and must never be hand-edited
// (Decision 3 + §6). Records predate schema versioning in some deployments, so
// missing schema_version is assumed-current and backfilled.

import * as path from 'path';
import { listJSON, readJSON } from './json';
import { registryDir } from './paths';
import { Parsed, ProjectRecord, REGISTRY_SCHEMA_VERSION, guardSchemaVersion } from './schema';
import { PathWatcher, WatchOptions } from './watcher';

/** A registry record with its schema-version guard result. */
export type ParsedRecord = Parsed<ProjectRecord>;

export class RegistryWatcher {
  private readonly watcher: PathWatcher;
  private readonly listeners = new Set<() => void>();

  constructor(
    private readonly env: NodeJS.ProcessEnv = process.env,
    watchOpts: WatchOptions = {},
  ) {
    this.watcher = new PathWatcher(registryDir(env), watchOpts);
    this.watcher.onChange(() => this.emit());
  }

  /** The directory being watched. */
  get dir(): string {
    return registryDir(this.env);
  }

  /** Register a change listener (fires when any record file changes). */
  onChange(listener: () => void): { dispose: () => void } {
    this.listeners.add(listener);
    return { dispose: () => this.listeners.delete(listener) };
  }

  /**
   * Load all project records, sorted by project_id for stable display.
   * Malformed files are skipped; version-guard failures are retained as
   * degraded entries (raw JSON preserved) so the UI can show them instead of
   * silently dropping a project.
   */
  async list(): Promise<ParsedRecord[]> {
    const dir = registryDir(this.env);
    const names = await listJSON(dir);
    const out: ParsedRecord[] = [];
    for (const name of names.sort()) {
      const raw = await readJSON(path.join(dir, name));
      if (raw === undefined || typeof raw !== 'object') {
        continue;
      }
      out.push(
        guardSchemaVersion<ProjectRecord>(raw, raw as ProjectRecord, REGISTRY_SCHEMA_VERSION, {
          assumeUnversioned: true,
        }),
      );
    }
    return out.sort((a, b) => a.value.project_id.localeCompare(b.value.project_id));
  }

  /** Load a single record by project id, or undefined if not present. */
  async get(projectID: string): Promise<ParsedRecord | undefined> {
    const raw = await readJSON(path.join(registryDir(this.env), `${projectID}.json`));
    if (raw === undefined || typeof raw !== 'object') {
      return undefined;
    }
    return guardSchemaVersion<ProjectRecord>(raw, raw as ProjectRecord, REGISTRY_SCHEMA_VERSION, {
      assumeUnversioned: true,
    });
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
