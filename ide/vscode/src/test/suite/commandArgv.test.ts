// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the pure slot-command core (ext.6): argv construction, model
// allowlist gating, PR-URL extraction, arg coercion, and shell quoting. These
// import ONLY `../../commands/argv` (no `vscode`), so they run headless under
// plain mocha and never dispatch a real agent.

import * as assert from 'assert';
import {
  SlotRef,
  bdShowArgv,
  canLand,
  canOpenPr,
  coerceSlotRef,
  gitDiffArgs,
  isModelChoiceAllowed,
  landArgv,
  mergeArgv,
  modelChoices,
  modelLabelAddArgv,
  modelLabelRemoveArgv,
  nudgeArgv,
  parsePrUrl,
  shellQuote,
  slotRef,
  stopArgv,
  stopRunArgv,
  tailArgv,
} from '../../commands/argv';
import { Slot } from '../../data/schema';

function ref(overrides: Partial<SlotRef> = {}): SlotRef {
  return {
    projectId: 'koryph',
    projectRoot: '/repo/koryph',
    phaseId: 'koryph-i2n',
    beadId: 'koryph-i2n',
    branch: 'feat/koryph-i2n',
    worktree: '/wt/koryph-i2n',
    baseCommit: 'abc1234',
    model: 'sonnet',
    status: 'running',
    note: '',
    ...overrides,
  };
}

describe('slot command argv', () => {
  it('stop (graceful) → stop --project <id> <phase>', () => {
    assert.deepStrictEqual(stopArgv(ref()), ['stop', '--project', 'koryph', 'koryph-i2n']);
  });

  it('stop (force) inserts --force before the phase', () => {
    assert.deepStrictEqual(stopArgv(ref(), { force: true }), [
      'stop',
      '--project',
      'koryph',
      '--force',
      'koryph-i2n',
    ]);
  });

  it('stop whole run omits the phase id', () => {
    assert.deepStrictEqual(stopRunArgv('koryph'), ['stop', '--project', 'koryph']);
  });

  it('nudge passes the text as a single positional arg', () => {
    assert.deepStrictEqual(nudgeArgv(ref(), 'do the thing'), [
      'nudge',
      '--project',
      'koryph',
      'koryph-i2n',
      'do the thing',
    ]);
  });

  it('tail follows the phase', () => {
    assert.deepStrictEqual(tailArgv(ref()), [
      'tail',
      '--project',
      'koryph',
      'koryph-i2n',
      '--follow',
    ]);
  });

  it('merge selects the branch; land selects the bead', () => {
    assert.deepStrictEqual(mergeArgv(ref()), ['merge', '--project', 'koryph', 'feat/koryph-i2n']);
    assert.deepStrictEqual(landArgv(ref({ beadId: 'koryph-9ov' })), [
      'land',
      '--project',
      'koryph',
      'koryph-9ov',
    ]);
  });

  it('bd argv runs with -C <root> and the model:<tier> label', () => {
    assert.deepStrictEqual(modelLabelAddArgv(ref(), 'opus'), [
      '-C',
      '/repo/koryph',
      'label',
      'add',
      'koryph-i2n',
      'model:opus',
    ]);
    assert.deepStrictEqual(modelLabelRemoveArgv(ref(), 'haiku'), [
      '-C',
      '/repo/koryph',
      'label',
      'remove',
      'koryph-i2n',
      'model:haiku',
    ]);
    assert.deepStrictEqual(bdShowArgv(ref()), ['-C', '/repo/koryph', 'show', 'koryph-i2n']);
  });

  it('git diff uses the three-dot base…HEAD range', () => {
    assert.deepStrictEqual(gitDiffArgs('abc1234'), ['diff', 'abc1234...HEAD']);
  });
});

describe('model allowlist gating (Decision 5)', () => {
  it('always offers haiku/sonnet/opus', () => {
    assert.deepStrictEqual(modelChoices([]), ['haiku', 'sonnet', 'opus']);
    assert.deepStrictEqual(modelChoices(undefined), ['haiku', 'sonnet', 'opus']);
  });

  it('offers fable ONLY when the project allowlists it', () => {
    assert.deepStrictEqual(modelChoices(['haiku', 'sonnet', 'opus']), ['haiku', 'sonnet', 'opus']);
    assert.deepStrictEqual(modelChoices(['sonnet', 'fable']), ['haiku', 'sonnet', 'opus', 'fable']);
  });

  it('matches fable case- and whitespace-insensitively', () => {
    assert.ok(modelChoices([' Fable ']).includes('fable'));
    assert.ok(!isModelChoiceAllowed('fable', ['opus']));
    assert.ok(isModelChoiceAllowed('fable', ['opus', 'fable']));
    assert.ok(isModelChoiceAllowed('opus', []));
  });
});

describe('status gating + PR url', () => {
  it('gates open-PR / land on pr-opened status', () => {
    assert.ok(canOpenPr(ref({ status: 'pr-opened' })));
    assert.ok(!canOpenPr(ref({ status: 'running' })));
    assert.ok(canLand(ref({ status: 'pr-opened' })));
    assert.ok(!canLand(ref({ status: 'review' })));
  });

  it('extracts the PR URL from the slot note', () => {
    assert.strictEqual(
      parsePrUrl('PR #42 opened: https://github.com/acme/proj/pull/42'),
      'https://github.com/acme/proj/pull/42',
    );
    assert.strictEqual(parsePrUrl('no url here'), undefined);
    assert.strictEqual(parsePrUrl(undefined), undefined);
  });
});

describe('slotRef + coercion', () => {
  const slot: Slot = {
    phase_id: 'koryph-i2n',
    branch: 'feat/x',
    worktree: '/wt/x',
    session_id: 's',
    agent: 'implementer',
    model: 'opus',
    account_profile: 'personal',
    billing_mode: 'subscription',
    status: 'running',
    attempts: 1,
    commits: 0,
    cost_usd: 0,
    note: 'PR #7 opened: https://x/pull/7',
  };

  it('builds a SlotRef, defaulting beadId to phaseId when bead_id is absent', () => {
    const r = slotRef('koryph', '/repo', slot, 'base99');
    assert.strictEqual(r.beadId, 'koryph-i2n');
    assert.strictEqual(r.baseCommit, 'base99');
    assert.strictEqual(r.note, 'PR #7 opened: https://x/pull/7');
  });

  it('prefers an explicit bead_id', () => {
    const r = slotRef('koryph', '/repo', { ...slot, bead_id: 'koryph-9ov' });
    assert.strictEqual(r.beadId, 'koryph-9ov');
  });

  it('coerces a raw ref, a tree item (.slotRef), and rejects non-slots', () => {
    const r = ref();
    assert.strictEqual(coerceSlotRef(r), r);
    assert.deepStrictEqual(coerceSlotRef({ slotRef: r }), r);
    assert.strictEqual(coerceSlotRef(undefined), undefined);
    assert.strictEqual(coerceSlotRef({ foo: 1 }), undefined);
    assert.strictEqual(coerceSlotRef('koryph-i2n'), undefined);
  });
});

describe('shellQuote', () => {
  it('passes bare-word-safe tokens through', () => {
    assert.strictEqual(shellQuote('koryph'), 'koryph');
    assert.strictEqual(shellQuote('feat/koryph-i2n'), 'feat/koryph-i2n');
    assert.strictEqual(shellQuote('abc123...HEAD'), 'abc123...HEAD');
  });

  it('quotes spaces and escapes embedded single quotes', () => {
    assert.strictEqual(shellQuote('do the thing'), `'do the thing'`);
    assert.strictEqual(shellQuote(''), "''");
    assert.strictEqual(shellQuote("it's"), `'it'\\''s'`);
  });
});
