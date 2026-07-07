<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# VS Code extension architecture

This document is for contributors working on the koryph VS Code extension
(`ide/vscode/`). For end-user documentation see the
[IDE integration](../../docs/user-guide/running-waves.md) chapter.

## Data layer

**Constraint (koryph-5ew):** the extension MUST consume all agent and project
state exclusively through the `internal/cockpit` layer. Specifically, it calls
`koryph cockpit --json --project <id>` (implemented in
`cmd/koryph/cockpit.go`) which internally calls
`cockpit.NewLedgerProvider(...).Refresh()`.

### Why this matters

Both the terminal TUI (`koryph tui`, powered by `internal/tui`) and the VS
Code panel visualise the same koryph run state. If the extension reads
ledger/governor/quota files directly, the two surfaces will inevitably drift:

- The cockpit layer applies **TTL caching** (`burndownTTL`, `queueTTL`,
  `graphTTL`) to avoid hammering the filesystem on every render tick.
  Bypassing it re-introduces polling pressure.
- Future changes to the on-disk format only need to be handled once — in
  `cockpit.LedgerProvider`, not once in the TUI and again in the extension.
- The TUI calls `cockpit.Provider.Refresh()` on every 100 ms tick. The
  extension uses the same codepath, so the two surfaces cannot diverge.

### Data flow

```
Extension (TypeScript)
  └─ CockpitReader.snapshot()       ide/vscode/src/data/cockpitReader.ts
       └─ CliAdapter.cockpit()       ide/vscode/src/data/cli.ts
            └─ koryph cockpit --json --project <id>
                 └─ cockpit.NewLedgerProvider(...).Refresh()
                      ├─ ledger.Store.LoadLatest()
                      ├─ govern.Store.Pools() / PoolStatus()
                      ├─ beads.Adapter.Ready() / List()   (cached at queueTTL)
                      └─ quota.LoadConfig()               (cached at burndownTTL)
```

`CockpitReader` watches the project's `.plan-logs/koryph/` directory for
filesystem changes so the tree refreshes immediately on any run event, then
calls `koryph cockpit --json` to fetch the authoritative snapshot.

### Constraint enforcement

**Do NOT add new direct imports of the following Go packages to anything under
`ide/vscode/`, nor to any `cmd/koryph/` command that exclusively serves the
extension:**

| Package | Correct path |
|---|---|
| `internal/ledger` (`.Store`, `.LoadLatest`) | via `cockpit.LedgerProvider` |
| `internal/govern` (`.Store`, `.Pools`, `.PoolStatus`) | via `cockpit.LedgerProvider` |
| `internal/beads` (`.Adapter`, `.Ready`, `.List`) | via `cockpit.LedgerProvider` |
| `internal/quota` (`.LoadConfig`, `.Snapshot`) | via `cockpit.LedgerProvider` |

### Adding a new displayed field

1. Add the field to `cockpit.Snapshot` (or a sub-snapshot) in
   `internal/cockpit/snapshot.go`.
2. Populate it in `cockpit.LedgerProvider.Refresh()` (or a helper it calls)
   in `internal/cockpit/ledger_provider.go`.
3. Map it in `snapshotToWire()` into the wire struct `CockpitSnapshot` /
   `CockpitSlot` in `cmd/koryph/cockpit.go`.
4. Add the corresponding TypeScript field to the schema type in
   `ide/vscode/src/data/schema.ts`.
5. Read it in the TypeScript consumer via `CockpitReader.snapshot()`.

Never bypass `cockpit.LedgerProvider` to add a direct file read.

## Modules at a glance

| Module | Purpose |
|---|---|
| `src/data/cockpitReader.ts` | **Primary data layer.** Wraps file-watch trigger + `koryph cockpit --json` call. |
| `src/data/cli.ts` | Shells out to `koryph` / `bd` binaries. Mutations only + cockpit query. |
| `src/data/schema.ts` | TypeScript mirror of Go on-disk and wire types. |
| `src/data/paths.ts` | Resolves `KORYPH_HOME`-relative filesystem paths. |
| `src/data/registryWatcher.ts` | Watches `~/.koryph/registry.d/` for project list changes. |
| `src/data/governorReader.ts` | **Grandfathered** direct read; do not add new reads here. |
| `src/data/ledgerWatcher.ts` | **Grandfathered** direct read; do not add new reads here. |
| `src/data/quotaReader.ts` | **Grandfathered** direct read; do not add new reads here. |
| `src/tree/agentThreadsProvider.ts` | VS Code `TreeDataProvider`; consumes `CockpitReader`. |
| `src/tree/model.ts` | Pure tree-model builder (no VS Code dependency; fully tested). |
| `src/statusbar/` | Quota status-bar items (still use `QuotaReader` + CLI for slow refresh). |
| `src/commands/` | Slot mutation commands (CLI-only; never read state). |
