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

// threadsModel is the Bubble Tea model for the Threads tab.
// It renders the live slot table: bead, stage, model, retries, elapsed,
// cost vs estimate, status line — with a state filter and per-thread retry and
// model-escalation detail.
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
		switch km.String() {
		case "f":
			// Cycle the state filter and rebuild.
			m.filter = (m.filter + 1) % threadFilterCount
			m.rebuild()
			if c := m.table.Cursor(); c >= len(m.visible) {
				m.table.SetCursor(maxInt(0, len(m.visible)-1))
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

// titleBar renders the filter + counts line above the table.
func (m *threadsModel) titleBar() string {
	total := len(m.snap.Slots)
	shown := len(m.visible)
	active := 0
	for _, sl := range m.snap.Slots {
		if !sl.Terminal {
			active++
		}
	}
	title := fmt.Sprintf("Threads  filter:%s  showing %d/%d  (%d active)  [f=filter  enter=detail]",
		m.filter.label(), shown, total, active)
	return lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(title)
}

// SetSnapshot implements TabModel. It refreshes the table rows from a new snapshot.
func (m *threadsModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
	m.rebuild()
}

// rebuild recomputes the filtered visible slot slice and table rows.
func (m *threadsModel) rebuild() {
	cols := threadColumns(m.width)
	statusW := cols[len(cols)-1].Width

	m.visible = m.visible[:0]
	rows := make([]table.Row, 0, len(m.snap.Slots))
	for _, sl := range m.snap.Slots {
		if !m.filter.matches(sl) {
			continue
		}
		m.visible = append(m.visible, sl)
		rows = append(rows, slotToRow(sl, statusW))
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

// threadColumns returns column definitions scaled to terminal width.
// Minimum terminal width is 80 columns. The Bead column is deliberately narrow
// (it shows the bead id, not the title) so the Status column — the live agent
// step — gets the bulk of the width.
func threadColumns(width int) []table.Column {
	if width < 80 {
		width = 80
	}
	// Fixed-width columns.
	beadW, stageW, modelW, retryW, elapsedW, costW := 15, 13, 14, 11, 8, 13
	fixed := beadW + stageW + modelW + retryW + elapsedW + costW
	// Remaining width (minus inter-column padding) goes to the status line.
	statusW := width - fixed - 8
	if statusW < 12 {
		statusW = 12
	}
	return []table.Column{
		{Title: "Bead", Width: beadW},
		{Title: "Stage", Width: stageW},
		{Title: "Model", Width: modelW},
		{Title: "Retries", Width: retryW},
		{Title: "Elapsed", Width: elapsedW},
		{Title: "Cost/Est", Width: costW},
		{Title: "Status", Width: statusW},
	}
}

// slotToRow converts a cockpit.SlotSnapshot to a table.Row. statusW bounds the
// status cell so long agent step strings get an ellipsis rather than a hard cut.
func slotToRow(sl cockpit.SlotSnapshot, statusW int) table.Row {
	id := sl.BeadID
	if id == "" {
		id = sl.PhaseID
	}
	bead := truncate(id, 15)
	stage := truncate(sl.Stage, 13)
	model := truncate(modelWithEscalation(sl), 14)
	retries := truncate(retrySummary(sl), 11)
	elapsed := formatElapsed(sl.Elapsed)
	cost := formatCost(sl.CostUSD, sl.EstimateUSD)
	status := sl.StatusLine
	if status == "" {
		status = sl.StatusJSON
	}
	status = truncate(status, statusW)
	return table.Row{bead, stage, model, retries, elapsed, cost, status}
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

// maxInt returns the larger of a and b.
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
