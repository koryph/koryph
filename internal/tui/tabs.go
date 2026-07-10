// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
)

// TabModel is implemented by every registered tab sub-model.
// The interface mirrors tea.Model but keeps Update's return type as TabModel
// rather than tea.Model so the registry can store heterogeneous tab values
// without an additional type assertion on every Update call.
//
// Invariant: implementations must be safe to store as concrete pointer values
// in a []TabModel slice (i.e. use pointer receivers for all mutating methods).
type TabModel interface {
	// Init is called once on application start; may return an initial Cmd.
	Init() tea.Cmd
	// Update handles a Bubble Tea message. It returns the (possibly-modified)
	// model and an optional command. The returned model is stored back into the
	// tab slot; returning nil is treated as a no-op and preserves the previous
	// value.
	Update(tea.Msg) (TabModel, tea.Cmd)
	// View renders the tab content to a string.
	View() string
	// SetSnapshot pushes a freshly-assembled cockpit snapshot into the tab.
	// Called after every successful provider refresh.
	SetSnapshot(cockpit.Snapshot)
	// Resize updates the tab's content area dimensions. Called on every
	// tea.WindowSizeMsg and whenever the active tab changes.
	Resize(w, h int)
	// IsCapturingInput reports whether the tab currently has a text-input
	// focused. When true, App.Update bypasses global key bindings (quit, help,
	// tab-switch, project-cycle, reload) and routes the key directly to the tab
	// so that letters like 'q', 'r', 'p', and special chars in bead IDs are
	// delivered to the input rather than triggering application-level actions.
	// Tabs with no text inputs should always return false.
	IsCapturingInput() bool
}

// TabDef describes one registered TUI tab.
// Each tab source file populates tabRegistry from its init() by calling
// registerTab — identical to the cmd/koryph/cmdregistry.go pattern.
type TabDef struct {
	// Name is the display label shown in the tab bar.
	Name string
	// Order controls left-to-right tab position. Lower values appear first;
	// ties preserve insertion order (sort.SliceStable).
	Order int
	// Hidden omits the tab from the tab bar and from Tab/Shift-Tab cycling.
	// The tab model is still constructed and still receives snapshots, and the
	// App can switch to it programmatically (the Detail panel does this when the
	// operator presses Enter on a thread or queue row). It is a panel that is
	// reachable only by selecting a row — never a standalone destination — so
	// presenting it as a cyclable tab was dead UI (koryph TUI detail-tab issue).
	Hidden bool
	// New is the factory that constructs a fresh TabModel.
	// theme is the active color theme; readOnly disables write actions
	// (nudge, drain). Tabs that have no actions may ignore readOnly.
	// It is called exactly once per App initialisation.
	New func(theme Theme, readOnly bool) TabModel
}

// RegisteredTabCount reports how many tabs are registered — exported for
// tests that navigate by Tab presses so they derive counts from the registry
// instead of hardcoding today's sibling composition.
func RegisteredTabCount() int { return len(tabRegistry) }

// VisibleTabCount reports how many tabs appear in the tab bar and participate
// in Tab/Shift-Tab cycling (i.e. registered minus hidden overlays like Detail).
// Exported so navigation tests derive the cycle length from the registry rather
// than hardcoding it.
func VisibleTabCount() int {
	n := 0
	for _, def := range tabRegistry {
		if !def.Hidden {
			n++
		}
	}
	return n
}

// tabRegistry is the ordered list of registered tab definitions.
// Populated via registerTab; never written after init() completes.
var tabRegistry []TabDef

// registerTab appends def to tabRegistry and re-sorts by Order.
// It is called from init() functions in each tab source file; registration
// order does not affect correctness (tabs are re-sorted on every call).
func registerTab(def TabDef) {
	tabRegistry = append(tabRegistry, def)
	sort.SliceStable(tabRegistry, func(i, j int) bool {
		return tabRegistry[i].Order < tabRegistry[j].Order
	})
}

// renderTabBar renders the top tab bar highlighting the tab at activeIdx.
// activeIdx is an index into tabRegistry.
func renderTabBar(activeIdx int, theme Theme, width int) string {
	var parts []string
	for i, def := range tabRegistry {
		if def.Hidden {
			// Hidden panels (Detail) never appear in the bar. When one is the
			// active tab — reached by selecting a row — no bar entry is
			// highlighted, which reads correctly as a modal overlay.
			continue
		}
		label := fmt.Sprintf(" %s ", def.Name)
		if i == activeIdx {
			parts = append(parts, theme.TabActive.Render(label))
		} else {
			parts = append(parts, theme.TabInactive.Render(label))
		}
	}
	bar := strings.Join(parts, "")
	// Pad to full width.
	padding := width - lipglossLen(bar)
	if padding > 0 {
		bar += strings.Repeat(" ", padding)
	}
	return theme.TabBar.Render(bar)
}

// nextVisibleTab returns the index of the next non-hidden tab after cur,
// wrapping around. When no non-hidden tab other than cur exists it returns cur.
// dir must be +1 (Tab) or -1 (Shift-Tab).
func nextVisibleTab(cur, dir, n int) int {
	if n == 0 {
		return cur
	}
	for step := 1; step <= n; step++ {
		i := ((cur+dir*step)%n + n) % n
		if i >= 0 && i < len(tabRegistry) && !tabRegistry[i].Hidden {
			return i
		}
	}
	return cur
}

// firstVisibleTab returns the index of the first non-hidden tab, or 0 if every
// tab is hidden (which never happens — at least Threads is always visible).
func firstVisibleTab() int {
	for i, def := range tabRegistry {
		if !def.Hidden {
			return i
		}
	}
	return 0
}

// lipglossLen returns the visual width of a lipgloss-rendered string,
// stripping ANSI escape codes.
func lipglossLen(s string) int {
	n := 0
	inEsc := false
	for _, r := range s {
		switch {
		case r == '\x1b':
			inEsc = true
		case inEsc && r == 'm':
			inEsc = false
		case !inEsc:
			n++
		}
	}
	return n
}
