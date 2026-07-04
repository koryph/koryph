<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Testing

This chapter explains how the test suite works, how to run it, and how to
extend it.

## Running the suite

All test commands are Makefile targets (see `make help` for the full list):

| Target | What it does |
|---|---|
| `make test` | `go test ./...` — the full suite, no frills |
| `make test-race` | Same with `-race` for data-race detection |
| `make cover` | Same with `-cover` for a per-package summary |
| `make gate` | `fmt-check` → `build` → `vet` → `test` — the green gate CI runs |

The **green gate** is the canonical pre-merge bar.  A PR is mergeable only
when `make gate` passes cleanly.

## VS Code extension (`ide/vscode/`)

The extension has its own toolchain (npm/tsc/esbuild/mocha) and is kept out
of `make gate` so the Go gate stays fast and doesn't require Node. It has a
dedicated CI job instead.

| Target | What it does |
|---|---|
| `make ext-build` | `npm ci` + `npm run bundle` — esbuild bundle to `dist/extension.js` |
| `make ext-test` | `npm ci` + `npm test` — plain-mocha unit suite (`src/test/suite/**`) |

Both targets print a notice and skip (exit 0) if `npm` is not on `$PATH`,
mirroring how `make lint` and `make reuse` treat optional tools.

`npm test` runs `pretest` (`tsc -p ./`) then mocha over
`out/test/suite/**/*.test.js` — the fixture-driven, pure-Node unit tests
(per `.mocharc.json`). It does **not** invoke `@vscode/test-electron`: the
in-host smoke suite under `src/test/vscode-suite/` runs separately via
`npm run test:electron` (needs a VS Code download and a display), and is
intentionally not part of `make ext-test` or CI.

## Package overview

| Package | Approach |
|---|---|
| `internal/engine` | End-to-end integration via fake `bd` + fake `claude` scripts |
| `internal/anthro` | Unit tests against fake HTTP backends; one opt-in live test |
| `internal/sched` | Pure-logic unit tests for wave scheduling |
| `internal/ledger`, `beads`, `dispatch`, … | Unit tests per package |

## Engine end-to-end tests

`internal/engine/engine_test.go` drives the full engine loop without a
network or real tools.

### Fake script fixtures

Two shell-script constants are written to `t.TempDir()` as executable
binaries at test startup:

**`fakeBDScript`** — a minimal `bd` stub.  On the first `ready` call it
returns `ready.json` (one open task); on subsequent calls it returns `[]`.
Mutations (`update`, `close`, `comment`) succeed silently and are appended
to `bd.log` so tests can assert what the engine asked bd to do.

**`fakeClaudeScript`** — a well-behaved implementer stub.  It commits one
file (`agent-work.txt`) to the worktree, writes `SUMMARY.md` to
`$KORYPH_SUMMARY_PATH`, and emits a JSON cost line (`total_cost_usd: 0.42`)
so the ledger cost-recording path is exercised.

### Environment overrides

The engine reads two env vars at startup that redirect which binaries it
calls:

| Variable | Purpose |
|---|---|
| `KORYPH_BD_BIN` | Path to the `bd` binary (default: `bd` on `$PATH`) |
| `KORYPH_CLAUDE_BIN` | Path to the `claude` binary (default: `claude` on `$PATH`) |

`newFixture` sets both via `t.Setenv` to point at the fake scripts, so the
tests never touch the real tools.  Two additional overrides tighten timing:
`KORYPH_BACKOFF_SEC=0` eliminates requeue delays; `KORYPH_NO_NPX=1`
prevents any Node fallback.

### The fixture

`newFixture(t, fixOpts{})` wires up a complete mock world:

- a temp git repo seeded with a `koryph.project.json` and an
  `implementer.md` agent persona
- a registry entry pointing at the temp repo
- a `KORYPH_HOME` isolated from `~/.koryph`
- the fake `bd` and `claude` binaries, with a `ready.json` that holds one
  open task (`tb1`)

`fixOpts` lets individual tests vary the setup: `expectedIdentity`,
`migrationStatus`, `workSource`, and `mergePolicy`.

### Key tests

| Test | What it exercises |
|---|---|
| `TestRunOnceMergesAndDrains` | Full happy-path: frontier → dispatch → auto-merge → ledger → drain |
| `TestRunAccountMismatchFailsClosed` | Identity check fires before any state is touched |
| `TestRunMergePendingWithoutAutoMerge` | without `--auto-merge` leaves branch + worktree intact, posts bd comment |
| `TestRunRefusesUnvalidatedProject` | Engine rejects a project that hasn't been validated |
| `TestRunRefusesMarkdownWorkSource` | Legacy `markdown` work-source is blocked until migrated |

## Anthro engine-import guardrail

`internal/anthro/anthro_test.go` contains `TestEngineNeverImportsAnthro`,
which parses every `.go` file under `internal/engine` with the Go AST and
fails the build if any of them imports `internal/anthro`.

The rule: the engine loop must never make per-token API calls implicitly.
All Anthropic traffic is routed through the dispatch and review layers
explicitly; the engine itself stays cost-neutral.

## Live API tests

`internal/anthro/batch_live_test.go` exercises the full
`BatchSubmit → BatchWait` path against the real Anthropic Message Batches
API.  It is **skipped by default**; to run it, set `KORYPH_BATCH_API_KEY`
explicitly (the ambient `ANTHROPIC_API_KEY` is intentionally refused):

```sh
KORYPH_BATCH_API_KEY=sk-ant-… \
  go test -v ./internal/anthro/ -run TestBatchLive -timeout 15m
```

## Extending the suite

**Add a new engine scenario**: call `newFixture` with the appropriate
`fixOpts`, then call `engine.Run` and assert on the returned `Outcome`,
the ledger, and `f.bdLog(t)`.

**Test a new `bd` behaviour**: extend `fakeBDScript` to handle the new
subcommand, or add a new case to the `switch` inside the script constant,
and update the `ready.json` fixture as needed.

**Add a unit test to a sub-package**: write a standard `_test.go` file in
the package directory.  No external deps or network access are needed for
any package except `internal/anthro` (live test only).
