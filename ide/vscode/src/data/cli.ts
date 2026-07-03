// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// CliAdapter — shells out to the `koryph` and `bd` binaries (Decision 3: every
// mutation goes through the CLI, never direct file writes or PID signals). This
// module provides only the read-side fan-out the data layer needs — namely
// `koryph board --json` (§10b, cross-project enumeration) — plus the generic
// spawn primitive that command modules (ext.6) build mutations on later.
//
// KORYPH_HOME is respected: whatever home the extension resolved is forwarded
// so the CLI reads the same registry/slots/quota tree the watchers read. The
// ambient env is otherwise passed through unchanged — koryph itself scrubs
// credentials and rebuilds account identity from the registry (§7), so the
// extension never needs to (and must not) manage CLAUDE_CONFIG_DIR.

import { spawn } from 'child_process';
import { tryParse } from './json';
import { koryphHome } from './paths';
import { BoardEntry } from './schema';

export interface CliOptions {
  /** koryph binary (default "koryph"; override for tests / non-PATH installs). */
  koryphBin?: string;
  /** bd binary (default "bd"). */
  bdBin?: string;
  /** Working directory for the child (default: inherit). */
  cwd?: string;
  /** Environment (default process.env); KORYPH_HOME is ensured from paths. */
  env?: NodeJS.ProcessEnv;
  /** Per-invocation timeout in ms (default 40_000 — the ccusage worst case). */
  timeoutMs?: number;
}

export interface CliResult {
  code: number;
  stdout: string;
  stderr: string;
  /** True when the process was killed by the timeout. */
  timedOut: boolean;
}

const DEFAULT_TIMEOUT_MS = 40_000;

export class CliAdapter {
  private readonly koryphBin: string;
  private readonly bdBin: string;
  private readonly cwd?: string;
  private readonly env: NodeJS.ProcessEnv;
  private readonly timeoutMs: number;

  constructor(opts: CliOptions = {}) {
    this.koryphBin = opts.koryphBin ?? 'koryph';
    this.bdBin = opts.bdBin ?? 'bd';
    this.cwd = opts.cwd;
    // Ensure KORYPH_HOME is set to the resolved home so the CLI and the
    // watchers agree on which state tree they operate over.
    const base = opts.env ?? process.env;
    this.env = { ...base, KORYPH_HOME: koryphHome(base) };
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
  }

  /** Run `koryph <args...>` and capture output. */
  koryph(args: string[]): Promise<CliResult> {
    return this.run(this.koryphBin, args);
  }

  /** Run `bd <args...>` and capture output. */
  bd(args: string[]): Promise<CliResult> {
    return this.run(this.bdBin, args);
  }

  /**
   * Cross-project enumeration via `koryph board --json` (§10b). Returns the
   * parsed board entries, or throws with the CLI's stderr on failure so
   * callers can fall back to the registry+ledger watchers.
   */
  async board(): Promise<BoardEntry[]> {
    const res = await this.koryph(['board', '--json']);
    if (res.code !== 0) {
      throw new Error(
        `koryph board --json exited ${res.code}${res.timedOut ? ' (timed out)' : ''}: ${res.stderr.trim()}`,
      );
    }
    const parsed = tryParse(res.stdout);
    if (!Array.isArray(parsed)) {
      throw new Error('koryph board --json did not return a JSON array');
    }
    return parsed as BoardEntry[];
  }

  /** Generic spawn + capture. Never rejects on non-zero exit — inspect .code. */
  run(bin: string, args: string[]): Promise<CliResult> {
    return new Promise<CliResult>((resolve, reject) => {
      const child = spawn(bin, args, {
        cwd: this.cwd,
        env: this.env,
        stdio: ['ignore', 'pipe', 'pipe'],
      });
      let stdout = '';
      let stderr = '';
      let timedOut = false;
      const timer = setTimeout(() => {
        timedOut = true;
        child.kill('SIGKILL');
      }, this.timeoutMs);
      if (typeof timer.unref === 'function') {
        timer.unref();
      }
      child.stdout.on('data', (d) => {
        stdout += d.toString();
      });
      child.stderr.on('data', (d) => {
        stderr += d.toString();
      });
      child.on('error', (err) => {
        clearTimeout(timer);
        reject(err);
      });
      child.on('close', (code) => {
        clearTimeout(timer);
        resolve({ code: code ?? -1, stdout, stderr, timedOut });
      });
    });
  }
}
