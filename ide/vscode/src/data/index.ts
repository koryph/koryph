// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Public surface of the koryph data layer: schema types, path resolution,
// watchers, readers, and the CLI adapter. UI layers (tree view, transcript
// webviews, status bar — later beads) consume only this module.

export * from './schema';
export * from './paths';
export * from './json';
export { PathWatcher } from './watcher';
export type { WatchOptions, ChangeListener } from './watcher';
export { RegistryWatcher } from './registryWatcher';
export type { ParsedRecord } from './registryWatcher';
export { LedgerWatcher } from './ledgerWatcher';
export type { ParsedRun } from './ledgerWatcher';
export { GovernorReader } from './governorReader';
export type { GovernorSnapshot } from './governorReader';
export { QuotaReader } from './quotaReader';
export type { ParsedQuotaConfig } from './quotaReader';
export { StreamReader } from './streamReader';
export type { StreamEvent } from './streamReader';
export { CliAdapter } from './cli';
export type { CliOptions, CliResult } from './cli';
export { BeadTitleCache, parseBeadTitle } from './beadTitle';
export { StatusReader, parseStatusReport, formatStatusReport } from './statusReader';
export { CockpitReader } from './cockpitReader';
