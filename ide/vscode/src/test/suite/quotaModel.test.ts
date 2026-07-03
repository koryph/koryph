// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the pure quota status-bar model (ext.4 §5): involved-account
// selection, snapshot formatting + governor level, cached-ceiling fallback,
// data-age rendering, and `koryph quota show --json` parsing. Plain node.

import * as assert from 'assert';
import { ProjectNode } from '../../tree/model';
import { ProjectRecord, QuotaLevel, QuotaSnapshot, Usage } from '../../data/schema';
import {
  formatAge,
  formatCeilingOnly,
  formatSnapshot,
  involvedAccounts,
  parseQuotaSnapshots,
  pctCell,
} from '../../statusbar/quotaModel';

function project(id: string, account: string, runStatus?: string): ProjectNode {
  return {
    projectId: id,
    name: id,
    root: `/src/${id}`,
    account,
    pinned: true,
    degraded: false,
    record: { project_id: id } as ProjectRecord,
    run: runStatus
      ? { runId: 'r', status: runStatus, wave: 1, slots: [], degraded: false, raw: {} }
      : undefined,
  };
}

function usage(account: string, spent5h: number, ceil5h: number, spentWk: number, ceilWk: number): Usage {
  return {
    account,
    at: '2026-07-03T09:16:00Z',
    window_5h: { hours: 5, spent_usd: spent5h, ceiling_usd: ceil5h, source: 'ccusage', approx: false },
    weekly: { hours: 168, spent_usd: spentWk, ceiling_usd: ceilWk, source: 'ccusage', approx: false },
  };
}

describe('involvedAccounts', () => {
  it('includes only accounts owning a visible project with an active run', () => {
    const visible = [
      project('koryph', 'personal', 'running'),
      project('ncp', 'work', undefined), // no run → excluded
      project('other', 'personal', 'running'), // dup account collapses
      project('dead', 'work', 'done'), // done run → excluded
    ];
    assert.deepStrictEqual(involvedAccounts(visible), ['personal']);
  });
});

describe('formatSnapshot', () => {
  const now = Date.parse('2026-07-03T09:19:00Z');

  it('renders percentages for both windows and echoes the level', () => {
    const snap: QuotaSnapshot = {
      account: 'personal',
      level: QuotaLevel.Warn,
      calibrated: true,
      usage: usage('personal', 12.4, 20, 57.4, 140),
    };
    const item = formatSnapshot(snap, now, '⚡');
    assert.strictEqual(item.text, '⚡ personal 62% 5h · 41% wk');
    assert.strictEqual(item.level, QuotaLevel.Warn);
    assert.strictEqual(item.stale, false);
    assert.ok(item.tooltip.includes('3m ago'));
    assert.ok(item.tooltip.includes('calibrated'));
  });

  it('fails closed to 100% when a window is unavailable', () => {
    const u = usage('personal', 0, 0, 0, 0);
    u.window_5h.source = 'unavailable';
    u.weekly.source = 'unavailable';
    const item = formatSnapshot(
      { account: 'personal', level: QuotaLevel.Stop, calibrated: false, usage: u },
      now,
      '⚡',
    );
    assert.strictEqual(item.text, '⚡ personal 100% 5h · 100% wk');
  });
});

describe('formatCeilingOnly', () => {
  it('shows ceilings and marks stale when no snapshot is available', () => {
    const cfg = {
      known: true,
      schemaVersion: 1,
      raw: {},
      value: {
        account: 'personal',
        window_ceiling_usd: 20,
        weekly_ceiling_usd: 140,
        per_agent_max_usd: 25,
        per_tier_usd: {},
        size_multiplier: {},
        safety_margin: 1.5,
      },
    };
    const item = formatCeilingOnly('personal', cfg, '⚡');
    assert.strictEqual(item.stale, true);
    assert.strictEqual(item.level, QuotaLevel.OK);
    assert.ok(item.tooltip.includes('$20/5h · $140/wk'));
  });

  it('degrades gracefully with no cached config', () => {
    const item = formatCeilingOnly('mystery', undefined, '⚡');
    assert.strictEqual(item.stale, true);
    assert.ok(item.tooltip.includes('ceilings unknown'));
  });
});

describe('formatAge & pctCell', () => {
  it('humanizes elapsed time', () => {
    const now = Date.parse('2026-07-03T10:00:00Z');
    assert.strictEqual(formatAge('2026-07-03T09:59:30Z', now), 'just now');
    assert.strictEqual(formatAge('2026-07-03T09:45:00Z', now), '15m ago');
    assert.strictEqual(formatAge('2026-07-03T08:30:00Z', now), '1h 30m ago');
    assert.strictEqual(formatAge(undefined, now), 'age unknown');
    assert.strictEqual(formatAge('not-a-date', now), 'age unknown');
  });

  it('clamps and rounds percentages', () => {
    assert.strictEqual(pctCell(0.615), '62%');
    assert.strictEqual(pctCell(1.5), '100%');
    assert.strictEqual(pctCell(-0.2), '0%');
  });
});

describe('parseQuotaSnapshots', () => {
  it('parses the array shape and rejects a non-array (older CLI table)', () => {
    const json = JSON.stringify([
      { account: 'personal', level: 'ok', calibrated: true, usage: usage('personal', 1, 20, 5, 140) },
    ]);
    const snaps = parseQuotaSnapshots(json);
    assert.ok(snaps);
    assert.strictEqual(snaps!.length, 1);
    assert.strictEqual(snaps![0].account, 'personal');

    assert.strictEqual(parseQuotaSnapshots('ACCOUNT  LEVEL\npersonal  ok'), undefined);
    assert.strictEqual(parseQuotaSnapshots('not json'), undefined);
  });

  it('drops malformed entries missing an account', () => {
    const snaps = parseQuotaSnapshots(JSON.stringify([{ level: 'ok' }, { account: 'x', level: 'ok' }]));
    assert.deepStrictEqual(snaps!.map((s) => s.account), ['x']);
  });
});
