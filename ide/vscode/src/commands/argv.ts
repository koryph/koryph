// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Pure argv construction + model-allowlist gating for the slot commands
// (ext.6). This module imports NOTHING from `vscode` on purpose: it is the
// unit-tested core that the VS Code-facing handlers in `./index.ts` build on.
// Every mutation the extension performs is a CLI shell-out (Decision 3); the
// exact argument vectors are assembled here so they can be asserted without a
// VS Code host and without ever dispatching a real agent.
//
// The command → mechanism mapping is the normative table in
// docs/designs/2026-07-vscode-extension.md §3:
//
//   Stop (graceful)  koryph stop --project <id> <phase>
//   Stop (force)     koryph stop --project <id> --force <phase>
//   Stop whole run   koryph stop --project <id>
//   Nudge            koryph nudge --project <id> <phase> "text"
//   Tail             koryph tail --project <id> <phase> --follow
//   Merge            koryph merge --project <id> <branch>
//   Land             koryph land  --project <id> <bead>
//   Change model     bd -C <root> label add/remove <bead> model:<tier>

import { Slot } from '../data/schema';

// ---------------------------------------------------------------------------
// SlotRef — the minimal, UI-agnostic handle a command operates on
// ---------------------------------------------------------------------------

/**
 * Everything a slot command needs, distilled from a `ledger.Slot` plus the
 * project it belongs to. Tree items (ext.4) and the palette slot picker both
 * normalize down to this shape so the handlers never touch tree internals.
 */
export interface SlotRef {
  /** Registry project id (`--project`). */
  projectId: string;
  /** Absolute project root — cwd for `bd`/`git` shell-outs. */
  projectRoot: string;
  /** Phase id (== bead id for bd-sourced runs); the stop/nudge/tail selector. */
  phaseId: string;
  /** Bead id for `bd` operations (label, show, land). Falls back to phaseId. */
  beadId: string;
  /** Agent branch (`koryph merge` selector). */
  branch: string;
  /** Worktree path (open worktree, git diff cwd). */
  worktree: string;
  /** Base commit for the diff range (from the manifest; empty if unknown). */
  baseCommit: string;
  /** Current model tier (pre-selects the change-model quick-pick). */
  model: string;
  /** Slot status (gates PR / merge / land availability). */
  status: string;
  /** Slot note — carries the PR URL for pr-opened slots. */
  note: string;
}

/**
 * Build a SlotRef from a ledger Slot + project coordinates. `baseCommit` comes
 * from the per-slot manifest (read separately) and may be empty.
 */
export function slotRef(
  projectId: string,
  projectRoot: string,
  slot: Slot,
  baseCommit = '',
): SlotRef {
  return {
    projectId,
    projectRoot,
    phaseId: slot.phase_id,
    beadId: slot.bead_id && slot.bead_id.length > 0 ? slot.bead_id : slot.phase_id,
    branch: slot.branch ?? '',
    worktree: slot.worktree ?? '',
    baseCommit,
    model: slot.model ?? '',
    status: slot.status ?? '',
    note: slot.note ?? '',
  };
}

// ---------------------------------------------------------------------------
// koryph argv builders
// ---------------------------------------------------------------------------

/** `koryph stop --project <id> [--force] <phase>` (graceful SIGTERM by default). */
export function stopArgv(ref: SlotRef, opts: { force?: boolean } = {}): string[] {
  const argv = ['stop', '--project', ref.projectId];
  if (opts.force) {
    argv.push('--force');
  }
  argv.push(ref.phaseId);
  return argv;
}

/** `koryph stop --project <id>` — stop every live slot in the run. */
export function stopRunArgv(projectId: string): string[] {
  return ['stop', '--project', projectId];
}

/** `koryph nudge --project <id> <phase> "<text>"` — appends INBOX + bd comment. */
export function nudgeArgv(ref: SlotRef, text: string): string[] {
  return ['nudge', '--project', ref.projectId, ref.phaseId, text];
}

/** `koryph tail --project <id> <phase> --follow` — the zero-parse fallback. */
export function tailArgv(ref: SlotRef): string[] {
  return ['tail', '--project', ref.projectId, ref.phaseId, '--follow'];
}

/** `koryph merge --project <id> <branch>` — land a finished agent branch. */
export function mergeArgv(ref: SlotRef): string[] {
  return ['merge', '--project', ref.projectId, ref.branch];
}

/** `koryph land --project <id> <bead>` — land an engine-opened PR (pr-opened). */
export function landArgv(ref: SlotRef): string[] {
  return ['land', '--project', ref.projectId, ref.beadId];
}

// ---------------------------------------------------------------------------
// bd argv builders (change model — Decision 5)
// ---------------------------------------------------------------------------

/** `bd -C <root> label add <bead> model:<tier>`. */
export function modelLabelAddArgv(ref: SlotRef, tier: string): string[] {
  return ['-C', ref.projectRoot, 'label', 'add', ref.beadId, `model:${tier}`];
}

/** `bd -C <root> label remove <bead> model:<tier>` — best-effort de-dupe. */
export function modelLabelRemoveArgv(ref: SlotRef, tier: string): string[] {
  return ['-C', ref.projectRoot, 'label', 'remove', ref.beadId, `model:${tier}`];
}

/** `bd -C <root> show <bead>` — feeds the "Show bead" output channel. */
export function bdShowArgv(ref: SlotRef): string[] {
  return ['-C', ref.projectRoot, 'show', ref.beadId];
}

// ---------------------------------------------------------------------------
// git argv (diff vs base — terminal fallback for the Git-extension path)
// ---------------------------------------------------------------------------

/**
 * `git diff <base>...HEAD` args, run with cwd = worktree. The three-dot range
 * matches the design's `base_commit…branch` intent: changes introduced on the
 * branch since it forked from base.
 */
export function gitDiffArgs(baseCommit: string): string[] {
  return ['diff', `${baseCommit}...HEAD`];
}

// ---------------------------------------------------------------------------
// Model allowlist gating (Decision 5)
// ---------------------------------------------------------------------------

/** The always-offered standard tiers, in ascending capability order. */
export const STANDARD_MODEL_TIERS = ['haiku', 'sonnet', 'opus'] as const;

/** The gated tier — only offered when the project explicitly allowlists it. */
export const GATED_MODEL_TIER = 'fable';

/**
 * The model tiers to show in the change-model quick-pick. haiku/sonnet/opus are
 * always offered; `fable` appears ONLY when the project's `allowed_models`
 * includes it (mirrors modelroute's fable guard — a resolved fable is illegal
 * unless allowlisted, so offering it otherwise would be a lie). Case- and
 * whitespace-insensitive on the allowlist entries.
 */
export function modelChoices(allowedModels: readonly string[] | undefined): string[] {
  const tiers: string[] = [...STANDARD_MODEL_TIERS];
  const allowsFable = (allowedModels ?? []).some(
    (m) => m.trim().toLowerCase() === GATED_MODEL_TIER,
  );
  if (allowsFable) {
    tiers.push(GATED_MODEL_TIER);
  }
  return tiers;
}

/** Reports whether a tier may be offered given the project allowlist. */
export function isModelChoiceAllowed(
  tier: string,
  allowedModels: readonly string[] | undefined,
): boolean {
  return modelChoices(allowedModels).includes(tier);
}

// ---------------------------------------------------------------------------
// Status gating
// ---------------------------------------------------------------------------

/** Open PR is only meaningful for a pr-opened slot. */
export function canOpenPr(ref: SlotRef): boolean {
  return ref.status === 'pr-opened';
}

/** Land targets pr-opened slots; Merge targets a ready-to-integrate branch. */
export function canLand(ref: SlotRef): boolean {
  return ref.status === 'pr-opened';
}

/**
 * Duck-type an arbitrary command argument (a tree item from ext.4, or a raw
 * SlotRef) into a SlotRef. Tree items attach the ref under `.slotRef`. Returns
 * undefined when the value carries no slot (a palette invocation).
 */
export function coerceSlotRef(arg: unknown): SlotRef | undefined {
  if (!arg || typeof arg !== 'object') {
    return undefined;
  }
  const obj = arg as Record<string, unknown>;
  if (obj.slotRef && typeof obj.slotRef === 'object') {
    return coerceSlotRef(obj.slotRef);
  }
  if (typeof obj.projectId === 'string' && typeof obj.phaseId === 'string') {
    return arg as SlotRef;
  }
  return undefined;
}

/**
 * Minimal POSIX single-quote escaping for assembling a terminal command line
 * from an argv array. Bare-word safe tokens pass through unquoted.
 */
export function shellQuote(arg: string): string {
  if (arg === '') {
    return "''";
  }
  if (/^[A-Za-z0-9_./:@%+=-]+$/.test(arg)) {
    return arg;
  }
  return `'${arg.replace(/'/g, `'\\''`)}'`;
}

/**
 * Extract the PR URL a pr-opened slot records in its note
 * (`PR #<n> opened: <url>` — internal/engine/poll.go). Returns undefined when
 * absent.
 */
export function parsePrUrl(note: string | undefined): string | undefined {
  if (!note) {
    return undefined;
  }
  const m = note.match(/https?:\/\/\S+/);
  return m ? m[0] : undefined;
}
