// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// In-host smoke test: the extension activates and returns a wired data layer.
// Runs only under `npm run test:electron` (needs the VS Code host).

import * as assert from 'assert';
import * as vscode from 'vscode';
import { activate, deactivate } from '../../extension';

describe('activation (in-host smoke)', () => {
  it('activates and exposes the read-only data layer, then deactivates', () => {
    const ctx = { subscriptions: [] as vscode.Disposable[] } as vscode.ExtensionContext;
    const data = activate(ctx);
    assert.ok(data.registry, 'registry watcher present');
    // governor reader removed in koryph-5ew: agent state now flows through
    // CockpitReader (koryph cockpit --json) rather than direct file reads.
    assert.ok(data.quota, 'quota reader present');
    assert.ok(ctx.subscriptions.length >= 3, 'disposables registered');
    deactivate();
  });
});
