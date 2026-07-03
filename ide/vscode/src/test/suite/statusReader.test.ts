// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the agent-heartbeat status.json reader (ext.4 §2). The tree
// surfaces {state, step, pct} in tooltips *labeled possibly stale*. Plain node.

import * as assert from 'assert';
import {
  StatusReader,
  formatStatusReport,
  parseStatusReport,
} from '../../data/statusReader';
import { FIXTURE_REPO } from './helpers';

describe('parseStatusReport', () => {
  it('extracts the three heartbeat fields', () => {
    const r = parseStatusReport({ state: 'implementing', step: 'wiring', pct: 40 });
    assert.deepStrictEqual(r, { state: 'implementing', step: 'wiring', pct: 40 });
  });

  it('ignores wrong-typed fields and returns undefined when empty', () => {
    assert.deepStrictEqual(parseStatusReport({ state: 'x', pct: 'nope' }), { state: 'x' });
    assert.strictEqual(parseStatusReport({}), undefined);
    assert.strictEqual(parseStatusReport({ pct: Infinity }), undefined);
    assert.strictEqual(parseStatusReport(null), undefined);
    assert.strictEqual(parseStatusReport('str'), undefined);
  });
});

describe('formatStatusReport', () => {
  it('always flags the line as agent-reported and possibly stale', () => {
    const line = formatStatusReport({ state: 'testing', step: 'run gate', pct: 80 });
    assert.ok(line);
    assert.ok(line!.startsWith('agent-reported (may be stale):'));
    assert.ok(line!.includes('testing'));
    assert.ok(line!.includes('80%'));
    assert.ok(line!.includes('run gate'));
  });

  it('returns undefined for an empty report', () => {
    assert.strictEqual(formatStatusReport(undefined), undefined);
    assert.strictEqual(formatStatusReport({}), undefined);
  });
});

describe('StatusReader', () => {
  it('reads the fixture slot heartbeat', async () => {
    const r = await new StatusReader().read(FIXTURE_REPO, '20260703-091422', 'koryph-i2n');
    assert.ok(r);
    assert.strictEqual(r!.state, 'implementing');
    assert.strictEqual(r!.pct, 40);
  });

  it('returns undefined for a slot with no status.json', async () => {
    const r = await new StatusReader().read(FIXTURE_REPO, '20260703-091422', 'koryph-5ov');
    assert.strictEqual(r, undefined);
  });
});
