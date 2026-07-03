// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import { RegistryWatcher } from '../../data/registryWatcher';
import { FIXTURE_HOME, copyTree, fixtureEnv, mkScratch, rmScratch, waitFor } from './helpers';

describe('RegistryWatcher', () => {
  it('lists fixture records sorted by project_id', async () => {
    const w = new RegistryWatcher(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const recs = await w.list();
      const ids = recs.map((r) => r.value.project_id);
      assert.deepStrictEqual(ids, ['futureproj', 'koryph', 'ncp_roadmap']);
    } finally {
      w.dispose();
    }
  });

  it('parses a well-formed record with all decision fields', async () => {
    const w = new RegistryWatcher(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const rec = await w.get('koryph');
      assert.ok(rec);
      assert.strictEqual(rec!.known, true);
      assert.strictEqual(rec!.value.account_profile, 'personal');
      assert.strictEqual(rec!.value.expected_identity, 'cody@mccain.family');
      assert.deepStrictEqual(rec!.value.allowed_models, ['opus', 'sonnet', 'haiku']);
      assert.strictEqual(rec!.value.migration_status, 'validated');
    } finally {
      w.dispose();
    }
  });

  it('degrades a newer-schema record instead of dropping it', async () => {
    const w = new RegistryWatcher(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      const rec = await w.get('futureproj');
      assert.ok(rec);
      assert.strictEqual(rec!.known, false);
      assert.strictEqual(rec!.schemaVersion, 99);
      // Raw JSON is preserved for raw-display fallback.
      assert.ok(rec!.raw && typeof rec!.raw === 'object');
    } finally {
      w.dispose();
    }
  });

  it('returns undefined for an unknown project', async () => {
    const w = new RegistryWatcher(fixtureEnv(), { pollOnly: true, pollMs: 50 });
    try {
      assert.strictEqual(await w.get('nope'), undefined);
    } finally {
      w.dispose();
    }
  });

  it('fires onChange when a record file is added', async () => {
    const scratch = mkScratch();
    const home = path.join(scratch, 'home');
    copyTree(FIXTURE_HOME, home);
    const env = { ...process.env, KORYPH_HOME: home } as NodeJS.ProcessEnv;
    const w = new RegistryWatcher(env, { pollOnly: true, pollMs: 40 });
    let fired = 0;
    w.onChange(() => {
      fired++;
    });
    try {
      fs.writeFileSync(
        path.join(home, 'registry.d', 'added.json'),
        JSON.stringify({ schema_version: 1, project_id: 'added', account_profile: 'personal' }),
      );
      const ok = await waitFor(() => fired > 0);
      assert.strictEqual(ok, true, 'expected onChange to fire');
      const recs = await w.list();
      assert.ok(recs.some((r) => r.value.project_id === 'added'));
    } finally {
      w.dispose();
      rmScratch(scratch);
    }
  });
});
