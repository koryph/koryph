// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// ConfigEditorBanner — surfaces the run-start caveat whenever a user opens
// koryph.project.json (Design §6). Kept free of `vscode` imports so that the
// trigger-and-tracking logic is unit-testable under plain mocha.
//
// VS Code wiring lives in src/config/editProjectConfig.ts and extension.ts.

import * as path from 'path';

/** Filename of the project config (not a full path). */
export const CONFIG_FILENAME = 'koryph.project.json';

/**
 * Caveat surfaced in the editor banner.  The running engine reads project
 * config **once** — at run start — so edits only take effect on the next
 * `koryph run`.
 */
export const CAVEAT_MESSAGE =
  'Applies on next `koryph run` — the running engine loaded config at run start.';

/**
 * Registry-record guidance surfaced alongside the edit command.  The registry
 * files are git-committed by the koryph store; they must be mutated through
 * `koryph project` CLI commands, never by hand-editing the JSON.
 */
export const REGISTRY_CAVEAT =
  'Registry-record fields (account, allowed models, billing guard) are managed ' +
  'by `koryph project` CLI commands — do not edit `~/.koryph/registry.d/*.json` ' +
  'directly; those files are git-committed by the store.';

/**
 * True when the given file-system path (or basename) refers to the koryph
 * project-config file.
 */
export function isProjectConfigPath(filePath: string): boolean {
  return path.basename(filePath) === CONFIG_FILENAME;
}

/**
 * A short display label for a config file: `<parent-dir>/<filename>`.
 * Pure path helper — does not touch the VS Code API.
 */
export function configFileLabel(filePath: string): string {
  return path.basename(path.dirname(filePath)) + '/' + path.basename(filePath);
}

/**
 * ConfigEditorBanner tracks which documents have already received the caveat
 * notification this session so the message fires once per document opening
 * (not on every editor-focus cycle).
 *
 * The class is deliberately free of the `vscode` module — callers inject the
 * `notify` callback so the logic is exercisable in a plain-Node test runner.
 */
export class ConfigEditorBanner {
  /** Document URIs (or any string key) that have been notified this session. */
  private readonly notified = new Set<string>();

  /**
   * Notify function injected at construction time.  Production callers pass
   * `vscode.window.showInformationMessage`; tests pass a spy.
   */
  private readonly notify: (message: string) => void;

  constructor(notify: (message: string) => void) {
    this.notify = notify;
  }

  /**
   * Call when a document becomes active (or is explicitly opened by the edit
   * command).  When `filePath` names `koryph.project.json` and the banner has
   * not yet been shown for `key`, fires the notification and records `key`.
   *
   * @param filePath - Absolute or relative path of the document.
   * @param key      - Dedup key (typically `document.uri.toString()`; defaults
   *                   to `filePath` when omitted).
   */
  maybeShow(filePath: string, key?: string): void {
    if (!isProjectConfigPath(filePath)) {
      return;
    }
    const dedup = key ?? filePath;
    if (this.notified.has(dedup)) {
      return;
    }
    this.notified.add(dedup);
    this.notify(CAVEAT_MESSAGE);
  }

  /**
   * Force-show the banner for a given key (used by the edit command which
   * always wants to surface the caveat, even if the file was already open).
   */
  forceShow(filePath: string, key?: string): void {
    const dedup = key ?? filePath;
    this.notified.delete(dedup); // reset so maybeShow fires
    this.maybeShow(filePath, dedup);
  }

  /**
   * Reset a key so the next `maybeShow` fires again (e.g. when a document is
   * closed and later re-opened).
   */
  reset(key: string): void {
    this.notified.delete(key);
  }

  /** True when the banner has already been shown for `key`. */
  wasShown(key: string): boolean {
    return this.notified.has(key);
  }
}
