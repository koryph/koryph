// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// BeadTitleCache — slot rows render "<bead id> <title>" (§2), and the title
// comes from `bd show <bead> --json`, cached. Shelling `bd` per row on every
// tree refresh would be wasteful and could block rendering, so titles are
// fetched lazily, memoized, and folded in on the next refresh. The tree renders
// immediately with the bead id alone and upgrades to the title when it arrives.

import { CliAdapter } from './cli';
import { tryParse } from './json';

/**
 * Extract a bead title from `bd show --json` output. bd may emit either a bare
 * object or an array/`{issues:[…]}` envelope; we accept the first object with a
 * string `title`. Returns undefined on any shape we do not recognize.
 */
export function parseBeadTitle(stdout: string): string | undefined {
  const parsed = tryParse(stdout);
  return titleOf(parsed);
}

function titleOf(v: unknown): string | undefined {
  if (!v) {
    return undefined;
  }
  if (Array.isArray(v)) {
    for (const item of v) {
      const t = titleOf(item);
      if (t) {
        return t;
      }
    }
    return undefined;
  }
  if (typeof v === 'object') {
    const o = v as Record<string, unknown>;
    if (typeof o.title === 'string' && o.title.trim().length > 0) {
      return o.title.trim();
    }
    // Common envelope shapes.
    if (Array.isArray(o.issues)) {
      return titleOf(o.issues);
    }
    if (o.issue) {
      return titleOf(o.issue);
    }
  }
  return undefined;
}

/** Cache key is scoped by project root so same-id beads across repos don't collide. */
function key(projectRoot: string, beadId: string): string {
  return `${projectRoot}::${beadId}`;
}

export class BeadTitleCache {
  private readonly titles = new Map<string, string>();
  private readonly inflight = new Map<string, Promise<string | undefined>>();

  constructor(private readonly cli: CliAdapter) {}

  /** The cached title if known, else undefined (fetch it with `fetch`). */
  cached(projectRoot: string, beadId: string): string | undefined {
    return this.titles.get(key(projectRoot, beadId));
  }

  /**
   * Resolve a bead title, memoizing the result and coalescing concurrent
   * fetches. Never rejects — a failed `bd show` resolves to undefined so the
   * tree keeps the bare bead id.
   */
  async fetch(projectRoot: string, beadId: string): Promise<string | undefined> {
    const k = key(projectRoot, beadId);
    const hit = this.titles.get(k);
    if (hit !== undefined) {
      return hit;
    }
    const pending = this.inflight.get(k);
    if (pending) {
      return pending;
    }
    const p = this.load(projectRoot, beadId).finally(() => this.inflight.delete(k));
    this.inflight.set(k, p);
    return p;
  }

  private async load(projectRoot: string, beadId: string): Promise<string | undefined> {
    try {
      const res = await this.cli.bd(['-C', projectRoot, 'show', beadId, '--json']);
      if (res.code !== 0) {
        return undefined;
      }
      const title = parseBeadTitle(res.stdout);
      if (title) {
        this.titles.set(key(projectRoot, beadId), title);
      }
      return title;
    } catch {
      return undefined;
    }
  }

  /** Forget all cached titles (e.g. on a full data reset). */
  clear(): void {
    this.titles.clear();
  }
}
