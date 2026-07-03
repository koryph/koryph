// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import { LedgerWatcher } from '../../data/ledgerWatcher';
import { FIXTURE_REPO } from './helpers';
import { isTerminal } from '../../data/schema';

describe('LedgerWatcher', () => {
  it('loads the latest run via the latest symlink', async () => {
    const w = new LedgerWatcher(FIXTURE_REPO, { pollOnly: true, pollMs: 50 });
    try {
      const run = await w.load();
      assert.ok(run, 'expected a run');
      assert.strictEqual(run!.known, true);
      assert.strictEqual(run!.value.schema_version, 2);
      assert.strictEqual(run!.value.run_id, '20260703-091422');
      assert.strictEqual(run!.value.status, 'running');
      assert.strictEqual(run!.value.wave, 3);
      assert.strictEqual(Object.keys(run!.value.slots).length, 4);
    } finally {
      w.dispose();
    }
  });

  it('exposes typed slot fields the tree view needs', async () => {
    const w = new LedgerWatcher(FIXTURE_REPO, { pollOnly: true, pollMs: 50 });
    try {
      const run = await w.load();
      const i2n = run!.value.slots['koryph-i2n'];
      assert.strictEqual(i2n.model, 'opus');
      assert.strictEqual(i2n.status, 'running');
      assert.strictEqual(i2n.cost_usd, 0.42);
      assert.strictEqual(i2n.branch, 'feat/koryph-i2n-completions');
      assert.strictEqual(i2n.pid, 4821);
      assert.strictEqual(isTerminal(i2n.status), false);

      const merged = run!.value.slots['koryph-5ov'];
      assert.strictEqual(merged.status, 'merged');
      assert.strictEqual(isTerminal(merged.status), true);
    } finally {
      w.dispose();
    }
  });

  it('returns undefined when the project has no run', async () => {
    const w = new LedgerWatcher('/nonexistent/repo', { pollOnly: true, pollMs: 50 });
    try {
      assert.strictEqual(await w.load(), undefined);
    } finally {
      w.dispose();
    }
  });
});
