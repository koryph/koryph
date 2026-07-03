// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Pure tree-model builder for the Agent Threads view (§2). Imports NOTHING from
// `vscode`: it takes already-loaded data-layer results (registry records, the
// latest run per project, governor leases, the window's workspace-folder paths,
// and the resolved `koryph.showAllProjects` setting) and produces the grouped,
// pinned, ordered model the provider renders. All grouping/pinning/ordering
// logic lives here so it is unit-tested with fixtures, without a VS Code host.

import * as path from 'path';
import { ParsedRun } from '../data/ledgerWatcher';
import { ParsedRecord } from '../data/registryWatcher';
import { Lease, ProjectRecord, Run, Slot, isTerminal } from '../data/schema';

/** One dispatched slot, ready to render as a row. */
export interface SlotNode {
  slot: Slot;
  /** Convenience: the phase id (row key). */
  phaseId: string;
  /** Whether this slot's status is terminal (affects cost vs ~streaming). */
  terminal: boolean;
}

/** The active run under a project, with its slots ordered for display. */
export interface RunNode {
  runId: string;
  status: string;
  wave: number;
  /** Slots ordered by `updated_at` descending (most-recently-touched first). */
  slots: SlotNode[];
  /** True when the ledger schema_version was newer than this build understands. */
  degraded: boolean;
  /** The raw run (retained for degraded raw-JSON display). */
  raw: unknown;
}

/** One managed project (pinned or under "Other projects"). */
export interface ProjectNode {
  projectId: string;
  name: string;
  root: string;
  account: string;
  pinned: boolean;
  /** True when the registry record schema_version was newer than supported. */
  degraded: boolean;
  record: ProjectRecord;
  /** The latest run, or undefined when the project has no run yet. */
  run?: RunNode;
}

/** The complete tree model for one render pass. */
export interface TreeModel {
  /** Pinned projects — always top-level and expanded. */
  pinned: ProjectNode[];
  /** Non-pinned projects — grouped under the "Other projects" node. */
  others: ProjectNode[];
  /**
   * Whether the "Other projects" node renders expanded (children visible) by
   * default. Resolved from `koryph.showAllProjects` with Decision-6 defaulting.
   */
  showOthers: boolean;
  /** Live agent count across *visible* projects (governor leases) — the badge. */
  liveAgentCount: number;
}

/** Inputs to a single tree build. All IO is done by the caller. */
export interface BuildTreeInput {
  /** Every managed project's registry record (with schema guard). */
  records: ParsedRecord[];
  /** The latest run per project id (undefined when the project has no run). */
  runs: Map<string, ParsedRun | undefined>;
  /** Active governor leases (one per live agent). */
  leases: Lease[];
  /** Absolute fs paths of the window's workspace folders. */
  workspaceRoots: string[];
  /**
   * The `koryph.showAllProjects` setting. `undefined` means "unset" → apply the
   * Decision-6 default (on when nothing is pinned, off otherwise).
   */
  showAllProjects?: boolean;
}

/**
 * Build the tree model: pin projects whose registry root matches a workspace
 * folder, group the rest under "Other projects", order slots by updated_at, and
 * compute the live-agent badge over the visible set.
 */
export function buildTree(input: BuildTreeInput): TreeModel {
  const pinned: ProjectNode[] = [];
  const others: ProjectNode[] = [];

  for (const rec of input.records) {
    const node = projectNode(rec, input.runs.get(rec.value.project_id));
    node.pinned = isPinned(node.root, input.workspaceRoots);
    (node.pinned ? pinned : others).push(node);
  }

  // Stable display order: project id within each group.
  pinned.sort((a, b) => a.projectId.localeCompare(b.projectId));
  others.sort((a, b) => a.projectId.localeCompare(b.projectId));

  const showOthers = input.showAllProjects ?? pinned.length === 0;

  // Badge: leases for visible projects only. "Other" projects count only when
  // their group is expanded (showOthers) — a collapsed group hides its agents.
  const visible = new Set<string>(pinned.map((p) => p.projectId));
  if (showOthers) {
    for (const o of others) {
      visible.add(o.projectId);
    }
  }
  const liveAgentCount = input.leases.filter((l) => visible.has(l.project)).length;

  return { pinned, others, showOthers, liveAgentCount };
}

/**
 * Whether a project root is pinned by the current workspace folders. A match is
 * either exact, or a containment either way (folder inside the project, or the
 * project inside a multi-root folder) so a worktree/subdir opened as the folder
 * still pins its project. Path comparison is normalized and case-preserving.
 */
export function isPinned(root: string, workspaceRoots: string[]): boolean {
  if (!root) {
    return false;
  }
  const r = normalize(root);
  for (const ws of workspaceRoots) {
    const w = normalize(ws);
    if (w === r || contains(r, w) || contains(w, r)) {
      return true;
    }
  }
  return false;
}

/** True when `parent` contains `child` (or they are equal), by path segments. */
function contains(parent: string, child: string): boolean {
  if (parent === child) {
    return true;
  }
  const withSep = parent.endsWith(path.sep) ? parent : parent + path.sep;
  return child.startsWith(withSep);
}

function normalize(p: string): string {
  const norm = path.normalize(p);
  // Drop a lone trailing separator so "/a/b/" and "/a/b" compare equal.
  return norm.length > 1 && norm.endsWith(path.sep) ? norm.slice(0, -1) : norm;
}

function projectNode(rec: ParsedRecord, run: ParsedRun | undefined): ProjectNode {
  return {
    projectId: rec.value.project_id,
    name: rec.value.name || rec.value.project_id,
    root: rec.value.root ?? '',
    account: rec.value.account_profile ?? '',
    pinned: false,
    degraded: !rec.known,
    record: rec.value,
    run: run ? runNode(run) : undefined,
  };
}

function runNode(run: ParsedRun): RunNode {
  const value: Run = run.value;
  const slots = Object.values(value.slots ?? {}).map(
    (slot): SlotNode => ({ slot, phaseId: slot.phase_id, terminal: isTerminal(slot.status) }),
  );
  slots.sort(compareSlots);
  return {
    runId: value.run_id ?? '',
    status: value.status ?? '',
    wave: value.wave ?? 0,
    slots,
    degraded: !run.known,
    raw: run.raw,
  };
}

/**
 * Order slots most-recently-updated first (§2). Slots without an `updated_at`
 * sort last; ties break by phase id for a stable render.
 */
export function compareSlots(a: SlotNode, b: SlotNode): number {
  const at = a.slot.updated_at ?? '';
  const bt = b.slot.updated_at ?? '';
  if (at !== bt) {
    // Missing timestamps sink to the bottom; otherwise newest (max) first.
    if (!at) {
      return 1;
    }
    if (!bt) {
      return -1;
    }
    return bt.localeCompare(at);
  }
  return a.phaseId.localeCompare(b.phaseId);
}

/** A run is "active" (worth a quota/status-bar mention) when it is not done. */
export function isActiveRun(run: RunNode | undefined): boolean {
  if (!run) {
    return false;
  }
  const s = run.status.toLowerCase();
  return s !== 'done' && s !== 'aborted' && s !== '';
}

/** A slot's headline cost cell: fixed cost when terminal, "~streaming" live. */
export function slotCostCell(node: SlotNode): string {
  if (!node.terminal && !isTerminal(node.slot.status)) {
    // Running/queued: cost is still accruing; the ledger cost is not final.
    return node.slot.status === 'queued' ? '—' : '~streaming';
  }
  return `$${(node.slot.cost_usd ?? 0).toFixed(2)}`;
}
