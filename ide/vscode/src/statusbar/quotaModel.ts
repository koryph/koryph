// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Pure quota status-bar model (§5). Imports NOTHING from `vscode`: it decides
// which accounts deserve a status-bar item, formats each item's text/tooltip,
// and reports the governor level so the glue can pick a ThemeColor — all without
// a VS Code host. The slow, blocking part (running `koryph quota show --json`)
// lives in the glue; this module only shapes already-fetched data (or cached
// ceilings between snapshots).

import { ProjectNode, isActiveRun } from '../tree/model';
import { ParsedQuotaConfig } from '../data/quotaReader';
import { tryParse } from '../data/json';
import {
  QuotaLevel,
  QuotaSnapshot,
  Usage,
  quotaLevel,
  windowFraction,
} from '../data/schema';

/**
 * Parse `koryph quota show --json` output (an array of quotaSnapshot). Returns
 * undefined when the output isn't the expected array — the caller then degrades
 * to cached ceilings (an older CLI without `--json` prints a table, not JSON).
 */
export function parseQuotaSnapshots(stdout: string): QuotaSnapshot[] | undefined {
  const parsed = tryParse(stdout);
  if (!Array.isArray(parsed)) {
    return undefined;
  }
  return parsed.filter(
    (s): s is QuotaSnapshot =>
      !!s && typeof s === 'object' && typeof (s as QuotaSnapshot).account === 'string',
  );
}

/** A rendered status-bar item for one account. */
export interface QuotaItem {
  account: string;
  /** Status-bar text (e.g. "$(zap) personal 62% 5h · 41% wk"). */
  text: string;
  /** Governor level (drives the background ThemeColor in the glue). */
  level: QuotaLevel;
  /** Multi-line tooltip with the full snapshot + data age. */
  tooltip: string;
  /** True when this text came from cached ceilings, not a live snapshot. */
  stale: boolean;
}

/**
 * The accounts that own a *visible* project with an *active* run (§5). Deduped,
 * sorted for a stable status-bar order. "Visible" is the caller's decision
 * (pinned always; others only when the group is expanded).
 */
export function involvedAccounts(visible: ProjectNode[]): string[] {
  const accounts = new Set<string>();
  for (const p of visible) {
    if (p.account && isActiveRun(p.run)) {
      accounts.add(p.account);
    }
  }
  return [...accounts].sort();
}

/** Percentage cell for a window, failing closed (100%) when unmeasurable. */
export function pctCell(frac: number): string {
  const clamped = Math.max(0, Math.min(1, frac));
  return `${Math.round(clamped * 100)}%`;
}

/**
 * Format a live snapshot into a status-bar item. `zap` is the codicon prefix
 * ("$(zap)") the glue passes so this stays vscode-free but renders an icon.
 */
export function formatSnapshot(
  snap: QuotaSnapshot,
  nowMs: number,
  zap = '$(zap)',
): QuotaItem {
  const u = snap.usage;
  const w5 = windowFraction(u?.window_5h);
  const wk = windowFraction(u?.weekly);
  const level = snap.level ?? quotaLevel(u);
  const text = `${zap} ${snap.account} ${pctCell(w5)} 5h · ${pctCell(wk)} wk`;
  return {
    account: snap.account,
    text,
    level,
    stale: false,
    tooltip: snapshotTooltip(snap, nowMs),
  };
}

/**
 * Format a between-snapshots placeholder from the cached per-account config
 * (§5): ceilings are known, live spend is not, so no percentage is shown and
 * the item is marked stale. Level is unknown → treated as OK coloring (the glue
 * shows no alarming background until a real snapshot lands).
 */
export function formatCeilingOnly(
  account: string,
  cfg: ParsedQuotaConfig | undefined,
  zap = '$(zap)',
): QuotaItem {
  const c = cfg?.value;
  const ceilings = c
    ? `$${c.window_ceiling_usd.toFixed(0)}/5h · $${c.weekly_ceiling_usd.toFixed(0)}/wk`
    : 'ceilings unknown';
  return {
    account,
    text: `${zap} ${account} … (refreshing)`,
    level: QuotaLevel.OK,
    stale: true,
    tooltip: [
      `Koryph quota — ${account}`,
      'No live snapshot yet (refreshing async).',
      `Calibrated ceilings: ${ceilings}`,
      cfg && !cfg.known ? `⚠ config ${cfg.degradedReason}` : '',
    ]
      .filter(Boolean)
      .join('\n'),
  };
}

/** Human "N ago" for an ISO instant relative to nowMs; "just now" under a minute. */
export function formatAge(atISO: string | undefined, nowMs: number): string {
  if (!atISO) {
    return 'age unknown';
  }
  const at = Date.parse(atISO);
  if (Number.isNaN(at)) {
    return 'age unknown';
  }
  const deltaMs = Math.max(0, nowMs - at);
  const mins = Math.floor(deltaMs / 60_000);
  if (mins < 1) {
    return 'just now';
  }
  if (mins < 60) {
    return `${mins}m ago`;
  }
  const hrs = Math.floor(mins / 60);
  return `${hrs}h ${mins % 60}m ago`;
}

function windowLine(label: string, w: Usage['window_5h']): string {
  if (!w) {
    return `${label}: unavailable`;
  }
  const pct = pctCell(windowFraction(w));
  const src = w.source && w.source !== 'unavailable' ? w.source : 'unavailable';
  const approx = w.approx ? ' (approx)' : '';
  return `${label}: $${w.spent_usd.toFixed(2)}/$${w.ceiling_usd.toFixed(2)} ${pct} [${src}]${approx}`;
}

function snapshotTooltip(snap: QuotaSnapshot, nowMs: number): string {
  const u = snap.usage;
  return [
    `Koryph quota — ${snap.account}`,
    `Governor level: ${snap.level}${snap.calibrated ? ' (calibrated)' : ' (uncalibrated)'}`,
    windowLine('5h window', u?.window_5h),
    windowLine('Weekly', u?.weekly),
    `Data age: ${formatAge(u?.at, nowMs)}`,
    'Calibrate: /koryph-calibrate',
  ].join('\n');
}
