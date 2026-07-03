// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Slot commands (ext.6) — the palette + context-menu actions that steer a
// running wave. Every mutation is a `koryph`/`bd` CLI shell-out (Decision 3):
// this module never signals a PID and never writes koryph state. The argument
// vectors come from the pure, unit-tested `./argv` module; this file is only
// the VS Code glue (quick-picks, input boxes, confirmations, terminals, the
// Git-extension diff, output channels).

import * as vscode from 'vscode';
import { CliAdapter } from '../data/cli';
import {
  SlotRef,
  bdShowArgv,
  canLand,
  canOpenPr,
  coerceSlotRef,
  gitDiffArgs,
  landArgv,
  mergeArgv,
  modelChoices,
  modelLabelAddArgv,
  modelLabelRemoveArgv,
  nudgeArgv,
  parsePrUrl,
  shellQuote,
  stopArgv,
  stopRunArgv,
  tailArgv,
} from './argv';

/** Command ids — also referenced by package.json `contributes`. */
export const CommandIds = {
  Stop: 'koryph.slot.stop',
  StopForce: 'koryph.slot.stopForce',
  StopRun: 'koryph.slot.stopRun',
  Nudge: 'koryph.slot.nudge',
  ChangeModel: 'koryph.slot.changeModel',
  OpenTranscript: 'koryph.slot.openTranscript',
  Tail: 'koryph.slot.tail',
  OpenWorktree: 'koryph.slot.openWorktree',
  ShowDiff: 'koryph.slot.showDiff',
  OpenPr: 'koryph.slot.openPr',
  Merge: 'koryph.slot.merge',
  Land: 'koryph.slot.land',
  ShowBead: 'koryph.slot.showBead',
} as const;

/** Dependencies the command layer needs, injected so activate() stays thin. */
export interface CommandDeps {
  /** Read/mutation adapter (shared with the data layer). */
  cli: CliAdapter;
  /** `koryph` binary for terminal shell-outs (default "koryph"). */
  koryphBin?: string;
  /** `bd` binary for terminal shell-outs (default "bd"). */
  bdBin?: string;
  /**
   * Palette fallback: resolve a slot when a command is invoked without a
   * context-menu argument. Returns undefined if the user cancels. When absent,
   * context-free invocations show a guidance message.
   */
  pickSlot?: () => Promise<SlotRef | undefined>;
  /**
   * Transcript webview opener (ext.5). When absent, "Open transcript" falls
   * back to opening the raw stream.jsonl in an editor.
   */
  openTranscript?: (ref: SlotRef) => void | Promise<void>;
  /** Look up a slot's manifest base_commit (for the diff range). Optional. */
  resolveBaseCommit?: (ref: SlotRef) => Promise<string | undefined>;
}

/**
 * Register every slot command. Returns a Disposable that unregisters them all
 * (also pushed onto `context.subscriptions`).
 */
export function registerSlotCommands(
  context: vscode.ExtensionContext,
  deps: CommandDeps,
): vscode.Disposable {
  const bead = vscode.window.createOutputChannel('Koryph — bd show');
  context.subscriptions.push(bead);

  const reg = (id: string, fn: (arg: unknown) => unknown) =>
    vscode.commands.registerCommand(id, fn);

  const disposables = [
    reg(CommandIds.Stop, (a) => withSlot(a, deps, (ref) => stopSlot(deps, ref, false))),
    reg(CommandIds.StopForce, (a) => withSlot(a, deps, (ref) => stopSlot(deps, ref, true))),
    reg(CommandIds.StopRun, (a) => withSlot(a, deps, (ref) => stopRun(deps, ref))),
    reg(CommandIds.Nudge, (a) => withSlot(a, deps, (ref) => nudgeSlot(deps, ref))),
    reg(CommandIds.ChangeModel, (a) => withSlot(a, deps, (ref) => changeModel(deps, ref))),
    reg(CommandIds.OpenTranscript, (a) => withSlot(a, deps, (ref) => openTranscript(deps, ref))),
    reg(CommandIds.Tail, (a) => withSlot(a, deps, (ref) => tailSlot(deps, ref))),
    reg(CommandIds.OpenWorktree, (a) => withSlot(a, deps, (ref) => openWorktree(ref))),
    reg(CommandIds.ShowDiff, (a) => withSlot(a, deps, (ref) => showDiff(deps, ref))),
    reg(CommandIds.OpenPr, (a) => withSlot(a, deps, (ref) => openPr(ref))),
    reg(CommandIds.Merge, (a) => withSlot(a, deps, (ref) => mergeSlot(deps, ref))),
    reg(CommandIds.Land, (a) => withSlot(a, deps, (ref) => landSlot(deps, ref))),
    reg(CommandIds.ShowBead, (a) => withSlot(a, deps, (ref) => showBead(deps, bead, ref))),
  ];
  context.subscriptions.push(...disposables);
  return vscode.Disposable.from(...disposables);
}

// ---------------------------------------------------------------------------
// Argument resolution
// ---------------------------------------------------------------------------

/** Resolve the target slot (from the arg, else the palette picker) and run fn. */
async function withSlot(
  arg: unknown,
  deps: CommandDeps,
  fn: (ref: SlotRef) => unknown,
): Promise<void> {
  let ref = coerceSlotRef(arg);
  if (!ref && deps.pickSlot) {
    ref = await deps.pickSlot();
  }
  if (!ref) {
    if (!deps.pickSlot) {
      void vscode.window.showInformationMessage(
        'Koryph: run this from a slot in the Agent Threads view.',
      );
    }
    return;
  }
  try {
    await fn(ref);
  } catch (err) {
    void vscode.window.showErrorMessage(`Koryph: ${(err as Error).message}`);
  }
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

async function stopSlot(deps: CommandDeps, ref: SlotRef, force: boolean): Promise<void> {
  const label = force ? 'Force-stop (SIGKILL)' : 'Stop (SIGTERM)';
  const detail = force
    ? `SIGKILL ${ref.phaseId}. UNCOMMITTED worktree work is LOST — commits are the only checkpoint.`
    : `SIGTERM ${ref.phaseId}. In-flight, uncommitted work may be lost.`;
  const ok = await vscode.window.showWarningMessage(
    `${label} agent ${ref.phaseId}?`,
    { modal: true, detail },
    force ? 'Force Stop' : 'Stop',
  );
  if (!ok) {
    return;
  }
  await runKoryph(deps, stopArgv(ref, { force }), `stopped ${ref.phaseId}`);
}

async function stopRun(deps: CommandDeps, ref: SlotRef): Promise<void> {
  const ok = await vscode.window.showWarningMessage(
    `Stop the whole run for ${ref.projectId}?`,
    { modal: true, detail: 'SIGTERM to every live agent in this run. Uncommitted work may be lost.' },
    'Stop Run',
  );
  if (!ok) {
    return;
  }
  await runKoryph(deps, stopRunArgv(ref.projectId), `stopped run ${ref.projectId}`);
}

async function nudgeSlot(deps: CommandDeps, ref: SlotRef): Promise<void> {
  const text = await vscode.window.showInputBox({
    title: `Nudge ${ref.phaseId}`,
    prompt: 'Appended to the agent INBOX.md and posted as a bd comment.',
    placeHolder: 'e.g. focus on the failing test in foo_test.go first',
    ignoreFocusOut: true,
  });
  if (text === undefined || text.trim() === '') {
    return;
  }
  await runKoryph(deps, nudgeArgv(ref, text), `nudged ${ref.phaseId}`);
}

async function changeModel(deps: CommandDeps, ref: SlotRef): Promise<void> {
  // Allowlist gating: fable only when the record allowlists it. The record is
  // read via the CLI so we always reflect current allowed_models.
  const allowed = await allowedModels(deps, ref.projectId);
  const tiers = modelChoices(allowed);
  const picked = await vscode.window.showQuickPick(
    tiers.map((t) => ({
      label: t,
      description: t === ref.model ? 'current' : undefined,
    })),
    { title: `Change model for ${ref.beadId}`, placeHolder: 'Resolved at dispatch time (Decision 5)' },
  );
  if (!picked) {
    return;
  }
  const tier = picked.label;

  // Edit the bead label: drop the other plain model:<tier> labels (best-effort),
  // then add the chosen one. The engine re-resolves the label on next dispatch.
  for (const other of tiers) {
    if (other !== tier) {
      await deps.cli.bd(modelLabelRemoveArgv(ref, other)); // ignore non-zero (label absent)
    }
  }
  const addRes = await deps.cli.bd(modelLabelAddArgv(ref, tier));
  if (addRes.code !== 0) {
    throw new Error(`bd label add failed: ${addRes.stderr.trim() || `exit ${addRes.code}`}`);
  }

  // Honest about dispatch-time resolution: offer requeue-now vs next-dispatch.
  const choice = await vscode.window.showInformationMessage(
    `Set ${ref.beadId} → model:${tier}. Model is resolved at dispatch time.`,
    { modal: false },
    'Stop + requeue now',
    'Apply next dispatch',
  );
  if (choice === 'Stop + requeue now') {
    await runKoryph(deps, stopArgv(ref), `requeued ${ref.phaseId} (re-resolves model on next dispatch)`);
  }
}

async function openTranscript(deps: CommandDeps, ref: SlotRef): Promise<void> {
  if (deps.openTranscript) {
    await deps.openTranscript(ref);
    return;
  }
  // The rich webview panel arrives with ext.5; until it wires an opener here,
  // point the user at the zero-parse terminal tail.
  const tail = 'Tail in Terminal';
  const choice = await vscode.window.showInformationMessage(
    `Koryph: transcript panel for ${ref.phaseId} arrives with ext.5.`,
    tail,
  );
  if (choice === tail) {
    tailSlot(deps, ref);
  }
}

function tailSlot(deps: CommandDeps, ref: SlotRef): void {
  runInTerminal(`koryph tail ${ref.phaseId}`, koryphBin(deps), tailArgv(ref));
}

async function openWorktree(ref: SlotRef): Promise<void> {
  if (!ref.worktree) {
    throw new Error(`slot ${ref.phaseId} has no worktree path`);
  }
  const uri = vscode.Uri.file(ref.worktree);
  const choice = await vscode.window.showQuickPick(
    [
      { label: 'Open in new window', id: 'new' },
      { label: 'Add to workspace', id: 'add' },
      { label: 'Reveal in file manager', id: 'reveal' },
    ],
    { title: `Open worktree for ${ref.phaseId}`, placeHolder: ref.worktree },
  );
  if (!choice) {
    return;
  }
  switch (choice.id) {
    case 'new':
      await vscode.commands.executeCommand('vscode.openFolder', uri, { forceNewWindow: true });
      break;
    case 'add':
      vscode.workspace.updateWorkspaceFolders(
        vscode.workspace.workspaceFolders?.length ?? 0,
        0,
        { uri, name: `${ref.projectId}:${ref.phaseId}` },
      );
      break;
    case 'reveal':
      await vscode.commands.executeCommand('revealFileInOS', uri);
      break;
  }
}

async function showDiff(deps: CommandDeps, ref: SlotRef): Promise<void> {
  if (!ref.worktree) {
    throw new Error(`slot ${ref.phaseId} has no worktree`);
  }
  let base = ref.baseCommit;
  if (!base && deps.resolveBaseCommit) {
    base = (await deps.resolveBaseCommit(ref)) ?? '';
  }
  if (!base) {
    throw new Error(`no base_commit known for ${ref.phaseId} — cannot compute diff range`);
  }
  // Primary: the built-in Git extension multi-file diff. Falls back to a
  // terminal `git diff` on any failure (older VS Code, no Git extension, etc.).
  const shown = await tryGitExtensionDiff(ref, base);
  if (!shown) {
    runInTerminal(
      `git diff ${ref.phaseId}`,
      'git',
      gitDiffArgs(base),
      ref.worktree,
    );
  }
}

async function openPr(ref: SlotRef): Promise<void> {
  if (!canOpenPr(ref)) {
    throw new Error(`slot ${ref.phaseId} is not pr-opened (status: ${ref.status})`);
  }
  const url = parsePrUrl(ref.note);
  if (!url) {
    throw new Error(`no PR URL recorded for ${ref.phaseId}`);
  }
  await vscode.env.openExternal(vscode.Uri.parse(url));
}

function mergeSlot(deps: CommandDeps, ref: SlotRef): void {
  if (!ref.branch) {
    throw new Error(`slot ${ref.phaseId} has no branch to merge`);
  }
  runInTerminal(`koryph merge ${ref.branch}`, koryphBin(deps), mergeArgv(ref));
}

function landSlot(deps: CommandDeps, ref: SlotRef): void {
  if (!canLand(ref)) {
    throw new Error(`land targets pr-opened slots (status: ${ref.status})`);
  }
  runInTerminal(`koryph land ${ref.beadId}`, koryphBin(deps), landArgv(ref));
}

async function showBead(
  deps: CommandDeps,
  channel: vscode.OutputChannel,
  ref: SlotRef,
): Promise<void> {
  channel.clear();
  channel.show(true);
  channel.appendLine(`$ bd show ${ref.beadId}`);
  const res = await deps.cli.bd(bdShowArgv(ref));
  channel.append(res.stdout);
  if (res.stderr.trim()) {
    channel.appendLine(`\n[stderr] ${res.stderr.trim()}`);
  }
}

// ---------------------------------------------------------------------------
// Shared plumbing
// ---------------------------------------------------------------------------

function koryphBin(deps: CommandDeps): string {
  return deps.koryphBin ?? 'koryph';
}

/** Run a `koryph` mutation via the adapter and toast success/failure. */
async function runKoryph(deps: CommandDeps, argv: string[], okMsg: string): Promise<void> {
  const res = await deps.cli.koryph(argv);
  if (res.code === 0) {
    void vscode.window.showInformationMessage(`Koryph: ${okMsg}`);
  } else {
    throw new Error(res.stderr.trim() || `koryph ${argv[0]} exited ${res.code}`);
  }
}

/** Look up a project's allowed_models via `koryph project show --json`. */
async function allowedModels(deps: CommandDeps, projectId: string): Promise<string[]> {
  try {
    const res = await deps.cli.koryph(['project', 'show', projectId, '--json']);
    if (res.code === 0) {
      const rec = JSON.parse(res.stdout) as { allowed_models?: string[] };
      if (Array.isArray(rec.allowed_models)) {
        return rec.allowed_models;
      }
    }
  } catch {
    /* fall through to the conservative default */
  }
  return [];
}

/** Send a command to a named integrated terminal (interactive output visible). */
function runInTerminal(name: string, bin: string, args: string[], cwd?: string): void {
  const terminal = vscode.window.createTerminal({ name, cwd });
  terminal.show(true);
  terminal.sendText([bin, ...args].map(shellQuote).join(' '));
}

/**
 * Try the built-in Git extension's multi-file diff of base…HEAD. Returns true
 * when it rendered something, false (or throws-caught) so the caller can fall
 * back to a terminal `git diff`.
 */
async function tryGitExtensionDiff(ref: SlotRef, base: string): Promise<boolean> {
  try {
    const ext = vscode.extensions.getExtension('vscode.git');
    if (!ext) {
      return false;
    }
    const gitApi = (await ext.activate()).getAPI(1);
    const repo =
      gitApi.getRepository?.(vscode.Uri.file(ref.worktree)) ??
      gitApi.repositories?.find((r: { rootUri: vscode.Uri }) => r.rootUri.fsPath === ref.worktree);
    if (!repo || typeof repo.diffBetween !== 'function') {
      return false;
    }
    const changes: Array<{ uri: vscode.Uri; originalUri: vscode.Uri }> = await repo.diffBetween(
      base,
      'HEAD',
    );
    if (!changes || changes.length === 0) {
      void vscode.window.showInformationMessage(`Koryph: no changes on ${ref.branch} since base.`);
      return true;
    }
    const toGitUri = gitApi.toGitUri?.bind(gitApi);
    if (!toGitUri) {
      return false;
    }
    const resources: Array<[vscode.Uri, vscode.Uri, vscode.Uri]> = changes.map((c) => [
      c.uri,
      toGitUri(c.originalUri, base),
      toGitUri(c.uri, 'HEAD'),
    ]);
    await vscode.commands.executeCommand(
      'vscode.changes',
      `${ref.phaseId}: ${base.slice(0, 8)}…HEAD`,
      resources,
    );
    return true;
  } catch {
    return false;
  }
}
