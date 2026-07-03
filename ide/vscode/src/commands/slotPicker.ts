// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Palette slot picker — when a slot command is invoked from the command
// palette (no context-menu argument), enumerate the live slots across managed
// projects so the user can choose one. Read-only: registry records + the
// latest per-project ledger, exactly the sources the tree view (ext.4) uses.

import * as vscode from 'vscode';
import { LedgerWatcher } from '../data/ledgerWatcher';
import { RegistryWatcher } from '../data/registryWatcher';
import { isTerminal } from '../data/schema';
import { SlotRef, slotRef } from './argv';

/**
 * Build a palette picker over currently-known slots. Non-terminal slots sort
 * first (they are the actionable ones). Returns undefined on cancel or when no
 * runs exist.
 */
export function makeSlotPicker(registry: RegistryWatcher): () => Promise<SlotRef | undefined> {
  return async () => {
    const records = await registry.list();
    const refs: SlotRef[] = [];
    for (const rec of records) {
      const root = rec.value.root;
      if (!root) {
        continue;
      }
      const watcher = new LedgerWatcher(root);
      try {
        const run = await watcher.load();
        if (!run?.value?.slots) {
          continue;
        }
        for (const slot of Object.values(run.value.slots)) {
          refs.push(slotRef(rec.value.project_id, root, slot));
        }
      } finally {
        watcher.dispose();
      }
    }
    if (refs.length === 0) {
      void vscode.window.showInformationMessage('Koryph: no active runs found.');
      return undefined;
    }
    refs.sort((a, b) => {
      const at = isTerminal(a.status) ? 1 : 0;
      const bt = isTerminal(b.status) ? 1 : 0;
      return at - bt || a.phaseId.localeCompare(b.phaseId);
    });
    const picked = await vscode.window.showQuickPick(
      refs.map((ref) => ({
        label: ref.phaseId,
        description: `${ref.projectId} · ${ref.status} · ${ref.model}`,
        detail: ref.branch,
        ref,
      })),
      { title: 'Select a koryph slot', placeHolder: 'project · status · model' },
    );
    return picked?.ref;
  };
}
