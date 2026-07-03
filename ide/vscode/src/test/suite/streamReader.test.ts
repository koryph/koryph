// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as fs from 'fs';
import * as path from 'path';
import { StreamReader } from '../../data/streamReader';
import { FIXTURE_REPO, mkScratch, rmScratch } from './helpers';

const FIXTURE_STREAM = path.join(
  FIXTURE_REPO,
  '.plan-logs',
  'koryph',
  '20260703-091422',
  'koryph-i2n',
  'stream.jsonl',
);

describe('StreamReader', () => {
  it('parses the whole fixture stream in one pass', async () => {
    const r = new StreamReader(FIXTURE_STREAM);
    const events = await r.readAll();
    // The fixture has 9 well-formed JSON lines.
    assert.strictEqual(events.length, 9);
    assert.strictEqual(events[0].type, 'system');
    const result = events[events.length - 1];
    assert.strictEqual(result.type, 'result');
    r.dispose();
  });

  it('surfaces unknown event types verbatim (forward-compatible envelope)', async () => {
    const r = new StreamReader(FIXTURE_STREAM);
    const events = await r.readAll();
    const unknown = events.find((e) => e.type === 'koryph_checkpoint');
    assert.ok(unknown, 'unknown event type should be retained, not dropped');
    assert.ok(unknown!.raw && typeof unknown!.raw === 'object');
    r.dispose();
  });

  it('advances by byte offset and holds a partial trailing line', async () => {
    const scratch = mkScratch();
    const file = path.join(scratch, 'stream.jsonl');
    try {
      fs.writeFileSync(file, '{"type":"a","n":1}\n');
      const r = new StreamReader(file);

      let events = await r.read();
      assert.strictEqual(events.length, 1);
      assert.strictEqual(events[0].type, 'a');

      // Nothing new yet.
      assert.strictEqual((await r.read()).length, 0);

      // Append a complete line plus an incomplete fragment.
      fs.appendFileSync(file, '{"type":"b","n":2}\n{"type":"c",');
      events = await r.read();
      assert.strictEqual(events.length, 1, 'only the complete line should parse');
      assert.strictEqual(events[0].type, 'b');

      // Complete the fragment — the held remainder joins the new bytes.
      fs.appendFileSync(file, '"n":3}\n');
      events = await r.read();
      assert.strictEqual(events.length, 1);
      assert.strictEqual(events[0].type, 'c');

      r.dispose();
    } finally {
      rmScratch(scratch);
    }
  });

  it('detects truncation/rotation and re-reads from the top', async () => {
    const scratch = mkScratch();
    const file = path.join(scratch, 'stream.jsonl');
    try {
      fs.writeFileSync(file, '{"type":"old","n":1}\n{"type":"old","n":2}\n');
      const r = new StreamReader(file);
      assert.strictEqual((await r.read()).length, 2);

      // A new run reuses the path with a shorter file.
      fs.writeFileSync(file, '{"type":"new","n":1}\n');
      const events = await r.read();
      assert.strictEqual(events.length, 1);
      assert.strictEqual(events[0].type, 'new');
      assert.strictEqual(r.position, '{"type":"new","n":1}\n'.length);

      r.dispose();
    } finally {
      rmScratch(scratch);
    }
  });

  it('returns [] for an absent stream file', async () => {
    const r = new StreamReader('/nonexistent/stream.jsonl');
    assert.deepStrictEqual(await r.read(), []);
    r.dispose();
  });
});
