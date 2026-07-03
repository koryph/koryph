// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import { GovernorReader } from '../../data/governorReader';
import { DEFAULT_MAX_GLOBAL_AGENTS } from '../../data/schema';
import { fixtureEnv } from './helpers';

describe('GovernorReader', () => {
  it('reads the machine-wide cap from governor.json', async () => {
    const g = new GovernorReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const { max, present } = await g.cap();
      assert.strictEqual(max, 4);
      assert.strictEqual(present, true);
    } finally {
      g.dispose();
    }
  });

  it('defaults the cap when governor.json is absent', async () => {
    const env = { ...process.env, KORYPH_HOME: '/nonexistent/home' } as NodeJS.ProcessEnv;
    const g = new GovernorReader(env, { pollOnly: true, pollMs: 50 });
    try {
      const { max, present } = await g.cap();
      assert.strictEqual(max, DEFAULT_MAX_GLOBAL_AGENTS);
      assert.strictEqual(present, false);
    } finally {
      g.dispose();
    }
  });

  it('lists leases (excluding demand/) sorted by acquired_at', async () => {
    const g = new GovernorReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const leases = await g.leases();
      assert.strictEqual(leases.length, 2);
      assert.deepStrictEqual(
        leases.map((l) => l.bead),
        ['koryph-i2n', 'koryph-fr3.1'],
      );
      assert.strictEqual(leases[0].pid, 4821);
    } finally {
      g.dispose();
    }
  });

  it('reads per-project demand heartbeats', async () => {
    const g = new GovernorReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const demands = await g.demands();
      assert.strictEqual(demands.length, 1);
      assert.strictEqual(demands[0].project, 'koryph');
    } finally {
      g.dispose();
    }
  });

  it('counts live agents globally and per project (badge signal)', async () => {
    const g = new GovernorReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      assert.strictEqual(await g.liveCount(), 2);
      assert.strictEqual(await g.liveCount('koryph'), 2);
      assert.strictEqual(await g.liveCount('ncp_roadmap'), 0);
    } finally {
      g.dispose();
    }
  });

  it('assembles a full snapshot in one call', async () => {
    const g = new GovernorReader(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const snap = await g.snapshot();
      assert.strictEqual(snap.maxGlobalAgents, 4);
      assert.strictEqual(snap.configPresent, true);
      assert.strictEqual(snap.leases.length, 2);
      assert.strictEqual(snap.demands.length, 1);
    } finally {
      g.dispose();
    }
  });
});
