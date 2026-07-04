<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# koryph tui: the terminal cockpit (2026-07-04)

Status: approved for implementation; epic + children filed from §6.
Origin: operator direction — a TUI to watch while building software with
koryph: executing threads, the hierarchical queue, navigable bead detail,
loop efficiency and calibration, and whatever else an operator should see.

## 1. Positioning and framework

`koryph tui` is the **terminal sibling of the VS Code cockpit (koryph-ew2)**
— same information, no editor required, works over SSH. The two surfaces
MUST share a view-model layer (`internal/cockpit`: typed snapshots +
subscriptions over slots, queue, governor, quota, estimator, events) so
neither reimplements data access; ew2's extension migrates onto it as it
grows.

**Framework: Bubble Tea** (charmbracelet) + bubbles (table/viewport/help)
+ lipgloss (styling) — the modern canonical Go TUI stack: Elm-style
update loop, first-class keyboard handling, mouse support (click targets
via bubblezone) for the "clickable" requirement while staying
keyboard-first (`hjkl`/arrows/enter everywhere; the mouse is an
enhancement, never a requirement). tview considered and rejected: solid
but widget-era; the ecosystem, testability (teatest), and momentum are
Bubble Tea's.

**Data sources** — all already exist; the TUI adds no new state:
run ledger + slot files (threads), `bd` graph (queue/detail), deferral
reasons (engine events), `governor.json`, quota snapshots, estimator
bias/MAPE (koryph-6bl), health-patrol findings (koryph-gus), and the obs
telemetry JSONL (koryph-jr8) as the live event feed — the TUI is the
first first-class *consumer* of the observability epic. Refresh: file
watch on telemetry/ledger + low-frequency poll fallback; target <150 ms
perceived latency on updates, zero writes in monitor mode.

## 2. Views (tabs)

1. **Threads** — live slot table: bead, stage
   (`building/review/rebase/gate/merge`), model+persona, attempt,
   elapsed, cost-so-far vs estimate (±confidence once known), last
   status-line from the agent's status.json, pid. Requeues/parks flash
   inline. This is `koryph roster` made alive.
2. **Queue** — the hierarchical epic → children tree, every bead
   annotated with its true state: `running / ready / dep-blocked (on X) /
   footprint-deferred (conflict with Y) / human / deferred-until /
   merged / parked`. The "why isn't this running?" answer is ON the row —
   the deferral reason the engine already computes, surfaced instead of
   grepped.
3. **Bead detail** (enter on any bead, anywhere) — description,
   acceptance, labels/footprint, notes; **navigable dependency graph**
   (deps and reverse-deps as focusable rows — enter jumps to that bead,
   backspace returns; blockers highlighted); attempt history with requeue
   causes; estimate vs actual cost; worktree/branch; `t` jumps to a log
   tail of the running agent.
4. **Efficiency** — the self-hosting-case-study dashboard, live:
   dispatched-per-refill sparkline, achieved vs permitted concurrency,
   top deferral tokens (the coupling measurement), governor pool state
   (cap, probe/settle/breaker), quota window burn bars, estimator
   calibration table (bucket / n / bias / MAPE / corrected?).
5. **Events** — the merged live feed (dispatches, merges with SHAs,
   requeues with causes, review verdicts, patrol findings, cap changes),
   filterable by bead/component/level.

Global: project switcher (koryph is multi-project — `p` cycles,
header shows current), help overlay (`?`), colorblind-safe default theme,
degrades to 80×24.

## 3. Actions: monitor-first, two safe verbs

v1 is a **monitor** — but two safe, reversible actions earn their keys
because reaching for another terminal mid-incident is the worst moment:
`n` nudge (compose a message to a bead's INBOX/notes) and `D` drain
(stop dispatching, finish in-flight; confirmation modal). Nothing
destructive: no kill, no merge, no bead mutation from the TUI in v1 —
those stay deliberate CLI acts. Every action funnels through the same
CLI code paths (no parallel implementations).

## 4. Non-goals (v1)

- Not a web UI; not remote multi-machine aggregation.
- No bead editing/creation (planning stays in skills/CLI).
- No log-viewer ambitions beyond tail-with-follow (use `koryph obs tail`
  / real tooling for deep analysis).
- No config editing.

## 5. Decisions (resolved during T1)

- **Naming**: `koryph tui` (confirmed; `board`/`roster` stay as one-shots).
- **Bubble Tea version**: pinned to **v1** series (`bubbletea v1.3.10`,
  `lipgloss v1.1.0`, `bubbles v1.0.0`) — v2 is still in preview and the
  v1 API is stable and well-tested. Upgrade to v2 when it reaches GA.
  The `teatest` harness is from `charmbracelet/x/exp/teatest`
  (v0.0.0-20260629+). Pinned in `go.mod`.
- **Refresh model**: 100 ms poll via `tea.Tick` (no file-watch in T1;
  below the 150 ms perceived-latency floor). File-watch can be layered
  in T5/T6 as an optimization.
- **Events-tab scrollback**: bounded ring, persists across tab switches
  (deferred to T5 — not implemented in T1).

## 5b. T1 scope summary

T1 shipped: `internal/cockpit` (Snapshot + Provider interface +
LedgerProvider over ledger + govern), `internal/tui` (Bubble Tea app
shell: header, tab bar, Threads tab, help overlay, project switcher,
80×24 minimum-size guard, 100 ms poll refresh), `cmd/koryph/tui.go`
command, teatest harness (4 tests). Docs/reference auto-generated.

## 5c. Burndown tab scope (koryph-9af.7)

Shipped: `internal/cockpit/burndown.go` — `BurndownSnapshot` types
(`EpicBurndown`, `BacklogBurndown`, `CostBurndown`, `DurationStat`,
`BurndownFit`) + `computeBurndown` computation engine reading ledger
history + beads adapter; `LedgerProvider` extended with burndown cache
(5 s TTL, no ccusage subprocess); `internal/tui/burndown.go` — Burndown
tab Bubble Tea model with four sections; `TabBurndown` added as T2 in
the tab bar. All projections surface P50/P90 from observed variance;
sparse states render "insufficient history (n=N)". Duration stats are
per-model-tier wall-time from DispatchedAt→MergedAt.

Follow-ups filed: live quota window (requires ccusage background refresh);
exact critical-path via full dep-graph traversal (needs `bd list --all`
with dep links); persistent duration-stat accumulator alongside 6bl's
cost stats.

## 6. Sequencing (the epic's children)

T1 foundation (`internal/cockpit` view-model + app shell + threads view
MVP) → T2 queue tree ∥ T3 bead detail (both consume the graph provider)
→ T4 efficiency dashboard (needs 6bl/jr8 signals, both landed) → T5
events feed + nudge/drain actions → T6 polish + docs (user-guide chapter,
ide-integration.md cross-link, concepts pointers) + teatest coverage.
ew2 (VS Code) adopts `internal/cockpit` in its next bead — constraint
noted on that epic.
