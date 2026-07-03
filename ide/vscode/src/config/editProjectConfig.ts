// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// "Koryph: Edit Project Config" command (Design §6).
//
// Opens koryph.project.json with JSON Schema validation (wired via the
// `jsonValidation` contribution in package.json) and surfaces two caveats:
//   1. The run-start caveat (ConfigEditorBanner) — edits apply on next run.
//   2. Registry guidance — account/model/billing fields must be changed
//      through `koryph project` CLI, not by hand-editing the registry files.

import * as vscode from 'vscode';
import { CONFIG_FILENAME, ConfigEditorBanner, REGISTRY_CAVEAT } from './banner';

export const EDIT_PROJECT_CONFIG_CMD = 'koryph.editProjectConfig';

/**
 * Register the "Koryph: Edit Project Config" command and wire the
 * ConfigEditorBanner to the active-editor lifecycle.
 *
 * Returns a Disposable that unregisters everything (also pushed onto
 * `context.subscriptions`).
 */
export function registerConfigEditor(
  context: vscode.ExtensionContext,
  banner: ConfigEditorBanner,
): vscode.Disposable {
  // Fire the banner when any koryph.project.json becomes the active editor
  // (e.g. opened from the file explorer or via a workspace search).
  const editorSub = vscode.window.onDidChangeActiveTextEditor((editor) => {
    if (editor) {
      banner.maybeShow(editor.document.fileName, editor.document.uri.toString());
    }
  });

  // When a document is closed, reset its key so re-opening shows the banner.
  const closeSub = vscode.workspace.onDidCloseTextDocument((doc) => {
    banner.reset(doc.uri.toString());
  });

  // The edit command: find the config file, open it, surface both caveats.
  const cmdSub = vscode.commands.registerCommand(
    EDIT_PROJECT_CONFIG_CMD,
    () => editProjectConfig(banner),
  );

  context.subscriptions.push(editorSub, closeSub, cmdSub);
  return vscode.Disposable.from(editorSub, closeSub, cmdSub);
}

// ---------------------------------------------------------------------------
// Command handler
// ---------------------------------------------------------------------------

async function editProjectConfig(banner: ConfigEditorBanner): Promise<void> {
  // Locate koryph.project.json: prefer workspace root, fall back to a
  // workspace-wide find (handles multi-root workspaces).
  const uri = await findProjectConfig();
  if (!uri) {
    void vscode.window.showWarningMessage(
      `Koryph: no ${CONFIG_FILENAME} found in the current workspace.`,
    );
    return;
  }

  // Open the file in the active editor group (VS Code will apply the
  // jsonValidation contribution automatically for .json files).
  const doc = await vscode.workspace.openTextDocument(uri);
  await vscode.window.showTextDocument(doc);

  // Always surface the run-start caveat when opened via this command.
  banner.forceShow(doc.fileName, doc.uri.toString());

  // Surface the registry guidance as a separate, persistent notification.
  // Using showInformationMessage keeps it distinct from the caveat message.
  void vscode.window.showInformationMessage(REGISTRY_CAVEAT);
}

/**
 * Find the project config in the current workspace.
 *
 * Strategy:
 *   1. Check each workspace folder root for koryph.project.json directly
 *      (O(1), covers the typical single-root case).
 *   2. Fall back to `vscode.workspace.findFiles` for multi-root or unusual
 *      layouts (excludes node_modules / .git / common build dirs).
 */
async function findProjectConfig(): Promise<vscode.Uri | undefined> {
  // Fast path: root of each workspace folder.
  for (const folder of vscode.workspace.workspaceFolders ?? []) {
    const candidate = vscode.Uri.joinPath(folder.uri, CONFIG_FILENAME);
    try {
      await vscode.workspace.fs.stat(candidate);
      return candidate; // found
    } catch {
      // Not here; try the next folder.
    }
  }

  // Slow path: workspace-wide search (capped at 5 results to bound cost).
  const found = await vscode.workspace.findFiles(
    `**/${CONFIG_FILENAME}`,
    '{**/node_modules/**,**/.git/**,**/dist/**,**/out/**}',
    5,
  );
  if (found.length === 0) {
    return undefined;
  }
  if (found.length === 1) {
    return found[0];
  }
  // Multiple results: let the user pick.
  const items = found.map((u) => ({
    label: vscode.workspace.asRelativePath(u),
    uri: u,
  }));
  const picked = await vscode.window.showQuickPick(items, {
    title: `Multiple ${CONFIG_FILENAME} files found — choose one`,
    placeHolder: 'Select the project config to edit',
  });
  return picked?.uri;
}
