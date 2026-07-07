// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import {
  DRAIN_FRACTION,
  LEDGER_SCHEMA_VERSION,
  QuotaLevel,
  Run,
  STOP_FRACTION,
  THROTTLE_FRACTION,
  Usage,
  WARN_FRACTION,
  Window,
  guardSchemaVersion,
  isTerminal,
  quotaLevel,
  readSchemaVersion,
  windowFraction,
} from '../../data/schema';

describe('schema: version guard', () => {
  it('accepts a matching schema_version', () => {
    const raw = { schema_version: LEDGER_SCHEMA_VERSION, run_id: 'r1' };
    const p = guardSchemaVersion<Run>(raw, raw as unknown as Run, LEDGER_SCHEMA_VERSION);
    assert.strictEqual(p.known, true);
    assert.strictEqual(p.schemaVersion, LEDGER_SCHEMA_VERSION);
    assert.strictEqual(p.degradedReason, undefined);
  });

  it('accepts an older (non-zero) version — fields are additive', () => {
    const raw = { schema_version: 1 };
    const p = guardSchemaVersion(raw, raw, 2);
    assert.strictEqual(p.known, true);
  });

  it('degrades on a newer version but retains raw JSON', () => {
    const raw = { schema_version: 99, some_future_field: true };
    const p = guardSchemaVersion(raw, raw, 2);
    assert.strictEqual(p.known, false);
    assert.strictEqual(p.schemaVersion, 99);
    assert.match(p.degradedReason ?? '', /newer than supported/);
    assert.deepStrictEqual(p.raw, raw);
  });

  it('degrades on a missing version by default', () => {
    const raw = { run_id: 'r1' };
    const p = guardSchemaVersion(raw, raw, 2);
    assert.strictEqual(p.known, false);
    assert.match(p.degradedReason ?? '', /missing schema_version/);
  });

  it('assumes current when unversioned and told to', () => {
    const raw = { account: 'work' };
    const p = guardSchemaVersion(raw, raw, 1, { assumeUnversioned: true });
    assert.strictEqual(p.known, true);
    assert.strictEqual(p.schemaVersion, 0);
  });

  it('readSchemaVersion returns 0 for absent / non-numeric', () => {
    assert.strictEqual(readSchemaVersion({}), 0);
    assert.strictEqual(readSchemaVersion({ schema_version: 'x' }), 0);
    assert.strictEqual(readSchemaVersion(null), 0);
    assert.strictEqual(readSchemaVersion({ schema_version: 2 }), 2);
  });
});

describe('schema: ledger terminal states', () => {
  it('mirrors ledger.Terminal', () => {
    for (const s of ['merged', 'pr-opened', 'done', 'failed', 'conflict', 'blocked', 'merge-pending']) {
      assert.strictEqual(isTerminal(s), true, `${s} should be terminal`);
    }
    for (const s of ['queued', 'dispatching', 'running', 'stuck', 'review']) {
      assert.strictEqual(isTerminal(s), false, `${s} should not be terminal`);
    }
  });
});

describe('schema: quota banding', () => {
  const win = (spent: number, ceiling: number, source = 'ccusage'): Window => ({
    hours: 5,
    spent_usd: spent,
    ceiling_usd: ceiling,
    source,
    approx: false,
  });

  it('fails closed (fraction 1.0) when unmeasurable', () => {
    assert.strictEqual(windowFraction(undefined), 1.0);
    assert.strictEqual(windowFraction(win(1, 0)), 1.0);
    assert.strictEqual(windowFraction(win(1, 10, 'unavailable')), 1.0);
  });

  it('computes spent/ceiling otherwise', () => {
    assert.strictEqual(windowFraction(win(5, 20)), 0.25);
  });

  it('bands ok/warn/throttle/drain/stop off the max of both windows', () => {
    const at = '2026-07-03T00:00:00Z';
    const mk = (w5: Window, wk: Window): Usage => ({
      account: 'personal',
      at,
      window_5h: w5,
      weekly: wk,
    });
    // ok: max fraction < 0.90
    assert.strictEqual(quotaLevel(mk(win(1, 20), win(1, 140))), QuotaLevel.OK);
    // warn: max fraction >= 0.90 (18.1/20 = 90.5%)
    assert.strictEqual(quotaLevel(mk(win(18.1, 20), win(1, 140))), QuotaLevel.Warn);
    // throttle: max fraction >= 0.94 (18.9/20 = 94.5%)
    assert.strictEqual(quotaLevel(mk(win(18.9, 20), win(1, 140))), QuotaLevel.Throttle);
    // drain: max fraction >= 0.97 via weekly (136/140 ≈ 97.1%)
    assert.strictEqual(quotaLevel(mk(win(1, 20), win(136, 140))), QuotaLevel.Drain);
    // stop: max fraction >= 0.99 (19.9/20 = 99.5%)
    assert.strictEqual(quotaLevel(mk(win(19.9, 20), win(1, 140))), QuotaLevel.Stop);
  });

  it('ladder threshold constants mirror engine defaults', () => {
    assert.strictEqual(WARN_FRACTION, 0.90);
    assert.strictEqual(THROTTLE_FRACTION, 0.94);
    assert.strictEqual(DRAIN_FRACTION, 0.97);
    assert.strictEqual(STOP_FRACTION, 0.99);
  });
});
