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

Live table of dispatched slots. The Bead column is a narrow id column; the
Description column (the bead's short title) takes the larger flexible share —
operators recognize rows by title, not id — and the live Status step gets the
rest. Terminals ≥ 110 columns also get a Mem column:

| Column | Description |
|--------|-------------|
| Bead | The bead ID occupying this slot |
| Description | The bead's short title, so a row is legible without opening Detail (blank when the title can't be resolved — e.g. a markdown phase, or before the first queue refresh) |
| Stage | `dispatching`, `running`, `review`, `merge-pending`, `merged`, `failed`, … |
| Model | The Claude model tier in use; a trailing `↑` marks a slot whose model rationale records an escalation |
| Retries | Re-dispatch count with cause codes — `×N g/m/c/rl/bk` (gate, merge, conflict, rate-limit, budget-kill); `—` on a clean first attempt |
| Elapsed | Wall time since dispatch (final wall time once terminal) |
| Cost/Est | Actual spend vs the estimator's pre-dispatch estimate |
| Mem(MB) | Average/peak resident memory of the agent's process cohort (wide terminals only) |
| Status | Last line from the agent's `status.json`; `☠ dead pid <pid> — reconcile: koryph stop then koryph merge` when a slot the ledger still marks `running` has lost its agent process (a zombie — see below); `⚠ stalled <age>` when a running agent's step heartbeat has been silent for 15+ minutes; `✗ <death reason>` on a classified failure |

The title line above the table shows the active filter and counts —
`filter:active  showing 3/7  (3 active)` — plus `☠ N zombie`, `⚠ N stalled`,
and `✗ N failed` tallies whenever those need attention. Stall detection is
structured (work assigned + `status.json` mtime + liveness), never output
scraping.

#### Zombie slots: running status, dead process

A slot the ledger marks `running` has its recorded agent pid probed with a
read-only, best-effort liveness check (the same signal-0 probe `koryph
board`'s LIVE column uses). Normally the engine's exit handling reclassifies a
dead agent within a tick, but a lost signal can leave a slot marked `running`
for hours with nothing alive behind it — the exact incident this check exists
to catch (2026-07-10: an operator trusted a stale "running" row because
nothing correlated it against the process's actual liveness). The check is
scoped to the `running` stage on purpose: `review`, `stuck`, and
`dispatching` slots legitimately have no live *agent* process while the engine
drives the post-build stages (review, rebase, gate, merge), so they are never
flagged — only a slot claiming an agent is actively working while its process
is gone counts. A zombie slot outranks a stalled one in both the Status cell
and the title-bar tally — a dead process is not "quiet", it's gone. Reconcile
with `koryph stop` then `koryph merge` if the slot doesn't clear on its own;
the engine's health patrol also surfaces the same condition as a
`dead-active-agent` event in the Events feed. The probe never mutates the slot
— if it can't run (unreadable /proc, permissions), the slot renders exactly as
it did before this check existed.

Press `Enter` on any slot row to open the **Detail** panel for that bead
(including its per-bead resource usage). Press `Esc` or `Backspace` to return.

#### Stopping a thread (`s`)

Press `s` on a live slot to gracefully stop it: the TUI records the
operator-stop sentinel first (so the engine's death classification **parks**
the bead instead of auto-retrying it into a race with your hand-work), then
sends SIGTERM to the agent's process group — exactly `koryph stop <phase>`.
A confirmation prompt replaces the title line; `y` confirms, any other key
cancels. Never SIGKILL: uncommitted worktree work survives. Disabled in
`--read-only` mode. To re-dispatch a parked bead later, clear its parked
state from the CLI.

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
| `s` | Gracefully stop the selected thread (confirm with `y`) |
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
| `requeue` | amber | A slot came back for another attempt |
| `fail` | red | A running slot died (failed/conflict/blocked) — includes the engine's death classification and any block note, so an escalation watcher can decide whether a higher-tier model should take over |
| `patrol` | amber | A warn-level health-patrol finding (stuck claim, stale worktree, …); `(auto-fixed)` when the engine repaired it itself |
| `drain` | red | An operator requested a graceful wind-down |
| `nudge` | gray | An operator nudged a bead (from the TUI or `koryph nudge`) |
| `cap-change` / `resize` | cyan | A concurrency-cap override was applied |

Event messages lead with the bead's **title** (`<title> [id]`) so the feed is
readable at a glance. Events are ordered oldest → newest; the feed is bounded
to 500 entries. To watch for escalation candidates, filter with `/fail`.

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
is what it is, which footprint tokens are most contended, whether the quota
estimator is tracking reality, and where the tokens are going. Sections
top-to-bottom:

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

One labeled block per **AI provider/runtime** with a configured quota
ceiling — different dispatched threads can run under different runtimes
(each bead may carry a `runtime:<name>` label), and each runtime is billed
against its own provider's rate limits with its own measurement source. Only
one block ("claude") exists today, since claude is the only runtime koryph
ships an adapter for; a future runtime's quota reader adds its own block here
with no further changes to this tab.

Each block shows two burn bars — 5-hour window and weekly window — as
`[filled bar] $spent/$ceiling (pct%)`. Live spend comes from a background
transcript scan of the account's Claude sessions (refreshed every minute, no
subprocess), marked `spend ≈ transcript scan`; the same two fractions appear
compactly in the status bar as `claude 5h N% wk M%`. When no transcripts are
readable the bar renders empty with a hint to run `koryph quota usage`; when
the block's quota source is `uncalibrated`, it shows a hint to run
`koryph quota calibrate` instead of bars.

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

#### 6. Token Economy

The "how many tokens am I consuming, on which model, and is caching working"
section:

- **Cache-hit ratios** — `cache_read / (fresh + cache_read + cache_creation)`
  over the **last 24 h** (the actionable number) and all-time. A tripwire
  warning appears when the recent share collapses below 80 % — check
  prompt-prefix hygiene.
- **Tokens/bead trend** — sparkline of mean tokens per bead per day.
- **Per-model rollup** — token classes and accumulated cost per *serving*
  model (the model that actually answered, so fallback downgrades are
  attributed correctly). This is the table that drives "do I need to change
  models or serialize to stay inside the 5 h / weekly allowances".
- **Recent beads** — per-bead token composition, most recent dispatches
  first, labeled by bead **title**.

#### Efficiency tab keys

The Efficiency tab has no interactive keys beyond global navigation in v1.

---

### Queue (tab 4)

Hierarchical view of the project's work queue. Epics appear at the top level;
their child beads are nested below, drawn with `├─ / └─ / │` tree connectors so
the grouping is unambiguous. Columns are `State | Title | P | Reason | ID` —
the title leads (that is how you recognize work), priority is its own column,
and the bead id trails in gray for cross-referencing with `koryph nudge`/
`koryph stop`. Blocked rows name their blockers by **title** ("needs: fix the
gateway; add retry budget +1 more"), and a footprint deferral names the
in-flight bead it waits on. Each row shows the bead's true dispatch state as
computed by the scheduler.

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
| `dep-blocked` | red | Has one or more open dependencies (named by title in Reason) |
| `fp-deferred` | amber | Ready but footprint conflicts with a running bead (named in Reason) |
| `human` | purple | Carries a `no-dispatch` / human-only label |
| `deferred` | yellow | Carries a `deferred-until:<date>` label |
| `parked` | gray | Parked label or status |
| `waiting` | gray | Open with no visible open deps, but withheld by `bd ready` — an `in_progress` claim held elsewhere (possibly stale), sync lag, or an eligibility filter. These beads will **not** dispatch; older builds mislabeled them `ready` |
| `epic` | bold | Container node (epic, feature, decision) — not directly dispatchable |

#### Freshness

Queue data is assembled by a background `bd` scan (~15 s cold), so the tab
shows *"queue refreshing…"* briefly on startup. If the data stops refreshing —
`bd` slow or contended, or a refresh pass timed out — the title bar appends
`(data Ns old)` so a frozen tree is never mistaken for live state; the next
refresh clears it automatically.

#### Grouping modes

`m` cycles three arrangements, shown in the title bar as `mode:<name>`:

| Mode | Arrangement |
|------|-------------|
| `epics` (default) | Hierarchy — epics with their children nested beneath, siblings ordered by priority |
| `priority` | Every bead in one flat list sorted by priority then id — "what dispatches next" |
| `issues` | The flat priority list minus container rows (epics/features) — only directly workable issues |

In `epics` mode, `Space` folds/unfolds the selected epic and `F` toggles
between collapse-all (epic level only) and expand-all. Folding keys are
inert in the flat modes.

#### Metadata search

`/` opens a search prompt. Terms are whitespace-separated and **all** must
match (AND); matching is case-insensitive:

| Term | Matches |
|------|---------|
| `label:<substr>` | any label containing the substring (e.g. `label:area:engine`) |
| `type:<t>` | issue type equals (`task`, `bug`, `epic`, …) |
| `state:<s>` | queue state contains (`ready`, `dep-blocked`, `running`, …) |
| `p:<n>` | priority equals (`p:1` = P1) |
| anything else | bead id or title contains the text |

`Enter` applies (an empty query clears), `Esc` cancels and keeps the previous
query. The active query renders in the title. In the epic tree, an epic stays
visible while any of its descendants match; the search composes with the `f`
state filter.

#### Queue tab keys

| Key | Action |
|-----|--------|
| `↑`/`k`, `↓`/`j` | Move selection up/down |
| `g` | Jump to top |
| `G` | Jump to bottom |
| `m` | Cycle grouping mode (`epics` → `priority` → `issues`) |
| `/` | Open the metadata search prompt |
| `Space` | Fold/unfold the selected epic (`epics` mode) |
| `F` | Toggle collapse-all / expand-all (`epics` mode) |
| `f` | Cycle state filter (`all` → `running` → `ready` → `blocked` → `deferred`) |
| `Enter` | Open inline bead detail panel |
| `Esc` / `Backspace` / `q` | Close bead detail panel |

The state filter cycles through five modes:

| Filter | Shows |
|--------|-------|
| `all` | Everything |
| `running` | Running beads only |
| `ready` | Running + ready |
| `blocked` | `dep-blocked` + `fp-deferred` + `res-deferred` |
| `deferred` | `fp-deferred` + `res-deferred` + `deferred-until` + `human` + `parked` + `waiting` |

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
- Description and notes — the body is a **scrollable viewport**, so long
  descriptions and plans are fully readable.
- Live slot information (branch, worktree, model, cost vs estimate, log path,
  death classification, and any ledger note on failure).
- **Resources** — per-bead clock times and process-cohort usage (see below).
- Attempt history with per-attempt requeue cause.

Within Detail, `j`/`k` (or `↑`/`↓`) scroll the body line-by-line, `Ctrl-D`/
`Ctrl-U` scroll half-pages, `g`/`G` jump to top/bottom, `n`/`N` cycle focus
through the dependency rows (the view follows the focused row), `Enter` jumps
into the focused dep, `Backspace` pops the navigation stack, and `t` tails
the raw agent log.

`T` (capital) opens the **activity tail**: the agent's live `stream.jsonl`
parsed into its train of thought — **thinking**, **tool calls** (with the tool
name and its key argument), and **assistant messages** — following as the agent
works. Filter with the number keys shown in the footer:

| Key | Shows |
|-----|-------|
| `0` (or `a`) | Everything, interleaved in stream order |
| `1` | Thinking only |
| `2` | Tool calls only |
| `3` | Assistant messages only |

Each footer entry carries a live count, so `2:tools(18)` tells you how many tool
calls are in the current window. When the agent hands work to a nested subagent
the divider `── subagent …<id> ──` marks whose activity you are reading
(`── main agent ──` on return).

By default the tail reads the **last 512 KB** of the stream — enough for a live
window and cheap to re-read many times a second. Press `h` to switch to **full
history** (`Activity [full history]` in the header): the whole run from the first
event, so you can scroll all the way back to the agent's opening thoughts. Full
history is parsed **incrementally** — the stream is read once and only the newly
appended bytes are parsed on each refresh — so it stays cheap even on a large,
still-growing stream. Press `h` again to return to the bounded tail window. The
footer's `h full`/`h tail` hint names the mode the key switches to.

The tail **follows live** and keeps following even when nothing else on screen
changes — the reasoning stream advances many times a second while the slot's
status heartbeat is quiet, and the tail tracks the stream, not the heartbeat.
Scroll up (`↑`/`↓`, `PgUp`/`PgDn`) and follow **auto-pauses** (the header shows
`[paused]`) so you can read back through earlier activity without being yanked to
the bottom; scroll back to the bottom, or press `f`, to resume (`[follow]`).
`T`/`Esc` returns to the detail panel. If the visible window holds no entries of
the selected kind yet, the pane says so rather than going blank.

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

The bottom line of the TUI is one de-duplicated fleet summary:

- **here N** — agents running/dispatching in **this project only**, counted
  from its own slots.
- **fleet G/C** — running agents **across every project** sharing the same
  account, over the permitted concurrency cap; read deterministically from
  the governor's primary pool. This is deliberately a separate number from
  `here`: the governor's leases and cap are machine-global — every koryph
  project dispatching under the same account draws from the same pool — so a
  single combined `agents R/C` reading (an earlier build's design) silently
  compared this project's own count against a fleet-wide cap, which
  understated what the number actually meant. (That earlier build also
  showed separate `threads`/`running`/`gov` readouts that duplicated each
  other, and picked the gov pool via random map iteration — which is why it
  appeared to flicker between values.) `fleet` is omitted when no governor
  pool data is available.
- **ready N  blocked N** — queue pulse: beads that could dispatch now vs
  beads waiting on deps/footprints/resources.
- **✗ N failed** — terminal failed/conflict/blocked slots needing attention
  (red; hidden at zero).
- **`<runtime>` 5h N% wk M%** — one segment per AI provider with a
  configured quota ceiling, each showing BOTH the 5-hour and weekly window
  burn (green/amber/red/gray by the worse of the two; gray means a ceiling is
  set but nothing is currently measurable). Different dispatched threads may
  run under different runtimes, each billed against its own provider's rate
  limits — so this is a list of segments, not a single hardcoded pair of
  numbers, even though only `claude` exists to populate one today. Providers
  with no ceiling configured at all are omitted (see the Efficiency tab's
  Quota Windows section for the full "run koryph quota calibrate" hint).
- **⚠ message** — last error (e.g. failed refresh, rejected nudge).
- **✓ message** — last successful action (e.g. `nudged koryph-9af.6`).
- **?** / **q** key hints — always visible.
- **Mon DD HH:MM:SS** — timestamp of the last snapshot, carrying the date as
  well as the time so a cockpit left running across several days is never
  ambiguous. The Events feed likewise date-stamps each entry.

The governor read is **observation-only**: the TUI never prunes lease files
or advances the AIMD probe clock (earlier builds did both on every poll,
making the monitor a writer in the engine's control loop).

## Bead ID display

Bead IDs follow `<project>-<suffix>` (e.g. `koryph-9af.6`). Wherever a bead
ID has to fit a narrow column or line — the Threads tab's Bead column, the
Queue tab's trailing ID column, and the Detail panel's navigation
breadcrumb — the cockpit drops the redundant project prefix and shows just
the suffix (`9af.6`) rather than character-truncating the full id from the
right, which used to keep the prefix and cut the digits that actually
distinguish one bead from another (`koryph-9a…`). The full id is always
shown when there is room for it; the suffix-only form only kicks in once it
would not otherwise fit.

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
