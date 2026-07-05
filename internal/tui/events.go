// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// events.go implements the Events tab for the koryph TUI cockpit (koryph-9af.5).
//
// The Events tab shows a bounded ring of live engine events (dispatches,
// merges, requeues, drain/resize/nudge operator actions) sourced from
// cockpit.EventsSnapshot. Events can be filtered by substring.
//
// Two safe write actions are available (disabled in --read-only mode):
//   - 'n' nudge: compose a message to a bead's INBOX.md.
//   - 'D' drain: graceful wind-down with a confirmation modal.
//
// Both actions fan through the same code paths as the CLI commands
// (koryph nudge / koryph drain) via the App's doNudge / doDrain Cmds.
package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

func init() {
	registerTab(TabDef{
		Name:  "Events",
		Order: 2,
		New:   func(theme Theme, readOnly bool) TabModel { return newEventsModel(theme, readOnly) },
	})
}

// eventsMode is the modal state of the events tab.
type eventsMode int

const (
	modeNormal       eventsMode = iota // scrollable feed
	modeFilter                         // filter text-input active
	modeNudgeBeadID                    // enter bead ID to nudge
	modeNudgeMessage                   // enter message text
	modeDrainConfirm                   // drain confirmation modal
)

// eventsModel is the Bubble Tea model for the Events tab.
type eventsModel struct {
	theme    Theme
	readOnly bool
	width    int
	height   int

	// snap is the latest snapshot; events are drawn from snap.Events.
	snap cockpit.Snapshot

	// viewport renders the scrollable events list.
	vp viewport.Model

	// filter is the current substring filter (empty = show all).
	filter string

	// filterInput is used when mode == modeFilter.
	filterInput textinput.Model

	// nudgeIDInput is used when mode == modeNudgeBeadID.
	nudgeIDInput textinput.Model

	// nudgeMsgInput is used when mode == modeNudgeMessage.
	nudgeMsgInput textinput.Model

	// mode is the current modal state.
	mode eventsMode

	// lastResult is the most recent action result line (shown in the footer).
	lastResult string
}

// newEventsModel creates a fresh events tab model.
func newEventsModel(theme Theme, readOnly bool) *eventsModel {
	fi := textinput.New()
	fi.Placeholder = "filter..."
	fi.CharLimit = 80

	ni := textinput.New()
	ni.Placeholder = "bead id"
	ni.CharLimit = 64

	nm := textinput.New()
	nm.Placeholder = "message to agent"
	nm.CharLimit = 512

	vp := viewport.New(80, 20)

	return &eventsModel{
		theme:         theme,
		readOnly:      readOnly,
		width:         80,
		height:        24,
		vp:            vp,
		filterInput:   fi,
		nudgeIDInput:  ni,
		nudgeMsgInput: nm,
	}
}

// Init implements TabModel.
func (m *eventsModel) Init() tea.Cmd { return nil }

// IsCapturingInput implements TabModel. Returns true when a text input is
// focused so App.Update bypasses global keybindings and delivers all keys
// to the input (preventing 'q', 'r', 'p', 'tab' from firing global actions
// while the operator is typing a bead ID or nudge message).
func (m *eventsModel) IsCapturingInput() bool {
	return m.mode != modeNormal && m.mode != modeDrainConfirm
}

// Update implements TabModel.
func (m *eventsModel) Update(msg tea.Msg) (TabModel, tea.Cmd) {
	// Always handle action results regardless of modal state.
	if ar, ok := msg.(actionResultMsg); ok {
		m.handleActionResult(ar)
		return m, nil
	}
	switch m.mode {
	case modeFilter:
		return m.updateFilter(msg)
	case modeNudgeBeadID:
		return m.updateNudgeBeadID(msg)
	case modeNudgeMessage:
		return m.updateNudgeMessage(msg)
	case modeDrainConfirm:
		return m.updateDrainConfirm(msg)
	default:
		return m.updateNormal(msg)
	}
}

// updateNormal handles key events in normal (scrolling) mode.
func (m *eventsModel) updateNormal(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "/":
			m.mode = modeFilter
			m.filterInput.SetValue(m.filter)
			m.filterInput.Focus()
			return m, textinput.Blink
		case "n":
			if m.readOnly {
				m.lastResult = "read-only mode: nudge disabled"
				return m, nil
			}
			m.mode = modeNudgeBeadID
			m.nudgeIDInput.Reset()
			m.nudgeIDInput.Focus()
			return m, textinput.Blink
		case "D":
			if m.readOnly {
				m.lastResult = "read-only mode: drain disabled"
				return m, nil
			}
			m.mode = modeDrainConfirm
			return m, nil
		default:
			var cmd tea.Cmd
			m.vp, cmd = m.vp.Update(msg)
			return m, cmd
		}
	}
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	return m, cmd
}

// updateFilter handles key events while the filter text-input is active.
func (m *eventsModel) updateFilter(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter", "esc":
			m.filter = m.filterInput.Value()
			m.filterInput.Blur()
			m.mode = modeNormal
			m.rebuildContent()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(msg)
	return m, cmd
}

// updateNudgeBeadID handles key events while the bead-ID text-input is active.
func (m *eventsModel) updateNudgeBeadID(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if strings.TrimSpace(m.nudgeIDInput.Value()) == "" {
				return m, nil // require non-empty
			}
			m.mode = modeNudgeMessage
			m.nudgeMsgInput.Reset()
			m.nudgeMsgInput.Focus()
			return m, textinput.Blink
		case "esc":
			m.mode = modeNormal
			m.nudgeIDInput.Blur()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.nudgeIDInput, cmd = m.nudgeIDInput.Update(msg)
	return m, cmd
}

// updateNudgeMessage handles key events while the nudge-message text-input is active.
func (m *eventsModel) updateNudgeMessage(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			beadID := strings.TrimSpace(m.nudgeIDInput.Value())
			text := strings.TrimSpace(m.nudgeMsgInput.Value())
			if text == "" {
				return m, nil
			}
			m.mode = modeNormal
			m.nudgeMsgInput.Blur()
			// Emit the request so the App executes doNudge.
			return m, func() tea.Msg {
				return nudgeRequestMsg{BeadID: beadID, Message: text}
			}
		case "esc":
			m.mode = modeNormal
			m.nudgeMsgInput.Blur()
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.nudgeMsgInput, cmd = m.nudgeMsgInput.Update(msg)
	return m, cmd
}

// updateDrainConfirm handles key events in the drain confirmation modal.
func (m *eventsModel) updateDrainConfirm(msg tea.Msg) (TabModel, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "D":
			m.mode = modeNormal
			return m, func() tea.Msg { return drainRequestMsg{} }
		case "esc", "q":
			m.mode = modeNormal
			return m, nil
		}
	}
	return m, nil
}

// SetSnapshot implements TabModel. Rebuilds the viewport content.
func (m *eventsModel) SetSnapshot(snap cockpit.Snapshot) {
	m.snap = snap
	m.lastResult = "" // clear stale result on data refresh
	m.rebuildContent()
}

// Resize implements TabModel.
func (m *eventsModel) Resize(w, h int) {
	m.width = w
	m.height = h
	// Reserve lines: header(1) + footer(2).
	vpH := h - 3
	if vpH < 3 {
		vpH = 3
	}
	m.vp.Width = w
	m.vp.Height = vpH
	m.rebuildContent()
}

// View implements TabModel.
func (m *eventsModel) View() string {
	switch m.mode {
	case modeDrainConfirm:
		return m.viewDrainConfirm()
	case modeNudgeBeadID, modeNudgeMessage:
		return m.viewNudgeModal()
	default:
		return m.viewNormal()
	}
}

// --- view renderers ----------------------------------------------------------

// viewNormal renders the events feed with optional filter footer.
func (m *eventsModel) viewNormal() string {
	var b strings.Builder

	// Section header.
	header := m.sectionTitle("Events")
	b.WriteString(header)
	b.WriteRune('\n')

	// Viewport (scrollable events list).
	b.WriteString(m.vp.View())
	b.WriteRune('\n')

	// Footer.
	b.WriteString(m.renderFooter())

	return b.String()
}

// viewNudgeModal renders the nudge compose modal.
func (m *eventsModel) viewNudgeModal() string {
	var b strings.Builder

	// Header stays visible.
	b.WriteString(m.sectionTitle("Nudge bead"))
	b.WriteRune('\n')

	// Show a few lines of the feed for context.
	b.WriteString(m.vp.View())
	b.WriteRune('\n')

	// Modal overlay — rendered as a bordered box.
	var modal strings.Builder
	if m.mode == modeNudgeBeadID {
		modal.WriteString(" Bead ID: ")
		modal.WriteString(m.nudgeIDInput.View())
	} else {
		modal.WriteString(" Bead: ")
		modal.WriteString(m.nudgeIDInput.Value())
		modal.WriteString("\n Message: ")
		modal.WriteString(m.nudgeMsgInput.View())
	}
	modal.WriteString("\n [Enter] send  [Esc] cancel")
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Blue).
		Padding(0, 1).
		Width(m.width - 4).
		Render(modal.String())
	b.WriteString(box)

	return b.String()
}

// viewDrainConfirm renders the drain confirmation modal.
func (m *eventsModel) viewDrainConfirm() string {
	var b strings.Builder
	b.WriteString(m.sectionTitle("Drain"))
	b.WriteRune('\n')
	b.WriteString(m.vp.View())
	b.WriteRune('\n')

	projectID := m.snap.ProjectID
	if projectID == "" {
		projectID = "(no project)"
	}
	modalContent := fmt.Sprintf(
		" Stop new dispatch for project %q.\n In-flight slots finish normally.\n\n [D] confirm drain  [Esc] cancel",
		projectID,
	)
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.theme.Warning).
		Padding(0, 1).
		Width(m.width - 4).
		Render(modalContent)
	b.WriteString(box)
	return b.String()
}

// renderFooter renders the single-line footer shown in normal mode.
func (m *eventsModel) renderFooter() string {
	// Filter indicator.
	filterPart := ""
	if m.mode == modeFilter {
		filterPart = "filter: " + m.filterInput.View() + "  "
	} else if m.filter != "" {
		filterPart = lipgloss.NewStyle().Foreground(m.theme.Accent).
			Render(fmt.Sprintf("filter: %q  ", m.filter))
	}

	// Result message (last action outcome).
	resultPart := ""
	if m.lastResult != "" {
		resultPart = lipgloss.NewStyle().Foreground(m.theme.Done).
			Render(truncate(m.lastResult, 50)) + "  "
	}

	// Action hints.
	dim := lipgloss.NewStyle().Foreground(m.theme.Inactive)
	act := lipgloss.NewStyle().Foreground(m.theme.Accent).Bold(true)
	hints := act.Render("/") + dim.Render(" filter")
	if !m.readOnly {
		hints += "  " + act.Render("n") + dim.Render(" nudge") +
			"  " + act.Render("D") + dim.Render(" drain")
	} else {
		hints += "  " + dim.Render("[read-only]")
	}

	line := filterPart + resultPart + hints
	return lipgloss.NewStyle().
		Foreground(m.theme.Gray).
		Background(lipgloss.Color("#111111")).
		Width(m.width).
		Render(line)
}

// rebuildContent rebuilds the viewport content from the current snapshot events,
// applying the active filter.
func (m *eventsModel) rebuildContent() {
	evs := m.snap.Events.Events
	if len(evs) == 0 {
		m.vp.SetContent(
			lipgloss.NewStyle().Foreground(m.theme.Inactive).
				Render("  no events yet — waiting for engine activity…"),
		)
		return
	}

	var lines []string
	filterLower := strings.ToLower(m.filter)
	for _, ev := range evs {
		line := m.renderEvent(ev)
		if filterLower == "" || strings.Contains(strings.ToLower(line), filterLower) {
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		m.vp.SetContent(
			lipgloss.NewStyle().Foreground(m.theme.Inactive).
				Render(fmt.Sprintf("  no events match filter %q", m.filter)),
		)
		return
	}
	m.vp.SetContent(strings.Join(lines, "\n"))
	// Scroll to bottom so newest events are visible.
	m.vp.GotoBottom()
}

// renderEvent formats one TUIEvent as a display line.
func (m *eventsModel) renderEvent(ev cockpit.TUIEvent) string {
	ts := ev.Time.Format("15:04:05")
	tsStyle := lipgloss.NewStyle().Foreground(m.theme.Gray)

	var kindStyle lipgloss.Style
	switch ev.Kind {
	case "dispatch":
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Running).Bold(true)
	case "merge":
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Done).Bold(true)
	case "requeue":
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Warning).Bold(true)
	case "drain":
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Error).Bold(true)
	case "cap-change", "resize":
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Accent).Bold(true)
	default:
		kindStyle = lipgloss.NewStyle().Foreground(m.theme.Inactive)
	}

	msg := truncate(ev.Message, m.width-12)
	return tsStyle.Render(ts) + "  " + kindStyle.Render(fmt.Sprintf("%-10s", ev.Kind)) + "  " + msg
}

// sectionTitle renders a coloured section divider line.
func (m *eventsModel) sectionTitle(title string) string {
	pad := m.width - len(title) - 4
	if pad < 0 {
		pad = 0
	}
	bar := "─ " + title + " " + strings.Repeat("─", pad)
	if len(bar) > m.width {
		bar = bar[:m.width]
	}
	return lipgloss.NewStyle().Foreground(m.theme.Accent).Render(bar)
}

// handleActionResult updates lastResult when an action result arrives.
// The App handles execution; the tab intercepts the result to display it
// inline in the footer so operators don't need to look at the status bar.
func (m *eventsModel) handleActionResult(msg actionResultMsg) {
	if msg.Err != nil {
		m.lastResult = "⚠ " + msg.Err.Error()
	} else {
		m.lastResult = "✓ " + msg.Msg
	}
	// Auto-clear after a few seconds by NOT clearing — it clears on next SetSnapshot.
}
