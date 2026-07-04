// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package tui implements the koryph terminal cockpit using Bubble Tea v1.3.x.
//
// Architecture:
//   - App is the root Bubble Tea model: tab framework, project switcher,
//     help overlay, status bar, and refresh loop.
//   - Each tab is a TabModel stored in App.tabs, built once from tabRegistry
//     at construction time.  Adding a tab = adding one file with init().
//   - Only the active tab receives Update calls so inactive tabs are cheap.
//   - Data comes from cockpit.Provider (internal/cockpit), polled every
//     refreshInterval (default 100 ms) — below the 150 ms perceived-latency
//     target from the design doc.
//
// Minimum terminal floor: 80 × 24. If the terminal reports smaller, the TUI
// renders a warning and waits for a resize.
package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
)

const (
	// refreshInterval is the poll period for snapshot refreshes.
	refreshInterval = 100 * time.Millisecond

	// minWidth / minHeight define the smallest terminal the TUI supports.
	minWidth  = 80
	minHeight = 24
)

// tickMsg is the internal tick sent by the refresh timer.
type tickMsg time.Time

// snapshotMsg carries a freshly-loaded snapshot.
type snapshotMsg cockpit.Snapshot

// errMsg carries a non-fatal refresh error.
type errMsg struct{ err error }

// showDetailMsg tells the App to switch to the Detail tab and focus beadID.
type showDetailMsg struct{ beadID string }

// detailReadyMsg delivers a freshly-assembled BeadDetailSnapshot.
type detailReadyMsg struct{ snap cockpit.BeadDetailSnapshot }

// App is the root Bubble Tea model for the koryph terminal cockpit.
type App struct {
	// providers is the list of cockpit providers, one per project. The active
	// provider is providers[projectIdx].
	providers  []cockpit.Provider
	projectIdx int

	// activeTab is the index into tabs (and tabRegistry).
	activeTab int

	// tabs holds the live tab sub-models, one per registered tab definition,
	// in the same order as tabRegistry (sorted by TabDef.Order).
	tabs []TabModel

	// UI components.
	help      help.Model
	showHelp  bool
	keys      KeyMap
	theme     Theme
	lastError string

	// last snapshot.
	snap cockpit.Snapshot

	// terminal dimensions.
	width  int
	height int

	// detailTabIdx is the index of the Detail tab in tabs (set in NewApp).
	// -1 means no Detail tab is registered.
	detailTabIdx int
}

// NewApp creates and initialises the App model.
//
// providers must contain at least one Provider. The first provider is the
// initially active project.
func NewApp(providers []cockpit.Provider) *App {
	if len(providers) == 0 {
		panic("tui.NewApp: at least one provider is required")
	}
	theme := DefaultTheme()
	h := help.New()
	h.ShowAll = false

	// Build tab models from the registry (already sorted by Order).
	tabs := make([]TabModel, len(tabRegistry))
	for i, def := range tabRegistry {
		tabs[i] = def.New(theme)
	}

	// Find the detail tab index.
	detailIdx := -1
	for i, def := range tabRegistry {
		if def.Name == "Detail" {
			detailIdx = i
			break
		}
	}

	a := &App{
		providers:    providers,
		activeTab:    0,
		tabs:         tabs,
		help:         h,
		keys:         DefaultKeyMap(),
		theme:        theme,
		width:        minWidth,
		height:       minHeight,
		detailTabIdx: detailIdx,
	}
	return a
}

// Init implements tea.Model. It emits the first tick to kick off polling.
func (a App) Init() tea.Cmd {
	return tick()
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// Global keys are handled before tab-specific ones.
		switch {
		case key.Matches(msg, a.keys.Quit):
			return a, tea.Quit

		case key.Matches(msg, a.keys.Help):
			a.showHelp = !a.showHelp
			a.help.ShowAll = a.showHelp

		case key.Matches(msg, a.keys.NextTab):
			if len(a.tabs) > 0 {
				a.activeTab = (a.activeTab + 1) % len(a.tabs)
			}
			a.resizeTabs()

		case key.Matches(msg, a.keys.PrevTab):
			if len(a.tabs) > 0 {
				a.activeTab = (a.activeTab + len(a.tabs) - 1) % len(a.tabs)
			}
			a.resizeTabs()

		case key.Matches(msg, a.keys.NextProject):
			a.projectIdx = (a.projectIdx + 1) % len(a.providers)
			// Force an immediate refresh for the new project.
			cmds = append(cmds, a.doRefresh())

		case key.Matches(msg, a.keys.Reload):
			cmds = append(cmds, a.doRefresh())

		default:
			// Route to the active tab.
			var cmd tea.Cmd
			a, cmd = a.updateActiveTab(msg)
			cmds = append(cmds, cmd)
		}

	case tea.WindowSizeMsg:
		a.width = msg.Width
		a.height = msg.Height
		a.resizeTabs()
		a.help.Width = a.width

	case tickMsg:
		cmds = append(cmds, a.doRefresh(), tick())

	case snapshotMsg:
		a.snap = cockpit.Snapshot(msg)
		a.lastError = ""
		for i := range a.tabs {
			a.tabs[i].SetSnapshot(a.snap)
		}

	case errMsg:
		a.lastError = msg.err.Error()

	case showDetailMsg:
		// Switch to the Detail tab and push the beadID into it.
		if a.detailTabIdx >= 0 {
			a.activeTab = a.detailTabIdx
			a.resizeTabs()
			if setter, ok := a.tabs[a.detailTabIdx].(interface {
				SetBead(string)
			}); ok {
				setter.SetBead(msg.beadID)
			}
			// If the current snapshot already has detail for this bead, push it now.
			if a.snap.Detail.BeadID == msg.beadID {
				if dr, ok := a.tabs[a.detailTabIdx].(interface {
					SetDetail(cockpit.BeadDetailSnapshot)
				}); ok {
					dr.SetDetail(a.snap.Detail)
				}
			}
		}

	case detailReadyMsg:
		if a.detailTabIdx >= 0 {
			if dr, ok := a.tabs[a.detailTabIdx].(interface {
				SetDetail(cockpit.BeadDetailSnapshot)
			}); ok {
				dr.SetDetail(msg.snap)
			}
		}
	}

	return a, tea.Batch(cmds...)
}

// resizeTabs propagates the current terminal dimensions to all tab models.
func (a *App) resizeTabs() {
	contentH := a.height - headerHeight()
	for i := range a.tabs {
		a.tabs[i].Resize(a.width, contentH)
	}
}

// updateActiveTab routes a key message to the active tab sub-model.
func (a App) updateActiveTab(msg tea.Msg) (App, tea.Cmd) {
	if len(a.tabs) == 0 {
		return a, nil
	}
	updated, cmd := a.tabs[a.activeTab].Update(msg)
	if updated != nil {
		a.tabs[a.activeTab] = updated
	}
	return a, cmd
}

// View implements tea.Model.
func (a App) View() string {
	if a.width < minWidth || a.height < minHeight {
		return fmt.Sprintf(
			"\n  Terminal too small (%d×%d).\n  Resize to at least %d×%d.\n",
			a.width, a.height, minWidth, minHeight,
		)
	}

	var b strings.Builder

	// Header row: project name + run info.
	b.WriteString(a.renderHeader())
	b.WriteRune('\n')

	// Tab bar.
	b.WriteString(renderTabBar(a.activeTab, a.theme, a.width))
	b.WriteRune('\n')

	// Content area.
	contentH := a.height - headerHeight()
	_ = contentH

	if a.showHelp {
		b.WriteString(a.renderHelp())
	} else {
		b.WriteString(a.renderActiveTab())
	}

	// Status bar (pinned to last row via newlines is impractical in BT;
	// instead just append and let the terminal scroll).
	b.WriteString(a.renderStatusBar())

	return b.String()
}

// renderHeader renders the top header line.
func (a App) renderHeader() string {
	projectID := ""
	if len(a.providers) > 0 {
		projectID = a.providers[a.projectIdx].ProjectID()
	}
	runInfo := ""
	if a.snap.RunID != "" {
		runInfo = fmt.Sprintf("  run %s  wave %d  [%s]",
			a.snap.RunID, a.snap.Wave, a.snap.RunStatus)
	}
	title := fmt.Sprintf("koryph tui  project %s%s", projectID, runInfo)
	return a.theme.Header.Width(a.width).Render(title)
}

// renderActiveTab renders the currently-active tab content.
func (a App) renderActiveTab() string {
	if len(a.tabs) == 0 || a.activeTab >= len(a.tabs) {
		return ""
	}
	return a.tabs[a.activeTab].View()
}

// renderHelp renders the help overlay.
func (a App) renderHelp() string {
	helpView := a.help.View(a.keys)
	return a.theme.HelpBorder.Width(a.width - 4).Render(helpView)
}

// renderStatusBar renders the bottom status bar.
func (a App) renderStatusBar() string {
	slots := len(a.snap.Slots)
	// Count running slots.
	running := 0
	for _, sl := range a.snap.Slots {
		if sl.Stage == "running" || sl.Stage == "dispatching" {
			running++
		}
	}

	// Governor summary (first pool for now).
	govSummary := ""
	if len(a.snap.Governor.Pools) > 0 {
		for _, ps := range a.snap.Governor.Pools {
			govSummary = fmt.Sprintf("  gov %d/%d", ps.Leases, ps.Dynamic)
			break
		}
	}

	errPart := ""
	if a.lastError != "" {
		errPart = "  ⚠ " + truncate(a.lastError, 40)
	}

	helpHint := a.theme.HelpKey.Render("?") + a.theme.HelpDesc.Render(" help  ") +
		a.theme.HelpKey.Render("q") + a.theme.HelpDesc.Render(" quit")

	left := fmt.Sprintf("threads %d  running %d%s%s", slots, running, govSummary, errPart)
	right := lipgloss.NewStyle().
		Foreground(a.theme.Gray).
		Render(fmt.Sprintf("%s  %s", helpHint, a.snap.CapturedAt.Format("15:04:05")))

	gap := a.width - lipglossLen(left) - lipglossLen(right)
	if gap < 0 {
		gap = 0
	}
	line := a.theme.StatusBar.Width(a.width).Render(left + strings.Repeat(" ", gap) + right)
	return "\n" + line
}

// doRefresh returns a Cmd that reads a fresh snapshot from the active provider.
func (a App) doRefresh() tea.Cmd {
	p := a.providers[a.projectIdx]
	return func() tea.Msg {
		snap, err := p.Refresh()
		if err != nil {
			return errMsg{err}
		}
		return snapshotMsg(snap)
	}
}

// tick returns a Cmd that fires a tickMsg after refreshInterval.
func tick() tea.Cmd {
	return tea.Tick(refreshInterval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// headerHeight is the number of fixed rows above the content area:
// header (1) + tab bar (1).
func headerHeight() int { return 2 }
