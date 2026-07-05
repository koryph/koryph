// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// queue.go implements the Queue tab for the koryph TUI cockpit (koryph-9af.2).
//
// The Queue tab renders the project's hierarchical work queue: epics at the
// top level with their child beads nested below, every row annotated with its
// true dispatch state:
//
//   - running           — a slot is actively working this bead
//   - ready             — dep-unblocked, no footprint conflict
//   - dep-blocked       — has one or more open dependencies
//   - footprint-deferred — ready but footprint conflicts with a running bead
//   - human             — carries a no-dispatch / human-only label
//   - deferred-until    — carries a deferred-until:<date> label
//   - parked            — parked label or status
//   - container         — epic / feature / decision (not directly dispatchable)
//
// Navigation: j/k or arrow keys, space to expand/collapse epics, f to cycle
// through state filters, enter to open an inline bead detail panel, esc to
// close it.
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Queue",
		Order: 4,
		New:   func(theme Theme, _ bool) TabModel { return newQueueModel(theme) },
	})
}

// IsCapturingInput implements TabModel. The queue tab has no text inputs.
func (m *queueModel) IsCapturingInput() bool { return false }

// queueFilter is the active state filter for the queue tab.
type queueFilter int

const (
	queueFilterAll      queueFilter = iota
	queueFilterRunning              // running only
	queueFilterReady                // ready + running
	queueFilterBlocked              // dep-blocked + footprint-deferred
	queueFilterDeferred             // footprint-deferred + deferred-until + human + parked
	queueFilterCount                // sentinel — number of filter values
)

// filterLabel returns the display label for a filter.
func filterLabel(f queueFilter) string {
	switch f {
	case queueFilterAll:
		return "all"
	case queueFilterRunning:
		return "running"
	case queueFilterReady:
		return "ready"
	case queueFilterBlocked:
		return "blocked"
	case queueFilterDeferred:
		return "deferred"
	default:
		return "all"
	}
}

// flatRow is one rendered row in the flattened visible queue.
type flatRow struct {
	depth       int
	node        cockpit.QueueNode
	hasChildren bool
	expanded    bool
}

// queueModel is the Bubble Tea model for the Queue tab.
type queueModel struct {
	theme  Theme
	width  int
	height int

	// snap is the latest cockpit snapshot.
	snap cockpit.Snapshot

	// rows is the flattened, filtered list of visible rows.
	rows []flatRow

	// cursor is the index of the selected row in rows.
	cursor int

	// expanded tracks which nodes are expanded (keyed by issue ID).
	expanded map[string]bool

	// filter is the active state filter.
	filter queueFilter

	// detail, when non-nil, is the node whose detail panel is shown.
	detail *cockpit.QueueNode

	// detailScroll is the vertical scroll offset for the detail panel.
	detailScroll int
}

// newQueueModel creates an empty queue model.
func newQueueModel(theme Theme) *queueModel {
	return &queueModel{
		theme:    theme,
		width:    80,
		height:   24,
		expanded: make(map[string]bool),
	}
}

// Init implements TabModel.
func (m *queueModel) Init() tea.Cmd { return nil }

// Update implements TabModel.
func (m *queueModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.detail != nil {
			return m.updateDetail(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

// updateList handles key events when the list is the primary focus.
func (m *queueModel) updateList(msg tea.KeyMsg) (TabModel, tea.Cmd) {
	switch msg.String() {
	case "j", "down":
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "g":
		m.cursor = 0
	case "G":
		if len(m.rows) > 0 {
			m.cursor = len(m.rows) - 1
		}
	case " ":
		// Toggle expand/collapse on the selected row.
		if m.cursor < len(m.rows) {
			row := m.rows[m.cursor]
			if row.hasChildren {
				id := row.node.Issue.ID
				m.expanded[id] = !m.expanded[id]
				m.rebuildRows()
			}
		}
	case "f":
		// Cycle filter.
		m.filter = (m.filter + 1) % queueFilterCount
		m.rebuildRows()
		// Clamp cursor.
		if m.cursor >= len(m.rows) {
			m.cursor = len(m.rows) - 1
		}
		if m.cursor < 0 {
			m.cursor = 0
		}
	case "F":
		// Expand all.
		m.expandAll(m.snap.Queue.Roots, true)
		m.rebuildRows()
	case "enter":
		if m.cursor < len(m.rows) {
			node := m.rows[m.cursor].node
			m.detail = &node
			m.detailScroll = 0
		}
	}
	return m, nil
}

// updateDetail handles key events when the detail panel is open.
func (m *queueModel) updateDetail(msg tea.KeyMsg) (TabModel, tea.Cmd) {
	switch msg.String() {
	case "esc", "backspace", "q":
		m.detail = nil
	case "j", "down":
		m.detailScroll++
	case "k", "up":
		if m.detailScroll > 0 {
			m.detailScroll--
		}
	}
	return m, nil
}

// SetSnapshot implements TabModel. Rebuilds the flat row list.
func (m *queueModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
	m.rebuildRows()
	// Clamp cursor to new row count.
	if m.cursor >= len(m.rows) && len(m.rows) > 0 {
		m.cursor = len(m.rows) - 1
	}
}

// Resize implements TabModel.
func (m *queueModel) Resize(w, h int) {
	m.width = w
	m.height = h
}

// View implements TabModel.
func (m *queueModel) View() string {
	if m.snap.Queue.NodeCount == 0 {
		return m.emptyView()
	}
	if m.detail != nil {
		return m.detailView()
	}
	return m.listView()
}

// --- View renderers ----------------------------------------------------------

// emptyView renders the placeholder when no queue data is available.
func (m *queueModel) emptyView() string {
	return m.sectionTitle("Queue") + "\n" +
		lipgloss.NewStyle().Foreground(m.theme.Inactive).
			Render("  no queue data (bd not available or no open issues)")
}

// listView renders the scrollable queue list.
func (m *queueModel) listView() string {
	var b strings.Builder

	// Title bar with filter and counts.
	total := m.snap.Queue.NodeCount
	showing := len(m.rows)
	filterStr := filterLabel(m.filter)
	title := fmt.Sprintf("Queue  filter:%s  %d/%d  [f=filter  space=expand  enter=detail]",
		filterStr, showing, total)
	b.WriteString(m.sectionTitle(title))
	b.WriteRune('\n')

	// Compute visible rows (scrolled to keep cursor visible).
	avail := m.height - 3 // title + header + status
	if avail < 1 {
		avail = 1
	}
	start := 0
	if m.cursor >= avail {
		start = m.cursor - avail + 1
	}
	end := start + avail
	if end > len(m.rows) {
		end = len(m.rows)
	}

	// Column widths.
	stateW := 10
	idW := 14
	reasonW := 24
	titleW := m.width - stateW - idW - reasonW - 8 // 8 = indent(max 4) + separators
	if titleW < 10 {
		titleW = 10
	}

	// Header row.
	header := fmt.Sprintf("  %-*s  %-*s  %-*s  %s",
		stateW, "State",
		idW, "ID",
		titleW, "Title",
		"Reason/Blockers")
	b.WriteString(lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(header))
	b.WriteRune('\n')

	for i := start; i < end; i++ {
		row := m.rows[i]
		line := m.renderRow(row, i == m.cursor, stateW, idW, titleW, reasonW)
		b.WriteString(line)
		b.WriteRune('\n')
	}

	// Scroll indicator.
	if len(m.rows) > avail {
		pct := 0
		if len(m.rows) > 1 {
			pct = m.cursor * 100 / (len(m.rows) - 1)
		}
		indicator := lipgloss.NewStyle().Foreground(m.theme.Gray).
			Render(fmt.Sprintf("  (%d/%d  %d%%)", m.cursor+1, len(m.rows), pct))
		b.WriteString(indicator)
	}

	return b.String()
}

// renderRow renders one queue row, highlighted when selected.
func (m *queueModel) renderRow(row flatRow, selected bool, stateW, idW, titleW, reasonW int) string {
	node := row.node
	iss := node.Issue

	// Indent: 2 spaces per depth level.
	indent := strings.Repeat("  ", row.depth)

	// Expand/collapse indicator.
	expInd := "  "
	if row.hasChildren {
		if row.expanded {
			expInd = "▼ "
		} else {
			expInd = "▶ "
		}
	}

	stateBadge := m.stateBadge(node.State)
	id := truncate(iss.ID, idW)
	title := truncate(iss.Title, titleW)
	reason := truncate(node.Reason, reasonW)

	// Children count annotation for containers.
	if node.State == cockpit.QueueStateContainer && row.hasChildren {
		kids := fmt.Sprintf(" (%d)", len(node.Children))
		title = truncate(iss.Title, titleW-len(kids)) + kids
	}

	line := fmt.Sprintf("%s%s%-*s  %-*s  %-*s  %s",
		indent, expInd,
		stateW, stateBadge,
		idW, id,
		titleW, title,
		reason)

	if selected {
		return lipgloss.NewStyle().
			Background(m.theme.Blue).
			Foreground(m.theme.White).
			Render(line)
	}
	return m.stateStyle(node.State).Render(line)
}

// stateBadge returns the display badge for a queue state.
func (m *queueModel) stateBadge(state cockpit.QueueNodeState) string {
	switch state {
	case cockpit.QueueStateRunning:
		return lipgloss.NewStyle().Foreground(m.theme.Running).Bold(true).Render("running")
	case cockpit.QueueStateReady:
		return lipgloss.NewStyle().Foreground(m.theme.Done).Render("ready")
	case cockpit.QueueStateDepBlocked:
		return lipgloss.NewStyle().Foreground(m.theme.Error).Render("dep-blocked")
	case cockpit.QueueStateFootprintDeferred:
		return lipgloss.NewStyle().Foreground(m.theme.Warning).Render("fp-deferred")
	case cockpit.QueueStateHuman:
		return lipgloss.NewStyle().Foreground(m.theme.Purple).Render("human")
	case cockpit.QueueStateDeferredUntil:
		return lipgloss.NewStyle().Foreground(m.theme.Yellow).Render("deferred")
	case cockpit.QueueStateParked:
		return lipgloss.NewStyle().Foreground(m.theme.Gray).Render("parked")
	case cockpit.QueueStateContainer:
		return lipgloss.NewStyle().Foreground(m.theme.Accent).Render("epic")
	default:
		return lipgloss.NewStyle().Foreground(m.theme.Gray).Render(string(state))
	}
}

// stateStyle returns the base row style for a queue state (used for
// non-selected rows).
func (m *queueModel) stateStyle(state cockpit.QueueNodeState) lipgloss.Style {
	switch state {
	case cockpit.QueueStateRunning:
		return lipgloss.NewStyle().Foreground(m.theme.Running)
	case cockpit.QueueStateReady:
		return lipgloss.NewStyle()
	case cockpit.QueueStateDepBlocked:
		return lipgloss.NewStyle().Foreground(m.theme.Error)
	case cockpit.QueueStateFootprintDeferred:
		return lipgloss.NewStyle().Foreground(m.theme.Warning)
	case cockpit.QueueStateHuman:
		return lipgloss.NewStyle().Foreground(m.theme.Purple)
	case cockpit.QueueStateDeferredUntil, cockpit.QueueStateParked:
		return lipgloss.NewStyle().Foreground(m.theme.Inactive)
	case cockpit.QueueStateContainer:
		return lipgloss.NewStyle().Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(m.theme.Inactive)
	}
}

// detailView renders the inline bead detail panel.
func (m *queueModel) detailView() string {
	node := m.detail
	iss := node.Issue

	var b strings.Builder
	avail := m.height - 3
	if avail < 4 {
		avail = 4
	}

	// Title bar.
	title := fmt.Sprintf("Bead Detail: %s  [esc=close  j/k=scroll]", iss.ID)
	b.WriteString(m.sectionTitle(title))
	b.WriteRune('\n')

	// Build the detail lines.
	lines := m.buildDetailLines(node)

	end := m.detailScroll + avail
	if end > len(lines) {
		end = len(lines)
	}
	start := m.detailScroll
	if start > len(lines) {
		start = 0
	}

	for _, line := range lines[start:end] {
		b.WriteString(line)
		b.WriteRune('\n')
	}

	if len(lines) > avail {
		indicator := lipgloss.NewStyle().Foreground(m.theme.Gray).
			Render(fmt.Sprintf("  (%d-%d of %d lines  j/k to scroll)",
				start+1, end, len(lines)))
		b.WriteString(indicator)
	}

	return b.String()
}

// buildDetailLines constructs the text lines for the detail panel.
func (m *queueModel) buildDetailLines(node *cockpit.QueueNode) []string {
	iss := node.Issue
	w := m.width - 4
	if w < 20 {
		w = 20
	}

	accent := lipgloss.NewStyle().Foreground(m.theme.Accent).Bold(true)
	dim := lipgloss.NewStyle().Foreground(m.theme.Inactive)

	field := func(label, value string) string {
		return fmt.Sprintf("  %s  %s",
			accent.Render(fmt.Sprintf("%-12s", label)),
			value)
	}

	var lines []string
	lines = append(lines, field("Title:", iss.Title))
	lines = append(lines, field("ID:", iss.ID))
	lines = append(lines, field("Type:", iss.IssueType))
	lines = append(lines, field("Status:", iss.Status))
	lines = append(lines, field("Priority:", fmt.Sprintf("P%d", iss.Priority)))
	lines = append(lines, field("State:", string(node.State)))

	if node.Reason != "" {
		lines = append(lines, field("Reason:", node.Reason))
	}

	if len(iss.Labels) > 0 {
		lines = append(lines, field("Labels:", strings.Join(iss.Labels, ", ")))
	}

	if iss.ParentID != "" {
		lines = append(lines, field("Parent:", iss.ParentID))
	}

	if iss.DependencyCount > 0 {
		lines = append(lines, field("Deps:", fmt.Sprintf("%d open", iss.DependencyCount)))
	}

	if iss.DependentCount > 0 {
		lines = append(lines, field("Depended:", fmt.Sprintf("%d open", iss.DependentCount)))
	}

	// Description (word-wrapped).
	if iss.Description != "" {
		lines = append(lines, "")
		lines = append(lines, accent.Render("  Description:"))
		for _, descLine := range wrapText(iss.Description, w) {
			lines = append(lines, "    "+descLine)
		}
	} else {
		lines = append(lines, dim.Render("  (no description)"))
	}

	// Notes.
	if iss.Notes != "" {
		lines = append(lines, "")
		lines = append(lines, accent.Render("  Notes:"))
		for _, noteLine := range wrapText(iss.Notes, w) {
			lines = append(lines, "    "+noteLine)
		}
	}

	// Children summary.
	if len(node.Children) > 0 {
		lines = append(lines, "")
		lines = append(lines, accent.Render(fmt.Sprintf("  Children (%d):", len(node.Children))))
		for _, child := range node.Children {
			badge := string(child.State)
			childLine := fmt.Sprintf("    %-12s  %s  %s",
				badge, child.Issue.ID, truncate(child.Issue.Title, w-30))
			lines = append(lines, m.stateStyle(child.State).Render(childLine))
		}
	}

	return lines
}

// --- Tree flattening ---------------------------------------------------------

// rebuildRows recomputes the flat visible row list from the current queue
// snapshot, respecting the expanded set and active filter.
func (m *queueModel) rebuildRows() {
	m.rows = m.rows[:0]
	for _, root := range m.snap.Queue.Roots {
		m.flattenNode(root, 0)
	}
}

// flattenNode appends visible rows for node and (if expanded) its subtree.
func (m *queueModel) flattenNode(node cockpit.QueueNode, depth int) {
	hasKids := len(node.Children) > 0
	isExpanded := m.expanded[node.Issue.ID]

	// Containers (epics) default to expanded unless explicitly collapsed.
	if hasKids && !m.expandedSet(node.Issue.ID) {
		isExpanded = true
		m.expanded[node.Issue.ID] = true
	}

	// Apply filter: always show containers (epics); for leaf nodes, apply the
	// active filter. A container is kept if any descendant matches (computed
	// below via visibility of children).
	if !m.stateVisible(node.State) && !hasKids {
		return
	}

	row := flatRow{
		depth:       depth,
		node:        node,
		hasChildren: hasKids,
		expanded:    isExpanded,
	}
	m.rows = append(m.rows, row)

	if isExpanded {
		for _, child := range node.Children {
			m.flattenNode(child, depth+1)
		}
	}
}

// expandedSet returns true if the issue ID has been explicitly set in m.expanded.
func (m *queueModel) expandedSet(id string) bool {
	_, ok := m.expanded[id]
	return ok
}

// expandAll recursively sets the expanded state of all nodes.
func (m *queueModel) expandAll(nodes []cockpit.QueueNode, expanded bool) {
	for _, n := range nodes {
		if len(n.Children) > 0 {
			m.expanded[n.Issue.ID] = expanded
			m.expandAll(n.Children, expanded)
		}
	}
}

// stateVisible reports whether a state passes the current filter.
func (m *queueModel) stateVisible(state cockpit.QueueNodeState) bool {
	switch m.filter {
	case queueFilterAll:
		return true
	case queueFilterRunning:
		return state == cockpit.QueueStateRunning
	case queueFilterReady:
		return state == cockpit.QueueStateRunning || state == cockpit.QueueStateReady
	case queueFilterBlocked:
		return state == cockpit.QueueStateDepBlocked || state == cockpit.QueueStateFootprintDeferred
	case queueFilterDeferred:
		return state == cockpit.QueueStateFootprintDeferred ||
			state == cockpit.QueueStateDeferredUntil ||
			state == cockpit.QueueStateHuman ||
			state == cockpit.QueueStateParked
	default:
		return true
	}
}

// --- helpers -----------------------------------------------------------------

// sectionTitle renders a section header bar (same style as burndown tab).
func (m *queueModel) sectionTitle(title string) string {
	suffix := strings.Repeat("─", max0(m.width-len(title)-4))
	bar := "─ " + title + " " + suffix
	if len(bar) > m.width {
		bar = bar[:m.width]
	}
	return lipgloss.NewStyle().Foreground(m.theme.Accent).Render(bar)
}

// max0 returns n if n >= 0, else 0.
func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}
