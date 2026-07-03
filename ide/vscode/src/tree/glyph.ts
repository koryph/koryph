// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Pure status → glyph mapping for the Agent Threads tree (§2). Imports NOTHING
// from `vscode`: the provider turns `icon` into a `vscode.ThemeIcon` and
// `symbol` feeds the plain-text fallbacks the fixtures assert against. Keeping
// this pure lets the grouping tests check row rendering without a VS Code host.

import { SlotStatus } from '../data/schema';

/** A rendered status decoration: a text symbol + a codicon id + a short word. */
export interface StatusGlyph {
  /** Single-character text symbol (used in the ASCII tree + tests). */
  symbol: string;
  /** VS Code codicon id (without the `$(...)` wrapper). */
  icon: string;
  /** Whether the icon should spin (running/dispatching). */
  spin: boolean;
  /** The canonical status word (echoes the input, lower-cased). */
  word: string;
}

const GLYPHS: Record<string, Omit<StatusGlyph, 'word'>> = {
  [SlotStatus.Queued]: { symbol: '○', icon: 'circle-outline', spin: false },
  [SlotStatus.Dispatching]: { symbol: '◔', icon: 'loading', spin: true },
  [SlotStatus.Running]: { symbol: '●', icon: 'sync', spin: true },
  [SlotStatus.Stuck]: { symbol: '◼', icon: 'warning', spin: false },
  [SlotStatus.Review]: { symbol: '◐', icon: 'eye', spin: false },
  [SlotStatus.MergePending]: { symbol: '◕', icon: 'git-merge', spin: false },
  [SlotStatus.Merged]: { symbol: '✓', icon: 'check', spin: false },
  [SlotStatus.PROpened]: { symbol: '⇡', icon: 'git-pull-request', spin: false },
  [SlotStatus.Done]: { symbol: '✓', icon: 'pass', spin: false },
  [SlotStatus.Failed]: { symbol: '✗', icon: 'error', spin: false },
  [SlotStatus.Conflict]: { symbol: '✗', icon: 'git-merge', spin: false },
  [SlotStatus.Blocked]: { symbol: '⊘', icon: 'circle-slash', spin: false },
};

const UNKNOWN: Omit<StatusGlyph, 'word'> = { symbol: '·', icon: 'question', spin: false };

/** Map a slot status string to its glyph, degrading unknown statuses safely. */
export function statusGlyph(status: string | undefined): StatusGlyph {
  const word = (status ?? '').toLowerCase();
  const base = GLYPHS[word] ?? UNKNOWN;
  return { ...base, word: word || 'unknown' };
}
