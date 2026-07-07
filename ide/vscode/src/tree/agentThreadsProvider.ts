// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// AgentThreadsProvider — the "Koryph" activity-bar TreeDataProvider (§2). All
// grouping/pinning/ordering is the pure `./model` builder; this file is only the
// VS Code glue: it loads the data layer (registry + per-project cockpit snapshot),
// turns the model into TreeItems (glyphs, tooltips, context values ext.6's menus
// key off), sets the tree badge to the live-agent count, and refreshes on any
// watcher tick. Read-only: it never mutates koryph state.
//
// Data layer constraint (koryph-5ew): agent/project state MUST flow through
// CockpitReader (which calls `koryph cockpit --json`). Do NOT add new direct
// ledger/govern/quota file reads — see docs/developer-guide/ide-setup.md §"Data layer".

import * as vscode from 'vscode';
import { slotRef } from '../commands/argv';
import { BeadTitleCache } from '../data/beadTitle';
import { CockpitReader } from '../data/cockpitReader';
import { CliAdapter } from '../data/cli';
import { RegistryWatcher } from '../data/registryWatcher';
import { CockpitSlot, CockpitSnapshot, LEDGER_SCHEMA_VERSION, Lease, Run, Slot, isTerminal } from '../data/schema';
import { StatusReader, formatStatusReport } from '../data/statusReader';
import { ParsedRun } from '../data/ledgerWatcher';
import { statusGlyph } from './glyph';
import {
  ProjectNode,
  SlotNode,
  TreeModel,
  buildTree,
  isActiveRun,
  slotCostCell,
} from './model';

/** The tree's view id (matches package.json `contributes.views`). */
export const AGENT_THREADS_VIEW = 'koryph.agentThreads';

// Element kinds. A discriminated union keeps getChildren/getTreeItem exhaustive.
interface ProjectElement {
  kind: 'project';
  node: ProjectNode;
}
interface OtherProjectsElement {
  kind: 'others';
  nodes: ProjectNode[];
  /** Whether the group renders expanded by default (resolved by the builder). */
  expanded: boolean;
}
interface SlotElement {
  kind: 'slot';
  project: ProjectNode;
  slot: SlotNode;
  /** ext.6 command handles read the target off `.slotRef`. */
  slotRef: ReturnType<typeof slotRef>;
}
export type TreeElement = ProjectElement | OtherProjectsElement | SlotElement;

export class AgentThreadsProvider implements vscode.TreeDataProvider<TreeElement> {
  private readonly _onDidChangeTreeData = new vscode.EventEmitter<TreeElement | undefined>();
  readonly onDidChangeTreeData = this._onDidChangeTreeData.event;

  // CockpitReader per project — replaces the old per-project LedgerWatcher.
  // All agent/project state flows through cockpit.LedgerProvider.Refresh()
  // via `koryph cockpit --json` (koryph-5ew data-layer constraint).
  private readonly cockpitReaders = new Map<string, CockpitReader>();
  private readonly disposables: vscode.Disposable[] = [];
  private view?: vscode.TreeView<TreeElement>;

  constructor(
    private readonly registry: RegistryWatcher,
    private readonly cli: CliAdapter,
    private readonly titles: BeadTitleCache,
    private readonly status: StatusReader = new StatusReader(),
  ) {
    // Any registry change re-renders (and re-syncs cockpit readers).
    this.disposables.push(this.registry.onChange(() => this.refresh()));
  }

  /** Attach the created TreeView so the provider can set its badge. */
  attach(view: vscode.TreeView<TreeElement>): void {
    this.view = view;
  }

  refresh(): void {
    this._onDidChangeTreeData.fire(undefined);
  }

  dispose(): void {
    this._onDidChangeTreeData.dispose();
    for (const r of this.cockpitReaders.values()) {
      r.dispose();
    }
    this.cockpitReaders.clear();
    for (const d of this.disposables) {
      d.dispose();
    }
  }

  // --- tree data -----------------------------------------------------------

  async getChildren(element?: TreeElement): Promise<TreeElement[]> {
    if (!element) {
      return this.rootChildren();
    }
    if (element.kind === 'others') {
      return element.nodes.map((node) => ({ kind: 'project', node }) as ProjectElement);
    }
    if (element.kind === 'project') {
      return this.slotChildren(element.node);
    }
    return [];
  }

  getTreeItem(element: TreeElement): vscode.TreeItem {
    switch (element.kind) {
      case 'others':
        return this.othersItem(element);
      case 'project':
        return this.projectItem(element);
      case 'slot':
        return this.slotItem(element);
    }
  }

  // --- model → elements ----------------------------------------------------

  private async rootChildren(): Promise<TreeElement[]> {
    const model = await this.buildModel();
    this.updateBadge(model.liveAgentCount);

    const out: TreeElement[] = model.pinned.map((node) => ({ kind: 'project', node }));
    if (model.others.length > 0) {
      out.push({ kind: 'others', nodes: model.others, expanded: model.showOthers });
    }
    return out;
  }

  private slotChildren(node: ProjectNode): TreeElement[] {
    if (!node.run) {
      return [];
    }
    return node.run.slots.map((slot) => ({
      kind: 'slot',
      project: node,
      slot,
      slotRef: slotRef(node.projectId, node.root, slot.slot),
    }));
  }

  /**
   * The projects currently visible in the tree (pinned always; "others" only
   * when their group is expanded). The quota status bar (§5) uses this to decide
   * which accounts own a visible project with an active run.
   */
  async visibleProjects(): Promise<ProjectNode[]> {
    const model = await this.buildModel();
    return model.showOthers ? [...model.pinned, ...model.others] : model.pinned;
  }

  /** Load the data layer and delegate all shaping to the pure builder. */
  private async buildModel(): Promise<TreeModel> {
    const records = await this.registry.list();
    this.syncCockpitReaders(records.map((r) => r.value.project_id).filter(Boolean) as string[]);

    // Fetch one cockpit snapshot per project. All state (slots + governor)
    // flows through cockpit.LedgerProvider.Refresh() — the same path as the TUI.
    const snapshots = new Map<string, CockpitSnapshot | undefined>();
    await Promise.all(
      records.map(async (rec) => {
        const root = rec.value.root;
        const id = rec.value.project_id;
        if (!root || !id) {
          snapshots.set(id ?? '', undefined);
          return;
        }
        snapshots.set(id, await this.cockpitReaderFor(id, root).snapshot());
      }),
    );

    // Map cockpit snapshots to the ParsedRun format that buildTree() expects,
    // so the pure model builder stays unchanged and tests keep working.
    const runs = new Map<string, ParsedRun | undefined>();
    for (const [id, snap] of snapshots) {
      runs.set(id, snap ? cockpitToRun(snap) : undefined);
    }

    // Synthesise Lease objects from non-terminal slots so buildTree can compute
    // the per-visible-project badge count (liveAgentCount). This is more
    // accurate than the old governor.leases() call because each snapshot's
    // slots are already project-scoped.
    const leases: Lease[] = [];
    for (const [id, snap] of snapshots) {
      if (!snap) {
        continue;
      }
      for (const slot of snap.slots) {
        if (!isTerminal(slot.stage)) {
          leases.push({
            project: id,
            bead: slot.bead_id ?? '',
            pid: slot.pid ?? 0,
            engine_pid: 0,
            model: slot.model,
            acquired_at: slot.dispatched_at ?? '',
          });
        }
      }
    }

    return buildTree({
      records,
      runs,
      leases,
      workspaceRoots: workspaceRoots(),
      showAllProjects: showAllProjectsSetting(),
    });
  }

  // --- TreeItems -----------------------------------------------------------

  private projectItem(element: ProjectElement): vscode.TreeItem {
    const node = element.node;
    const active = isActiveRun(node.run);
    const item = new vscode.TreeItem(
      node.name,
      node.run && node.run.slots.length > 0
        ? node.pinned
          ? vscode.TreeItemCollapsibleState.Expanded
          : vscode.TreeItemCollapsibleState.Collapsed
        : vscode.TreeItemCollapsibleState.None,
    );
    item.id = `project:${node.projectId}`;
    item.iconPath = new vscode.ThemeIcon(node.pinned ? 'pinned' : 'repo');
    // "Stop Whole Run" (ext.6) keys off viewItem == koryphRun.
    item.contextValue = active ? 'koryphRun' : 'koryphProject';
    // ext.6's stopRun reads projectId off .slotRef.
    (item as unknown as { slotRef: unknown }).slotRef = {
      projectId: node.projectId,
      projectRoot: node.root,
      phaseId: '',
      beadId: '',
      branch: '',
      worktree: '',
      baseCommit: '',
      model: '',
      status: node.run?.status ?? '',
      note: '',
    };
    item.description = node.degraded
      ? '⚠ newer registry schema'
      : runSummary(node);
    item.tooltip = projectTooltip(node);
    return item;
  }

  private othersItem(element: OtherProjectsElement): vscode.TreeItem {
    const item = new vscode.TreeItem(
      `Other projects (${element.nodes.length} hidden)`,
      element.expanded
        ? vscode.TreeItemCollapsibleState.Expanded
        : vscode.TreeItemCollapsibleState.Collapsed,
    );
    item.id = 'koryph:others';
    item.contextValue = 'koryphOtherProjects';
    item.iconPath = new vscode.ThemeIcon('folder-library');
    item.tooltip = 'Projects with no workspace folder open in this window. Toggle koryph.showAllProjects.';
    return item;
  }

  private slotItem(element: SlotElement): vscode.TreeItem {
    const { slot, project } = element;
    const s = slot.slot;
    const glyph = statusGlyph(s.status);
    const title = this.resolveTitle(project.root, element.slotRef.beadId);
    const label = title ? `${element.slotRef.beadId} · ${title}` : element.slotRef.beadId;

    const item = new vscode.TreeItem(label, vscode.TreeItemCollapsibleState.None);
    item.id = `slot:${project.projectId}:${s.phase_id}`;
    item.iconPath = new vscode.ThemeIcon(glyph.icon);
    // ext.6 menus match /^koryphSlot/ and status substrings (pr-opened, review…).
    item.contextValue = `koryphSlot ${glyph.word}`;
    const cost = slotCostCell(slot);
    item.description = `${glyph.word} · ${s.model || '?'} · ${cost}`;
    item.tooltip = this.slotTooltip(element);
    // Single-click opens the transcript (ext.5 / fallback in ext.6).
    item.command = {
      command: 'koryph.slot.openTranscript',
      title: 'Open Transcript',
      arguments: [item],
    };
    return item;
  }

  // --- enrichment ----------------------------------------------------------

  private updateBadge(count: number): void {
    if (!this.view) {
      return;
    }
    this.view.badge =
      count > 0
        ? { value: count, tooltip: `${count} live koryph agent${count === 1 ? '' : 's'}` }
        : undefined;
  }

  /** Cached bead title, kicking off a background fetch + refresh on first miss. */
  private resolveTitle(root: string, beadId: string): string | undefined {
    const hit = this.titles.cached(root, beadId);
    if (hit !== undefined) {
      return hit;
    }
    void this.titles.fetch(root, beadId).then((t) => {
      if (t) {
        this.refresh();
      }
    });
    return undefined;
  }

  private slotTooltip(element: SlotElement): vscode.MarkdownString {
    const s = element.slot.slot;
    const md = new vscode.MarkdownString();
    md.appendMarkdown(`**${element.slotRef.beadId}** — ${s.status}\n\n`);
    const rows: Array<[string, string | undefined]> = [
      ['Persona', s.agent],
      ['Model', s.model + (s.effort ? ` (${s.effort})` : '')],
      ['Account', s.account_profile],
      ['Verified', s.verified_identity],
      ['Attempts', String(s.attempts ?? 0)],
      ['Commits', String(s.commits ?? 0)],
      ['Branch', s.branch],
      ['Worktree', s.worktree],
      ['Cost', slotCostCell(element.slot)],
    ];
    for (const [k, v] of rows) {
      if (v) {
        md.appendMarkdown(`- **${k}:** ${v}\n`);
      }
    }
    // Agent-reported heartbeat from the previous read, explicitly flagged as
    // possibly stale (§2). The read itself is async (below) and lands on the
    // next render — tooltips can't be mutated after return.
    const cached = this.statusCache.get(this.statusKey(element));
    if (cached) {
      md.appendMarkdown(`\n${cached}\n`);
    }
    void this.refreshStatusLine(element);
    return md;
  }

  private statusKey(element: SlotElement): string {
    return `${element.project.projectId}:${element.slot.phaseId}`;
  }

  /**
   * Read the slot's status.json into the cache and refresh if the heartbeat
   * line changed, so the *next* render of this tooltip shows it. Best-effort
   * and off the render path (§2: status.json is agent-authored, possibly stale).
   */
  private async refreshStatusLine(element: SlotElement): Promise<void> {
    const run = element.project.run;
    if (!run) {
      return;
    }
    const report = await this.status.read(element.project.root, run.runId, element.slot.phaseId);
    const line = formatStatusReport(report);
    const key = this.statusKey(element);
    if (line && this.statusCache.get(key) !== line) {
      this.statusCache.set(key, line);
      this.refresh();
    }
  }

  private readonly statusCache = new Map<string, string>();

  // --- cockpit reader lifecycle --------------------------------------------

  private cockpitReaderFor(projectId: string, root: string): CockpitReader {
    let r = this.cockpitReaders.get(projectId);
    if (!r) {
      r = new CockpitReader(projectId, root, this.cli);
      r.onChange(() => this.refresh());
      this.cockpitReaders.set(projectId, r);
    }
    return r;
  }

  /** Dispose cockpit readers for projects that vanished from the registry. */
  private syncCockpitReaders(liveIds: string[]): void {
    const live = new Set(liveIds);
    for (const [id, r] of this.cockpitReaders) {
      if (!live.has(id)) {
        r.dispose();
        this.cockpitReaders.delete(id);
      }
    }
  }
}

// ---------------------------------------------------------------------------
// Cockpit → legacy model mapping
// ---------------------------------------------------------------------------

/**
 * Map a CockpitSnapshot to the ParsedRun format expected by buildTree().
 * This shim keeps the pure model builder unchanged while the data source
 * migrates to the cockpit layer (koryph-5ew).
 *
 * Fields the TUI surfaces but the extension does not yet render (agent,
 * account_profile, effort, etc.) are left empty — the tooltip guards on
 * non-empty before showing them.
 */
function cockpitToRun(snap: CockpitSnapshot): ParsedRun {
  const slots: Record<string, Slot> = {};
  for (const cs of snap.slots) {
    slots[cs.phase_id] = cockpitSlotToSlot(cs);
  }
  const run: Run = {
    schema_version: LEDGER_SCHEMA_VERSION,
    run_id: snap.run_id ?? '',
    project_id: snap.project_id,
    engine_version: '',
    started_at: '',
    updated_at: snap.captured_at,
    status: snap.run_status ?? '',
    wave: snap.wave,
    source: 'cockpit',
    slots,
  };
  return { known: true, schemaVersion: LEDGER_SCHEMA_VERSION, value: run, raw: snap };
}

/** Map one CockpitSlot to the Slot wire type. */
function cockpitSlotToSlot(cs: CockpitSlot): Slot {
  return {
    phase_id: cs.phase_id,
    bead_id: cs.bead_id,
    branch: cs.branch ?? '',
    worktree: cs.worktree ?? '',
    session_id: '',
    agent: '',
    model: cs.model ?? '',
    account_profile: '',
    billing_mode: '',
    status: cs.stage,
    attempts: cs.attempt,
    commits: 0,
    cost_usd: cs.cost_usd,
    pid: cs.pid,
    dispatched_at: cs.dispatched_at,
    // updated_at: not available in cockpit snapshot; slots sort by phase_id.
  };
}

// --- pure-ish glue helpers -------------------------------------------------

/** The window's workspace-folder fs paths (empty when no folder is open). */
function workspaceRoots(): string[] {
  return (vscode.workspace.workspaceFolders ?? []).map((f) => f.uri.fsPath);
}

/** The `koryph.showAllProjects` setting, or undefined when unset (default). */
function showAllProjectsSetting(): boolean | undefined {
  const cfg = vscode.workspace.getConfiguration('koryph');
  const inspected = cfg.inspect<boolean>('showAllProjects');
  const explicit =
    inspected?.workspaceFolderValue ??
    inspected?.workspaceValue ??
    inspected?.globalValue;
  return explicit;
}

function runSummary(node: ProjectNode): string {
  if (!node.run) {
    return '(no active run)';
  }
  const total = node.run.slots.length;
  const live = node.run.slots.filter((s) => !s.terminal).length;
  return `${node.run.runId} · ${node.run.status} · wave ${node.run.wave} · ${live}/${total} live`;
}

function projectTooltip(node: ProjectNode): vscode.MarkdownString {
  const md = new vscode.MarkdownString();
  md.appendMarkdown(`**${node.name}** (${node.projectId})\n\n`);
  md.appendMarkdown(`- **Account:** ${node.account || '?'}\n`);
  md.appendMarkdown(`- **Root:** ${node.root || '?'}\n`);
  md.appendMarkdown(`- **Pinned:** ${node.pinned ? 'yes (workspace folder match)' : 'no'}\n`);
  if (node.run) {
    md.appendMarkdown(`- **Run:** ${node.run.runId} (${node.run.status}, wave ${node.run.wave})\n`);
  }
  if (node.degraded) {
    md.appendMarkdown(`\n⚠ Registry record uses a newer schema — showing best-effort fields.`);
  }
  return md;
}
