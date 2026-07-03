// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Small, dependency-free helpers for reading the JSON state files koryph
// writes atomically. Reads are best-effort: a missing file, a partial write
// caught mid-flight, or malformed JSON yields `undefined` rather than throwing,
// so a watcher tick never crashes the extension.

import * as fs from 'fs';
import * as fsp from 'fs/promises';

/** Parse JSON, returning undefined (never throwing) on any error. */
export function tryParse(text: string): unknown | undefined {
  try {
    return JSON.parse(text);
  } catch {
    return undefined;
  }
}

/** Read + parse a JSON file synchronously; undefined if absent or malformed. */
export function readJSONSync(file: string): unknown | undefined {
  let text: string;
  try {
    text = fs.readFileSync(file, 'utf8');
  } catch {
    return undefined;
  }
  return tryParse(text);
}

/** Read + parse a JSON file; undefined if absent or malformed. */
export async function readJSON(file: string): Promise<unknown | undefined> {
  let text: string;
  try {
    text = await fsp.readFile(file, 'utf8');
  } catch {
    return undefined;
  }
  return tryParse(text);
}

/**
 * Parse a JSONL buffer into one raw value per non-empty line. A trailing
 * partial line (no terminating newline) is returned separately so an
 * incremental reader can hold it back until the rest arrives.
 */
export function parseJSONL(buffer: string): { records: unknown[]; remainder: string } {
  const records: unknown[] = [];
  let start = 0;
  let nl = buffer.indexOf('\n', start);
  while (nl !== -1) {
    const line = buffer.slice(start, nl).trim();
    if (line.length > 0) {
      const parsed = tryParse(line);
      if (parsed !== undefined) {
        records.push(parsed);
      }
    }
    start = nl + 1;
    nl = buffer.indexOf('\n', start);
  }
  return { records, remainder: buffer.slice(start) };
}

/** List *.json entries in a directory (basenames); [] if the dir is absent. */
export async function listJSON(dir: string): Promise<string[]> {
  let entries: string[];
  try {
    entries = await fsp.readdir(dir);
  } catch {
    return [];
  }
  return entries.filter((e) => e.endsWith('.json'));
}
