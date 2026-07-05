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

// threadsModel is the Bubble Tea model for the Threads tab.
// It renders the live slot table: bead, stage, model, attempt, elapsed,
// cost vs estimate, status line.
type threadsModel struct {
	table  table.Model
	theme  Theme
	width  int
	height int
	snap   cockpit.Snapshot
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
		table: t,
		theme: theme,
		width: 80,
	}
}

// Init implements TabModel.
func (m *threadsModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Threads tab has no text inputs.
func (m *threadsModel) IsCapturingInput() bool { return false }

// Update implements TabModel.
func (m *threadsModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	var cmd tea.Cmd
	// Intercept Enter to open the detail panel for the selected row's bead.
	if km, ok := msg.(tea.KeyMsg); ok && km.Type == tea.KeyEnter {
		if idx := m.table.Cursor(); idx >= 0 && idx < len(m.snap.Slots) {
			beadID := m.snap.Slots[idx].BeadID
			if beadID != "" {
				return m, func() tea.Msg { return showDetailMsg{beadID: beadID} }
			}
		}
	}
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// View implements TabModel.
func (m *threadsModel) View() string {
	return m.table.View()
}

// SetSnapshot implements TabModel. It refreshes the table rows from a new snapshot.
func (m *threadsModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
	rows := make([]table.Row, 0, len(snap.Slots))
	for _, sl := range snap.Slots {
		rows = append(rows, slotToRow(sl))
	}
	m.table.SetRows(rows)
}

// Resize implements TabModel. It updates column widths and table height.
func (m *threadsModel) Resize(w, h int) {
	m.width = w
	m.height = h
	// Reserve rows for header (1) + tab bar (1) + status bar (1) = 3.
	tableH := h - 3 - 1 // -1 for the table header row itself
	if tableH < 1 {
		tableH = 1
	}
	m.table.SetColumns(threadColumns(w))
	m.table.SetHeight(tableH)
	m.table.SetStyles(tableStyles(m.theme))
}

// threadColumns returns column definitions scaled to terminal width.
// Minimum terminal width is 80 columns.
func threadColumns(width int) []table.Column {
	if width < 80 {
		width = 80
	}
	// Fixed-width columns: Stage(12) Model(16) Attempt(7) Elapsed(9) Cost(14)
	// Remaining goes to Bead title; StatusLine gets the leftover.
	fixed := 12 + 16 + 7 + 9 + 14
	remaining := width - fixed
	beadW := remaining * 2 / 5
	if beadW < 12 {
		beadW = 12
	}
	statusW := remaining - beadW
	if statusW < 8 {
		statusW = 8
	}
	return []table.Column{
		{Title: "Bead", Width: beadW},
		{Title: "Stage", Width: 12},
		{Title: "Model", Width: 16},
		{Title: "Try", Width: 7},
		{Title: "Elapsed", Width: 9},
		{Title: "Cost/Est", Width: 14},
		{Title: "Status", Width: statusW},
	}
}

// slotToRow converts a cockpit.SlotSnapshot to a table.Row.
func slotToRow(sl cockpit.SlotSnapshot) table.Row {
	bead := truncate(sl.Title, 30)
	if bead == "" {
		bead = truncate(sl.PhaseID, 30)
	}
	stage := truncate(sl.Stage, 12)
	model := truncate(shortModel(sl.Model), 16)
	attempt := fmt.Sprintf("%d", sl.Attempt)
	elapsed := formatElapsed(sl.Elapsed)
	cost := formatCost(sl.CostUSD, sl.EstimateUSD)
	status := truncate(sl.StatusLine, 40)
	if status == "" {
		status = truncate(sl.StatusJSON, 40)
	}
	return table.Row{bead, stage, model, attempt, elapsed, cost, status}
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
	// Show cost / estimate with a simple ratio indicator.
	pct := cost / estimate * 100
	_ = pct
	return fmt.Sprintf("$%.2f/$%.2f", cost, estimate)
}

// truncate limits s to maxLen runes, appending "…" if truncated.
func truncate(s string, maxLen int) string {
	runes := []rune(s)
	if len(runes) <= maxLen {
		return s
	}
	return string(runes[:maxLen-1]) + "…"
}
