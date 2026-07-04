// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// detail.go implements the Detail tab model for the koryph TUI cockpit
// (koryph-9af.3). It renders the full bead detail panel including metadata,
// dependency graph, attempt history, and log-tail shortcut.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Detail",
		Order: 2,
		New:   func(theme Theme) TabModel { return newDetailModel(theme) },
	})
}

// detailRow is a navigable row in the detail panel (e.g. a dep link).
type detailRow struct {
	label  string
	value  string
	zoneID string
}

// detailModel is the Bubble Tea model for the Detail tab.
type detailModel struct {
	theme  Theme
	width  int
	height int

	// beadID is the currently focused bead. Empty means nothing is selected.
	beadID string

	// detail is the last fetched detail snapshot.
	detail cockpit.BeadDetailSnapshot

	// rows holds navigable rows (dep links, etc.) for keyboard/mouse selection.
	rows   []detailRow
	cursor int

	// zonePrefix is unique to this model instance to avoid zone ID collisions.
	zonePrefix string
}

// newDetailModel creates an empty detail model.
func newDetailModel(theme Theme) *detailModel {
	// Initialize the global zone manager if not already done.
	// bubblezone's package-level functions require NewGlobal() to have been
	// called first; calling it multiple times is a no-op.
	zone.NewGlobal()
	return &detailModel{
		theme:      theme,
		width:      80,
		zonePrefix: zone.NewPrefix(),
	}
}

// Init implements TabModel.
func (m *detailModel) Init() tea.Cmd { return nil }

// SetBead sets the focused bead ID and clears the stale detail.
func (m *detailModel) SetBead(beadID string) {
	m.beadID = beadID
	m.detail = cockpit.BeadDetailSnapshot{}
	m.rows = nil
	m.cursor = 0
}

// SetDetail stores a freshly-assembled detail snapshot.
func (m *detailModel) SetDetail(d cockpit.BeadDetailSnapshot) {
	m.detail = d
	m.rebuildRows()
}

// SetSnapshot implements TabModel. Refreshes the detail if a new snapshot
// carries an updated detail for our focused bead.
func (m *detailModel) SetSnapshot(snap cockpit.Snapshot) {
	if snap.Detail.BeadID != "" && snap.Detail.BeadID == m.beadID {
		m.detail = snap.Detail
		m.rebuildRows()
	}
}

// Resize implements TabModel.
func (m *detailModel) Resize(w, h int) {
	m.width = w
	m.height = h
}

// rebuildRows rebuilds the navigable rows from the current detail snapshot.
func (m *detailModel) rebuildRows() {
	m.rows = nil
	for i, dep := range m.detail.Deps {
		m.rows = append(m.rows, detailRow{
			label:  "dep",
			value:  dep,
			zoneID: zone.Mark(fmt.Sprintf("%sdep-%d", m.zonePrefix, i), dep),
		})
	}
	for i, rdep := range m.detail.ReverseDeps {
		m.rows = append(m.rows, detailRow{
			label:  "rdep",
			value:  rdep,
			zoneID: zone.Mark(fmt.Sprintf("%srdep-%d", m.zonePrefix, i), rdep),
		})
	}
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
}

// Update implements TabModel.
func (m *detailModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyUp:
			if m.cursor > 0 {
				m.cursor--
			}
		case tea.KeyDown:
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}
		case tea.KeyEscape:
			// Esc: clear focus and return to Threads tab (emit a tab-switch).
			m.beadID = ""
			m.detail = cockpit.BeadDetailSnapshot{}
			m.rows = nil
			m.cursor = 0
		}

	case tea.MouseMsg:
		// Check if any zone was clicked.
		for i, row := range m.rows {
			zi := zone.Get(row.zoneID)
			if zi != nil && zi.InBounds(msg) {
				m.cursor = i
			}
		}
	}
	return m, nil
}

// View implements TabModel.
func (m *detailModel) View() string {
	if m.beadID == "" {
		return lipgloss.NewStyle().
			Foreground(m.theme.Gray).
			Padding(1, 2).
			Render("No bead selected. Press Enter on a thread to view details.")
	}

	d := m.detail
	if d.BeadID == "" {
		return lipgloss.NewStyle().
			Foreground(m.theme.Gray).
			Padding(1, 2).
			Render(fmt.Sprintf("Loading detail for %s…", m.beadID))
	}

	var b strings.Builder
	w := m.width
	if w < 40 {
		w = 40
	}

	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Width(14)
	valueStyle := lipgloss.NewStyle().Foreground(m.theme.White)
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Cyan).MarginTop(1)
	dimStyle := lipgloss.NewStyle().Foreground(m.theme.Gray)
	selectedStyle := lipgloss.NewStyle().Foreground(m.theme.White).Background(m.theme.Blue).Bold(true)
	blockerStyle := lipgloss.NewStyle().Foreground(m.theme.Error)

	// --- Title / header -------------------------------------------------------
	statusColor := m.theme.StatusColor(d.Status)
	statusBadge := lipgloss.NewStyle().Foreground(statusColor).Bold(true).Render(d.Status)
	titleLine := lipgloss.NewStyle().Bold(true).Foreground(m.theme.White).Width(w - 2).
		Render(fmt.Sprintf("[%s] %s  %s", d.BeadID, truncate(d.Title, 60), statusBadge))
	b.WriteString(titleLine + "\n")

	// --- Meta fields ----------------------------------------------------------
	b.WriteString(labelStyle.Render("Type:") + " " + valueStyle.Render(d.IssueType) + "\n")
	b.WriteString(labelStyle.Render("Priority:") + " " + valueStyle.Render(fmt.Sprintf("%d", d.Priority)) + "\n")
	if d.ParentID != "" {
		b.WriteString(labelStyle.Render("Parent:") + " " + valueStyle.Render(d.ParentID) + "\n")
	}
	if len(d.Labels) > 0 {
		b.WriteString(labelStyle.Render("Labels:") + " " + dimStyle.Render(strings.Join(d.Labels, "  ")) + "\n")
	}
	if d.Branch != "" {
		b.WriteString(labelStyle.Render("Branch:") + " " + valueStyle.Render(d.Branch) + "\n")
	}
	if d.Worktree != "" {
		b.WriteString(labelStyle.Render("Worktree:") + " " + dimStyle.Render(truncate(d.Worktree, 50)) + "\n")
	}
	if d.CostUSD > 0 || d.EstimateUSD > 0 {
		b.WriteString(labelStyle.Render("Cost/Est:") + " " + valueStyle.Render(formatDetailCost(d.CostUSD, d.EstimateUSD)) + "\n")
	}
	if d.LogPath != "" {
		b.WriteString(labelStyle.Render("Log:") + " " + dimStyle.Render(truncate(d.LogPath, 60)) + "\n")
	}

	// --- Description ----------------------------------------------------------
	if d.Description != "" {
		b.WriteString(sectionStyle.Render("Description") + "\n")
		for _, line := range wrapText(d.Description, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Acceptance criteria --------------------------------------------------
	if d.Acceptance != "" {
		b.WriteString(sectionStyle.Render("Acceptance") + "\n")
		for _, line := range wrapText(d.Acceptance, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Notes ----------------------------------------------------------------
	if d.Notes != "" {
		b.WriteString(sectionStyle.Render("Notes") + "\n")
		for _, line := range wrapText(d.Notes, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Dependencies (navigable) ---------------------------------------------
	depRowBase := 0
	if len(d.Deps) > 0 {
		b.WriteString(sectionStyle.Render("Depends on") + "\n")
		for i, dep := range d.Deps {
			rid := fmt.Sprintf("%sdep-%d", m.zonePrefix, i)
			rowStr := fmt.Sprintf("  ← %s", dep)
			var rendered string
			if depRowBase+i == m.cursor {
				rendered = zone.Mark(rid, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(rid, blockerStyle.Render(rowStr))
			}
			b.WriteString(rendered + "\n")
		}
		depRowBase += len(d.Deps)
	}

	// --- Reverse deps (navigable) ---------------------------------------------
	if len(d.ReverseDeps) > 0 {
		b.WriteString(sectionStyle.Render("Blocked by this") + "\n")
		for i, rdep := range d.ReverseDeps {
			rid := fmt.Sprintf("%srdep-%d", m.zonePrefix, i)
			rowStr := fmt.Sprintf("  → %s", rdep)
			var rendered string
			if depRowBase+i == m.cursor {
				rendered = zone.Mark(rid, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(rid, dimStyle.Render(rowStr))
			}
			b.WriteString(rendered + "\n")
		}
	}

	// --- Attempt history ------------------------------------------------------
	if len(d.AttemptHistory) > 0 {
		b.WriteString(sectionStyle.Render("Attempt history") + "\n")
		for _, rec := range d.AttemptHistory {
			cause := ""
			if rec.RequeueCause != "" {
				cause = "  requeue:" + rec.RequeueCause
			}
			line := fmt.Sprintf("  #%d  %-14s  %-12s  cost $%.3f  %s%s",
				rec.Attempt,
				rec.Status,
				truncate(rec.Model, 12),
				rec.CostUSD,
				formatElapsed(rec.Elapsed),
				cause,
			)
			b.WriteString(dimStyle.Render(line) + "\n")
		}
	}

	// --- Footer hint ----------------------------------------------------------
	b.WriteString("\n" + dimStyle.Render("↑/↓ navigate deps  Esc back  t tail log") + "\n")

	return zone.Scan(b.String())
}

// wrapText wraps s at maxWidth characters, splitting on spaces.
func wrapText(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		maxWidth = 60
	}
	var lines []string
	for _, para := range strings.Split(s, "\n") {
		if len([]rune(para)) <= maxWidth {
			lines = append(lines, para)
			continue
		}
		words := strings.Fields(para)
		cur := ""
		for _, w := range words {
			if cur == "" {
				cur = w
			} else if len([]rune(cur))+1+len([]rune(w)) <= maxWidth {
				cur += " " + w
			} else {
				lines = append(lines, cur)
				cur = w
			}
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	return lines
}

// formatDetailCost formats cost vs estimate for the detail panel.
func formatDetailCost(cost, estimate float64) string {
	if cost == 0 && estimate == 0 {
		return "—"
	}
	if estimate == 0 {
		return fmt.Sprintf("$%.4f", cost)
	}
	return fmt.Sprintf("$%.4f / $%.4f", cost, estimate)
}

// formatDetailTime is a thin wrapper kept for future use.
var _ = formatDetailTime

func formatDetailTime(s string) string { return s }
