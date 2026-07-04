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

## 5. Open questions (decide during T1)

- Naming: `koryph tui` vs `koryph watch` (lean `tui`; `board`/`roster`
  stay as one-shots).
- Bubble Tea v2 vs v1 pinning at implementation time.
- Whether the Events tab persists scrollback across view switches
  (likely yes, bounded ring).

## 6. Sequencing (the epic's children)

T1 foundation (`internal/cockpit` view-model + app shell + threads view
MVP) → T2 queue tree ∥ T3 bead detail (both consume the graph provider)
→ T4 efficiency dashboard (needs 6bl/jr8 signals, both landed) → T5
events feed + nudge/drain actions → T6 polish + docs (user-guide chapter,
ide-integration.md cross-link, concepts pointers) + teatest coverage.
ew2 (VS Code) adopts `internal/cockpit` in its next bead — constraint
noted on that epic.
