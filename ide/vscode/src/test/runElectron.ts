// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// @vscode/test-electron harness (§9). Downloads a pinned VS Code, launches an
// extension host, and runs the in-host smoke suite (src/test/vscode-suite).
//
// This is NOT part of `npm test`: it needs a display (or xvfb) and a network
// download, so it is opt-in via `npm run test:electron`. The fixture-driven
// data-layer unit tests (pure Node, no host) are the gate that `npm test`
// runs. Keeping the two split is what lets `make ext-test` stay green on
// headless Go-only machines.

import * as path from 'path';
import { runTests } from '@vscode/test-electron';

async function main(): Promise<void> {
  const extensionDevelopmentPath = path.resolve(__dirname, '..', '..');
  const extensionTestsPath = path.resolve(__dirname, 'vscode-suite', 'index');
  await runTests({ extensionDevelopmentPath, extensionTestsPath });
}

main().catch((err) => {
  console.error('electron test run failed:', err);
  process.exit(1);
});
