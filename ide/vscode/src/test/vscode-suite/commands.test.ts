// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// In-host smoke test: registering the slot commands makes every contributed
// command id resolvable, and a context-free invocation with no picker is a
// safe no-op (it must not throw). Runs only under `npm run test:electron`.

import * as assert from 'assert';
import * as vscode from 'vscode';
import { CliAdapter } from '../../data';
import { CommandIds, registerSlotCommands } from '../../commands';

describe('slot commands (in-host smoke)', () => {
  it('registers every command id and unregisters on dispose', async () => {
    const ctx = { subscriptions: [] as vscode.Disposable[] } as vscode.ExtensionContext;
    const disposable = registerSlotCommands(ctx, { cli: new CliAdapter() });

    const registered = await vscode.commands.getCommands(true);
    for (const id of Object.values(CommandIds)) {
      assert.ok(registered.includes(id), `command ${id} registered`);
    }

    // Context-free invocation with no pickSlot: a guidance no-op, never a throw.
    await vscode.commands.executeCommand(CommandIds.Stop);

    disposable.dispose();
    ctx.subscriptions.forEach((d) => d.dispose());
    const after = await vscode.commands.getCommands(true);
    assert.ok(!after.includes(CommandIds.Stop), 'command unregistered after dispose');
  });
});
