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
//   - resource-deferred — ready but a declared res:<kind> is at capacity
//     (koryph-4ql.10; carries the kind + holder)
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
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
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

// IsCapturingInput implements TabModel: true while the / search input is
// focused so App.Update routes every key here instead of firing global
// bindings (koryph-166, mirroring the Events tab's filter input).
func (m *queueModel) IsCapturingInput() bool { return m.queryActive }

// queueMode selects how the queue arranges rows (koryph-vy8).
type queueMode int

const (
	// queueModeEpics is the hierarchy: epics with their children nested,
	// siblings ordered by priority (the pre-existing default).
	queueModeEpics queueMode = iota
	// queueModePriority flattens every node into one list sorted by
	// priority then id — "what dispatches next", no grouping.
	queueModePriority
	// queueModeIssues is the flat priority list minus container rows
	// (epics/features and container-tasks) — only directly workable issues.
	queueModeIssues
	queueModeCount // sentinel
)

// modeLabel returns the display label for a grouping mode.
func modeLabel(m queueMode) string {
	switch m {
	case queueModePriority:
		return "priority"
	case queueModeIssues:
		return "issues"
	default:
		return "epics"
	}
}

// queueStaleAfter is how old the queue snapshot may be before the title bar
// calls out its age (koryph-b01). The provider recomputes every ~5 s
// (derivedTTL); several missed cycles means bd is slow, contended, or a
// refresh pass failed/timed out — the operator should know they are looking
// at a frozen tree, not live state.
const queueStaleAfter = 45 * time.Second

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
	// ancestorsLast holds, for each ancestor from the top level down to this
	// row's immediate parent, whether that ancestor was the last among its
	// visible siblings. Together with isLast it drives the ├─ / └─ / │ tree
	// connectors so the epic→child grouping is visually unambiguous.
	ancestorsLast []bool
	// isLast reports whether this row is the last among its visible siblings.
	isLast bool
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

	// mode is the active grouping/sorting mode (koryph-vy8).
	mode queueMode

	// query is the applied metadata search (koryph-166); queryActive is true
	// while the operator edits queryInput.
	query       string
	queryActive bool
	queryInput  textinput.Model

	// detail, when non-nil, is the node whose detail panel is shown.
	detail *cockpit.QueueNode

	// detailScroll is the vertical scroll offset for the detail panel.
	detailScroll int
}

// newQueueModel creates an empty queue model.
func newQueueModel(theme Theme) *queueModel {
	qi := textinput.New()
	qi.Placeholder = "label:area:engine type:task state:ready p:1 free-text"
	qi.CharLimit = 120
	return &queueModel{
		theme:      theme,
		width:      80,
		height:     24,
		expanded:   make(map[string]bool),
		queryInput: qi,
	}
}

// Init implements TabModel.
func (m *queueModel) Init() tea.Cmd { return nil }

// Update implements TabModel.
func (m *queueModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if m.queryActive {
			return m.updateQuery(msg)
		}
		if m.detail != nil {
			return m.updateDetail(msg)
		}
		return m.updateList(msg)
	}
	return m, nil
}

// updateQuery handles key events while the / search input is focused
// (koryph-166): enter applies the query (empty clears it), esc cancels and
// keeps the previously applied query, everything else edits the input.
func (m *queueModel) updateQuery(msg tea.KeyMsg) (TabModel, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.query = strings.TrimSpace(m.queryInput.Value())
		m.queryActive = false
		m.queryInput.Blur()
		m.rebuildRows()
		m.clampCursor()
		return m, nil
	case "esc":
		m.queryActive = false
		m.queryInput.Blur()
		return m, nil
	}
	var cmd tea.Cmd
	m.queryInput, cmd = m.queryInput.Update(msg)
	return m, cmd
}

// clampCursor keeps the cursor inside the current row list.
func (m *queueModel) clampCursor() {
	if m.cursor >= len(m.rows) {
		m.cursor = len(m.rows) - 1
	}
	if m.cursor < 0 {
		m.cursor = 0
	}
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
		// Toggle expand/collapse on the selected row (epic tree only —
		// flat modes build rows with hasChildren false, so this is inert).
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
		m.clampCursor()
	case "m":
		// Cycle grouping mode (koryph-vy8).
		m.mode = (m.mode + 1) % queueModeCount
		m.rebuildRows()
		m.clampCursor()
	case "/":
		// Open the metadata search input (koryph-166).
		m.queryActive = true
		m.queryInput.SetValue(m.query)
		m.queryInput.Focus()
		return m, textinput.Blink
	case "F":
		// Toggle expand-all/collapse-all (koryph-vy8): expand when anything
		// is collapsed, otherwise collapse everything to the epic level.
		if m.mode == queueModeEpics {
			m.expandAll(m.snap.Queue.Roots, m.anyCollapsed(m.snap.Queue.Roots))
			m.rebuildRows()
			m.clampCursor()
		}
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

// emptyView renders the placeholder when no queue data is available,
// distinguishing "the first background refresh hasn't landed yet" (a cold bd
// scan takes ~15 s; koryph-b01) from "bd genuinely returned nothing".
func (m *queueModel) emptyView() string {
	msg := "  no open issues (or bd not available)"
	if m.snap.Queue.ComputedAt.IsZero() {
		msg = "  queue refreshing — the first bd scan can take ~15 s…"
	}
	return m.sectionTitle("Queue") + "\n" +
		lipgloss.NewStyle().Foreground(m.theme.Inactive).Render(msg)
}

// listView renders the scrollable queue list.
func (m *queueModel) listView() string {
	var b strings.Builder

	// Title bar with mode, filter, query, and counts. Stale queue data — the
	// background refresh hasn't landed for several TTLs (bd slow, contended,
	// or a pass timed out; koryph-b01) — is called out with its age so the
	// operator never mistakes a frozen tree for live state.
	total := m.snap.Queue.NodeCount
	showing := len(m.rows)
	stale := ""
	if age := m.snap.CapturedAt.Sub(m.snap.Queue.ComputedAt); !m.snap.Queue.ComputedAt.IsZero() && age > queueStaleAfter {
		stale = fmt.Sprintf("  (data %ds old)", int(age.Seconds()))
	}
	queryStr := ""
	if m.query != "" {
		queryStr = fmt.Sprintf("  /%s", m.query)
	}
	title := fmt.Sprintf("Queue  mode:%s  filter:%s%s  %d/%d%s  [m=mode  f=filter  /=search  space=fold  F=fold-all  enter=detail]",
		modeLabel(m.mode), filterLabel(m.filter), queryStr, showing, total, stale)
	b.WriteString(m.sectionTitle(title))
	b.WriteRune('\n')

	// Search input line, while editing (koryph-166).
	inputLines := 0
	if m.queryActive {
		b.WriteString("  search> " + m.queryInput.View())
		b.WriteRune('\n')
		inputLines = 1
	}

	// Compute visible rows (scrolled to keep cursor visible).
	avail := m.height - 3 - inputLines // title + header + status (+ input)
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
	stateW := 12 // widest badge is "res-deferred" (12)
	idW := 16
	reasonW := 24
	titleW := m.width - stateW - idW - reasonW - 6 // 6 = three 2-space separators
	if titleW < 10 {
		titleW = 10
	}

	// Header row. No leading indent: the data rows start at column 0 with the
	// State badge (the tree indentation lives inside the Title column), so the
	// header must start there too to stay aligned.
	header := fmt.Sprintf("%-*s  %-*s  %-*s  %s",
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
//
// Layout: the State and ID columns are fixed-width and stay aligned at every
// depth; the hierarchy is drawn in the Title column via ├─ / └─ / │ tree
// connectors plus the ▼ / ▶ expander. Column text is padded by VISUAL width
// (padRight, rune-aware) rather than fmt's byte-based %-*s so styled/box-drawing
// runes never skew alignment — the padding-vs-ANSI bug that made the old queue
// read as an unaligned flat jumble.
func (m *queueModel) renderRow(row flatRow, selected bool, stateW, idW, titleW, reasonW int) string {
	node := row.node
	iss := node.Issue

	badge := padRight(stateBadgeText(node.State), stateW)
	id := padRight(truncate(iss.ID, idW), idW)

	// Tree connectors + expander live inside the Title column so State/ID stay
	// column-aligned with the header across all depths.
	prefix := treeConnector(row.ancestorsLast, row.isLast)
	expander := "  "
	if row.hasChildren {
		if row.expanded {
			expander = "▼ "
		} else {
			expander = "▶ "
		}
	}
	titleText := iss.Title
	if node.State == cockpit.QueueStateContainer && row.hasChildren {
		titleText = fmt.Sprintf("%s (%d)", iss.Title, len(node.Children))
	}
	titleAvail := titleW - lipglossLen(prefix) - lipglossLen(expander)
	if titleAvail < 4 {
		titleAvail = 4
	}
	title := padRight(prefix+expander+truncate(titleText, titleAvail), titleW)

	reason := truncate(node.Reason, reasonW)

	line := badge + "  " + id + "  " + title + "  " + reason

	if selected {
		return lipgloss.NewStyle().
			Background(m.theme.Blue).
			Foreground(m.theme.White).
			Render(line)
	}
	return m.stateStyle(node.State).Render(line)
}

// treeConnector builds the ├─ / └─ / │ prefix for a queue row from its ancestor
// last-sibling flags. Top-level rows (no ancestors) get NO connector: they have
// no parent, and drawing one made every root read as a child of some invisible
// node — the block of standalone tasks after the last epic looked like that
// epic's children, which is exactly the "epics on top, children below" misread
// (koryph-b01 follow-up). Ancestor levels contribute a vertical bar ("│  ")
// when that ancestor had following siblings, or blank ("   ") when it was the
// last — the standard `tree(1)` rendering.
func treeConnector(ancestorsLast []bool, isLast bool) string {
	if len(ancestorsLast) == 0 {
		return ""
	}
	var b strings.Builder
	for _, last := range ancestorsLast[1:] {
		if last {
			b.WriteString("   ")
		} else {
			b.WriteString("│  ")
		}
	}
	if isLast {
		b.WriteString("└─ ")
	} else {
		b.WriteString("├─ ")
	}
	return b.String()
}

// padRight pads s with spaces to w visual columns, measured by lipglossLen
// (runes, ignoring ANSI) so multi-byte box-drawing characters and any styling
// do not throw off alignment. Returns s unchanged when already at least w wide.
func padRight(s string, w int) string {
	n := lipglossLen(s)
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// stateBadgeText returns the plain (unstyled) state badge label. Coloring is
// applied to the whole row by stateStyle, so keeping this plain lets padRight
// align the column correctly.
func stateBadgeText(state cockpit.QueueNodeState) string {
	switch state {
	case cockpit.QueueStateRunning:
		return "running"
	case cockpit.QueueStateReady:
		return "ready"
	case cockpit.QueueStateDepBlocked:
		return "dep-blocked"
	case cockpit.QueueStateFootprintDeferred:
		return "fp-deferred"
	case cockpit.QueueStateResourceDeferred:
		return "res-deferred"
	case cockpit.QueueStateHuman:
		return "human"
	case cockpit.QueueStateDeferredUntil:
		return "deferred"
	case cockpit.QueueStateParked:
		return "parked"
	case cockpit.QueueStateContainer:
		return "epic"
	default:
		return string(state)
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
	case cockpit.QueueStateResourceDeferred:
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
	if node.ResourceKind != "" {
		lines = append(lines, field("Resource:", node.ResourceKind))
	}
	if node.ResourceHolder != "" {
		lines = append(lines, field("Held by:", node.ResourceHolder))
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
// snapshot, respecting the grouping mode (koryph-vy8), the expanded set, the
// state filter, and the metadata query (koryph-166).
func (m *queueModel) rebuildRows() {
	m.rows = m.rows[:0]
	switch m.mode {
	case queueModeEpics:
		roots := m.visibleNodes(m.snap.Queue.Roots)
		for i, root := range roots {
			m.flattenNode(root, 0, nil, i == len(roots)-1)
		}
	default:
		// Flat modes: every node in one list, sorted by priority then id;
		// issues mode additionally drops container rows (epics/features and
		// container-tasks — nothing directly workable there).
		var flat []cockpit.QueueNode
		var collect func(nodes []cockpit.QueueNode)
		collect = func(nodes []cockpit.QueueNode) {
			for _, n := range nodes {
				dropContainer := m.mode == queueModeIssues && n.State == cockpit.QueueStateContainer
				if m.nodeVisible(n) && !dropContainer {
					flat = append(flat, n)
				}
				collect(n.Children)
			}
		}
		collect(m.snap.Queue.Roots)
		sort.SliceStable(flat, func(i, j int) bool {
			if flat[i].Issue.Priority != flat[j].Issue.Priority {
				return flat[i].Issue.Priority < flat[j].Issue.Priority
			}
			return flat[i].Issue.ID < flat[j].Issue.ID
		})
		for i, n := range flat {
			m.rows = append(m.rows, flatRow{
				node:   n,
				isLast: i == len(flat)-1,
				// hasChildren stays false: flat modes never nest or fold.
			})
		}
	}
}

// nodeVisible reports whether one node itself passes the state filter AND the
// metadata query (koryph-166).
func (m *queueModel) nodeVisible(n cockpit.QueueNode) bool {
	return m.stateVisible(n.State) && matchQueueQuery(n, m.query)
}

// subtreeVisible reports whether n or any descendant is visible — an ancestor
// chain must stay on screen so a matching descendant keeps its grouping.
func (m *queueModel) subtreeVisible(n cockpit.QueueNode) bool {
	if m.nodeVisible(n) {
		return true
	}
	for _, c := range n.Children {
		if m.subtreeVisible(c) {
			return true
		}
	}
	return false
}

// visibleNodes filters a sibling slice to those that will render: a node
// shows when it (or any descendant) passes the state filter and metadata
// query — a container earns its row from its matching descendants, and one
// whose whole subtree is filtered out hides with it. Computing the rendered
// set up front lets flattenNode assign correct last-sibling flags for the
// tree connectors even when some siblings are filtered out.
func (m *queueModel) visibleNodes(nodes []cockpit.QueueNode) []cockpit.QueueNode {
	out := make([]cockpit.QueueNode, 0, len(nodes))
	for _, n := range nodes {
		if m.subtreeVisible(n) {
			out = append(out, n)
		}
	}
	return out
}

// anyCollapsed reports whether any container in the tree is currently
// collapsed — the F expand/collapse-all toggle's pivot (koryph-vy8). An
// unset entry counts as expanded (flattenNode's default).
func (m *queueModel) anyCollapsed(nodes []cockpit.QueueNode) bool {
	for _, n := range nodes {
		if len(n.Children) > 0 {
			if set := m.expandedSet(n.Issue.ID); set && !m.expanded[n.Issue.ID] {
				return true
			}
			if m.anyCollapsed(n.Children) {
				return true
			}
		}
	}
	return false
}

// matchQueueQuery reports whether a node satisfies every whitespace-separated
// term of the metadata query (koryph-166). Terms:
//
//	label:<substr>  any label contains substr
//	type:<t>        issue type equals t
//	state:<s>       queue state contains s
//	p:<n>           priority equals n (P<n> also accepted)
//	<text>          id or title contains text
//
// All matching is case-insensitive; an empty query matches everything.
func matchQueueQuery(n cockpit.QueueNode, query string) bool {
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if !matchQueueTerm(n, term) {
			return false
		}
	}
	return true
}

func matchQueueTerm(n cockpit.QueueNode, term string) bool {
	switch {
	case strings.HasPrefix(term, "label:"):
		want := strings.TrimPrefix(term, "label:")
		for _, l := range n.Issue.Labels {
			if strings.Contains(strings.ToLower(l), want) {
				return true
			}
		}
		return false
	case strings.HasPrefix(term, "type:"):
		return strings.EqualFold(n.Issue.IssueType, strings.TrimPrefix(term, "type:"))
	case strings.HasPrefix(term, "state:"):
		return strings.Contains(strings.ToLower(string(n.State)), strings.TrimPrefix(term, "state:"))
	case strings.HasPrefix(term, "p:"):
		want := strings.TrimPrefix(strings.TrimPrefix(term, "p:"), "p")
		return fmt.Sprintf("%d", n.Issue.Priority) == want
	default:
		return strings.Contains(strings.ToLower(n.Issue.ID), term) ||
			strings.Contains(strings.ToLower(n.Issue.Title), term)
	}
}

// flattenNode appends visible rows for node and (if expanded) its subtree.
// ancestorsLast/isLast carry the tree-connector context (see flatRow).
func (m *queueModel) flattenNode(node cockpit.QueueNode, depth int, ancestorsLast []bool, isLast bool) {
	hasKids := len(node.Children) > 0
	isExpanded := m.expanded[node.Issue.ID]

	// Containers (epics) default to expanded unless explicitly collapsed.
	if hasKids && !m.expandedSet(node.Issue.ID) {
		isExpanded = true
		m.expanded[node.Issue.ID] = true
	}

	row := flatRow{
		depth:         depth,
		node:          node,
		hasChildren:   hasKids,
		expanded:      isExpanded,
		ancestorsLast: append([]bool(nil), ancestorsLast...),
		isLast:        isLast,
	}
	m.rows = append(m.rows, row)

	if isExpanded {
		kids := m.visibleNodes(node.Children)
		childAncestors := append(append([]bool(nil), ancestorsLast...), isLast)
		for j, child := range kids {
			m.flattenNode(child, depth+1, childAncestors, j == len(kids)-1)
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
		return state == cockpit.QueueStateDepBlocked ||
			state == cockpit.QueueStateFootprintDeferred ||
			state == cockpit.QueueStateResourceDeferred
	case queueFilterDeferred:
		return state == cockpit.QueueStateFootprintDeferred ||
			state == cockpit.QueueStateResourceDeferred ||
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
