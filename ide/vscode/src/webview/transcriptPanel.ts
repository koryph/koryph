// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// KoryphTranscriptPanel — the per-slot transcript webview (design §4). One
// panel per slot (re-invoking Open Transcript reveals the existing one),
// unlimited concurrent panels across slots, `retainContextWhenHidden` so a
// backgrounded panel keeps its scroll and DOM. It:
//
//   - drives a StreamReader over the slot's stream.jsonl and feeds a pure
//     TranscriptModel, pushing incremental upsert ops to the webview;
//   - tails stderr.log and session.log (plain text tabs);
//   - renders a header strip (bead / status / model / attempts) with
//     stop / nudge / worktree buttons wired to the ext.6 command layer;
//   - loads NOTHING from the network or disk into the webview — a strict,
//     nonce-based CSP (see transcriptHtml.ts) is the only script/style source.
//
// Single-writer discipline (Decision 3): this panel is READ-ONLY on koryph
// state. Every button routes through vscode.commands (the CLI shell-outs), so
// the webview never mutates a file or signals a PID directly.

import * as path from 'path';
import * as fsp from 'fs/promises';
import * as vscode from 'vscode';
import { SlotRef } from '../commands/argv';
import { CommandIds } from '../commands';
import { StreamReader } from '../data/streamReader';
import { PathWatcher } from '../data/watcher';
import { TranscriptModel } from './transcriptModel';
import { TranscriptHeader, transcriptHtml } from './transcriptHtml';

/** Cap on how much of a plain log tab we ship (bytes) — tail of the file. */
const LOG_TAIL_BYTES = 256 * 1024;

/** A stable per-slot key so a second "Open Transcript" reveals, not duplicates. */
function panelKey(ref: SlotRef): string {
  return `${ref.projectId}::${ref.phaseId}`;
}

export class KoryphTranscriptPanel {
  private static readonly open = new Map<string, KoryphTranscriptPanel>();

  /**
   * Open (or reveal) the transcript panel for a slot. Exported as the
   * `openTranscript` hook the command layer calls.
   */
  static show(ref: SlotRef, column: vscode.ViewColumn = vscode.ViewColumn.Beside): KoryphTranscriptPanel {
    const key = panelKey(ref);
    const existing = KoryphTranscriptPanel.open.get(key);
    if (existing) {
      existing.ref = ref; // refresh header coordinates
      existing.panel.reveal(existing.panel.viewColumn ?? column);
      existing.postHeader();
      return existing;
    }
    const panel = vscode.window.createWebviewPanel(
      'koryphTranscript',
      `⛓ ${ref.phaseId}`,
      column,
      {
        enableScripts: true,
        retainContextWhenHidden: true,
        // No resource roots: the webview loads only its inline, nonce-guarded
        // script/style — nothing from disk or the network.
        localResourceRoots: [],
      },
    );
    const inst = new KoryphTranscriptPanel(panel, ref);
    KoryphTranscriptPanel.open.set(key, inst);
    return inst;
  }

  private readonly model = new TranscriptModel();
  private readonly reader: StreamReader;
  private readonly disposables: vscode.Disposable[] = [];
  private readonly logWatchers: PathWatcher[] = [];
  private disposed = false;

  private constructor(
    private readonly panel: vscode.WebviewPanel,
    private ref: SlotRef,
  ) {
    this.reader = new StreamReader(this.streamPath());
    this.panel.webview.html = this.render();

    this.panel.webview.onDidReceiveMessage(
      (msg) => this.onMessage(msg),
      undefined,
      this.disposables,
    );
    this.panel.onDidDispose(() => this.dispose(), undefined, this.disposables);

    // Drain the stream incrementally as it grows.
    const streamSub = this.reader.watch(() => void this.drainStream());
    this.disposables.push({ dispose: () => streamSub.dispose() });

    // Tail the plain log tabs.
    this.watchLog('stderr', this.logPath('stderr.log'));
    this.watchLog('session', this.logPath('session.log'));
  }

  // -------------------------------------------------------------------------
  // Path resolution
  // -------------------------------------------------------------------------

  /** The stream.jsonl path (from the ledger slot). */
  private streamPath(): string {
    return this.ref.stream;
  }

  /** The phase directory (holds stream.jsonl, stderr.log, session.log). */
  private phaseDir(): string {
    return this.ref.stream ? path.dirname(this.ref.stream) : '';
  }

  private logPath(name: string): string {
    const dir = this.phaseDir();
    return dir ? path.join(dir, name) : '';
  }

  // -------------------------------------------------------------------------
  // Webview messaging
  // -------------------------------------------------------------------------

  private render(): string {
    const nonce = makeNonce();
    return transcriptHtml(nonce, this.panel.webview.cspSource);
  }

  private header(): TranscriptHeader {
    return {
      bead: this.ref.beadId || this.ref.phaseId,
      status: this.ref.status,
      model: this.ref.model,
      attempts: this.ref.attempts ?? 0,
      worktree: this.ref.worktree,
      project: this.ref.projectId,
    };
  }

  private onMessage(msg: unknown): void {
    const m = msg as { type?: string; action?: string };
    switch (m?.type) {
      case 'ready':
        void this.hydrate();
        return;
      case 'action':
        void this.runAction(m.action);
        return;
      default:
        return;
    }
  }

  /** Send the initial header + full snapshot + logs once the webview is ready. */
  private async hydrate(): Promise<void> {
    this.postHeader();
    // Replay the whole stream from the top into the model, then snapshot.
    const events = await this.reader.readAll();
    this.model.apply(events);
    this.post({
      type: 'ops',
      ops: this.model.snapshot().map((item) => ({ item })),
      spend: this.model.spend(),
    });
    await this.refreshLog('stderr', this.logPath('stderr.log'));
    await this.refreshLog('session', this.logPath('session.log'));
  }

  private async drainStream(): Promise<void> {
    if (this.disposed) {
      return;
    }
    const events = await this.reader.read();
    if (events.length === 0) {
      return;
    }
    const ops = this.model.apply(events);
    this.post({ type: 'ops', ops, spend: this.model.spend() });
  }

  private postHeader(): void {
    this.post({ type: 'header', header: this.header() });
  }

  private post(message: unknown): void {
    if (!this.disposed) {
      void this.panel.webview.postMessage(message);
    }
  }

  private async runAction(action: string | undefined): Promise<void> {
    // Every action is an ext.6 command shell-out (Decision 3) — the panel never
    // mutates state itself. If the commands bead is not present the command id
    // simply won't be registered and executeCommand rejects; we swallow that so
    // the panel stays a read-only viewer.
    const id =
      action === 'stop'
        ? CommandIds.Stop
        : action === 'nudge'
          ? CommandIds.Nudge
          : action === 'worktree'
            ? CommandIds.OpenWorktree
            : undefined;
    if (!id) {
      return;
    }
    try {
      await vscode.commands.executeCommand(id, this.ref);
    } catch {
      void vscode.window.showInformationMessage(
        `Koryph: the "${action}" command is unavailable in this build.`,
      );
    }
  }

  // -------------------------------------------------------------------------
  // Plain log tabs
  // -------------------------------------------------------------------------

  private watchLog(tab: 'stderr' | 'session', file: string): void {
    if (!file) {
      return;
    }
    const watcher = new PathWatcher(file);
    watcher.onChange(() => void this.refreshLog(tab, file));
    this.logWatchers.push(watcher);
  }

  private async refreshLog(tab: 'stderr' | 'session', file: string): Promise<void> {
    if (!file || this.disposed) {
      return;
    }
    const text = await readTail(file, LOG_TAIL_BYTES);
    this.post({ type: 'log', tab, text });
  }

  // -------------------------------------------------------------------------

  dispose(): void {
    if (this.disposed) {
      return;
    }
    this.disposed = true;
    KoryphTranscriptPanel.open.delete(panelKey(this.ref));
    this.reader.dispose();
    for (const w of this.logWatchers) {
      w.dispose();
    }
    for (const d of this.disposables) {
      try {
        d.dispose();
      } catch {
        /* ignore */
      }
    }
    this.panel.dispose();
  }
}

/** Read the last `maxBytes` of a file as UTF-8 (empty string if absent). */
async function readTail(file: string, maxBytes: number): Promise<string> {
  let handle: fsp.FileHandle | undefined;
  try {
    const stat = await fsp.stat(file);
    const start = Math.max(0, stat.size - maxBytes);
    const length = stat.size - start;
    if (length === 0) {
      return '';
    }
    handle = await fsp.open(file, 'r');
    const buffer = Buffer.alloc(length);
    const { bytesRead } = await handle.read(buffer, 0, length, start);
    const prefix = start > 0 ? '…(truncated)…\n' : '';
    return prefix + buffer.subarray(0, bytesRead).toString('utf8');
  } catch {
    return '';
  } finally {
    if (handle) {
      await handle.close();
    }
  }
}

/** A per-load CSP nonce (128 bits of base36 from a non-crypto source is fine
 * for CSP: the nonce only needs to be unguessable within one webview load). */
function makeNonce(): string {
  let s = '';
  const alphabet = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789';
  for (let i = 0; i < 32; i++) {
    s += alphabet[Math.floor(Math.random() * alphabet.length)];
  }
  return s;
}
