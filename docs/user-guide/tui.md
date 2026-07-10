<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph tui — terminal cockpit

`koryph tui` is the interactive terminal cockpit for watching and lightly
operating a running koryph project. It requires no editor, works over SSH,
and shares the same data layer (`internal/cockpit`) as the VS Code extension
(koryph-ew2).

## Launch

```sh
koryph tui [--project ID | --all-projects] [--read-only]
```

- **Default (no selection flag)** — shows the project whose repository root
  contains the current directory: run `koryph tui` from anywhere inside a
  checkout you have added to koryph (the repo root or any subdirectory) and it
  opens that project's cockpit. Outside every registered project, the command
  cannot guess which one you mean, so it lists the registered projects and asks
  you to name one with `--project` (or `--all-projects`) rather than opening an
  unrelated cockpit.
- **`--project ID`** — show only the named project, regardless of the current
  directory.
- **`--all-projects`**, **`-a`** — show every registered project (the aggregate
  cockpit; cycle between them with `p`). Mutually exclusive with `--project`.
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

The TUI has five tabs in the bar — Threads, Burndown, Events, Efficiency, Queue
— plus the Detail overlay, which is reached by selecting a row rather than by
cycling. Use `Tab` / `Shift+Tab` to move between the visible tabs. Each tab
receives snapshot data from the same `internal/cockpit` provider so all tabs
show consistent data from the same refresh.

---

### Threads (tab 0)

Live table of dispatched slots. The Bead column is a narrow id column paired
with a compact Description (the bead's short title); the leftover width is split
so the Status column — the live agent step — still gets the bulk of it:

| Column | Description |
|--------|-------------|
| Bead | The bead ID occupying this slot |
| Description | The bead's short title, so a row is legible without opening Detail (blank when the title can't be resolved — e.g. a markdown phase, or before the first queue refresh) |
| Stage | `dispatching`, `running`, `review`, `merge-pending`, `merged`, `failed`, … |
| Model | The Claude model tier in use; a trailing `↑` marks a slot whose model rationale records an escalation |
| Retries | Re-dispatch count with cause codes — `×N g/m/c/rl/bk` (gate, merge, conflict, rate-limit, budget-kill); `—` on a clean first attempt |
| Elapsed | Wall time since dispatch (final wall time once terminal) |
| Cost/Est | Actual spend vs the estimator's pre-dispatch estimate |
| Status | Last line from the agent's `status.json` |

The title line above the table shows the active filter and counts:
`filter:active  showing 3/7  (3 active)`.

Press `Enter` on any slot row to open the **Detail** panel for that bead
(including its per-bead resource usage). Press `Esc` or `Backspace` to return.

#### Filtering, and why a merged thread is not "active"

A slot stays in the run ledger after it finishes — merged/failed/done slots are
retained for history and crash recovery, not deleted. They are **finished work,
not live workers**, so the Threads tab hides them by default rather than listing
them as active threads. Cycle the filter with `f`:

| Filter | Shows |
|--------|-------|
| `active` (default) | Non-terminal slots — the live workers (running, dispatching, review, stuck) |
| `all` | Every slot, terminal ones included |
| `terminal` | Only finished slots (merged, done, failed, conflict, blocked, …) |

#### Threads tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Move selection up/down |
| `f` | Cycle the state filter (`active` → `all` → `terminal`) |
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
their child beads are nested below, drawn with `├─ / └─ / │` tree connectors so
the grouping is unambiguous. The State and ID columns stay aligned at every
depth; the hierarchy lives in the Title column. Each row shows the bead's true
dispatch state as computed by the scheduler.

**Closed parents stay grouped.** Over a multi-day run an epic often closes while
a few of its children are still open. `bd list` omits closed issues, so those
children would otherwise orphan to the top level and the queue would read as a
flat list. The Queue tab reconstructs a container for such a parent (fetching
its title via `bd show`) so its open children remain nested under it.

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

### Detail (overlay — opened from a row, not a tab)

Detail is an **overlay, not a tab**: it is deliberately absent from the tab bar
and skipped by `Tab`/`Shift+Tab` cycling, because it can only show something
once you have selected a bead. It opens when you press `Enter` on a row in the
**Threads** or **Queue** tab, and `Esc`/`Backspace` returns you to the tab you
came from. It shows a full bead detail snapshot fetched asynchronously from the
active provider, including:

- Bead metadata (ID, type, status, priority, labels, parent, deps).
- Description and notes.
- Live slot information (branch, worktree, model, cost vs estimate, log path).
- **Resources** — per-bead clock times and process-cohort usage (see below).
- Attempt history with per-attempt requeue cause.

Within Detail, `↑`/`↓` navigate dependency rows, `Enter` jumps into a dep,
`Backspace` pops the navigation stack, and `t` tails the agent log.

#### Resources section (per-bead process metrics)

For any slot that has been sampled, Detail renders measured resource usage so
you can calibrate orchestration against what a bead actually consumed:

| Field | Meaning |
|-------|---------|
| Started / Finished | Dispatch and terminal wall-clock instants, with **date and time** (a wave can span days) |
| Wall | Wall-clock duration (finish − start, or now − start while live) |
| Memory | Average and peak resident memory (MB) across the agent process cohort |
| CPU | Cumulative CPU seconds and utilization (`100%` = one core saturated for the whole window; a multi-core-bound bead reads above 100%) |
| Disk I/O | Bytes read/written — Linux only; shown as `n/a on this platform` on macOS |

Metrics cover the whole agent process cohort (the `Setsid` session: the shell,
the model CLI, and tool subprocesses), sampled on the engine poll tick. See
[the design doc](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-process-metrics.md)
for the sampler architecture and the eBPF/kernel-hook accuracy roadmap.

---

## Status bar

The bottom line of the TUI shows:

- **threads N** — total slot count in the snapshot.
- **running N** — count of slots in `running` or `dispatching` stage.
- **gov N/N** — first governor pool's `leases/dynamic` cap.
- **⚠ message** — last error (e.g. failed refresh, rejected nudge).
- **✓ message** — last successful action (e.g. `nudged koryph-9af.6`).
- **?** / **q** key hints — always visible.
- **Mon DD HH:MM:SS** — timestamp of the last snapshot, carrying the date as
  well as the time so a cockpit left running across several days is never
  ambiguous. The Events feed likewise date-stamps each entry.

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
- [Design document](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-tui-cockpit.md)
