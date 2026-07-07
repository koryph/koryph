// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Extension entry point. Activation wires the read-only data layer (ext.3) to
// the UI: the "Koryph" activity-bar tree of agent threads (ext.4), the quota
// status-bar items (ext.4/§5), the slot commands (ext.6), and the project
// config editor UX (ext.7). Every mutation still goes through the CLI — the
// extension never writes koryph state.

import * as vscode from 'vscode';
import {
  BeadTitleCache,
  CliAdapter,
  QuotaReader,
  RegistryWatcher,
} from './data';
import { registerSlotCommands } from './commands';
import { makeSlotPicker } from './commands/slotPicker';
import { ConfigEditorBanner, registerConfigEditor } from './config';
import { AGENT_THREADS_VIEW, AgentThreadsProvider } from './tree/agentThreadsProvider';
import { QuotaStatusBar } from './statusbar/quotaStatusBar';
import { KoryphTranscriptPanel } from './webview/transcriptPanel';

/**
 * The data-layer + UI handles created at activation.
 */
export interface KoryphExtension {
  registry: RegistryWatcher;
  quota: QuotaReader;
  tree: AgentThreadsProvider;
  statusBar: QuotaStatusBar;
  banner: ConfigEditorBanner;
}

let ext: KoryphExtension | undefined;

export function activate(context: vscode.ExtensionContext): KoryphExtension {
  const registry = new RegistryWatcher();
  const quota = new QuotaReader();
  const cli = new CliAdapter();
  const titles = new BeadTitleCache(cli);

  // Tree view — agent threads (§2). The provider owns per-project CockpitReaders
  // (koryph-5ew) and refreshes on any registry or cockpit-watch change.
  // All agent/project state flows through `koryph cockpit --json` via CockpitReader.
  const tree = new AgentThreadsProvider(registry, cli, titles);
  const view = vscode.window.createTreeView(AGENT_THREADS_VIEW, {
    treeDataProvider: tree,
    showCollapseAll: true,
  });
  tree.attach(view);

  // Quota status bar (§5) — slow async refresh, reflecting the tree's visible
  // projects. Repaint when the tree changes so involved accounts stay in sync.
  const statusBar = new QuotaStatusBar(cli, quota, () => tree.visibleProjects());
  const treeChangeSub = tree.onDidChangeTreeData(() => statusBar.refreshSoon());
  statusBar.start();

  // Slot commands (ext.6): every mutation shells the CLI. The palette picker
  // enumerates live slots when a command is run without a tree-item argument.
  // The transcript webview (ext.5) is the "Open Transcript" opener.
  registerSlotCommands(context, {
    cli,
    pickSlot: makeSlotPicker(registry),
    openTranscript: (ref) => {
      KoryphTranscriptPanel.show(ref);
    },
  });

  // Config editing UX (ext.7): JSON Schema binding via jsonValidation
  // contribution (package.json), "Koryph: Edit Project Config" command, and
  // the persistent run-start caveat banner.
  const banner = new ConfigEditorBanner((msg) =>
    void vscode.window.showInformationMessage(msg),
  );
  registerConfigEditor(context, banner);

  context.subscriptions.push(
    view,
    treeChangeSub,
    { dispose: () => tree.dispose() },
    { dispose: () => statusBar.dispose() },
    { dispose: () => registry.dispose() },
    { dispose: () => quota.dispose() },
  );

  ext = { registry, quota, tree, statusBar, banner };
  return ext;
}

export function deactivate(): void {
  ext?.tree.dispose();
  ext?.statusBar.dispose();
  ext?.registry.dispose();
  ext?.quota.dispose();
  ext = undefined;
}
