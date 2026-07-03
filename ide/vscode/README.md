<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph — VS Code extension

Makes running koryph waves *visible and steerable* from the editor. See the
design at [`docs/designs/2026-07-vscode-extension.md`](../../docs/designs/2026-07-vscode-extension.md).

**Status: data layer only** (bead ext.3). This bead ships the read-only
file-watching + CLI-shelling data layer that later beads (tree view, transcript
webviews, status bar, slot commands) build on. There is no UI yet.

## Architecture (this bead)

koryph is entirely file + signal based — no daemon, no socket. The extension is
a **file-watching client + CLI shell-out**, never a server peer, and is
**read-only** on every koryph state file (single-writer discipline: the engine
owns `ledger.json`; the CLI owns the rest). Every mutation, when later beads add
them, shells out to `koryph` / `bd`.

`src/data/`:

| Module | Reads | Notes |
|---|---|---|
| `schema.ts` | — | TS transcriptions of the Go types + `schema_version` guard |
| `paths.ts` | — | `KORYPH_HOME` resolution (mirrors `internal/paths`) |
| `watcher.ts` | any path | `fs.watch` with a polling fallback |
| `registryWatcher.ts` | `~/.koryph/registry.d/*.json` | project records |
| `ledgerWatcher.ts` | `<repo>/.plan-logs/koryph/latest/ledger.json` | per-project run (schema v2) |
| `governorReader.ts` | `~/.koryph/slots/*`, `governor.json` | live global slot picture |
| `quotaReader.ts` | `~/.koryph/quota/*.json` | cached account ceilings |
| `streamReader.ts` | per-slot `stream.jsonl` | byte-offset incremental JSONL parse |
| `cli.ts` | `koryph board --json` | child_process adapter (respects `KORYPH_HOME`) |

Unknown/newer `schema_version` values **degrade to raw JSON**, never crash.

## Build & test

```sh
npm install
npm test          # fixture-driven data-layer unit tests (pure Node, headless)
npm run bundle    # esbuild → dist/extension.js
```

`npm test` compiles (`tsc`) and runs the mocha suite in `src/test/suite`
against realistic fixtures in `src/test/fixtures` (synthesized from the Go type
definitions — no real agents are ever run).

`npm run test:electron` runs the `@vscode/test-electron` in-host smoke suite.
It is **opt-in** (needs a display / xvfb + a VS Code download) and is not part
of `npm test`, so the Go-only gate stays green on headless machines.
