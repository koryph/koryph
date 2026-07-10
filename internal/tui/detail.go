// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// detail.go implements the Detail tab model for the koryph TUI cockpit
// (koryph-9af.3). It renders the full bead detail panel including metadata,
// dependency graph, attempt history, and log-tail shortcut.
//
// Navigation:
//   - j/↓ and k/↑ move keyboard focus through dep/reverse-dep rows.
//   - Enter on a focused dep row pushes the current bead onto the navigation
//     stack and opens the selected dep bead.
//   - Backspace/Esc pops the navigation stack; when the stack is empty an
//     Esc emits detailBackMsg so the App returns to the previous tab.
//   - 't' toggles the log-tail viewport (viewport follows on each tick).
//   - Mouse clicks on dep rows via bubblezone set keyboard focus.
package tui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	zone "github.com/lrstanley/bubblezone"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Detail",
		Order: 99,
		// Hidden: the Detail panel is an overlay reached only by pressing Enter
		// on a Threads or Queue row, never a standalone tab. It is kept out of
		// the tab bar and Tab-cycling; Esc/Backspace returns to the originating
		// tab (see App.detailBackMsg handling).
		Hidden: true,
		New:    func(theme Theme, _ bool) TabModel { return newDetailModel(theme) },
	})
}

// detailRow is a navigable row in the detail panel (e.g. a dep link).
type detailRow struct {
	id        string // bead ID this row points to
	label     string // "dep" or "rdep"
	isBlocker bool   // true when dep is not yet closed (blocks this bead)
	zoneID    string // stable zone ID for mouse-click hit testing
}

// detailModel is the Bubble Tea model for the Detail tab.
type detailModel struct {
	theme  Theme
	width  int
	height int

	// beadID is the currently focused bead. Empty means nothing is selected.
	beadID string

	// navStack is the bead-ID history before the current bead. Backspace pops.
	navStack []string

	// detail is the last fetched detail snapshot.
	detail cockpit.BeadDetailSnapshot

	// rows holds navigable rows (dep links) for keyboard/mouse selection.
	rows   []detailRow
	cursor int

	// logMode is true when the log-tail viewport is shown instead of the detail.
	logMode bool

	// logVP is the viewport component used for the log-tail.
	logVP viewport.Model

	// logFollow enables auto-scroll to bottom on each tick.
	logFollow bool

	// zonePrefix is unique to this model instance to avoid zone ID collisions.
	zonePrefix string
}

// newDetailModel creates an empty detail model.
func newDetailModel(theme Theme) *detailModel {
	// bubblezone's package-level functions require NewGlobal() to have been
	// called first; calling it multiple times is a no-op.
	zone.NewGlobal()
	vp := viewport.New(80, 20)
	return &detailModel{
		theme:      theme,
		width:      80,
		zonePrefix: zone.NewPrefix(),
		logVP:      vp,
	}
}

// Init implements TabModel.
func (m *detailModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Detail tab has no text inputs.
func (m *detailModel) IsCapturingInput() bool { return false }

// SetBead sets the focused bead ID and clears stale detail.
// Called by App when showDetailMsg is received.
func (m *detailModel) SetBead(beadID string) {
	m.beadID = beadID
	m.detail = cockpit.BeadDetailSnapshot{}
	m.rows = nil
	m.cursor = 0
	m.logMode = false
}

// SetDetail stores a freshly-assembled detail snapshot.
// Called by App when detailReadyMsg is received.
func (m *detailModel) SetDetail(d cockpit.BeadDetailSnapshot) {
	m.detail = d
	m.beadID = d.BeadID
	m.rebuildRows()
}

// SetSnapshot implements TabModel. Refreshes the detail if a new snapshot
// carries an updated detail for our focused bead.
func (m *detailModel) SetSnapshot(snap cockpit.Snapshot) {
	if snap.Detail.BeadID != "" && snap.Detail.BeadID == m.beadID {
		m.detail = snap.Detail
		m.rebuildRows()
	}
	// If in log-tail mode, re-read the log file for the latest content.
	if m.logMode && m.detail.LogPath != "" {
		m.refreshLog()
	}
}

// Resize implements TabModel.
func (m *detailModel) Resize(w, h int) {
	m.width = w
	m.height = h
	m.logVP.Width = w
	m.logVP.Height = h - 4 // leave room for header/footer
}

// rebuildRows rebuilds the navigable rows from the current detail snapshot.
// Deps (blockers for this bead) come first, then reverse-deps.
func (m *detailModel) rebuildRows() {
	m.rows = nil
	for i, dep := range m.detail.Deps {
		m.rows = append(m.rows, detailRow{
			id:        dep,
			label:     "dep",
			isBlocker: true, // forward dep = this bead is blocked by it
			zoneID:    fmt.Sprintf("%sdep-%d", m.zonePrefix, i),
		})
	}
	for i, rdep := range m.detail.ReverseDeps {
		m.rows = append(m.rows, detailRow{
			id:     rdep,
			label:  "rdep",
			zoneID: fmt.Sprintf("%srdep-%d", m.zonePrefix, i),
		})
	}
	if m.cursor >= len(m.rows) {
		m.cursor = 0
	}
}

// logTailBytes is the maximum number of bytes read from a log file on each
// refresh tick. Caps the read at 32 KB to avoid re-reading arbitrarily large
// files on every 100 ms interval.
const logTailBytes = 32 * 1024

// refreshLog re-reads the tail of the log file and updates the viewport.
func (m *detailModel) refreshLog() {
	if m.detail.LogPath == "" {
		return
	}
	f, err := os.Open(m.detail.LogPath)
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log unavailable: %v)", err))
		return
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log stat error: %v)", err))
		return
	}
	size := fi.Size()
	offset := size - logTailBytes
	if offset < 0 {
		offset = 0
	}
	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			m.logVP.SetContent(fmt.Sprintf("(log seek error: %v)", err))
			return
		}
	}
	buf, err := io.ReadAll(f)
	if err != nil {
		m.logVP.SetContent(fmt.Sprintf("(log read error: %v)", err))
		return
	}
	content := string(buf)
	if offset > 0 {
		// Align to a newline boundary so the first visible line is not a
		// partial UTF-8 sequence or mid-line fragment from the seek offset.
		if nl := strings.IndexByte(content, '\n'); nl >= 0 {
			content = content[nl+1:]
		}
		content = "[\u2026truncated\u2026]\n" + content
	}
	m.logVP.SetContent(content)
	if m.logFollow {
		m.logVP.GotoBottom()
	}
}

// Update implements TabModel.
func (m *detailModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// In log-tail mode most keys are routed to the viewport.
		if m.logMode {
			switch msg.String() {
			case "t", "esc":
				m.logMode = false
				return m, nil
			case "f":
				m.logFollow = !m.logFollow
				if m.logFollow {
					m.logVP.GotoBottom()
				}
				return m, nil
			}
			var cmd tea.Cmd
			m.logVP, cmd = m.logVP.Update(msg)
			return m, cmd
		}

		// Normal detail mode key handling.
		switch msg.String() {
		case "t":
			// Toggle log-tail mode.
			m.logMode = true
			m.logFollow = true
			m.refreshLog()
			return m, nil

		case "j", "down":
			if m.cursor < len(m.rows)-1 {
				m.cursor++
			}

		case "k", "up":
			if m.cursor > 0 {
				m.cursor--
			}

		case "enter":
			// Navigate into the focused dep/rdep row.
			if len(m.rows) > 0 && m.cursor >= 0 && m.cursor < len(m.rows) {
				targetID := m.rows[m.cursor].id
				// Push current bead onto the nav stack.
				if m.beadID != "" {
					m.navStack = append(m.navStack, m.beadID)
				}
				// Emit showDetailMsg to switch to that bead.
				id := targetID
				return m, func() tea.Msg { return showDetailMsg{beadID: id} }
			}

		case "backspace":
			// Pop the nav stack; if non-empty navigate to the previous bead.
			if len(m.navStack) > 0 {
				prev := m.navStack[len(m.navStack)-1]
				m.navStack = m.navStack[:len(m.navStack)-1]
				return m, func() tea.Msg { return showDetailMsg{beadID: prev} }
			}
			// Stack empty — emit detailBackMsg to return to the previous tab.
			return m, func() tea.Msg { return detailBackMsg{} }

		case "esc":
			// Esc always returns to the previous tab (clears nav stack too).
			m.navStack = nil
			return m, func() tea.Msg { return detailBackMsg{} }
		}

	case tea.MouseMsg:
		// Check if any dep/rdep zone was clicked.
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
	dimStyle := lipgloss.NewStyle().Foreground(m.theme.Gray)

	// Log-tail mode: render the viewport.
	if m.logMode {
		followIndicator := ""
		if m.logFollow {
			followIndicator = "  [follow]"
		}
		hdr := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).
			Render(fmt.Sprintf("Log tail: %s%s", truncate(m.detail.LogPath, 60), followIndicator))
		ftr := dimStyle.Render("t/esc back  f toggle-follow  ↑/↓ scroll")
		return zone.Scan(hdr + "\n" + m.logVP.View() + "\n" + ftr)
	}

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
	sectionStyle := lipgloss.NewStyle().Bold(true).Foreground(m.theme.Cyan)
	selectedStyle := lipgloss.NewStyle().Foreground(m.theme.White).Background(m.theme.Blue).Bold(true)
	blockerStyle := lipgloss.NewStyle().Foreground(m.theme.Error)
	rdepStyle := lipgloss.NewStyle().Foreground(m.theme.Done)

	// --- Navigation breadcrumb ---------------------------------------------------
	if len(m.navStack) > 0 {
		crumb := strings.Join(m.navStack, " → ") + " → " + d.BeadID
		b.WriteString(dimStyle.Render("  "+crumb) + "\n")
	}

	// --- Title / header ----------------------------------------------------------
	statusColor := m.theme.StatusColor(d.Status)
	statusBadge := lipgloss.NewStyle().Foreground(statusColor).Bold(true).Render(d.Status)
	titleLine := lipgloss.NewStyle().Bold(true).Foreground(m.theme.White).Width(w - 2).
		Render(fmt.Sprintf("[%s] %s  %s", d.BeadID, truncate(d.Title, 60), statusBadge))
	b.WriteString(titleLine + "\n")

	// --- Meta fields -------------------------------------------------------------
	b.WriteString(labelStyle.Render("Type:") + " " + valueStyle.Render(d.IssueType) + "\n")
	b.WriteString(labelStyle.Render("Priority:") + " " + valueStyle.Render(fmt.Sprintf("P%d", d.Priority)) + "\n")
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
		b.WriteString(labelStyle.Render("Worktree:") + " " + dimStyle.Render(truncate(d.Worktree, w-20)) + "\n")
	}
	if d.CostUSD > 0 || d.EstimateUSD > 0 {
		b.WriteString(labelStyle.Render("Cost/Est:") + " " + valueStyle.Render(formatDetailCost(d.CostUSD, d.EstimateUSD)) + "\n")
	}
	if d.LogPath != "" {
		b.WriteString(labelStyle.Render("Log:") + " " + dimStyle.Render(truncate(d.LogPath, w-20)) + "\n")
	}

	// --- Resources (koryph process-metrics) --------------------------------------
	// Per-bead clock times and process-cohort resource usage, so an operator can
	// calibrate orchestration against what each bead actually consumed.
	if !d.StartedAt.IsZero() || d.ResourceSamples > 0 {
		b.WriteString(sectionStyle.Render("Resources") + "\n")
		if !d.StartedAt.IsZero() {
			b.WriteString(labelStyle.Render("Started:") + " " + valueStyle.Render(formatTimestamp(d.StartedAt)) + "\n")
		}
		finished := "running"
		if !d.FinishedAt.IsZero() {
			finished = formatTimestamp(d.FinishedAt)
		}
		b.WriteString(labelStyle.Render("Finished:") + " " + valueStyle.Render(finished) + "\n")
		if wall := detailWall(d); wall > 0 {
			b.WriteString(labelStyle.Render("Wall:") + " " + valueStyle.Render(formatElapsed(wall)) + "\n")
		}
		if d.ResourceSamples > 0 {
			mem := fmt.Sprintf("avg %d MB · peak %d MB", d.AvgRSSMB, d.PeakRSSMB)
			b.WriteString(labelStyle.Render("Memory:") + " " + valueStyle.Render(mem) + "\n")
			cpu := fmt.Sprintf("%.0fs · %.0f%% util", d.CPUSeconds, d.CPUUtilPct)
			b.WriteString(labelStyle.Render("CPU:") + " " + valueStyle.Render(cpu) + "\n")
			if d.IOReadMB > 0 || d.IOWriteMB > 0 {
				io := fmt.Sprintf("%.1f MB read · %.1f MB written", d.IOReadMB, d.IOWriteMB)
				b.WriteString(labelStyle.Render("Disk I/O:") + " " + valueStyle.Render(io) + "\n")
			} else {
				b.WriteString(labelStyle.Render("Disk I/O:") + " " + dimStyle.Render("n/a on this platform") + "\n")
			}
		}
	}

	// --- Description -------------------------------------------------------------
	if d.Description != "" {
		b.WriteString(sectionStyle.Render("Description") + "\n")
		for _, line := range wrapText(d.Description, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Acceptance criteria -----------------------------------------------------
	if d.Acceptance != "" {
		b.WriteString(sectionStyle.Render("Acceptance") + "\n")
		for _, line := range wrapText(d.Acceptance, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Notes -------------------------------------------------------------------
	if d.Notes != "" {
		b.WriteString(sectionStyle.Render("Notes") + "\n")
		for _, line := range wrapText(d.Notes, w-4) {
			b.WriteString("  " + dimStyle.Render(line) + "\n")
		}
	}

	// --- Dependencies (navigable, blockers highlighted) --------------------------
	depOffset := 0
	if len(d.Deps) > 0 {
		b.WriteString(sectionStyle.Render("Depends on") + "\n")
		for i, dep := range d.Deps {
			rowStr := fmt.Sprintf("  ← %s", dep)
			var rendered string
			if depOffset+i == m.cursor {
				rendered = zone.Mark(m.rows[i].zoneID, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(m.rows[i].zoneID, blockerStyle.Render(rowStr))
			}
			b.WriteString(rendered + "\n")
		}
		depOffset += len(d.Deps)
	}

	// --- Reverse deps (navigable) ------------------------------------------------
	if len(d.ReverseDeps) > 0 {
		b.WriteString(sectionStyle.Render("Blocked by this") + "\n")
		for i, rdep := range d.ReverseDeps {
			rowStr := fmt.Sprintf("  → %s", rdep)
			var rendered string
			if depOffset+i == m.cursor {
				rendered = zone.Mark(m.rows[depOffset+i].zoneID, selectedStyle.Render(rowStr))
			} else {
				rendered = zone.Mark(m.rows[depOffset+i].zoneID, rdepStyle.Render(rowStr))
			}
			b.WriteString(rendered + "\n")
		}
	}

	// --- Attempt history ---------------------------------------------------------
	if len(d.AttemptHistory) > 0 {
		b.WriteString(sectionStyle.Render("Attempt history") + "\n")
		for _, rec := range d.AttemptHistory {
			cause := ""
			if rec.RequeueCause != "" {
				cause = "  requeue:" + rec.RequeueCause
			}
			line := fmt.Sprintf("  #%d  %-14s  %-12s  $%.3f  %s%s",
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

	// --- Footer hint -------------------------------------------------------------
	hint := "↑/↓ navigate  enter jump  ⌫ back  t tail log  esc return"
	if len(m.navStack) > 0 {
		hint = "↑/↓ navigate  enter jump  ⌫ pop stack  esc return to tab"
	}
	b.WriteString("\n" + dimStyle.Render(hint) + "\n")

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
		for _, word := range words {
			if cur == "" {
				cur = word
			} else if len([]rune(cur))+1+len([]rune(word)) <= maxWidth {
				cur += " " + word
			} else {
				lines = append(lines, cur)
				cur = word
			}
		}
		if cur != "" {
			lines = append(lines, cur)
		}
	}
	return lines
}

// detailWall returns the bead's wall-clock duration: dispatch → finish when
// terminal, or dispatch → snapshot time while still live. Zero when the start
// is unknown or the computed span is negative.
func detailWall(d cockpit.BeadDetailSnapshot) time.Duration {
	if d.StartedAt.IsZero() {
		return 0
	}
	end := d.ComputedAt
	if !d.FinishedAt.IsZero() {
		end = d.FinishedAt
	}
	if w := end.Sub(d.StartedAt); w > 0 {
		return w
	}
	return 0
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
