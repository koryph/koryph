<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph tui — terminal cockpit

`koryph tui` is the interactive terminal cockpit for watching and lightly
operating a running koryph project. It requires no editor, works over SSH,
and shares the same data layer (`internal/cockpit`) as the VS Code extension
(koryph-ew2).

## Launch

```sh
koryph tui [--project ID] [--read-only]
```

- **`--project`** — show only the named project (default: all registered projects).
- **`--read-only`** — disables write actions (nudge, drain); safe for shared
  or observer sessions.

## Navigation

| Key | Action |
|-----|--------|
| `Tab` / `Shift+Tab` | Cycle tabs |
| `↑`/`k`, `↓`/`j` | Scroll (within a tab) |
| `p` | Cycle to next registered project |
| `r` | Force immediate refresh |
| `?` | Toggle full help overlay |
| `q` / `Ctrl-C` | Quit |

## Tabs

### Threads (tab 0)

Live table of every dispatched slot: bead, stage, model, attempt, elapsed,
cost vs estimate, and last status line from the agent's `status.json`.

### Burndown (tab 1)

Trajectory projections: epic burndown with velocity sparklines and P50/P90
ETAs, backlog drain ETA, cost-to-drain vs quota window, and per-model-tier
duration stats (P50/P90).

### Events (tab 2, koryph-9af.5)

Merged live feed of engine events sourced from ledger slot-state transitions
and the machine-wide audit log (`~/.koryph/audit.jsonl`):

- **dispatch** — a bead was dispatched to a worktree (green).
- **merge** — a bead was merged (green).
- **requeue** — a slot transitioned to a non-running state (amber).
- **drain** — an operator requested a graceful wind-down (red).
- **cap-change** / **resize** — a width override was applied (cyan).

Events are ordered oldest → newest. The feed is bounded to 500 entries.

#### Events tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Scroll the feed |
| `/` | Enter filter mode (substring match on event text) |
| `Enter` / `Esc` | Confirm / cancel filter |
| `n` | Compose a nudge to a live bead's INBOX.md |
| `D` | Request a graceful drain (opens confirmation modal) |

`n` and `D` are disabled in `--read-only` mode.

#### Nudge (`n`)

Prompts for a bead ID then a message. If the bead is currently dispatched
(has a live slot in the active run), the message is appended to its
`INBOX.md` — the same path as `koryph nudge`. If the bead is queued but
not yet dispatched, the TUI shows an instruction to use `koryph nudge`
from the CLI instead (which can reach the `bd` notes path, which the TUI
does not have access to).

#### Drain (`D`)

Shows a confirmation modal. Press `D` again to confirm, `Esc` to cancel.
Writes the drain sentinel for the active project — identical to
`koryph drain --project <id>`. In-flight slots finish normally; no new
dispatch occurs after the sentinel is written.

## Status bar

Bottom line shows: thread count, running threads, governor state, last
error, and timestamp. The `?` / `q` key hints are always visible.

## Minimum terminal size

80 × 24 columns × rows. Smaller terminals show a resize prompt.

## See also

- `koryph nudge` — nudge a bead from the CLI (supports queued beads).
- `koryph drain` — request a graceful wind-down from the CLI.
- [Design document](../designs/2026-07-tui-cockpit.md)
