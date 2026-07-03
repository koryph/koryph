// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// In-host mocha runner for the @vscode/test-electron smoke suite. Discovers
// compiled *.test.js siblings without a glob dependency.

import * as fs from 'fs';
import * as path from 'path';
import Mocha = require('mocha');

export function run(): Promise<void> {
  const mocha = new Mocha({ ui: 'bdd', color: true, timeout: 20000 });
  const dir = __dirname;
  for (const name of fs.readdirSync(dir)) {
    if (name.endsWith('.test.js')) {
      mocha.addFile(path.join(dir, name));
    }
  }
  return new Promise((resolve, reject) => {
    try {
      mocha.run((failures) => {
        if (failures > 0) {
          reject(new Error(`${failures} test(s) failed`));
        } else {
          resolve();
        }
      });
    } catch (err) {
      reject(err as Error);
    }
  });
}
