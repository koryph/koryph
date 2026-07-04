// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// burndown.go implements the Burndown tab model for the koryph TUI cockpit
// (koryph-9af.7). It renders four sections:
//
//  1. Epic Burndown — per-epic table with sparkline, velocity, and P50/P90 ETA.
//  2. Backlog Burndown — whole-graph drain ETA with observed parallelism and
//     critical-path lower bound.
//  3. Cost Burndown — projected cost-to-drain vs remaining quota window, with
//     green/amber/red fits-in-window indicator.
//  4. Duration Stats — per-model-tier wall-time P50/P90 from ledger history.
//
// All projections render a P50/P90 range. Sparse-data states render
// "insufficient history (n=N)" instead of extrapolating from noise.
package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

// burndownModel is the Bubble Tea model for the Burndown tab.
type burndownModel struct {
	theme  Theme
	width  int
	height int
	snap   cockpit.Snapshot

	// epicScroll is the vertical scroll offset for the epic table.
	epicScroll int
}

// newBurndownModel creates an empty burndown model.
func newBurndownModel(theme Theme) burndownModel {
	return burndownModel{
		theme:  theme,
		width:  80,
		height: 24,
	}
}

// Init implements tea.Model for burndownModel.
func (m burndownModel) Init() tea.Cmd { return nil }

// Update implements tea.Model for burndownModel.
func (m burndownModel) Update(msg tea.Msg) (burndownModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "j", "down":
			m.epicScroll++
		case "k", "up":
			if m.epicScroll > 0 {
				m.epicScroll--
			}
		}
	}
	return m, nil
}

// View implements tea.Model for burndownModel.
func (m burndownModel) View() string {
	bd := m.snap.Burndown

	var b strings.Builder
	avail := m.height - 2 // reserve status bar rows
	if avail < 6 {
		avail = 6
	}

	// Divide the available height roughly equally among the four sections.
	// Epic section gets a bit more room for the table.
	epicH := avail / 3
	if epicH < 4 {
		epicH = 4
	}
	statsH := avail - epicH - 4 // 4 lines for backlog+cost summary
	if statsH < 3 {
		statsH = 3
	}

	b.WriteString(m.renderEpicSection(bd, epicH))
	b.WriteRune('\n')
	b.WriteString(m.renderBacklogSection(bd))
	b.WriteRune('\n')
	b.WriteString(m.renderCostSection(bd))
	b.WriteRune('\n')
	b.WriteString(m.renderDurationSection(bd, statsH))

	return b.String()
}

// setSnapshot refreshes the model from a new snapshot.
func (m *burndownModel) setSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
}

// resize updates the model dimensions.
func (m *burndownModel) resize(w, h int) {
	m.width = w
	m.height = h
}

// --- section renderers ------------------------------------------------------

// renderEpicSection renders the Epic Burndown section.
func (m burndownModel) renderEpicSection(bd cockpit.BurndownSnapshot, maxRows int) string {
	title := m.sectionTitle("Epic Burndown")
	if len(bd.Epics) == 0 && bd.AllEpicsSummary.Total == 0 {
		return title + "\n" + m.dimText("  no epic data yet")
	}

	// Column widths.
	// Epic(variable) | Remaining | Velocity  | Sparkline | ETA P50/P90
	w := m.width
	etaW := 32
	velW := 10
	remW := 10
	spkW := cockpit.SparklineLen + 2
	epicW := w - etaW - velW - remW - spkW - 4
	if epicW < 8 {
		epicW = 8
	}

	header := m.tableHeader(
		epicW, "Epic",
		remW, "Remaining",
		velW, "Vel/day",
		spkW, "Trend",
		etaW, "ETA (P50 / P90)",
	)

	var rows []string

	// Individual epics (scrollable).
	epics := bd.Epics
	if len(epics) > 0 {
		if m.epicScroll >= len(epics) {
			m.epicScroll = len(epics) - 1
		}
		visible := epics
		if len(visible) > maxRows-3 {
			visible = visible[m.epicScroll:]
			if len(visible) > maxRows-3 {
				visible = visible[:maxRows-3]
			}
		}
		for _, ep := range visible {
			rows = append(rows, m.epicRow(ep, epicW, remW, velW, spkW, etaW))
		}
	}

	// Summary row (always shown, separated by a thin line).
	if bd.AllEpicsSummary.Total > 0 {
		rows = append(rows, strings.Repeat("─", m.width))
		rows = append(rows, m.epicRow(bd.AllEpicsSummary, epicW, remW, velW, spkW, etaW))
	}

	return title + "\n" + header + "\n" + strings.Join(rows, "\n")
}

// renderBacklogSection renders the Backlog Burndown section.
func (m burndownModel) renderBacklogSection(bd cockpit.BurndownSnapshot) string {
	title := m.sectionTitle("Backlog Burndown")
	bl := bd.Backlog

	if bl.InsufficientHistory && bl.Ready == 0 {
		return title + "\n" + m.dimText("  no backlog data yet")
	}

	readyStr := fmt.Sprintf("ready: %d", bl.Ready)
	cpStr := fmt.Sprintf("critical path: ≥%d hops (approx)", bl.CriticalPathHops)
	parallelismStr := ""
	if bl.ParallelismN > 0 {
		parallelismStr = fmt.Sprintf("  parallelism: %.1f (n=%d)", bl.ObservedParallelism, bl.ParallelismN)
	}

	var etaStr string
	if bl.InsufficientHistory {
		etaStr = "insufficient history (n=" + burndownItoa(bl.HistoryN) + ")"
	} else {
		etaStr = cockpit.FormatETARange(bl.DrainETA_P50, bl.DrainETA_P90, bl.HistoryN, time.Now())
	}

	sparkLine := ""
	if bl.Sparkline != "" {
		sparkLine = "  trend: " + bl.Sparkline
	}

	line1 := fmt.Sprintf("  %s  %s%s", readyStr, cpStr, parallelismStr)
	line2 := fmt.Sprintf("  drain ETA: %s%s", etaStr, sparkLine)
	return title + "\n" + line1 + "\n" + line2
}

// renderCostSection renders the Cost Burndown section.
func (m burndownModel) renderCostSection(bd cockpit.BurndownSnapshot) string {
	title := m.sectionTitle("Cost Burndown")
	cb := bd.Cost

	fitStyle := lipgloss.NewStyle()
	fitText := cockpit.FitLabel(cb.Fit)
	switch cb.Fit {
	case cockpit.FitGreen:
		fitStyle = fitStyle.Foreground(m.theme.Done)
	case cockpit.FitAmber:
		fitStyle = fitStyle.Foreground(m.theme.Warning)
	case cockpit.FitRed:
		fitStyle = fitStyle.Foreground(m.theme.Error)
	default:
		fitStyle = fitStyle.Foreground(m.theme.Gray)
	}

	projStr := ""
	if cb.InsufficientHistory {
		projStr = fmt.Sprintf("est. $%.2f/bead (estimator default, n<%d obs)",
			cb.AvgCostPerBead, cockpit.MinSamples)
	} else {
		projStr = fmt.Sprintf("$%.2f P50 / $%.2f P90 (%.0f beads × $%.2f–$%.2f/bead)",
			cb.ProjectedP50USD, cb.ProjectedP90USD,
			float64(cb.RemainingBeads), cb.AvgCostPerBead,
			cb.ProjectedP90USD/float64(max1(cb.RemainingBeads)))
	}

	windowStr := ""
	if cb.WindowCeilingUSD > 0 {
		windowStr = fmt.Sprintf("  window: $%.2f left of $%.2f  %s",
			cb.WindowRemainingUSD, cb.WindowCeilingUSD,
			fitStyle.Render(fitText))
	} else {
		windowStr = "  window: not available (run koryph quota calibrate)"
	}

	line := fmt.Sprintf("  remaining: %d beads  projected: %s%s",
		cb.RemainingBeads, projStr, windowStr)
	return title + "\n" + line
}

// renderDurationSection renders the Duration Stats section.
func (m burndownModel) renderDurationSection(bd cockpit.BurndownSnapshot, maxRows int) string {
	title := m.sectionTitle("Duration Stats (DispatchedAt→MergedAt)")
	if len(bd.DurationStats) == 0 {
		return title + "\n" + m.dimText("  no completed slots in history yet")
	}

	// Header.
	tierW := 12
	nW := 5
	meanW := 10
	p50W := 10
	p90W := 10
	header := m.tableHeader(tierW, "Tier", nW, "N", meanW, "Mean", p50W, "P50", p90W, "P90")

	var rows []string
	shown := bd.DurationStats
	if len(shown) > maxRows-2 {
		shown = shown[:maxRows-2]
	}
	for _, ds := range shown {
		var row string
		if ds.Sparse {
			row = fmt.Sprintf("  %-*s  %-*d  %-*s",
				tierW, ds.Tier,
				nW-2, ds.N,
				meanW+p50W+p90W, fmt.Sprintf("insufficient history (n=%d)", ds.N))
		} else {
			row = fmt.Sprintf("  %-*s  %-*d  %-*s  %-*s  %-*s",
				tierW, ds.Tier,
				nW-2, ds.N,
				meanW-2, formatDur(ds.Mean),
				p50W-2, formatDur(ds.P50),
				p90W-2, formatDur(ds.P90))
		}
		rows = append(rows, row)
	}

	return title + "\n" + header + "\n" + strings.Join(rows, "\n")
}

// --- rendering helpers -------------------------------------------------------

// sectionTitle renders a section header bar.
func (m burndownModel) sectionTitle(title string) string {
	bar := "─ " + title + " " + strings.Repeat("─", m.width-len(title)-4)
	if len(bar) > m.width {
		bar = bar[:m.width]
	}
	return lipgloss.NewStyle().Foreground(m.theme.Accent).Render(bar)
}

// tableHeader renders a two-spaces-indented header row with fixed column widths.
func (m burndownModel) tableHeader(pairs ...interface{}) string {
	// pairs: (width, title, width, title, ...)
	var parts []string
	for i := 0; i+1 < len(pairs); i += 2 {
		w := pairs[i].(int)
		t := pairs[i+1].(string)
		col := fmt.Sprintf("%-*s", w, t)
		parts = append(parts, lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(col))
	}
	return "  " + strings.Join(parts, "  ")
}

// epicRow renders one row of the epic table.
func (m burndownModel) epicRow(ep cockpit.EpicBurndown, epicW, remW, velW, spkW, etaW int) string {
	title := truncate(ep.Title, epicW)
	rem := fmt.Sprintf("%d", ep.Remaining)
	if ep.Remaining == 0 {
		rem = lipgloss.NewStyle().Foreground(m.theme.Done).Render("0")
	}
	vel := ""
	if ep.VelocityPerDay > 0 {
		vel = fmt.Sprintf("%.1f", ep.VelocityPerDay)
	} else {
		vel = m.dimText("—")
	}
	spk := ep.Sparkline
	if spk == "" {
		spk = strings.Repeat(" ", spkW)
	}

	eta := ""
	if !ep.ETAP50.IsZero() {
		eta = cockpit.FormatETARange(ep.ETAP50, ep.ETAP90, ep.VelocityN, time.Now())
	} else if ep.VelocityN < cockpit.MinSamples {
		eta = m.dimText(fmt.Sprintf("insufficient history (n=%d)", ep.VelocityN))
	} else if ep.Remaining == 0 {
		eta = lipgloss.NewStyle().Foreground(m.theme.Done).Render("done")
	}

	return fmt.Sprintf("  %-*s  %-*s  %-*s  %-*s  %s",
		epicW, title,
		remW-2, rem,
		velW-2, vel,
		spkW-2, spk,
		eta)
}

// dimText returns s styled as inactive/gray.
func (m burndownModel) dimText(s string) string {
	return lipgloss.NewStyle().Foreground(m.theme.Inactive).Render(s)
}

// formatDur formats a duration for display in the stats table.
func formatDur(d time.Duration) string {
	if d <= 0 {
		return "—"
	}
	d = d.Round(time.Second)
	h := int(d.Hours())
	min := int(d.Minutes()) % 60
	sec := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh%02dm", h, min)
	}
	if min > 0 {
		return fmt.Sprintf("%dm%02ds", min, sec)
	}
	return fmt.Sprintf("%ds", sec)
}

// burndownItoa converts int to string (avoids importing strconv in this file).
func burndownItoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// max1 returns n if n >= 1, else 1 (avoids divide-by-zero).
func max1(n int) int {
	if n < 1 {
		return 1
	}
	return n
}
