// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Extension entry point. This bead (ext.3) ships the data layer only — no UI
// yet. Activation instantiates the read-only watchers/readers so later beads
// (tree view, transcript webviews, status bar, commands) can consume live
// koryph state; there are no contributed views or commands to trigger
// activation, so this activate() runs only when a later bead adds an
// activation event.

import * as vscode from 'vscode';
import { CliAdapter, GovernorReader, QuotaReader, RegistryWatcher } from './data';
import { registerSlotCommands } from './commands';
import { makeSlotPicker } from './commands/slotPicker';

/**
 * The data-layer handles created at activation. Later beads attach UI to these.
 */
export interface KoryphDataLayer {
  registry: RegistryWatcher;
  governor: GovernorReader;
  quota: QuotaReader;
}

let dataLayer: KoryphDataLayer | undefined;

export function activate(context: vscode.ExtensionContext): KoryphDataLayer {
  const registry = new RegistryWatcher();
  const governor = new GovernorReader();
  const quota = new QuotaReader();

  context.subscriptions.push(
    { dispose: () => registry.dispose() },
    { dispose: () => governor.dispose() },
    { dispose: () => quota.dispose() },
  );

  // Slot commands (ext.6): every mutation shells the CLI. The palette picker
  // enumerates live slots when a command is run without a tree-item argument.
  registerSlotCommands(context, {
    cli: new CliAdapter(),
    pickSlot: makeSlotPicker(registry),
  });

  dataLayer = { registry, governor, quota };
  return dataLayer;
}

export function deactivate(): void {
  dataLayer?.registry.dispose();
  dataLayer?.governor.dispose();
  dataLayer?.quota.dispose();
  dataLayer = undefined;
}
