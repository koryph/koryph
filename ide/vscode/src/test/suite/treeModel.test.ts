// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the pure Agent Threads tree-model builder (ext.4 §2). These
// run in plain node (no VS Code host): grouping, workspace-folder pinning, the
// Decision-6 showAllProjects default, slot ordering, the live-agent badge, and
// the streaming/terminal cost cell — all asserted against constructed fixtures
// plus the real ledger fixture loaded through LedgerWatcher.

import * as assert from 'assert';
import { ParsedRun } from '../../data/ledgerWatcher';
import { ParsedRecord } from '../../data/registryWatcher';
import { LedgerWatcher } from '../../data/ledgerWatcher';
import { Lease, ProjectRecord, Run, Slot } from '../../data/schema';
import {
  buildTree,
  compareSlots,
  isActiveRun,
  isPinned,
  slotCostCell,
} from '../../tree/model';
import { FIXTURE_REPO } from './helpers';

function rec(id: string, root: string, account = 'personal', known = true): ParsedRecord {
  return {
    known,
    schemaVersion: known ? 1 : 99,
    raw: { project_id: id },
    value: { project_id: id, name: id, root, account_profile: account } as ProjectRecord,
  };
}

function slot(phaseId: string, over: Partial<Slot> = {}): Slot {
  return {
    phase_id: phaseId,
    bead_id: phaseId,
    branch: `feat/${phaseId}`,
    worktree: `/wt/${phaseId}`,
    status: 'running',
    model: 'sonnet',
    cost_usd: 0.1,
    attempts: 1,
    commits: 0,
    ...over,
  } as Slot;
}

function run(projectId: string, slots: Slot[], over: Partial<Run> = {}): ParsedRun {
  const byId: Record<string, Slot> = {};
  for (const s of slots) {
    byId[s.phase_id] = s;
  }
  return {
    known: true,
    schemaVersion: 2,
    raw: {},
    value: {
      schema_version: 2,
      run_id: '20260703-091422',
      project_id: projectId,
      engine_version: '0.3.0',
      started_at: '2026-07-03T09:14:22Z',
      updated_at: '2026-07-03T09:16:31Z',
      status: 'running',
      wave: 3,
      source: 'bd',
      slots: byId,
      ...over,
    },
  };
}

function lease(project: string, bead: string, pid: number): Lease {
  return { project, bead, pid, engine_pid: 4800, acquired_at: '2026-07-03T09:15:10Z' };
}

describe('buildTree — grouping & pinning', () => {
  const records = [
    rec('koryph', '/src/koryph'),
    rec('ncp_roadmap', '/src/ncp'),
    rec('futureproj', '/src/future', 'personal', false),
  ];
  const runs = new Map<string, ParsedRun | undefined>([
    ['koryph', run('koryph', [slot('koryph-i2n')])],
    ['ncp_roadmap', undefined],
    ['futureproj', undefined],
  ]);

  it('pins the project whose root matches a workspace folder; others collapse', () => {
    const m = buildTree({ records, runs, leases: [], workspaceRoots: ['/src/koryph'] });
    assert.deepStrictEqual(m.pinned.map((p) => p.projectId), ['koryph']);
    assert.deepStrictEqual(m.others.map((p) => p.projectId).sort(), ['futureproj', 'ncp_roadmap']);
    // Decision 6: something is pinned → others hidden by default.
    assert.strictEqual(m.showOthers, false);
  });

  it('shows all projects by default when no workspace folder matches (Decision 6)', () => {
    const m = buildTree({ records, runs, leases: [], workspaceRoots: ['/somewhere/else'] });
    assert.strictEqual(m.pinned.length, 0);
    assert.strictEqual(m.showOthers, true);
    assert.strictEqual(m.others.length, 3);
  });

  it('honors an explicit showAllProjects=false even with nothing pinned', () => {
    const m = buildTree({
      records,
      runs,
      leases: [],
      workspaceRoots: [],
      showAllProjects: false,
    });
    assert.strictEqual(m.showOthers, false);
  });

  it('honors an explicit showAllProjects=true even with a pin', () => {
    const m = buildTree({
      records,
      runs,
      leases: [],
      workspaceRoots: ['/src/koryph'],
      showAllProjects: true,
    });
    assert.strictEqual(m.showOthers, true);
  });

  it('flags a degraded (newer-schema) registry record', () => {
    const m = buildTree({ records, runs, leases: [], workspaceRoots: [] });
    const future = m.others.find((p) => p.projectId === 'futureproj');
    assert.ok(future);
    assert.strictEqual(future!.degraded, true);
  });

  it('pins on containment (folder opened inside the project root)', () => {
    assert.strictEqual(isPinned('/src/koryph', ['/src/koryph/ide/vscode']), true);
    assert.strictEqual(isPinned('/src/koryph', ['/src/koryph']), true);
    assert.strictEqual(isPinned('/src/koryph', ['/src/koryph-worktrees/x']), false);
    assert.strictEqual(isPinned('', ['/src/koryph']), false);
  });
});

describe('buildTree — badge from leases', () => {
  const records = [rec('koryph', '/src/koryph'), rec('ncp_roadmap', '/src/ncp')];
  const runs = new Map<string, ParsedRun | undefined>([
    ['koryph', run('koryph', [slot('koryph-i2n')])],
    ['ncp_roadmap', run('ncp_roadmap', [slot('ncp-1')])],
  ]);

  it('counts leases only for visible projects', () => {
    const leases = [lease('koryph', 'koryph-i2n', 4821), lease('ncp_roadmap', 'ncp-1', 5000)];
    // Pin koryph → others collapsed → ncp lease not counted.
    const pinned = buildTree({ records, runs, leases, workspaceRoots: ['/src/koryph'] });
    assert.strictEqual(pinned.liveAgentCount, 1);
    // Show all → both counted.
    const all = buildTree({ records, runs, leases, workspaceRoots: [], showAllProjects: true });
    assert.strictEqual(all.liveAgentCount, 2);
  });
});

describe('slot ordering & cost cell', () => {
  it('orders slots by updated_at descending, missing timestamps last', () => {
    const s1 = { phaseId: 'a', terminal: false, slot: slot('a', { updated_at: '2026-07-03T09:10:00Z' }) };
    const s2 = { phaseId: 'b', terminal: false, slot: slot('b', { updated_at: '2026-07-03T09:16:00Z' }) };
    const s3 = { phaseId: 'c', terminal: false, slot: slot('c', { updated_at: undefined }) };
    const ordered = [s1, s2, s3].sort(compareSlots).map((s) => s.phaseId);
    assert.deepStrictEqual(ordered, ['b', 'a', 'c']);
  });

  it('builds slots ordered newest-first inside a run', () => {
    const r = run('koryph', [
      slot('old', { updated_at: '2026-07-03T09:10:00Z' }),
      slot('new', { updated_at: '2026-07-03T09:20:00Z' }),
    ]);
    const m = buildTree({
      records: [rec('koryph', '/src/koryph')],
      runs: new Map([['koryph', r]]),
      leases: [],
      workspaceRoots: ['/src/koryph'],
    });
    assert.deepStrictEqual(m.pinned[0].run!.slots.map((s) => s.phaseId), ['new', 'old']);
  });

  it('renders ~streaming for running, — for queued, and $cost for terminal', () => {
    assert.strictEqual(
      slotCostCell({ phaseId: 'r', terminal: false, slot: slot('r', { status: 'running' }) }),
      '~streaming',
    );
    assert.strictEqual(
      slotCostCell({ phaseId: 'q', terminal: false, slot: slot('q', { status: 'queued' }) }),
      '—',
    );
    assert.strictEqual(
      slotCostCell({ phaseId: 'm', terminal: true, slot: slot('m', { status: 'merged', cost_usd: 0.11 }) }),
      '$0.11',
    );
  });
});

describe('isActiveRun', () => {
  it('is true for a running run and false for done/absent', () => {
    const r = run('koryph', [slot('x')]).value;
    assert.strictEqual(isActiveRun({ runId: r.run_id, status: 'running', wave: 1, slots: [], degraded: false, raw: {} }), true);
    assert.strictEqual(isActiveRun({ runId: r.run_id, status: 'done', wave: 1, slots: [], degraded: false, raw: {} }), false);
    assert.strictEqual(isActiveRun(undefined), false);
  });
});

describe('buildTree — real ledger fixture', () => {
  it('groups the fixture koryph run with its four slots', async () => {
    const w = new LedgerWatcher(FIXTURE_REPO, { pollOnly: true, pollMs: 50 });
    try {
      const parsed = await w.load();
      const m = buildTree({
        records: [rec('koryph', FIXTURE_REPO)],
        runs: new Map([['koryph', parsed]]),
        leases: [lease('koryph', 'koryph-i2n', 4821), lease('koryph', 'koryph-fr3.1', 4930)],
        workspaceRoots: [FIXTURE_REPO],
      });
      const koryph = m.pinned[0];
      assert.strictEqual(koryph.run!.slots.length, 4);
      assert.strictEqual(koryph.run!.status, 'running');
      assert.strictEqual(koryph.run!.wave, 3);
      assert.strictEqual(m.liveAgentCount, 2);
    } finally {
      w.dispose();
    }
  });
});
