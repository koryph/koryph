// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for the status → glyph mapping (ext.4 §2). Plain node.

import * as assert from 'assert';
import { statusGlyph } from '../../tree/glyph';
import { SlotStatus } from '../../data/schema';

describe('statusGlyph', () => {
  it('maps every known slot status to a stable glyph', () => {
    for (const status of Object.values(SlotStatus)) {
      const g = statusGlyph(status);
      assert.strictEqual(g.word, status);
      assert.ok(g.symbol.length > 0, `symbol for ${status}`);
      assert.ok(g.icon.length > 0, `icon for ${status}`);
    }
  });

  it('spins running and dispatching only', () => {
    assert.strictEqual(statusGlyph('running').spin, true);
    assert.strictEqual(statusGlyph('dispatching').spin, true);
    assert.strictEqual(statusGlyph('review').spin, false);
    assert.strictEqual(statusGlyph('merged').spin, false);
  });

  it('is case-insensitive and degrades unknown statuses', () => {
    assert.strictEqual(statusGlyph('RUNNING').word, 'running');
    const unknown = statusGlyph('wat');
    assert.strictEqual(unknown.word, 'wat');
    assert.strictEqual(unknown.icon, 'question');
    assert.strictEqual(statusGlyph(undefined).word, 'unknown');
  });
});
