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
	"github.com/koryph/koryph/internal/dispatch"
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

// stopRequestMsg is sent by the Threads tab to request a graceful stop of one
// phase's agent (SIGTERM to the process group; the engine parks the phase).
type stopRequestMsg struct {
	PhaseID string
	PID     int
}

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
	notice    string // persistent startup notice (e.g. bd too old); shown in the header

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
		activeTab:    firstVisibleTab(),
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

// SetNotice sets a persistent startup notice shown in the header (e.g. a
// too-old bd that silently degrades the queue). Empty clears it.
func (a *App) SetNotice(s string) { a.notice = s }

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
				a.activeTab = nextVisibleTab(a.activeTab, +1, len(a.tabs))
			}
			a.resizeTabs()

		case key.Matches(msg, a.keys.PrevTab):
			if len(a.tabs) > 0 {
				a.activeTab = nextVisibleTab(a.activeTab, -1, len(a.tabs))
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
		// Live tails (Detail's log/activity views) advance far faster than the
		// slot status fields that gate snapshotUnchanged: a running agent's
		// stream.jsonl grows with reasoning/tool deltas many times a second
		// while its status.json heartbeat only changes once per step. doRefresh
		// therefore suppresses most snapshotMsgs, so the tail must re-read its
		// file on every tick directly — otherwise it freezes on whatever was
		// captured when the tail was opened. RefreshTail is a no-op unless a
		// tail is actually open, so this is cheap when the overlay is closed.
		if len(a.tabs) > 0 {
			if tr, ok := a.tabs[a.activeTab].(interface{ RefreshTail() }); ok {
				tr.RefreshTail()
			}
		}

	case snapshotMsg:
		wasIdle := a.isIdle()
		a.snap = cockpit.Snapshot(msg)
		a.lastError = ""
		for i := range a.tabs {
			a.tabs[i].SetSnapshot(a.snap)
		}
		// Idle→active transition: the previously-queued slow tick may not fire
		// for up to idleRefreshInterval. Schedule a fast tick now so the threads
		// and status bar start updating at refreshInterval immediately.
		if wasIdle && !a.isIdle() {
			cmds = append(cmds, a.adaptiveTick())
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

	case stopRequestMsg:
		cmds = append(cmds, a.doStop(msg.PhaseID, msg.PID))

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
// The content area excludes ALL chrome — header, tab bar, AND status bar
// (koryph-b01 follow-up): budgeting only the top chrome let a tab that fills
// its full allotment (the Queue tab always does) overflow the terminal, and
// the OLDEST lines — the header and tab bar — scrolled off screen.
func (a *App) resizeTabs() {
	contentH := a.height - headerHeight() - statusBarHeight
	if contentH < 1 {
		contentH = 1
	}
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

	// Content area, hard-clipped to its budget (koryph-b01 follow-up):
	// a tab whose View miscounts and returns more lines than it was given
	// must lose its own bottom rows, never push the header/tab bar off the
	// top of the terminal.
	contentH := a.height - headerHeight() - statusBarHeight
	if contentH < 1 {
		contentH = 1
	}
	content := a.renderActiveTab()
	if a.showHelp {
		content = a.renderHelp()
	}
	b.WriteString(clipLines(content, contentH))

	// Status bar (pinned to last row via newlines is impractical in BT;
	// instead just append and let the terminal scroll).
	b.WriteString(a.renderStatusBar())

	return b.String()
}

// clipLines truncates s to at most n lines (trailing newline dropped), so a
// tab's output can never exceed the vertical budget it was handed.
func clipLines(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
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
	if a.notice != "" {
		title += "   ⚠ " + a.notice
	}
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

// renderStatusBar renders the bottom status bar: one de-duplicated line of
// fleet state. The old separate "threads N" and "gov L/D" readouts showed the
// same running count twice — leases track running slots — and the gov pair
// was picked from RANDOM map iteration, so it flickered between pools on
// every render (koryph TUI status-bar issue).
//
// "here N" vs "fleet G/C": the governor's leases/cap are machine-global —
// shared by every koryph project dispatching under the same account — so a
// bare "agents R/C" reading R (this project's running count) against C (the
// global cap) understated what R actually meant. The two are now shown
// separately and labeled: how many of THIS project's slots are running right
// now, and how much of the GLOBAL concurrency budget the whole fleet (every
// project sharing that account) is currently consuming.
func (a App) renderStatusBar() string {
	running := 0
	failed := 0
	for _, sl := range a.snap.Slots {
		switch sl.Stage {
		case "running", "dispatching":
			running++
		case "failed", "conflict", "blocked":
			failed++
		}
	}

	agentsPart := fmt.Sprintf("here %d", running)
	if ps, ok := a.snap.Governor.PrimaryPool(); ok && ps.Dynamic > 0 {
		agentsPart += fmt.Sprintf("  fleet %d/%d", ps.Leases, ps.Dynamic)
	}

	// Queue pulse: ready vs blocked counts from the queue snapshot.
	queuePart := ""
	if ready, blocked := queueStateCounts(a.snap.Queue.Roots); ready+blocked > 0 {
		queuePart = fmt.Sprintf("  ready %d  blocked %d", ready, blocked)
	}

	// Failures needing attention (terminal failed/conflict/blocked slots).
	failPart := ""
	if failed > 0 {
		failPart = "  " + lipgloss.NewStyle().Foreground(a.theme.Error).
			Render(fmt.Sprintf("✗ %d failed", failed))
	}

	quotaPart := a.renderQuotaStatus()

	errPart := ""
	switch {
	case a.lastError != "":
		errPart = "  ⚠ " + truncate(a.lastError, 40)
	case a.lastInfo != "":
		errPart = "  ✓ " + truncate(a.lastInfo, 40)
	}

	helpHint := a.theme.HelpKey.Render("?") + a.theme.HelpDesc.Render(" help  ") +
		a.theme.HelpKey.Render("q") + a.theme.HelpDesc.Render(" quit")

	left := agentsPart + queuePart + failPart + quotaPart + errPart
	right := lipgloss.NewStyle().
		Foreground(a.theme.Gray).
		Render(fmt.Sprintf("%s  %s", helpHint, formatTimestamp(a.snap.CapturedAt)))

	// Size the content to the style's INNER width — StatusBar carries
	// horizontal padding, so filling the full terminal width made the bar
	// wrap onto a second row, overflow the vertical budget, and scroll the
	// header off (koryph-b01 follow-up). MaxWidth is the ANSI-aware backstop;
	// left is styled, so rune-based truncate would cut inside an escape
	// sequence — MaxWidth alone does the clipping.
	inner := a.width - a.theme.StatusBar.GetHorizontalFrameSize()
	gap := inner - lipglossLen(left) - lipglossLen(right)
	if gap < 0 {
		gap = 0
	}
	line := a.theme.StatusBar.Width(a.width).MaxWidth(a.width).
		Render(left + strings.Repeat(" ", gap) + right)
	return "\n" + line
}

// renderQuotaStatus renders a compact "<runtime> 5h N% wk M%" segment per AI
// provider with a configured quota ceiling (cockpit.ProviderQuotaSnapshot).
// Different dispatched threads may run under different providers, each
// billed against its own rate limits — this renders one segment per entry,
// so a second provider's numbers appear alongside the first with no further
// change once its quota measurement lands. Providers with no ceiling
// configured at all are omitted here (the Efficiency tab has the full
// "run koryph quota calibrate" hint); this is the always-visible pulse.
func (a App) renderQuotaStatus() string {
	var parts []string
	for _, pq := range a.snap.Efficiency.ProviderQuotas {
		if pq.Window5hCeiling <= 0 && pq.WeeklyCeiling <= 0 {
			continue // uncalibrated
		}
		label := pq.Runtime
		if label == "" {
			label = pq.Provider
		}

		w5h := "—"
		if pq.Window5hFrac >= 0 && pq.Window5hCeiling > 0 {
			w5h = fmt.Sprintf("%.0f%%", pq.Window5hFrac*100)
		}
		wk := "—"
		if pq.WeeklyFrac >= 0 && pq.WeeklyCeiling > 0 {
			wk = fmt.Sprintf("%.0f%%", pq.WeeklyFrac*100)
		}

		// Colour by the worse of the two measurable fractions; gray when
		// neither window is currently measurable (ceiling set but no live
		// spend yet) — green would falsely read as "healthy".
		maxFrac := -1.0
		if pq.Window5hFrac > maxFrac {
			maxFrac = pq.Window5hFrac
		}
		if pq.WeeklyFrac > maxFrac {
			maxFrac = pq.WeeklyFrac
		}
		style := lipgloss.NewStyle().Foreground(a.theme.Gray)
		switch {
		case maxFrac >= 0.90:
			style = lipgloss.NewStyle().Foreground(a.theme.Error)
		case maxFrac >= 0.75:
			style = lipgloss.NewStyle().Foreground(a.theme.Warning)
		case maxFrac >= 0:
			style = lipgloss.NewStyle().Foreground(a.theme.Done)
		}

		text := fmt.Sprintf("%s 5h %s wk %s", label, w5h, wk)
		if label == "" {
			text = fmt.Sprintf("5h %s wk %s", w5h, wk)
		}
		parts = append(parts, style.Render(text))
	}
	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, "  ")
}

// queueStateCounts walks the queue tree and tallies dispatchable-state rows:
// ready (could run now) and blocked (dep-blocked or footprint/resource
// deferred — waiting on something the operator may be able to unblock).
func queueStateCounts(nodes []cockpit.QueueNode) (ready, blocked int) {
	for _, n := range nodes {
		switch n.State {
		case cockpit.QueueStateReady:
			ready++
		case cockpit.QueueStateDepBlocked,
			cockpit.QueueStateFootprintDeferred,
			cockpit.QueueStateResourceDeferred:
			blocked++
		}
		r, b := queueStateCounts(n.Children)
		ready += r
		blocked += b
	}
	return ready, blocked
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
		auditOperatorAction("nudge", a.providers[a.projectIdx].ProjectID(), map[string]any{"bead_id": beadID})
		return actionResultMsg{Msg: fmt.Sprintf("nudged %s", beadID)}
	}
}

// doStop returns a Cmd that gracefully stops one phase's agent, mirroring
// cmdStop's single-phase path: record the operator-stop sentinel FIRST (so
// the engine's death classification parks the phase instead of auto-retrying
// it into a race with the operator), then SIGTERM the process group. Never
// SIGKILL — uncommitted worktree work survives a graceful stop.
func (a App) doStop(phaseID string, pid int) tea.Cmd {
	if a.readOnly {
		return func() tea.Msg {
			return actionResultMsg{Err: fmt.Errorf("stop: disabled in --read-only mode")}
		}
	}
	repoRoot := a.providers[a.projectIdx].RepoRoot()
	return func() tea.Msg {
		if phaseID == "" {
			return actionResultMsg{Err: fmt.Errorf("stop: no phase selected")}
		}
		if pid <= 0 {
			return actionResultMsg{Err: fmt.Errorf("stop: %s has no live pid", phaseID)}
		}
		if err := ledger.NewStore(repoRoot).RequestStop(phaseID); err != nil {
			// Best-effort mirror of cmdStop: a sentinel failure must not block
			// the actual stop, but the operator should know retry semantics
			// may differ.
			if serr := dispatch.StopGraceful(pid); serr != nil {
				return actionResultMsg{Err: fmt.Errorf("stop %s: %v (and sentinel failed: %v)", phaseID, serr, err)}
			}
			return actionResultMsg{Msg: fmt.Sprintf("stopped %s (sentinel failed: %v)", phaseID, err)}
		}
		if err := dispatch.StopGraceful(pid); err != nil {
			return actionResultMsg{Err: fmt.Errorf("stop %s: %v", phaseID, err)}
		}
		return actionResultMsg{Msg: fmt.Sprintf("SIGTERM sent to %s — engine will park it", phaseID)}
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
		auditOperatorAction("drain", projectID, nil)
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
//
// The sub-snapshot ComputedAt fields (Burndown, Graph, Efficiency, Queue) act
// as cache-epoch stamps: they advance whenever the provider's TTL fires and
// new data is assembled. Comparing them is intentionally coarse — a false-
// positive re-render is cheap and safe; a false-negative (missing an update to
// Burndown/Queue/Efficiency/Graph) is not.
func snapshotUnchanged(prev, next cockpit.Snapshot) bool {
	// Top-level run state.
	if prev.RunID != next.RunID || prev.RunStatus != next.RunStatus || prev.Wave != next.Wave {
		return false
	}
	// Slots: count, identity, and per-slot status fields.
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
		// A slot crossing the stall threshold changes its rendering (⚠ flag,
		// title-bar count) with no other field moving — on an otherwise-quiet
		// system that transition must still force a repaint.
		if (ps.StatusAge > stallAfter) != (ns.StatusAge > stallAfter) {
			return false
		}
	}
	// Sub-snapshot cache epochs. These change whenever the provider's TTL fires
	// so any freshly-assembled Burndown/Graph/Efficiency/Queue data propagates.
	if !prev.Burndown.ComputedAt.Equal(next.Burndown.ComputedAt) {
		return false
	}
	if !prev.Graph.ComputedAt.Equal(next.Graph.ComputedAt) {
		return false
	}
	if !prev.Efficiency.ComputedAt.Equal(next.Efficiency.ComputedAt) {
		return false
	}
	if !prev.Queue.ComputedAt.Equal(next.Queue.ComputedAt) {
		return false
	}
	// Governor: compare total leases (agent starts/stops) and effective dynamic
	// cap (AIMD adjustments) — the two values shown in the status bar and the
	// Efficiency tab's governor section.
	if governorLeases(prev.Governor) != governorLeases(next.Governor) ||
		governorDynamic(prev.Governor) != governorDynamic(next.Governor) {
		return false
	}
	// Events: length check plus last-event identity check for the at-capacity
	// rotation case: when len == eventsRingMax the oldest event is dropped and
	// the newest changes, keeping the length constant but the content stale.
	if len(prev.Events.Events) != len(next.Events.Events) {
		return false
	}
	if n := len(next.Events.Events); n > 0 {
		pe := prev.Events.Events[n-1]
		ne := next.Events.Events[n-1]
		if !pe.Time.Equal(ne.Time) || pe.Kind != ne.Kind {
			return false
		}
	}
	return true
}

// governorLeases returns the total number of active leases across all pools.
func governorLeases(g cockpit.GovernorSnapshot) int {
	total := 0
	for _, p := range g.Pools {
		total += p.Leases
	}
	return total
}

// governorDynamic returns the sum of effective dynamic caps across all pools.
func governorDynamic(g cockpit.GovernorSnapshot) int {
	total := 0
	for _, p := range g.Pools {
		total += p.Dynamic
	}
	return total
}

// headerHeight is the number of fixed rows above the content area:
// header (1) + tab bar (1).
func headerHeight() int { return 2 }

// statusBarHeight is the number of fixed rows below the content area:
// renderStatusBar's leading blank line (1) + the bar itself (1).
const statusBarHeight = 2
