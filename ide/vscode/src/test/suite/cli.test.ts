// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import { CliAdapter } from '../../data/cli';
import { mkScratch, rmScratch } from './helpers';

// The stub stands in for the real `koryph` binary so tests never dispatch real
// agents. It is written to a scratch dir at runtime (the boundary guard blocks
// checking standalone script files into fixtures/), made executable, and used
// as koryphBin.
const STUB_SOURCE = [
  '#!/usr/bin/env node',
  '"use strict";',
  'const args = process.argv.slice(2);',
  'if (args[0] === "board" && args.includes("--json")) {',
  '  if (process.env.KORYPH_STUB_MODE === "badboard") { process.stdout.write("{}"); process.exit(0); }',
  '  process.stdout.write(JSON.stringify([',
  '    {project_id:"koryph",migration_status:"validated",account:"personal",run_id:"r1",run_status:"running",slots:{running:1},live_pids:1},',
  '    {project_id:"ncp_roadmap",migration_status:"validated",account:"work",live_pids:0}',
  '  ]));',
  '  process.exit(0);',
  '}',
  'if (args[0] === "env-echo") {',
  '  process.stdout.write(JSON.stringify({KORYPH_HOME: process.env.KORYPH_HOME || ""}));',
  '  process.exit(0);',
  '}',
  'if (args[0] === "boom") { process.stderr.write("simulated failure\\n"); process.exit(2); }',
  'if (args[0] === "not-json") { process.stdout.write("nope"); process.exit(0); }',
  'process.exit(64);',
  '',
].join('\n');

describe('CliAdapter', () => {
  let scratch: string;
  let stub: string;

  before(() => {
    scratch = mkScratch();
    stub = path.join(scratch, 'koryph-stub');
    fs.writeFileSync(stub, STUB_SOURCE, { mode: 0o755 });
  });

  after(() => {
    rmScratch(scratch);
  });

  it('parses `koryph board --json` fan-out output', async () => {
    const cli = new CliAdapter({ koryphBin: stub });
    const board = await cli.board();
    assert.strictEqual(board.length, 2);
    assert.strictEqual(board[0].project_id, 'koryph');
    assert.strictEqual(board[0].live_pids, 1);
    assert.strictEqual(board[1].project_id, 'ncp_roadmap');
  });

  it('forwards KORYPH_HOME to the child process', async () => {
    const cli = new CliAdapter({ koryphBin: stub, env: { ...process.env, KORYPH_HOME: '/opt/kh' } });
    const res = await cli.koryph(['env-echo']);
    assert.strictEqual(res.code, 0);
    assert.deepStrictEqual(JSON.parse(res.stdout), { KORYPH_HOME: '/opt/kh' });
  });

  it('does not reject on non-zero exit — surfaces code + stderr', async () => {
    const cli = new CliAdapter({ koryphBin: stub });
    const res = await cli.koryph(['boom']);
    assert.strictEqual(res.code, 2);
    assert.match(res.stderr, /simulated failure/);
    assert.strictEqual(res.timedOut, false);
  });

  it('board() rejects when the CLI binary is missing (spawn error)', async () => {
    const cli = new CliAdapter({ koryphBin: '/nonexistent/koryph-binary' });
    await assert.rejects(() => cli.board());
  });

  it('board() rejects non-array JSON output', async () => {
    const cli = new CliAdapter({
      koryphBin: stub,
      env: { ...process.env, KORYPH_STUB_MODE: 'badboard' },
    });
    await assert.rejects(() => cli.board(), /did not return a JSON array/);
  });
});
