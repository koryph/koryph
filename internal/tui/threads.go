// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Threads",
		Order: 0,
		New:   func(theme Theme, _ bool) TabModel { return newThreadsModel(theme) },
	})
}

// threadFilter scopes which slots the Threads tab shows.
type threadFilter int

const (
	// threadFilterActive shows only non-terminal slots — the live workers.
	// This is the default: a merged/failed/done slot is finished work retained
	// in the ledger for history and recovery, NOT a running thread, so it must
	// not masquerade as one in the default view.
	threadFilterActive threadFilter = iota
	// threadFilterAll shows every slot, terminal ones included.
	threadFilterAll
	// threadFilterDone shows only terminal slots (merged/done/failed/…).
	threadFilterDone
	threadFilterCount // sentinel — number of filter values
)

// label returns the display label for a thread filter.
func (f threadFilter) label() string {
	switch f {
	case threadFilterActive:
		return "active"
	case threadFilterAll:
		return "all"
	case threadFilterDone:
		return "terminal"
	default:
		return "active"
	}
}

// matches reports whether slot sl passes filter f.
func (f threadFilter) matches(sl cockpit.SlotSnapshot) bool {
	switch f {
	case threadFilterActive:
		return !sl.Terminal
	case threadFilterDone:
		return sl.Terminal
	default: // threadFilterAll
		return true
	}
}

// stallAfter is how old a running slot's status.json heartbeat may be before
// the Threads tab flags the thread as stalled. Agents rewrite status.json at
// every step boundary; 15 minutes of silence on a live thread is the
// structured "needs a look" signal (work assigned + heartbeat stale +
// process alive — never output scraping).
const stallAfter = 15 * time.Minute

// threadsModel is the Bubble Tea model for the Threads tab.
// It renders the live slot table: bead, stage, model, retries, elapsed,
// cost vs estimate, memory, status line — with a state filter, per-thread
// retry and model-escalation detail, and a graceful per-thread stop ('s').
type threadsModel struct {
	table  table.Model
	theme  Theme
	width  int
	height int
	snap   cockpit.Snapshot

	// filter scopes which slots are shown (default: active workers only).
	filter threadFilter

	// visible is the slice of slots currently shown, parallel to the table's
	// rows, so the cursor index maps back to a slot for the Enter→detail jump
	// even when the filter hides some slots.
	visible []cockpit.SlotSnapshot

	// stopTarget is the slot awaiting stop confirmation; nil when no stop is
	// pending. Keyed by value (PhaseID/PID captured at 's' press) so a
	// snapshot refresh between 's' and 'y' cannot retarget the signal.
	stopTarget *cockpit.SlotSnapshot
}

// newThreadsModel creates an empty threads table model.
func newThreadsModel(theme Theme) *threadsModel {
	cols := threadColumns(80) // initial width; updated on WindowSizeMsg
	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(10),
		table.WithStyles(tableStyles(theme)),
	)
	return &threadsModel{
		table:  t,
		theme:  theme,
		width:  80,
		filter: threadFilterActive,
	}
}

// Init implements TabModel.
func (m *threadsModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Threads tab has no text inputs.
func (m *threadsModel) IsCapturingInput() bool { return false }

// Update implements TabModel.
func (m *threadsModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	if km, ok := msg.(tea.KeyMsg); ok {
		// Stop-confirmation modal: y confirms, anything else cancels.
		if m.stopTarget != nil {
			target := *m.stopTarget
			m.stopTarget = nil
			if km.String() == "y" {
				return m, func() tea.Msg {
					return stopRequestMsg{PhaseID: target.PhaseID, PID: target.PID}
				}
			}
			return m, nil
		}

		switch km.String() {
		case "f":
			// Cycle the state filter and rebuild.
			m.filter = (m.filter + 1) % threadFilterCount
			m.rebuild()
			if c := m.table.Cursor(); c >= len(m.visible) {
				m.table.SetCursor(max(0, len(m.visible)-1))
			}
			return m, nil
		case "s":
			// Graceful stop of the selected thread (confirm first). Only
			// meaningful for a live slot with a recorded pid.
			if idx := m.table.Cursor(); idx >= 0 && idx < len(m.visible) {
				sl := m.visible[idx]
				if !sl.Terminal && sl.PID > 0 {
					m.stopTarget = &sl
				}
			}
			return m, nil
		case "enter":
			// Open the detail panel for the selected row's bead.
			if idx := m.table.Cursor(); idx >= 0 && idx < len(m.visible) {
				beadID := m.visible[idx].BeadID
				if beadID != "" {
					return m, func() tea.Msg { return showDetailMsg{beadID: beadID} }
				}
			}
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// View implements TabModel.
func (m *threadsModel) View() string {
	return m.titleBar() + "\n" + m.table.View()
}

// titleBar renders the filter + counts line above the table — or the pending
// stop-confirmation prompt, which needs the operator's full attention.
func (m *threadsModel) titleBar() string {
	if m.stopTarget != nil {
		name := m.stopTarget.Title
		if name == "" || name == m.stopTarget.BeadID {
			name = m.stopTarget.PhaseID
		}
		prompt := fmt.Sprintf("Stop %s (pid %d)? SIGTERM — engine parks the bead.  [y=stop  any other key=cancel]",
			truncate(name, 40), m.stopTarget.PID)
		return lipgloss.NewStyle().Bold(true).Foreground(m.theme.Warning).Render(prompt)
	}

	total := len(m.snap.Slots)
	shown := len(m.visible)
	active, stalled, zombie, failed := 0, 0, 0, 0
	for _, sl := range m.snap.Slots {
		if !sl.Terminal {
			active++
			switch {
			case sl.Zombie:
				// Dead pid outranks a mere stall — it's not "quiet", it's gone.
				zombie++
			case slotStalled(sl):
				stalled++
			}
		} else if sl.Stage == "failed" || sl.Stage == "conflict" || sl.Stage == "blocked" {
			failed++
		}
	}
	title := fmt.Sprintf("Threads  filter:%s  showing %d/%d  (%d active)",
		m.filter.label(), shown, total, active)
	extra := ""
	if zombie > 0 {
		extra += "  " + lipgloss.NewStyle().Foreground(m.theme.Error).
			Render(fmt.Sprintf("☠ %d zombie", zombie))
	}
	if stalled > 0 {
		extra += "  " + lipgloss.NewStyle().Foreground(m.theme.Warning).
			Render(fmt.Sprintf("⚠ %d stalled", stalled))
	}
	if failed > 0 {
		extra += "  " + lipgloss.NewStyle().Foreground(m.theme.Error).
			Render(fmt.Sprintf("✗ %d failed", failed))
	}
	hints := "  [f=filter  enter=detail  s=stop]"
	return lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(title) +
		extra +
		lipgloss.NewStyle().Foreground(m.theme.Gray).Render(hints)
}

// slotStalled reports whether a live slot's step heartbeat has gone quiet:
// running stage, a status file that exists (StatusAge > 0), and no rewrite
// for stallAfter.
func slotStalled(sl cockpit.SlotSnapshot) bool {
	return (sl.Stage == "running" || sl.Stage == "review") &&
		sl.StatusAge > stallAfter
}

// SetSnapshot implements TabModel. It refreshes the table rows from a new snapshot.
func (m *threadsModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
	m.rebuild()
}

// rebuild recomputes the filtered visible slot slice and table rows.
func (m *threadsModel) rebuild() {
	cols := threadColumns(m.width)
	descW := cols[1].Width             // Description column
	statusW := cols[len(cols)-1].Width // Status column
	withMem := m.width >= thMemMinWidth

	m.visible = m.visible[:0]
	rows := make([]table.Row, 0, len(m.snap.Slots))
	for _, sl := range m.snap.Slots {
		if !m.filter.matches(sl) {
			continue
		}
		m.visible = append(m.visible, sl)
		rows = append(rows, slotToRow(sl, m.snap.ProjectID, descW, statusW, withMem))
	}
	m.table.SetColumns(cols)
	m.table.SetRows(rows)
	// SetRows/SetColumns can leave the bubbles table cursor at -1 (e.g. after
	// the first Resize rebuilt against an empty snapshot). Clamp it back to a
	// valid row so the highlight shows and Enter→detail has a selected slot.
	switch {
	case len(rows) == 0:
		// nothing selectable
	case m.table.Cursor() < 0:
		m.table.SetCursor(0)
	case m.table.Cursor() >= len(rows):
		m.table.SetCursor(len(rows) - 1)
	}
}

// Resize implements TabModel. It updates column widths and table height.
func (m *threadsModel) Resize(w, h int) {
	m.width = w
	m.height = h
	// Reserve rows: tab bar (1) + status bar (1) + filter title (1) = 3, plus
	// the table's own header row (1).
	tableH := h - 3 - 1
	if tableH < 1 {
		tableH = 1
	}
	m.table.SetHeight(tableH)
	m.table.SetStyles(tableStyles(m.theme))
	m.rebuild() // status truncation and column widths depend on width
}

// Fixed Threads column widths (Description and Status are computed from the
// leftover). Shared by threadColumns and slotToRow so cell truncation always
// matches the column width and ellipses land cleanly.
const (
	thBeadW    = 14
	thStageW   = 12
	thModelW   = 13
	thRetryW   = 10
	thElapsedW = 8
	thCostW    = 12
	thMemW     = 9

	// thMemMinWidth is the terminal width at which the Mem column appears —
	// narrower terminals keep the description/status space instead.
	thMemMinWidth = 110
)

// threadColumns returns column definitions scaled to terminal width (minimum
// 80). Description carries the bead's short title — the primary way an
// operator recognizes a row (ids are secondary) — so it takes the larger
// share of the leftover; the live Status step gets the rest. Wide terminals
// (≥ thMemMinWidth) add a Mem column (avg/peak RSS of the agent's process
// cohort) so per-thread memory pressure is visible without opening Detail.
func threadColumns(width int) []table.Column {
	if width < 80 {
		width = 80
	}
	withMem := width >= thMemMinWidth
	fixed := thBeadW + thStageW + thModelW + thRetryW + thElapsedW + thCostW
	pad := 16
	if withMem {
		fixed += thMemW
		pad += 2
	}
	remaining := width - fixed - pad
	if remaining < 24 {
		remaining = 24
	}
	// Titles-first: the Description (bead title) is how an operator recognizes
	// a row, so it takes the larger share; the live Status step gets the rest.
	descW := remaining * 3 / 5
	descW = min(max(descW, 10), 48)
	statusW := remaining - descW
	if statusW < 12 {
		statusW = 12
	}
	cols := []table.Column{
		{Title: "Bead", Width: thBeadW},
		{Title: "Description", Width: descW},
		{Title: "Stage", Width: thStageW},
		{Title: "Model", Width: thModelW},
		{Title: "Retries", Width: thRetryW},
		{Title: "Elapsed", Width: thElapsedW},
		{Title: "Cost/Est", Width: thCostW},
	}
	if withMem {
		cols = append(cols, table.Column{Title: "Mem(MB)", Width: thMemW})
	}
	cols = append(cols, table.Column{Title: "Status", Width: statusW})
	return cols
}

// slotToRow converts a cockpit.SlotSnapshot to a table.Row. descW/statusW bound
// the two variable-width cells so long titles and agent-step strings get an
// ellipsis rather than a hard cut. withMem must match the threadColumns
// column set for the same width. projectID lets the narrow Bead column drop
// the redundant "<project>-" prefix instead of cutting into the id's
// distinguishing suffix.
func slotToRow(sl cockpit.SlotSnapshot, projectID string, descW, statusW int, withMem bool) table.Row {
	id := sl.BeadID
	if id == "" {
		id = sl.PhaseID
	}
	bead := truncateBeadID(projectID, id, thBeadW)
	// Description is the bead's short title; when the provider could not resolve
	// a real title it falls back to the id, which we blank here rather than
	// repeat the id already shown in the Bead column.
	desc := sl.Title
	if desc == id {
		desc = ""
	}
	desc = truncate(desc, descW)
	stage := truncate(sl.Stage, thStageW)
	model := truncate(modelWithEscalation(sl), thModelW)
	retries := truncate(retrySummary(sl), thRetryW)
	elapsed := formatElapsed(sl.Elapsed)
	cost := formatCost(sl.CostUSD, sl.EstimateUSD)

	// Status cell: zombie, stall, and death classifications outrank the
	// (stale) last step line — a thread that has died, gone quiet, or failed
	// must SAY so. Zombie outranks stalled: a dead pid is not "quiet", it's
	// gone, and the incident this closes (koryph-k6o) is exactly a "running"
	// row silently masking a dead process for hours.
	status := sl.StatusLine
	if status == "" {
		status = sl.StatusJSON
	}
	switch {
	case sl.Zombie:
		status = fmt.Sprintf("☠ dead pid %d — reconcile: koryph stop then koryph merge", sl.PID)
	case slotStalled(sl):
		status = fmt.Sprintf("⚠ stalled %s · %s", formatElapsed(sl.StatusAge), status)
	case sl.Terminal && sl.DeathReason != "":
		status = fmt.Sprintf("✗ %s", sl.DeathReason)
	}
	status = truncate(status, statusW)

	row := table.Row{bead, desc, stage, model, retries, elapsed, cost}
	if withMem {
		mem := "—"
		if sl.ResourceSamples > 0 {
			mem = fmt.Sprintf("%d/%d", sl.AvgRSSMB, sl.PeakRSSMB)
		}
		row = append(row, truncate(mem, thMemW))
	}
	return append(row, status)
}

// modelWithEscalation renders the short model name, marked with an up-arrow when
// the ledger's model rationale indicates the tier was escalated (e.g. bumped
// after repeated failures). The engine freezes the model across ordinary
// requeues, so the arrow only appears when an escalation was explicitly
// recorded — giving the operator model-escalation visibility without inventing
// data.
func modelWithEscalation(sl cockpit.SlotSnapshot) string {
	name := shortModel(sl.Model)
	if sl.ModelWhy != "" && strings.Contains(strings.ToLower(sl.ModelWhy), "escalat") {
		name += "↑"
	}
	return name
}

// retrySummary renders a compact per-thread retry breakdown: the number of
// re-dispatches plus one-letter cause codes (g=gate, m=merge, c=conflict,
// rl=rate-limit, bk=budget-kill). "—" when the thread is on its first, clean
// attempt. This is the "show retries" surface for the Threads tab.
func retrySummary(sl cockpit.SlotSnapshot) string {
	retries := sl.Attempt - 1
	if retries < 0 {
		retries = 0
	}
	var causes []string
	add := func(n int, code string) {
		if n > 0 {
			causes = append(causes, fmt.Sprintf("%s%d", code, n))
		}
	}
	add(sl.GateRequeues, "g")
	add(sl.MergeRequeues, "m")
	add(sl.ConflictRequeues, "c")
	add(sl.RateLimitRequeues, "rl")
	add(sl.BudgetKillRequeues, "bk")

	if retries == 0 && len(causes) == 0 {
		return "—"
	}
	if len(causes) == 0 {
		return fmt.Sprintf("×%d", retries)
	}
	return fmt.Sprintf("×%d %s", retries, strings.Join(causes, ","))
}

// tableStyles returns lipgloss table styles from the theme.
func tableStyles(theme Theme) table.Styles {
	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(theme.Accent).
		BorderBottom(true).
		Bold(true).
		Foreground(theme.Accent)
	s.Selected = s.Selected.
		Foreground(theme.White).
		Background(theme.Blue).
		Bold(false)
	return s
}

// shortModel returns a display-ready model name truncated to fit the column.
func shortModel(model string) string {
	// Strip common prefixes for display compactness.
	replacements := []struct{ prefix, replacement string }{
		{"claude-opus-4", "opus-4"},
		{"claude-sonnet-4", "sonnet-4"},
		{"claude-sonnet-3-7", "sonnet-3.7"},
		{"claude-haiku-3-5", "haiku-3.5"},
		{"claude-", ""},
	}
	for _, r := range replacements {
		if strings.HasPrefix(model, r.prefix) {
			return r.replacement + model[len(r.prefix):]
		}
	}
	return model
}

// formatElapsed formats a duration for display in the Elapsed column.
func formatElapsed(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, m)
	}
	if m > 0 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// formatCost formats cost vs estimate for display. Returns "—" when both are zero.
func formatCost(cost, estimate float64) string {
	if cost == 0 && estimate == 0 {
		return "—"
	}
	if estimate == 0 {
		return fmt.Sprintf("$%.3f", cost)
	}
	return fmt.Sprintf("$%.2f/$%.2f", cost, estimate)
}

// truncate limits s to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
