// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// TranscriptModel — the pure line-of-thought renderer for a slot's
// stream.jsonl (design §4). It reduces a sequence of claude stream-json events
// (`--output-format stream-json --include-partial-messages`) into a stable,
// ordered list of render items and a running spend estimate. It is the
// unit-tested core of the transcript webview: NO vscode / DOM dependency, so
// the whole event grammar (assistant deltas, tool_use/tool_result chips, the
// final result footer, and unknown/forward-compatible envelopes) is asserted
// against fixture streams in plain Node.
//
// Grammar handled (best-effort, never throwing — malformed events degrade to a
// collapsed raw item rather than crashing):
//
//   system(init)                  → SystemItem (model / session / tools / cwd)
//   stream_event/content_block_delta/text_delta
//                                 → provisional AssistantItem (flowing text)
//   assistant.message.content[]   → text finalizes the provisional block;
//                                   tool_use opens a pending ToolItem;
//                                   usage updates the running spend
//   user.message.content[]/tool_result
//                                 → resolves the matching ToolItem (ok/error)
//   result                        → ResultItem footer (cost / duration / turns)
//   <anything else / unparseable> → RawItem (collapsed raw JSON)
//
// Items carry stable ids; `apply()` returns upsert ops so the webview can patch
// incrementally (a provisional assistant block becomes final in place; a
// pending tool chip flips to resolved in place) without re-rendering the whole
// transcript.

import { StreamEvent } from '../data/streamReader';

// ---------------------------------------------------------------------------
// Render item model
// ---------------------------------------------------------------------------

export type TranscriptItemKind = 'system' | 'assistant' | 'tool' | 'result' | 'raw';

/** system(init) — the run header claude emits first. */
export interface SystemItem {
  kind: 'system';
  id: string;
  model?: string;
  sessionId?: string;
  tools?: string[];
  cwd?: string;
}

/** A block of assistant prose. `provisional` while only partial deltas exist. */
export interface AssistantItem {
  kind: 'assistant';
  id: string;
  text: string;
  provisional: boolean;
}

/** A tool_use call, expandable; resolves when its tool_result arrives. */
export interface ToolItem {
  kind: 'tool';
  id: string;
  toolUseId?: string;
  name: string;
  /** One-line collapsed summary of the input (chip label). */
  inputSummary: string;
  /** The full input object, for the expanded view. */
  input?: unknown;
  status: 'pending' | 'ok' | 'error';
  /** One-line collapsed summary of the result. */
  resultSummary?: string;
  /** The full result payload, for the expanded view. */
  result?: unknown;
}

/** Cumulative token usage (as reported by claude; cumulative-ish, approximate). */
export interface TokenUsage {
  input?: number;
  output?: number;
}

/** The final result event — a cost/duration footer. */
export interface ResultItem {
  kind: 'result';
  id: string;
  subtype?: string;
  isError?: boolean;
  durationMs?: number;
  numTurns?: number;
  costUsd?: number;
  tokens?: TokenUsage;
}

/** Any unrecognized (or malformed) event — rendered as collapsed raw JSON. */
export interface RawItem {
  kind: 'raw';
  id: string;
  type?: string;
  raw: unknown;
}

export type TranscriptItem = SystemItem | AssistantItem | ToolItem | ResultItem | RawItem;

/**
 * The running spend estimate. Deliberately labeled approximate: the ledger's
 * authoritative `cost_usd` only lands at completion (design §4); until then we
 * surface the last cost/usage the stream reported.
 */
export interface TranscriptSpend {
  /** Last cost seen in the stream (result.total_cost_usd or any *cost_usd). */
  costUsd?: number;
  /** Last cumulative token usage seen. */
  tokens: TokenUsage;
  /** Always true — this is an estimate, never the authoritative ledger figure. */
  approximate: true;
}

/** One incremental change to apply to the rendered transcript (upsert by id). */
export interface TranscriptOp {
  item: TranscriptItem;
}

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------

const INPUT_SUMMARY_MAX = 140;
const RESULT_SUMMARY_MAX = 200;

export class TranscriptModel {
  private readonly order: string[] = [];
  private readonly byId = new Map<string, TranscriptItem>();
  private readonly toolByUseId = new Map<string, ToolItem>();
  private liveAssistant: AssistantItem | undefined;
  private seq = 0;
  private cost: number | undefined;
  private tokens: TokenUsage = {};

  /** Feed newly-read events; returns the upsert ops the view should apply. */
  apply(events: StreamEvent[]): TranscriptOp[] {
    const ops: TranscriptOp[] = [];
    const emit = (item: TranscriptItem) => {
      const isNew = !this.byId.has(item.id);
      this.byId.set(item.id, item);
      if (isNew) {
        this.order.push(item.id);
      }
      ops.push({ item });
    };
    for (const ev of events) {
      try {
        this.reduce(ev, emit);
      } catch {
        // A single malformed event must never break the stream: fall back to
        // a raw item so the user still sees *something* and following events
        // keep rendering.
        emit(this.rawItem(ev));
      }
    }
    return ops;
  }

  /** The full ordered item list (for a fresh webview / re-hydration). */
  snapshot(): TranscriptItem[] {
    return this.order.map((id) => this.byId.get(id)!).filter(Boolean);
  }

  /** The current running spend estimate. */
  spend(): TranscriptSpend {
    return { costUsd: this.cost, tokens: { ...this.tokens }, approximate: true };
  }

  // -------------------------------------------------------------------------

  private reduce(ev: StreamEvent, emit: (item: TranscriptItem) => void): void {
    const raw = ev.raw;
    switch (ev.type) {
      case 'system':
        emit(this.systemItem(raw));
        return;
      case 'stream_event':
        this.reduceStreamEvent(raw, emit);
        return;
      case 'assistant':
        this.reduceAssistant(raw, emit);
        return;
      case 'user':
        this.reduceUser(raw, emit);
        return;
      case 'result':
        emit(this.resultItem(raw));
        return;
      default:
        emit(this.rawItem(ev));
        return;
    }
  }

  private reduceStreamEvent(raw: unknown, emit: (item: TranscriptItem) => void): void {
    const event = obj(raw)?.event;
    const inner = obj(event);
    if (inner?.type !== 'content_block_delta') {
      // Non-text partial (e.g. message_start / content_block_start): ignore for
      // rendering — the authoritative `assistant` message carries the content.
      return;
    }
    const delta = obj(inner.delta);
    if (delta?.type !== 'text_delta' || typeof delta.text !== 'string') {
      return;
    }
    if (!this.liveAssistant) {
      this.liveAssistant = {
        kind: 'assistant',
        id: `a${this.seq++}`,
        text: delta.text,
        provisional: true,
      };
    } else {
      this.liveAssistant = { ...this.liveAssistant, text: this.liveAssistant.text + delta.text };
    }
    emit(this.liveAssistant);
  }

  private reduceAssistant(raw: unknown, emit: (item: TranscriptItem) => void): void {
    const message = obj(obj(raw)?.message);
    this.absorbUsage(message?.usage);
    const content = arr(message?.content);
    if (!content) {
      // A shape we do not recognize — keep it visible as raw.
      emit(this.rawItem({ type: 'assistant', raw }));
      return;
    }
    let consumedLive = false;
    for (const block of content) {
      const b = obj(block);
      if (!b) {
        continue;
      }
      if (b.type === 'text' && typeof b.text === 'string') {
        if (this.liveAssistant && !consumedLive) {
          // Finalize the flowing block in place (same id → in-place patch).
          const finalized: AssistantItem = { ...this.liveAssistant, text: b.text, provisional: false };
          this.liveAssistant = undefined;
          consumedLive = true;
          emit(finalized);
        } else {
          emit({ kind: 'assistant', id: `a${this.seq++}`, text: b.text, provisional: false });
        }
      } else if (b.type === 'tool_use') {
        // A tool call ends the current text turn.
        this.liveAssistant = undefined;
        emit(this.toolUseItem(b));
      }
    }
  }

  private reduceUser(raw: unknown, emit: (item: TranscriptItem) => void): void {
    const message = obj(obj(raw)?.message);
    const content = arr(message?.content);
    if (!content) {
      emit(this.rawItem({ type: 'user', raw }));
      return;
    }
    for (const block of content) {
      const b = obj(block);
      if (!b || b.type !== 'tool_result') {
        continue;
      }
      const useId = typeof b.tool_use_id === 'string' ? b.tool_use_id : undefined;
      const summary = summarize(flattenResult(b.content), RESULT_SUMMARY_MAX);
      const isError = b.is_error === true;
      const existing = useId ? this.toolByUseId.get(useId) : undefined;
      if (existing) {
        const resolved: ToolItem = {
          ...existing,
          status: isError ? 'error' : 'ok',
          resultSummary: summary,
          result: b.content,
        };
        this.toolByUseId.set(useId!, resolved);
        emit(resolved);
      } else {
        // Orphan result (no matching call seen — out-of-order or truncated).
        emit({
          kind: 'tool',
          id: `t${this.seq++}`,
          toolUseId: useId,
          name: '(result)',
          inputSummary: '',
          status: isError ? 'error' : 'ok',
          resultSummary: summary,
          result: b.content,
        });
      }
    }
  }

  private systemItem(raw: unknown): SystemItem {
    const o = obj(raw) ?? {};
    return {
      kind: 'system',
      id: `sys${this.seq++}`,
      model: str(o.model),
      sessionId: str(o.session_id),
      tools: arr(o.tools)?.filter((t): t is string => typeof t === 'string'),
      cwd: str(o.cwd),
    };
  }

  private toolUseItem(b: Record<string, unknown>): ToolItem {
    const name = str(b.name) ?? '(tool)';
    const item: ToolItem = {
      kind: 'tool',
      id: `t${this.seq++}`,
      toolUseId: str(b.id),
      name,
      inputSummary: summarizeInput(name, b.input),
      input: b.input,
      status: 'pending',
    };
    if (item.toolUseId) {
      this.toolByUseId.set(item.toolUseId, item);
    }
    return item;
  }

  private resultItem(raw: unknown): ResultItem {
    const o = obj(raw) ?? {};
    const cost = num(o.total_cost_usd) ?? num(o.cost_usd);
    if (cost !== undefined) {
      this.cost = cost;
    }
    this.absorbUsage(o.usage);
    return {
      kind: 'result',
      id: `result${this.seq++}`,
      subtype: str(o.subtype),
      isError: o.is_error === true,
      durationMs: num(o.duration_ms),
      numTurns: num(o.num_turns),
      costUsd: cost ?? this.cost,
      tokens: { ...this.tokens },
    };
  }

  private rawItem(ev: StreamEvent | { type?: string; raw: unknown }): RawItem {
    return { kind: 'raw', id: `raw${this.seq++}`, type: ev.type, raw: ev.raw };
  }

  /** Fold a usage object into the running estimate (last-write-wins). */
  private absorbUsage(usage: unknown): void {
    const u = obj(usage);
    if (!u) {
      return;
    }
    const input = num(u.input_tokens);
    const output = num(u.output_tokens);
    if (input !== undefined) {
      this.tokens.input = input;
    }
    if (output !== undefined) {
      this.tokens.output = output;
    }
    const cost = num(u.cost_usd) ?? num(u.total_cost_usd);
    if (cost !== undefined) {
      this.cost = cost;
    }
  }
}

// ---------------------------------------------------------------------------
// Summaries (pure, exported for tests)
// ---------------------------------------------------------------------------

/** Build a one-line chip label for a tool_use input (Read→path, Bash→cmd, …). */
export function summarizeInput(tool: string, input: unknown): string {
  const o = obj(input);
  if (o) {
    const preferred =
      str(o.command) ??
      str(o.file_path) ??
      str(o.path) ??
      str(o.pattern) ??
      str(o.url) ??
      str(o.description);
    if (preferred !== undefined) {
      return truncate(preferred.replace(/\s+/g, ' ').trim(), INPUT_SUMMARY_MAX);
    }
  }
  if (input === undefined) {
    return '';
  }
  return truncate(safeStringify(input).replace(/\s+/g, ' ').trim(), INPUT_SUMMARY_MAX);
}

/** Flatten a tool_result `content` (string, or [{type:'text',text}]) to text. */
export function flattenResult(content: unknown): string {
  if (typeof content === 'string') {
    return content;
  }
  const parts = arr(content);
  if (parts) {
    return parts
      .map((p) => {
        const o = obj(p);
        if (o && o.type === 'text' && typeof o.text === 'string') {
          return o.text;
        }
        return typeof p === 'string' ? p : '';
      })
      .join('');
  }
  return content === undefined ? '' : safeStringify(content);
}

function summarize(text: string, max: number): string {
  const firstLine = text.split('\n').find((l) => l.trim().length > 0) ?? '';
  return truncate(firstLine.trim(), max);
}

// ---------------------------------------------------------------------------
// Safe accessors — never throw on malformed input
// ---------------------------------------------------------------------------

function obj(v: unknown): Record<string, unknown> | undefined {
  return v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined;
}

function arr(v: unknown): unknown[] | undefined {
  return Array.isArray(v) ? v : undefined;
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

function truncate(s: string, max: number): string {
  return s.length > max ? `${s.slice(0, max - 1)}…` : s;
}

function safeStringify(v: unknown): string {
  try {
    return JSON.stringify(v) ?? String(v);
  } catch {
    return String(v);
  }
}
