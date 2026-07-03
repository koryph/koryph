// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as path from 'path';
import {
  demandDir,
  governorConfig,
  koryphHome,
  latestLedger,
  quotaDir,
  registryDir,
  slotsDir,
  streamFile,
} from '../../data/paths';

describe('paths: KORYPH_HOME resolution', () => {
  it('honours KORYPH_HOME override', () => {
    const env = { KORYPH_HOME: '/opt/koryph-home' } as NodeJS.ProcessEnv;
    assert.strictEqual(koryphHome(env), '/opt/koryph-home');
    assert.strictEqual(registryDir(env), '/opt/koryph-home/registry.d');
    assert.strictEqual(quotaDir(env), '/opt/koryph-home/quota');
    assert.strictEqual(slotsDir(env), '/opt/koryph-home/slots');
    assert.strictEqual(demandDir(env), '/opt/koryph-home/slots/demand');
    assert.strictEqual(governorConfig(env), '/opt/koryph-home/governor.json');
  });

  it('ignores an empty KORYPH_HOME and falls back to ~/.koryph', () => {
    const env = { KORYPH_HOME: '' } as NodeJS.ProcessEnv;
    assert.ok(koryphHome(env).endsWith(path.join('.koryph')));
  });

  it('resolves the latest ledger via the latest symlink', () => {
    const repo = '/home/dev/src/proj';
    assert.strictEqual(
      latestLedger(repo),
      path.join(repo, '.plan-logs', 'koryph', 'latest', 'ledger.json'),
    );
  });

  it('resolves a per-slot stream path', () => {
    const repo = '/home/dev/src/proj';
    assert.strictEqual(
      streamFile(repo, '20260703-091422', 'koryph-i2n'),
      path.join(repo, '.plan-logs', 'koryph', '20260703-091422', 'koryph-i2n', 'stream.jsonl'),
    );
  });
});
