// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// StatusReader — the agent-authored heartbeat at a slot's phase-dir
// status.json ({state, step, pct}; dispatch contract). This is NOT engine state
// and carries no schema_version: the tree shows it in tooltips *labeled
// possibly stale* (§2). Read-only and defensive — a partial/absent file yields
// undefined, never a throw.

import { readJSON } from './json';
import { phaseDir } from './paths';
import { StatusReport } from './schema';
import * as path from 'path';

/** Parse a raw status.json value into a StatusReport, or undefined if unusable. */
export function parseStatusReport(raw: unknown): StatusReport | undefined {
  if (!raw || typeof raw !== 'object') {
    return undefined;
  }
  const o = raw as Record<string, unknown>;
  const report: StatusReport = {};
  if (typeof o.state === 'string') {
    report.state = o.state;
  }
  if (typeof o.step === 'string') {
    report.step = o.step;
  }
  if (typeof o.pct === 'number' && Number.isFinite(o.pct)) {
    report.pct = o.pct;
  }
  if (report.state === undefined && report.step === undefined && report.pct === undefined) {
    return undefined;
  }
  return report;
}

/** Format a report for a tooltip line, always flagged as agent-reported. */
export function formatStatusReport(report: StatusReport | undefined): string | undefined {
  if (!report) {
    return undefined;
  }
  const parts: string[] = [];
  if (report.state) {
    parts.push(report.state);
  }
  if (typeof report.pct === 'number') {
    parts.push(`${Math.round(report.pct)}%`);
  }
  if (report.step) {
    parts.push(report.step);
  }
  if (parts.length === 0) {
    return undefined;
  }
  return `agent-reported (may be stale): ${parts.join(' · ')}`;
}

export class StatusReader {
  /**
   * Read a slot's status.json under `<repo>/.plan-logs/koryph/<runId>/<phaseId>`.
   * Returns undefined when absent, unreadable, or empty of the three fields.
   */
  async read(repoRoot: string, runId: string, phaseId: string): Promise<StatusReport | undefined> {
    const raw = await readJSON(path.join(phaseDir(repoRoot, runId, phaseId), 'status.json'));
    return parseStatusReport(raw);
  }
}
