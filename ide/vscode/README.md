<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph — VS Code extension

Makes running koryph waves *visible and steerable* from the editor. See the
design at [`docs/designs/2026-07-vscode-extension.md`](../../docs/designs/2026-07-vscode-extension.md).

**Status: data layer + slot commands** (beads ext.3, ext.6). The read-only
file-watching + CLI-shelling data layer is in place; the slot commands below are
wired. The tree view (ext.4) and transcript webviews (ext.5) are still pending.

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

## Slot commands (`src/commands/`, bead ext.6)

Palette + context-menu actions that steer a running wave. **Every mutation is a
`koryph` / `bd` CLI shell-out** — the extension never signals a PID and never
writes koryph state (Decision 3). `argv.ts` is the pure, unit-tested core
(argument vectors + model-allowlist gating); `index.ts` is the VS Code glue.

| Command | Mechanism |
|---|---|
| Stop (graceful / force) | `koryph stop --project <id> [--force] <phase>` (modal confirm; force is destructive-styled) |
| Stop whole run | `koryph stop --project <id>` |
| Nudge… | input box → `koryph nudge --project <id> <phase> "text"` |
| Change model… | quick-pick haiku/sonnet/opus (+ fable iff allowlisted) → `bd label add … model:<tier>`, then *Stop + requeue now* vs *Apply next dispatch* (honest about dispatch-time resolution, Decision 5) |
| Open transcript | ext.5 webview when present; otherwise offers Tail |
| Tail in terminal | `koryph tail --project <id> <phase> --follow` |
| Open worktree… | quick-pick: new window / add to workspace / reveal |
| Show diff vs base | Git-extension multi-file diff of `base_commit…HEAD`; terminal `git diff` fallback |
| Open PR | `pr-opened` slots: opens the PR URL parsed from the slot note |
| Merge / Land | `koryph merge <branch>` / `koryph land <bead>` in an integrated terminal |
| Show bead | output channel with `bd -C <root> show <bead>` |

Context-menu `when` clauses key off the tree item's `contextValue` (`koryphRun`,
`koryphSlot`, and status-suffixed variants like `koryphSlot.pr-opened`), a
contract the tree view (ext.4) must honor.

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
