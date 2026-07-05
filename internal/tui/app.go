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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
)

const (
	// refreshInterval is the poll period for snapshot refreshes when agents are
	// running — below the 150 ms perceived-latency target from the design doc.
	refreshInterval = 100 * time.Millisecond

	// idleRefreshInterval is the poll period when no slots are running or
	// dispatching. Throttling to 1 s keeps CPU usage negligible when idle.
	idleRefreshInterval = 1 * time.Second

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

// detailBackMsg is emitted by the Detail tab to request returning to the
// tab that was active before the detail panel was opened.
type detailBackMsg struct{}

// nudgeRequestMsg is sent by a tab (events) to request a nudge operation.
// The App handles it and routes to the active project's ledger.
type nudgeRequestMsg struct {
	BeadID  string
	Message string
}

// drainRequestMsg is sent by a tab (events) to request a drain on the active project.
type drainRequestMsg struct{}

// actionResultMsg carries the result of a nudge or drain operation.
type actionResultMsg struct {
	Err error
	Msg string
}

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

	// readOnly disables write actions (nudge, drain) — set by --read-only flag.
	readOnly bool

	// UI components.
	help      help.Model
	showHelp  bool
	keys      KeyMap
	theme     Theme
	lastError string // non-empty on the last failed action or refresh; cleared on next snapshot
	lastInfo  string // non-empty on the last succeeded action; shown with ✓ (no warning glyph)

	// last snapshot.
	snap cockpit.Snapshot

	// terminal dimensions.
	width  int
	height int

	// detailTabIdx is the index of the Detail tab in tabs (set in NewApp).
	// -1 means no Detail tab is registered.
	detailTabIdx int

	// prevTabIdx is the tab index that was active before opening the Detail tab.
	// Restored when detailBackMsg is received.
	prevTabIdx int

	// pendingBeadID is the beadID for which an async fetch is in flight.
	// detailReadyMsg is discarded if its snap.BeadID does not match this field,
	// preventing a stale late-arriving fetch from clobbering a newer selection.
	pendingBeadID string
}

// NewApp creates and initialises the App model.
//
// providers must contain at least one Provider. The first provider is the
// initially active project. readOnly disables write actions (nudge, drain).
func NewApp(providers []cockpit.Provider, readOnly bool) *App {
	if len(providers) == 0 {
		panic("tui.NewApp: at least one provider is required")
	}
	theme := DefaultTheme()
	h := help.New()
	h.ShowAll = false

	// Build tab models from the registry (already sorted by Order).
	tabs := make([]TabModel, len(tabRegistry))
	for i, def := range tabRegistry {
		tabs[i] = def.New(theme, readOnly)
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
		readOnly:     readOnly,
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
	return a.adaptiveTick()
}

// Update implements tea.Model.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd

	switch msg := msg.(type) {
	case tea.KeyMsg:
		// When the active tab has a text-input focused, bypass ALL global
		// key bindings and deliver the key directly to the tab. This prevents
		// 'q', 'r', 'p', Tab, and '?' from firing application-level actions
		// while the operator is typing a bead ID or nudge message.
		if len(a.tabs) > 0 && a.tabs[a.activeTab].IsCapturingInput() {
			var cmd tea.Cmd
			a, cmd = a.updateActiveTab(msg)
			cmds = append(cmds, cmd)
			break
		}

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
		cmds = append(cmds, a.doRefresh(), a.adaptiveTick())

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
			a.prevTabIdx = a.activeTab
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
			// Fetch fresh detail asynchronously (provider may have newer data
			// than the last Refresh snapshot; also covers the case where
			// snap.Detail.BeadID != msg.beadID, i.e. first navigation).
			a.pendingBeadID = msg.beadID
			cmds = append(cmds, a.doFetchDetail(msg.beadID))
		}

	case detailReadyMsg:
		// Discard stale results from a previously-selected bead; only apply
		// the detail if it matches the most-recently-requested beadID.
		if a.detailTabIdx >= 0 && msg.snap.BeadID == a.pendingBeadID {
			if dr, ok := a.tabs[a.detailTabIdx].(interface {
				SetDetail(cockpit.BeadDetailSnapshot)
			}); ok {
				dr.SetDetail(msg.snap)
			}
		}

	case tea.MouseMsg:
		// Route mouse events to the active tab so dep-row click handling in
		// the Detail tab (and any future tab) is reachable.
		var cmd tea.Cmd
		a, cmd = a.updateActiveTab(msg)
		cmds = append(cmds, cmd)

	case detailBackMsg:
		// Return to the tab that was active before the detail panel was opened.
		a.activeTab = a.prevTabIdx
		a.resizeTabs()

	case nudgeRequestMsg:
		cmds = append(cmds, a.doNudge(msg.BeadID, msg.Message))

	case drainRequestMsg:
		cmds = append(cmds, a.doDrain())

	case actionResultMsg:
		if msg.Err != nil {
			// Error: show via the warning channel (⚠); clear any prior success.
			a.lastError = msg.Err.Error()
			a.lastInfo = ""
		} else {
			// Success: route through the info channel (✓); clear any prior error.
			a.lastInfo = msg.Msg
			a.lastError = ""
		}
		// Also route to the active tab so it can update its in-tab result display.
		var cmd tea.Cmd
		a, cmd = a.updateActiveTab(msg)
		cmds = append(cmds, cmd)
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
	switch {
	case a.lastError != "":
		errPart = "  ⚠ " + truncate(a.lastError, 40)
	case a.lastInfo != "":
		errPart = "  ✓ " + truncate(a.lastInfo, 40)
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

// doNudge returns a Cmd that appends text to a bead's INBOX.md, mirroring
// cmdNudge's live-bead path (koryph-o72). If the bead is not dispatched, the
// result message instructs the operator to use koryph nudge from the CLI.
func (a App) doNudge(beadID, text string) tea.Cmd {
	// Enforce read-only mode here as well as in the tab key handler so that
	// future message sources (shortcuts, scripted tests) cannot bypass the check.
	if a.readOnly {
		return func() tea.Msg {
			return actionResultMsg{Err: fmt.Errorf("nudge: disabled in --read-only mode")}
		}
	}
	// Validate beadID to prevent path traversal: reject empty values, paths
	// containing directory separators, and dot-leading names.
	if beadID == "" ||
		strings.ContainsRune(beadID, '/') ||
		strings.ContainsRune(beadID, '\\') ||
		strings.HasPrefix(beadID, ".") {
		return func() tea.Msg {
			return actionResultMsg{Err: fmt.Errorf("nudge: invalid bead ID %q", beadID)}
		}
	}
	repoRoot := a.providers[a.projectIdx].RepoRoot()
	runID := a.snap.RunID
	return func() tea.Msg {
		if runID == "" {
			return actionResultMsg{Err: fmt.Errorf("nudge: no active run for this project")}
		}
		// Check whether the bead has a live slot in the snapshot.
		var dispatched bool
		for _, sl := range a.snap.Slots {
			if sl.PhaseID == beadID || sl.BeadID == beadID {
				dispatched = true
				break
			}
		}
		if !dispatched {
			return actionResultMsg{
				Msg: fmt.Sprintf("nudge: %s not dispatched — use 'koryph nudge' to reach queued beads", beadID),
			}
		}
		phaseDir := filepath.Join(paths.KoryphRoot(repoRoot), runID, beadID)
		if err := os.MkdirAll(phaseDir, 0o755); err != nil {
			return actionResultMsg{Err: fmt.Errorf("nudge: mkdir: %w", err)}
		}
		entry := fmt.Sprintf("\n---\n[%s] operator (tui):\n%s\n",
			time.Now().UTC().Format(time.RFC3339), text)
		inboxPath := filepath.Join(phaseDir, "INBOX.md")
		f, err := os.OpenFile(inboxPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return actionResultMsg{Err: fmt.Errorf("nudge: open INBOX: %w", err)}
		}
		if _, werr := f.WriteString(entry); werr != nil {
			f.Close()
			return actionResultMsg{Err: fmt.Errorf("nudge: write: %w", werr)}
		}
		if cerr := f.Close(); cerr != nil {
			return actionResultMsg{Err: fmt.Errorf("nudge: close: %w", cerr)}
		}
		return actionResultMsg{Msg: fmt.Sprintf("nudged %s", beadID)}
	}
}

// doDrain returns a Cmd that writes the drain sentinel for the active project,
// mirroring cmdDrain's requestDrain path (koryph-57v.1).
func (a App) doDrain() tea.Cmd {
	// Enforce read-only mode here as well as in the tab key handler.
	if a.readOnly {
		return func() tea.Msg {
			return actionResultMsg{Err: fmt.Errorf("drain: disabled in --read-only mode")}
		}
	}
	repoRoot := a.providers[a.projectIdx].RepoRoot()
	projectID := a.providers[a.projectIdx].ProjectID()
	return func() tea.Msg {
		if err := ledger.NewStore(repoRoot).RequestDrain(); err != nil {
			return actionResultMsg{Err: fmt.Errorf("drain: %w", err)}
		}
		return actionResultMsg{Msg: fmt.Sprintf("drain requested for %s", projectID)}
	}
}

// doRefresh returns a Cmd that reads a fresh snapshot from the active provider.
// When the new snapshot is semantically identical to the previous one
// (snapshotUnchanged), it returns nil — BubbleTea drops nil messages,
// suppressing an unnecessary re-render.
func (a App) doRefresh() tea.Cmd {
	p := a.providers[a.projectIdx]
	prev := a.snap // captured by value for change detection
	return func() tea.Msg {
		snap, err := p.Refresh()
		if err != nil {
			return errMsg{err}
		}
		if snapshotUnchanged(prev, snap) {
			return nil // nothing changed — suppress re-render
		}
		return snapshotMsg(snap)
	}
}

// doFetchDetail returns a Cmd that calls BeadDetail asynchronously on the
// active provider (if it implements DetailProvider) and delivers the result
// as a detailReadyMsg.
func (a App) doFetchDetail(beadID string) tea.Cmd {
	p := a.providers[a.projectIdx]
	dp, ok := p.(cockpit.DetailProvider)
	if !ok {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap := dp.BeadDetail(ctx, beadID, time.Now())
		return detailReadyMsg{snap: snap}
	}
}

// isIdle reports whether the project currently has no running or dispatching
// agents. Used to throttle the refresh interval when nothing is in flight.
func (a App) isIdle() bool {
	for _, sl := range a.snap.Slots {
		if sl.Stage == "running" || sl.Stage == "dispatching" {
			return false
		}
	}
	return true
}

// adaptiveTick returns a Cmd that fires a tickMsg after refreshInterval when
// agents are running, or idleRefreshInterval when the project is idle. This
// keeps CPU usage low when no slots are active.
func (a App) adaptiveTick() tea.Cmd {
	interval := refreshInterval
	if a.isIdle() {
		interval = idleRefreshInterval
	}
	return tea.Tick(interval, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// snapshotUnchanged reports whether next is semantically identical to prev for
// TUI rendering purposes. CapturedAt is intentionally excluded — it changes
// every tick even when no data has changed.
func snapshotUnchanged(prev, next cockpit.Snapshot) bool {
	if prev.RunID != next.RunID || prev.RunStatus != next.RunStatus || prev.Wave != next.Wave {
		return false
	}
	if len(prev.Slots) != len(next.Slots) {
		return false
	}
	for i := range prev.Slots {
		ps, ns := prev.Slots[i], next.Slots[i]
		if ps.PhaseID != ns.PhaseID || ps.Stage != ns.Stage ||
			ps.StatusLine != ns.StatusLine || ps.StatusJSON != ns.StatusJSON ||
			ps.Attempt != ns.Attempt {
			return false
		}
	}
	return len(prev.Events.Events) == len(next.Events.Events)
}

// headerHeight is the number of fixed rows above the content area:
// header (1) + tab bar (1).
func headerHeight() int { return 2 }
