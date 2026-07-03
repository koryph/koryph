// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// esbuild bundler for the koryph VS Code extension. The extension host runs
// the CommonJS bundle at dist/extension.js; `vscode` is provided by the host
// and must stay external. No runtime deps beyond the VS Code API (Decision 1).
'use strict';

const esbuild = require('esbuild');

const production = process.argv.includes('--production');
const watch = process.argv.includes('--watch');

/** @type {import('esbuild').BuildOptions} */
const options = {
  entryPoints: ['src/extension.ts'],
  bundle: true,
  outfile: 'dist/extension.js',
  platform: 'node',
  format: 'cjs',
  target: 'node18',
  external: ['vscode'],
  sourcemap: !production,
  minify: production,
  logLevel: 'info',
};

async function main() {
  if (watch) {
    const ctx = await esbuild.context(options);
    await ctx.watch();
    return;
  }
  await esbuild.build(options);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
