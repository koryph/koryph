// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Unit tests for parsing a bead title out of `bd show --json` (ext.4 §2). The
// title enriches slot rows and is cached. Plain node.

import * as assert from 'assert';
import { parseBeadTitle } from '../../data/beadTitle';

describe('parseBeadTitle', () => {
  it('reads a bare object title', () => {
    assert.strictEqual(parseBeadTitle('{"id":"koryph-i2n","title":"Add completions"}'), 'Add completions');
  });

  it('reads the first title from an array', () => {
    assert.strictEqual(parseBeadTitle('[{"title":"first"},{"title":"second"}]'), 'first');
  });

  it('reads an {issues:[…]} envelope', () => {
    assert.strictEqual(parseBeadTitle('{"issues":[{"title":"enveloped"}]}'), 'enveloped');
  });

  it('trims whitespace and skips empty titles', () => {
    assert.strictEqual(parseBeadTitle('{"title":"  spaced  "}'), 'spaced');
    assert.strictEqual(parseBeadTitle('{"title":"   "}'), undefined);
  });

  it('returns undefined for unrecognized or malformed output', () => {
    assert.strictEqual(parseBeadTitle('not json'), undefined);
    assert.strictEqual(parseBeadTitle('{"id":"x"}'), undefined);
    assert.strictEqual(parseBeadTitle('42'), undefined);
  });
});
