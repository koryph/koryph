// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import { PathWatcher } from '../../data/watcher';
import { mkScratch, rmScratch, waitFor } from './helpers';

describe('PathWatcher (polling fallback)', () => {
  it('detects file content changes via polling', async () => {
    const scratch = mkScratch();
    const file = path.join(scratch, 'x.json');
    fs.writeFileSync(file, '{"n":1}');
    const w = new PathWatcher(file, { pollOnly: true, pollMs: 30 });
    let fired = 0;
    w.onChange(() => {
      fired++;
    });
    try {
      // A change with a distinct size guarantees the signature differs even at
      // coarse mtime resolution.
      fs.writeFileSync(file, '{"n":22222}');
      assert.strictEqual(await waitFor(() => fired > 0), true);
    } finally {
      w.dispose();
      rmScratch(scratch);
    }
  });

  it('detects a path that appears after construction', async () => {
    const scratch = mkScratch();
    const file = path.join(scratch, 'later.json');
    const w = new PathWatcher(file, { pollOnly: true, pollMs: 30 });
    let fired = 0;
    w.onChange(() => {
      fired++;
    });
    try {
      fs.writeFileSync(file, '{"created":true}');
      assert.strictEqual(await waitFor(() => fired > 0), true);
    } finally {
      w.dispose();
      rmScratch(scratch);
    }
  });

  it('detects directory entry additions', async () => {
    const scratch = mkScratch();
    const w = new PathWatcher(scratch, { pollOnly: true, pollMs: 30, recursive: false });
    let fired = 0;
    w.onChange(() => {
      fired++;
    });
    try {
      fs.writeFileSync(path.join(scratch, 'new.json'), '{}');
      assert.strictEqual(await waitFor(() => fired > 0), true);
    } finally {
      w.dispose();
      rmScratch(scratch);
    }
  });

  it('stops firing after dispose', async () => {
    const scratch = mkScratch();
    const file = path.join(scratch, 'y.json');
    fs.writeFileSync(file, '1');
    const w = new PathWatcher(file, { pollOnly: true, pollMs: 30 });
    let fired = 0;
    w.onChange(() => {
      fired++;
    });
    w.dispose();
    try {
      fs.writeFileSync(file, '222');
      await new Promise((r) => setTimeout(r, 120));
      assert.strictEqual(fired, 0);
    } finally {
      rmScratch(scratch);
    }
  });
});
