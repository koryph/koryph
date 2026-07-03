// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import { QuotaReader } from '../../data/quotaReader';
import { fixtureEnv } from './helpers';

describe('QuotaReader', () => {
  it('reads a versioned account config with calibration', async () => {
    const q = new QuotaReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const cfg = await q.get('personal');
      assert.ok(cfg);
      assert.strictEqual(cfg!.known, true);
      assert.strictEqual(cfg!.value.window_ceiling_usd, 20);
      assert.strictEqual(cfg!.value.weekly_ceiling_usd, 140);
      assert.strictEqual(cfg!.value.per_tier_usd.opus, 9);
      assert.strictEqual(cfg!.value.calibration?.['opus:M'], 8.7);
    } finally {
      q.dispose();
    }
  });

  it('backfills an unversioned (pre-versioning) config', async () => {
    const q = new QuotaReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const cfg = await q.get('work');
      assert.ok(cfg);
      assert.strictEqual(cfg!.known, true);
      assert.strictEqual(cfg!.schemaVersion, 0);
      assert.strictEqual(cfg!.value.window_ceiling_usd, 50);
    } finally {
      q.dispose();
    }
  });

  it('lists all cached account configs', async () => {
    const q = new QuotaReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const all = await q.list();
      const accounts = all.map((c) => c.value.account).sort();
      assert.deepStrictEqual(accounts, ['personal', 'work']);
    } finally {
      q.dispose();
    }
  });

  it('returns undefined for an unknown account', async () => {
    const q = new QuotaReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      assert.strictEqual(await q.get('nobody'), undefined);
    } finally {
      q.dispose();
    }
  });
});
