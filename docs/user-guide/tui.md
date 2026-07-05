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
| `Tab` / `Shift+Tab` | Cycle tabs left/right |
| `↑`/`k`, `↓`/`j` | Scroll within a tab |
| `p` | Cycle to next registered project |
| `r` | Force immediate refresh |
| `?` | Toggle full help overlay |
| `q` / `Ctrl-C` | Quit |

## Read-only mode

Pass `--read-only` to disable all write actions. In this mode the nudge (`n`)
and drain (`D`) keys are silently rejected — the TUI shows a status-bar warning
and takes no action. Every other key and display function works normally.
Read-only mode is recommended for shared terminals, CI dashboards, and
observer sessions where you want visibility without the ability to accidentally
drain a running wave.

## Tabs

The TUI has six tabs. Use `Tab` / `Shift+Tab` to move between them, or the
number keys shown in the tab bar. Each tab receives snapshot data from the
same `internal/cockpit` provider so all tabs show consistent data from the
same refresh.

---

### Threads (tab 0)

Live table of every dispatched slot. One row per active or recently-finished
slot, showing:

| Column | Description |
|--------|-------------|
| Bead | The bead ID currently occupying this slot |
| Stage | `dispatching`, `running`, `reviewing`, `merging`, `done`, `failed` |
| Model | The Claude model tier in use |
| Attempt | Attempt number (retries increment this) |
| Elapsed | Wall time since the slot was claimed |
| Cost | Actual spend vs the estimator's pre-dispatch estimate |
| Status | Last line from the agent's `status.json` |

Press `Enter` on any slot row to open the **Detail** panel for that bead.
Press `Esc` or `Backspace` to return to Threads.

#### Threads tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Move selection up/down |
| `Enter` | Open Detail panel for selected bead |

---

### Burndown (tab 1)

Trajectory projections for the entire project backlog. The Burndown tab is
divided into four sections rendered top-to-bottom.

#### 1. Epic Burndown

A per-epic table. Each row shows:

- **Epic ID and title** — truncated to fit the terminal width.
- **Sparkline** — a fixed-width block-character chart of per-day completions
  over the recent history window (`cockpit.SparklineLen` days). Bars scale to
  the epic's own maximum — tall bars mean busy days, not absolute counts.
- **Velocity** — completions per day averaged over the sparkline window. Zero
  or very small values render as `—` (insufficient throughput to extrapolate).
- **P50 ETA** — median completion date under the observed velocity. Computed
  as `remaining / velocity`; shown as a date (e.g. `2026-07-12`) or
  `insufficient history (n=N)` when fewer than the minimum sample count of
  completed beads is available for the epic.
- **P90 ETA** — 90th-percentile completion date under the P90 duration model.
  Always ≥ P50. Also shows `insufficient history (n=N)` in sparse-data states.

Use `j`/`k` to scroll the epic table when there are more epics than fit on
screen.

**Uncertainty ranges**: The P50/P90 spread is computed from the observed
variance in completion times. High variance (long-tail epics, retries,
breaker trips) produces a wide P50→P90 range. When the spread exceeds
14 days the ETA range is colour-coded amber; >30 days is red. When there are
fewer than 3 completed beads for an epic, both ETAs show
`insufficient history (n=N)` and the sparkline is drawn from available data
only (it is not suppressed).

#### 2. Backlog Burndown

Whole-graph drain projection, showing:

- **Total open** — count of all open beads in the project graph.
- **Drain ETA** — estimated date when the backlog reaches zero under the
  current achieved concurrency and average bead velocity.
- **Critical path** — lower-bound on wall-clock drain time set by the longest
  sequential dependency chain that cannot be parallelised. Shown as a duration
  (e.g. `≥ 3d 4h`) when the dependency graph is available.
- **Achieved parallelism** — actual average concurrent slots over the observed
  window, vs the permitted concurrency cap from the governor.

#### 3. Cost Burndown

Cost-to-drain projection vs the remaining quota window. Shows:

- **Projected cost to drain** — estimated total spend to complete all open
  beads, derived from the estimator's per-bead cost model (corrected by
  live bias data from the Efficiency tab's Estimator Calibration section).
- **Quota remaining** — how much of the current 5-hour window (and weekly
  window) is left.
- **Fits-in-window indicator** — a green / amber / red badge:
  - **green** — projected cost fits comfortably within the remaining window.
  - **amber** — projected cost exceeds 80 % of the remaining window; may
    spill into the next window.
  - **red** — projected cost exceeds the remaining window; spill is likely.

#### 4. Duration Stats

Per-model-tier wall-time distribution over completed beads in the ledger:

| Column | Description |
|--------|-------------|
| Tier | Model tier name (e.g. `sonnet`, `haiku`, `opus`) |
| N | Number of completed beads used for this estimate |
| P50 | Median wall-time (50th percentile) |
| P90 | 90th-percentile wall-time |

Rows with fewer than 3 samples show `insufficient history (n=N)` for both
percentiles. The P50/P90 spread is informational — a wide gap indicates
high variance, often caused by retries or breaker trips in the sample window.

#### Burndown tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Scroll the epic table |

---

### Events (tab 2)

Merged live feed of engine events sourced from ledger slot-state transitions
and the machine-wide audit log (`~/.koryph/audit.jsonl`):

| Event type | Colour | Meaning |
|---|---|---|
| `dispatch` | green | A bead was dispatched to a worktree |
| `merge` | green | A bead's worktree was merged and closed |
| `requeue` | amber | A slot transitioned to a non-running state |
| `drain` | red | An operator requested a graceful wind-down |
| `cap-change` / `resize` | cyan | A concurrency-cap override was applied |

Events are ordered oldest → newest. The feed is bounded to 500 entries.

#### Events tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Scroll the feed |
| `/` | Enter filter mode (substring match on event text) |
| `Enter` / `Esc` | Confirm / cancel filter |
| `n` | Compose a nudge to a live bead's `INBOX.md` |
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

---

### Efficiency (tab 3)

The "self-hosting case study rendered live": shows exactly why concurrency
is what it is, which footprint tokens are most contended, and whether the
quota estimator is tracking reality. Five sections top-to-bottom:

#### 1. Dispatch Rate

- **Sparkline** — per-day dispatch count over `cockpit.SparklineLen` days.
  Bar height scales to the busiest day in the window.
- **Total / today** — aggregate and today's dispatch count for the sparkline
  window.
- **Concurrency gauge** — `achieved / permitted (%)` as a filled bar.
  - ≥ 90 % → green (at-cap, good utilisation).
  - ≥ 50 % → accent colour.
  - > 0 % → amber (underutilised).
  - 0 % → gray (idle).

#### 2. Top Deferral Tokens

Write-lock tokens most frequently held by active slots — a direct measurement
of serial bottlenecks. Rows with ≥ 3 slots locked are colour-coded amber
(high contention). Rows with ≥ 2 are accent-coloured.

#### 3. Governor Pools

One line per provider pool. Fields per line:

| Field | Meaning |
|-------|---------|
| Provider | Pool name (e.g. `claude.ai`, `api`) |
| `cap` | Static cap from config |
| `dyn` | AIMD-adjusted dynamic cap |
| `leases` | Active leases held right now |
| `AIMD` | Shown in accent when adaptive mode is active |
| `settling(Ns)` | Amber badge — AIMD settle period, N seconds remaining |
| `breaker=X` | Breaker state: `closed` (green), `half-open` (amber), `open` (red) |
| `probe:…` | Currently probing project/bead pair |

#### 4. Quota Windows

Two burn bars — 5-hour window and weekly window — each showing:
`[filled bar] $spent/$ceiling (pct%)`. When live spend data is unavailable,
the bar renders as empty with a hint to run `koryph quota usage`.
When the quota source is `uncalibrated`, the section shows a hint to run
`koryph quota calibrate`.

Bar colour escalates: green → yellow (≥ 80 %) → amber (≥ 90 %) → red (≥ 95 %).

#### 5. Estimator Calibration

Per `(tier:size)` bucket table showing whether the pre-dispatch cost estimator
is tracking observed reality:

| Column | Meaning |
|--------|---------|
| Bucket | `tier:size` identifier (e.g. `sonnet:medium`) |
| N | Completed beads in this bucket |
| Bias | Observed/estimated ratio (1.0 = perfect; green if within ±0.2, amber ±0.5, red outside) |
| MAPE% | Mean absolute percentage error |
| Corrected | Bias-corrected per-bead cost estimate |
| Base | Raw (uncorrected) per-bead estimate |

Rows with N = 0 show `—` for all computed fields.

#### Efficiency tab keys

The Efficiency tab has no interactive keys beyond global navigation in v1.

---

### Queue (tab 4)

Hierarchical view of the project's work queue. Epics appear at the top level;
their child beads are nested below. Each row shows the bead's true dispatch
state as computed by the scheduler.

#### Dispatch states

| State | Colour | Meaning |
|-------|--------|---------|
| `running` | green | A slot is actively working this bead |
| `ready` | white | Dep-unblocked, no footprint conflict — will be dispatched next wave |
| `dep-blocked` | red | Has one or more open dependencies |
| `fp-deferred` | amber | Ready but footprint conflicts with a running bead |
| `human` | purple | Carries a `no-dispatch` / human-only label |
| `deferred` | yellow | Carries a `deferred-until:<date>` label |
| `parked` | gray | Parked label or status |
| `epic` | bold | Container node (epic, feature, decision) — not directly dispatchable |

#### Queue tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Move selection up/down |
| `g` | Jump to top |
| `G` | Jump to bottom |
| `Space` | Toggle expand/collapse for the selected epic |
| `F` | Expand all epics |
| `f` | Cycle state filter (`all` → `running` → `ready` → `blocked` → `deferred`) |
| `Enter` | Open inline bead detail panel |
| `Esc` / `Backspace` / `q` | Close bead detail panel |

The state filter cycles through five modes:

| Filter | Shows |
|--------|-------|
| `all` | Everything |
| `running` | Running beads only |
| `ready` | Running + ready |
| `blocked` | `dep-blocked` + `fp-deferred` |
| `deferred` | `fp-deferred` + `deferred-until` + `human` + `parked` |

#### Inline detail panel

Press `Enter` on any row to open an inline detail panel showing the bead's
title, ID, type, status, priority, footprint labels, parent, dependency count,
description, notes, and children summary. Press `j`/`k` to scroll; press
`Esc`, `Backspace`, or `q` to close.

---

### Detail (tab 99 — opened from Threads)

The Detail tab is not reachable from the tab bar — it opens automatically when
you press `Enter` on a row in the Threads tab. It shows a full bead detail
snapshot fetched asynchronously from the active provider, including:

- Bead metadata (ID, type, status, priority, labels, parent, deps).
- Description and notes.
- Live slot information (stage, model, attempt, elapsed, cost).
- Recent events for this bead.

Press `Esc` or `Backspace` to return to the Threads tab.

---

## Status bar

The bottom line of the TUI shows:

- **threads N** — total slot count in the snapshot.
- **running N** — count of slots in `running` or `dispatching` stage.
- **gov N/N** — first governor pool's `leases/dynamic` cap.
- **⚠ message** — last error (e.g. failed refresh, rejected nudge).
- **✓ message** — last successful action (e.g. `nudged koryph-9af.6`).
- **?** / **q** key hints — always visible.
- **HH:MM:SS** — timestamp of the last snapshot.

## Minimum terminal size

80 × 24 columns × rows. Smaller terminals show a resize prompt and wait for a
`WindowSizeMsg` before rendering content.

## Concepts

| Concept | Where defined |
|---------|--------------|
| Snapshot data model | `internal/cockpit` package |
| Footprint tokens and wave scheduling | `internal/sched/footprint.go`, `internal/sched/wave.go` |
| Governor pools (AIMD, breaker, probe) | `docs/designs/2026-07-quota-governor.md` |
| Estimator calibration (bias/MAPE) | bead koryph-6bl |
| Nudge INBOX.md path | bead koryph-o72 |

## See also

- `koryph nudge` — nudge a bead from the CLI (supports queued beads).
- `koryph drain` — request a graceful wind-down from the CLI.
- [IDE cockpit vs terminal cockpit](../ide-integration.md#4-terminal-cockpit-vs-vs-code-cockpit)
- [Design document](../designs/2026-07-tui-cockpit.md)
