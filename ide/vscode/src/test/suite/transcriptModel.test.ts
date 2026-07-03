// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

import * as assert from 'assert';
import * as path from 'path';
import { StreamReader, StreamEvent } from '../../data/streamReader';
import {
  AssistantItem,
  RawItem,
  ResultItem,
  ToolItem,
  TranscriptItem,
  TranscriptModel,
  flattenResult,
  summarizeInput,
} from '../../webview/transcriptModel';
import { FIXTURE_REPO } from './helpers';

const FIXTURE_STREAM = path.join(
  FIXTURE_REPO,
  '.plan-logs',
  'koryph',
  '20260703-091422',
  'koryph-i2n',
  'stream.jsonl',
);

/** Wrap a raw record as a StreamEvent the way StreamReader does. */
function ev(raw: unknown): StreamEvent {
  const t = raw && typeof raw === 'object' ? (raw as { type?: unknown }).type : undefined;
  return typeof t === 'string' ? { type: t, raw } : { raw };
}

function byKind<T extends TranscriptItem['kind']>(
  items: TranscriptItem[],
  kind: T,
): Extract<TranscriptItem, { kind: T }>[] {
  return items.filter((i) => i.kind === kind) as Extract<TranscriptItem, { kind: T }>[];
}

describe('TranscriptModel — fixture stream', () => {
  it('reduces the fixture into the expected line of thought', async () => {
    const reader = new StreamReader(FIXTURE_STREAM);
    const model = new TranscriptModel();
    model.apply(await reader.readAll());
    reader.dispose();
    const items = model.snapshot();

    // system + one assistant block + two tool chips + one raw + one result.
    assert.deepStrictEqual(
      items.map((i) => i.kind),
      ['system', 'assistant', 'tool', 'tool', 'raw', 'result'],
    );

    const sys = items[0];
    assert.strictEqual(sys.kind, 'system');
    if (sys.kind === 'system') {
      assert.strictEqual(sys.model, 'claude-opus-4');
      assert.deepStrictEqual(sys.tools, ['Read', 'Edit', 'Bash']);
    }

    // The provisional delta was superseded by the authoritative assistant text.
    const assistant = byKind(items, 'assistant')[0] as AssistantItem;
    assert.strictEqual(assistant.provisional, false);
    assert.strictEqual(assistant.text, "I'll wire up the completions handler.");

    const tools = byKind(items, 'tool') as ToolItem[];
    assert.strictEqual(tools[0].name, 'Read');
    assert.strictEqual(tools[0].inputSummary, 'internal/completions/handler.go');
    assert.strictEqual(tools[0].status, 'ok');
    assert.ok((tools[0].resultSummary ?? '').startsWith('package completions'));
    assert.strictEqual(tools[1].name, 'Bash');
    assert.strictEqual(tools[1].inputSummary, 'go test ./internal/completions/...');
    assert.strictEqual(tools[1].status, 'ok');
  });

  it('renders unknown event types as raw (forward-compatible envelope)', async () => {
    const reader = new StreamReader(FIXTURE_STREAM);
    const model = new TranscriptModel();
    model.apply(await reader.readAll());
    reader.dispose();
    const raw = byKind(model.snapshot(), 'raw')[0] as RawItem;
    assert.strictEqual(raw.type, 'koryph_checkpoint');
    assert.ok(raw.raw && typeof raw.raw === 'object');
  });

  it('surfaces the running spend estimate, labeled approximate', async () => {
    const reader = new StreamReader(FIXTURE_STREAM);
    const model = new TranscriptModel();
    model.apply(await reader.readAll());
    reader.dispose();
    const spend = model.spend();
    assert.strictEqual(spend.approximate, true);
    assert.strictEqual(spend.costUsd, 0.42);
    assert.strictEqual(spend.tokens.input, 3600);
    assert.strictEqual(spend.tokens.output, 144);

    const result = byKind(model.snapshot(), 'result')[0] as ResultItem;
    assert.strictEqual(result.durationMs, 48210);
    assert.strictEqual(result.numTurns, 4);
    assert.strictEqual(result.costUsd, 0.42);
    assert.strictEqual(result.isError, false);
  });
});

describe('TranscriptModel — incremental deltas', () => {
  it('accumulates partial text deltas, then finalizes in place on the message', () => {
    const model = new TranscriptModel();
    const delta = (text: string) =>
      ev({ type: 'stream_event', event: { type: 'content_block_delta', delta: { type: 'text_delta', text } } });

    let ops = model.apply([delta('Hel')]);
    assert.strictEqual(ops.length, 1);
    let a = ops[0].item as AssistantItem;
    assert.strictEqual(a.text, 'Hel');
    assert.strictEqual(a.provisional, true);
    const liveId = a.id;

    ops = model.apply([delta('lo world')]);
    a = ops[0].item as AssistantItem;
    assert.strictEqual(a.id, liveId, 'same item id — patched in place, not appended');
    assert.strictEqual(a.text, 'Hello world');
    assert.strictEqual(a.provisional, true);

    // The authoritative assistant message finalizes the same block.
    ops = model.apply([
      ev({ type: 'assistant', message: { content: [{ type: 'text', text: 'Hello world.' }] } }),
    ]);
    a = ops[0].item as AssistantItem;
    assert.strictEqual(a.id, liveId);
    assert.strictEqual(a.text, 'Hello world.');
    assert.strictEqual(a.provisional, false);

    // Exactly one assistant item in the transcript (no duplicate flow + final).
    assert.strictEqual(model.snapshot().filter((i) => i.kind === 'assistant').length, 1);
  });
});

describe('TranscriptModel — tolerance', () => {
  it('never throws on malformed events; degrades to raw', () => {
    const model = new TranscriptModel();
    const garbage: StreamEvent[] = [
      { raw: null },
      { raw: 42 },
      { type: 'assistant', raw: { message: { content: 'not-an-array' } } },
      { type: 'assistant', raw: {} },
      { type: 'user', raw: { message: {} } },
      { type: 'stream_event', raw: { event: { type: 'message_start' } } }, // ignored, no item
      { type: 'weird_future_type', raw: { type: 'weird_future_type', a: 1 } },
    ];
    const ops = model.apply(garbage);
    // message_start yields no item; everything else is retained as raw.
    assert.ok(ops.length >= 5);
    assert.ok(model.snapshot().every((i) => i.kind === 'raw'));
  });

  it('handles an orphan tool_result (no matching call)', () => {
    const model = new TranscriptModel();
    model.apply([
      ev({
        type: 'user',
        message: {
          content: [{ type: 'tool_result', tool_use_id: 'toolu_x', content: 'done', is_error: true }],
        },
      }),
    ]);
    const tool = model.snapshot().find((i) => i.kind === 'tool') as ToolItem;
    assert.ok(tool);
    assert.strictEqual(tool.name, '(result)');
    assert.strictEqual(tool.status, 'error');
    assert.strictEqual(tool.resultSummary, 'done');
  });

  it('matches a tool_result to its earlier tool_use and flips status', () => {
    const model = new TranscriptModel();
    model.apply([
      ev({
        type: 'assistant',
        message: { content: [{ type: 'tool_use', id: 'toolu_9', name: 'Bash', input: { command: 'ls' } }] },
      }),
    ]);
    let tool = model.snapshot().find((i) => i.kind === 'tool') as ToolItem;
    assert.strictEqual(tool.status, 'pending');
    const id = tool.id;

    model.apply([
      ev({
        type: 'user',
        message: { content: [{ type: 'tool_result', tool_use_id: 'toolu_9', content: 'a\nb', is_error: false }] },
      }),
    ]);
    tool = model.snapshot().find((i) => i.kind === 'tool') as ToolItem;
    assert.strictEqual(tool.id, id, 'resolved in place, not appended');
    assert.strictEqual(tool.status, 'ok');
    assert.strictEqual(tool.resultSummary, 'a');
    assert.strictEqual(model.snapshot().filter((i) => i.kind === 'tool').length, 1);
  });
});

describe('summary helpers', () => {
  it('summarizeInput prefers command / file_path / url', () => {
    assert.strictEqual(summarizeInput('Bash', { command: 'go test ./...' }), 'go test ./...');
    assert.strictEqual(summarizeInput('Read', { file_path: 'a/b.go' }), 'a/b.go');
    assert.strictEqual(summarizeInput('X', { foo: 1 }), '{"foo":1}');
    assert.strictEqual(summarizeInput('X', undefined), '');
  });

  it('flattenResult handles string and text-block array content', () => {
    assert.strictEqual(flattenResult('plain'), 'plain');
    assert.strictEqual(
      flattenResult([{ type: 'text', text: 'a' }, { type: 'text', text: 'b' }]),
      'ab',
    );
    assert.strictEqual(flattenResult(undefined), '');
  });
});
