// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Shared test helpers: fixture path resolution and a temp-tree copier for the
// watcher tests (which mutate files to assert change notification).

import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';

/** Absolute path to src/test/fixtures (resolved from the compiled out/ tree). */
export const FIXTURES = path.resolve(__dirname, '..', '..', '..', 'src', 'test', 'fixtures');

/** The fixture koryph home stand-in. */
export const FIXTURE_HOME = path.join(FIXTURES, 'home');

/** The fixture project repo root (holds .plan-logs/koryph/...). */
export const FIXTURE_REPO = path.join(FIXTURES, 'repo');

/** An env whose KORYPH_HOME points at the fixture home. */
export function fixtureEnv(overrides: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  return { ...process.env, KORYPH_HOME: FIXTURE_HOME, ...overrides };
}

/** Create an empty scratch directory under the OS temp root. */
export function mkScratch(prefix = 'koryph-ext-'): string {
  return fs.mkdtempSync(path.join(os.tmpdir(), prefix));
}

/** Recursively copy a directory tree (files + subdirs). */
export function copyTree(src: string, dst: string): void {
  fs.mkdirSync(dst, { recursive: true });
  for (const entry of fs.readdirSync(src, { withFileTypes: true })) {
    const s = path.join(src, entry.name);
    const d = path.join(dst, entry.name);
    if (entry.isDirectory()) {
      copyTree(s, d);
    } else {
      fs.copyFileSync(s, d);
    }
  }
}

/** Remove a scratch tree, ignoring errors. */
export function rmScratch(dir: string): void {
  try {
    fs.rmSync(dir, { recursive: true, force: true });
  } catch {
    /* ignore */
  }
}

/** Poll `cond` until true or timeout; resolves the final value. */
export async function waitFor(cond: () => boolean, timeoutMs = 3000, stepMs = 25): Promise<boolean> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (cond()) {
      return true;
    }
    await sleep(stepMs);
  }
  return cond();
}

export function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}
