<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph VS Code extension

The koryph VS Code extension brings agent-wave visibility and steering into
the editor: live agent threads, transcript panels for reading the line of
thought, quota burn in the status bar, and worktree navigation — without
leaving VS Code.

Source lives at `ide/vscode/` (TypeScript, esbuild bundle, zero runtime deps
beyond the VS Code API). The extension is not yet published to the marketplace;
see [Build and side-load](#build-and-side-load) below. It installs as a `.vsix`
in under a minute.

The extension shares the same `internal/cockpit` data layer as the terminal
cockpit (`koryph tui`). Both surfaces show the same numbers from the same
source. Pick whichever fits the context — see the
[cockpit comparison table](../ide-integration.md#4-terminal-cockpit-vs-vs-code-cockpit).

---

## What the extension shows

### Agent threads tree view

The Activity Bar gains a **Koryph** container with one tree: projects →
active run → slots, ordered by most-recently-updated.

```text
▸ koryph            run 20260703-091422  running · wave 3 · 2/4 slots
    ● koryph-i2n   running   opus   $0.42   feat/koryph-i2n-completions
    ◐ koryph-fr3.1 review    sonnet $0.18   feat/fr3.1-keepassxc
    ✓ koryph-5ov   merged    sonnet $0.11
▸ ncp_roadmap       (no active run)
▹ Other projects (3 hidden)
```

Each slot row shows:

| Column | Description |
|--------|-------------|
| Status glyph | `●` running · `◐` reviewing · `✓` merged · `✗` failed · `⊘` blocked · `…` queued |
| Bead ID | The bead in this slot; tooltip shows the full title |
| Stage | dispatching / running / reviewing / merge-pending / merged / pr-opened / failed / conflict / blocked |
| Model tier | haiku / sonnet / opus / fable |
| Cost | Completed: `$N.NN`; running: `~streaming` |
| Branch | Git branch name |

Hovering a slot shows: persona, account profile, verified identity, attempt
number, and the agent's self-reported `status.json` step/percentage (labeled
as agent-authored and possibly stale — it is not a guarantee).

**Tree view badge** — count of live agents across visible projects, derived
from governor lease files (cheaper and more truthful than a full ledger scan).

**Project pinning** — projects whose registry `root` matches a workspace
folder are pinned and expanded. All others collapse under "Other projects",
controlled by the `koryph.showAllProjects` setting.

---

### Transcript panels

Click **Open transcript** on any slot to open a live `stream.jsonl` panel:

- Assistant text flows as deltas arrive; tool calls collapse to expandable
  single-line chips; the final `result` event renders a cost/duration footer.
- Header strip: bead, status, model, attempts, worktree shortcut,
  Stop and Nudge buttons — a complete cockpit for one agent.
- Tabs within the panel: **Transcript**, **stderr.log**, **session.log**.
- **Follow mode** toggle for auto-scroll; **pause** to freeze the view.
- Running spend estimate summed from stream usage fields (approximate;
  the ledger's authoritative `cost_usd` appears at completion).

Multiple panels can be open side-by-side. Each retains context when hidden.

---

### Quota status bar

One item per account that owns a visible project with an active run:

```
⚡ personal 62% 5h · 41% wk
```

Governor-level coloring:

| Level | Threshold | Color |
|-------|-----------|-------|
| ok | < 80 % | default (no background) |
| warn | ≥ 80 % | yellow (`statusBarItem.warningBackground`) |
| drain | ≥ 90 % | red (`statusBarItem.errorBackground`) |
| stop | ≥ 95 % | red (`statusBarItem.errorBackground`, same as drain) |

> **Note:** the status bar's thresholds (80 % / 90 % / 95 %, hard-coded in
> `ide/vscode/src/data/schema.ts`) are a fixed four-level band. They are
> **not** read from the per-account governor ladder described in [Billing and
> quota](billing-and-quota.md#the-governor-ladder), which defaults to
> 90 % / 94 % / 97 % / 99 % with a distinct `throttle` level and is
> configurable per account. The two can disagree — e.g. an account the
> extension colors `warn` (≥ 80 %) may still read `ok` from `koryph quota`
> (< 90 %). Treat the status bar as a coarse at-a-glance indicator and trust
> `koryph quota` / the TUI for the authoritative level.

Click the item for a full quota snapshot and a **Calibrate…** hint (points
at the `/koryph-calibrate` skill).

Quota data refreshes every `koryph.quotaRefreshMinutes` (default 5) by
running `koryph quota show --json` asynchronously. Between snapshots the
extension reads cached ceiling data from `~/.koryph/quota/<account>.json`
and shows the age of the data. The UI never blocks on the quota check.

---

## Commands

All commands are available from the Command Palette (`⌘⇧P` / `Ctrl+Shift+P`)
under the **Koryph** prefix and from right-click context menus on slot rows
in the tree view.

### Slot commands

| Command | Description |
|---------|-------------|
| **Koryph: Stop (graceful)** | Sends SIGTERM to the slot's process group. A confirmation dialog notes any uncommitted work. |
| **Koryph: Stop (force)** | Force-kills the slot. Uses a destructive-styled confirmation. |
| **Koryph: Stop whole run** | Stops all live slots for the project at once. |
| **Koryph: Nudge…** | Prompts for a message and appends it to the slot's `INBOX.md`; also adds a bd comment. |
| **Koryph: Change model…** | Quick-pick: haiku / sonnet / opus (fable if allow-listed). Updates the bead's `model:<tier>` label via `bd label`, then offers **Stop + requeue now** (engine requeues and re-resolves model at next dispatch) or **Apply next dispatch** (the running slot is unaffected). |
| **Koryph: Open transcript** | Opens a webview transcript panel for the slot. |
| **Koryph: Tail in terminal** | Opens an integrated terminal running `koryph tail --project <id> <phase> --follow` — zero-parse fallback. |
| **Koryph: Open worktree** | Quick-pick: **new window** / **add to workspace** / **reveal in Finder**. Path from `ledger.Slot.worktree`. |
| **Koryph: Show diff vs base** | `base_commit…branch` diff via the Git extension API; falls back to a `git diff` terminal. |
| **Koryph: Open PR** | Opens the PR URL for slots in `pr-opened` state. |
| **Koryph: Merge / Land** | Runs `koryph merge` / `koryph land` in an integrated terminal (interactive output visible). |
| **Koryph: Show bead** | Shows `bd show <id>` in an Output channel. |

### Project commands

| Command | Description |
|---------|-------------|
| **Koryph: Edit Project Config** | Opens `koryph.project.json` with JSON Schema validation and per-field hover docs. A persistent editor banner reads: *"Applies on next `koryph run` — the running engine loaded config at run start."* Registry-record fields (account, billing guard, models) should be changed via `koryph project` CLI, not by hand-editing the registry JSON. |

---

## Settings

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `koryph.showAllProjects` | boolean | † | Show all registered projects in the tree view, not just those matching the current workspace folders. |
| `koryph.quotaRefreshMinutes` | number | `5` | How often (in minutes) to refresh the quota status bar by running `koryph quota show --json`. The underlying check can take up to 40 s — keep this ≥ 5. |

† `showAllProjects` defaults to `true` when none of the current workspace
folders correspond to a registered koryph project (so the extension is still
useful in a window opened for unrelated work), and `false` otherwise.

---

## Per-account multi-instance behavior

The extension is **account-agnostic by construction**. It reads files and
shells out to `koryph`; it never reads or writes `CLAUDE_CONFIG_DIR` and
never launches `claude`. The consequences:

- The **same extension build** works correctly in both your personal and work
  VS Code instances (separate `--user-data-dir` accounts). There is no separate
  extension per account to install.
- Every dispatch-adjacent action goes through `koryph`, which rebuilds the
  agent environment from the registry record and fails closed on identity
  mismatch — the window's ambient environment never reaches a dispatched agent.
- **Project pinning is the only per-window difference.** A work-account window
  opened on project-A pins project-A and collapses others, while a personal
  window opened elsewhere pins its matched projects — but every project's data
  remains readable from any window via "Other projects".

This is an intentional consequence of the architecture (Decision 6 in the
[design document](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-vscode-extension.md)): the extension
can never dispatch, so it can never dispatch on the wrong account.

For a full explanation of the account isolation model see
[IDE integration § 3](../ide-integration.md#3-how-plugin-issued-commands-interoperate-with-koryph-accounts).

---

## Build and side-load

The extension is not yet published to the VS Code Marketplace. Install from
source with `vsce`:

```sh
# One-time prerequisite: vsce (VS Code Extension CLI)
npm install -g @vscode/vsce

# Build the .vsix package
cd path/to/koryph
make ext-build        # runs esbuild bundle + vsce package
                      # outputs ide/vscode/dist/koryph-<version>.vsix
```

Then install:

```sh
# From the command line:
code --install-extension ide/vscode/dist/koryph-*.vsix

# Or from the VS Code UI:
# Extensions (⌘⇧X) → ⋯ → Install from VSIX… → select the .vsix file
```

The `ext-build` and `ext-test` Makefile targets are optional — they no-op
with a notice when `node` is absent, so `make gate` stays green on Go-only
machines.

### Running the extension's tests

```sh
make ext-test
# or: cd ide/vscode && npm test
```

Tests are fixture-driven, using real ledger and stream samples from
`ide/vscode/src/test/fixtures/`. They exercise the data-layer parsers that
read `ledger.json`, `stream.jsonl`, and governor lease files.

---

## Architecture note

The extension is a **file-watching client** — no daemon, no socket, no event
bus in koryph core. It watches the files koryph already writes:

| File | Purpose |
|------|---------|
| `~/.koryph/registry.d/*.json` | Project registry (roots, accounts, billing) |
| `~/.koryph/slots/*` | Governor lease files (live agent count, pool state) |
| `~/.koryph/quota/<account>.json` | Cached quota ceilings |
| `<repo>/.plan-logs/koryph/latest/ledger.json` | Run ledger (slot state, cost, worktree, base commit) |
| `<phase-dir>/stream.jsonl` | Agent transcript stream |
| `<phase-dir>/status.json` | Agent-reported step and percentage |
| `<phase-dir>/INBOX.md` | Nudge target |

`fs.watch` with a polling fallback; `updated_at` fields are the change
signal. Every mutation goes through the `koryph` CLI, which owns locking,
audit logging (`~/.koryph/audit.jsonl`), and account verification. The
extension never writes koryph state files directly.

---

## See also

- [Terminal cockpit (`koryph tui`)](tui.md) — full six-tab view, works over SSH
  and headless; no Node.js dependency.
- [IDE integration](../ide-integration.md) — account isolation, CLI usage
  from the editor, and cockpit comparison.
- [Billing & quota](billing-and-quota.md) — calibrating the quota governor.
- [Design document](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-vscode-extension.md) — architecture,
  decision log, and bead decomposition for epic koryph-ew2.
