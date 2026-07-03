// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// QuotaStatusBar — one status-bar item per account that owns a visible project
// with an active run (§5). Refresh is slow and async (koryph.quotaRefreshMinutes,
// default 5) and NEVER blocks the UI: `koryph quota show --json` runs off the
// event loop with the documented 40 s worst case; between snapshots the item
// shows cached ceilings from ~/.koryph/quota/<account>.json. All shaping is the
// pure `./quotaModel`; this file is only the VS Code glue (items, colors, the
// refresh loop, the click quick-pick). Degrades gracefully when `--json` is
// absent on an older CLI.

import * as vscode from 'vscode';
import { CliAdapter } from '../data/cli';
import { QuotaReader } from '../data/quotaReader';
import { QuotaLevel, QuotaSnapshot } from '../data/schema';
import {
  QuotaItem,
  formatCeilingOnly,
  formatSnapshot,
  involvedAccounts,
  parseQuotaSnapshots,
} from './quotaModel';
import { ProjectNode } from '../tree/model';

/** Click command that pops the full snapshot for an account. */
export const QUOTA_SHOW_COMMAND = 'koryph.quota.showAccount';

const DEFAULT_REFRESH_MINUTES = 5;

export class QuotaStatusBar {
  private readonly items = new Map<string, vscode.StatusBarItem>();
  private readonly tooltips = new Map<string, string>();
  private timer?: ReturnType<typeof setInterval>;
  private readonly disposables: vscode.Disposable[] = [];
  private refreshing = false;

  constructor(
    private readonly cli: CliAdapter,
    private readonly quota: QuotaReader,
    /** Supplies the currently visible projects (from the tree provider). */
    private readonly getVisible: () => Promise<ProjectNode[]>,
  ) {
    this.disposables.push(
      vscode.commands.registerCommand(QUOTA_SHOW_COMMAND, (account: string) =>
        this.showAccount(account),
      ),
    );
  }

  /** Start the slow async refresh loop and do one immediate (non-blocking) pass. */
  start(): void {
    void this.refresh();
    this.scheduleTimer();
    // Re-time when the interval setting changes.
    this.disposables.push(
      vscode.workspace.onDidChangeConfiguration((e) => {
        if (e.affectsConfiguration('koryph.quotaRefreshMinutes')) {
          this.scheduleTimer();
        }
        if (e.affectsConfiguration('koryph.showAllProjects')) {
          void this.refresh();
        }
      }),
    );
  }

  /** Trigger an out-of-band refresh (e.g. after a tree change). Non-blocking. */
  refreshSoon(): void {
    void this.refresh();
  }

  dispose(): void {
    if (this.timer) {
      clearInterval(this.timer);
    }
    for (const item of this.items.values()) {
      item.dispose();
    }
    this.items.clear();
    for (const d of this.disposables) {
      d.dispose();
    }
  }

  // --- refresh -------------------------------------------------------------

  private scheduleTimer(): void {
    if (this.timer) {
      clearInterval(this.timer);
    }
    const mins = refreshMinutes();
    this.timer = setInterval(() => void this.refresh(), mins * 60_000);
    if (typeof this.timer.unref === 'function') {
      this.timer.unref();
    }
  }

  /**
   * Recompute the involved accounts, reconcile status-bar items, and repaint
   * each from a fresh snapshot (falling back to cached ceilings). Guarded so
   * overlapping ticks don't stack. Never throws to the caller.
   */
  private async refresh(): Promise<void> {
    if (this.refreshing) {
      return;
    }
    this.refreshing = true;
    try {
      const visible = await this.getVisible();
      const accounts = involvedAccounts(visible);
      this.reconcileItems(accounts);
      if (accounts.length === 0) {
        return;
      }

      const snapshots = await this.loadSnapshots();
      const byAccount = new Map(snapshots?.map((s) => [s.account, s]) ?? []);
      const now = Date.now();
      for (const account of accounts) {
        const snap = byAccount.get(account);
        const model = snap
          ? formatSnapshot(snap, now)
          : formatCeilingOnly(account, await this.quota.get(account));
        this.paint(account, model);
      }
    } catch {
      /* status bar is advisory; a failed refresh leaves the last paint in place */
    } finally {
      this.refreshing = false;
    }
  }

  /** Run `koryph quota show --json`; undefined when the CLI/flag is unavailable. */
  private async loadSnapshots(): Promise<QuotaSnapshot[] | undefined> {
    try {
      const res = await this.cli.koryph(['quota', 'show', '--json']);
      if (res.code !== 0) {
        return undefined; // older CLI without --json, or a transient failure
      }
      return parseQuotaSnapshots(res.stdout);
    } catch {
      return undefined;
    }
  }

  // --- item lifecycle ------------------------------------------------------

  private reconcileItems(accounts: string[]): void {
    const wanted = new Set(accounts);
    for (const [account, item] of this.items) {
      if (!wanted.has(account)) {
        item.dispose();
        this.items.delete(account);
        this.tooltips.delete(account);
      }
    }
    let priority = 100;
    for (const account of accounts) {
      if (!this.items.has(account)) {
        const item = vscode.window.createStatusBarItem(
          `koryph.quota.${account}`,
          vscode.StatusBarAlignment.Right,
          priority--,
        );
        item.name = `Koryph Quota — ${account}`;
        item.command = { command: QUOTA_SHOW_COMMAND, title: 'Show', arguments: [account] };
        this.items.set(account, item);
      }
    }
  }

  private paint(account: string, model: QuotaItem): void {
    const item = this.items.get(account);
    if (!item) {
      return;
    }
    item.text = model.text;
    item.tooltip = model.tooltip + (model.stale ? '\n(cached — awaiting live snapshot)' : '');
    item.backgroundColor = levelColor(model.level);
    this.tooltips.set(account, model.tooltip);
    item.show();
  }

  private async showAccount(account: string): Promise<void> {
    const body = this.tooltips.get(account) ?? `Koryph quota — ${account}`;
    const lines = body.split('\n').filter(Boolean);
    const picked = await vscode.window.showQuickPick(
      [...lines.map((label) => ({ label })), { label: '$(beaker) Calibrate…', detail: 'Run /koryph-calibrate' }],
      { title: `Koryph quota — ${account}` },
    );
    if (picked?.label.includes('Calibrate')) {
      void vscode.window.showInformationMessage(
        `Calibrate ${account} with the /koryph-calibrate slash command (or: koryph quota calibrate --account ${account}).`,
      );
    }
  }
}

// --- glue helpers ----------------------------------------------------------

function refreshMinutes(): number {
  const raw = vscode.workspace.getConfiguration('koryph').get<number>('quotaRefreshMinutes');
  if (typeof raw === 'number' && Number.isFinite(raw) && raw > 0) {
    return raw;
  }
  return DEFAULT_REFRESH_MINUTES;
}

/** Map a governor level to a status-bar background ThemeColor (undefined = OK). */
function levelColor(level: QuotaLevel): vscode.ThemeColor | undefined {
  switch (level) {
    case QuotaLevel.Warn:
      return new vscode.ThemeColor('statusBarItem.warningBackground');
    case QuotaLevel.Drain:
    case QuotaLevel.Stop:
      return new vscode.ThemeColor('statusBarItem.errorBackground');
    default:
      return undefined;
  }
}
