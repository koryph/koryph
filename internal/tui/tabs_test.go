// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/cockpit"
)

// --- minimal Provider for registry tests ------------------------------------

type stubProvider struct{ id string }

func (p *stubProvider) ProjectID() string                  { return p.id }
func (p *stubProvider) RepoRoot() string                   { return "/tmp/stub-" + p.id }
func (p *stubProvider) Refresh() (cockpit.Snapshot, error) { return cockpit.Snapshot{}, nil }

// --- minimal TabModel for registry tests ------------------------------------

type stubTab struct {
	name  string
	calls []string
}

func (s *stubTab) Init() tea.Cmd                      { s.calls = append(s.calls, "init"); return nil }
func (s *stubTab) Update(tea.Msg) (TabModel, tea.Cmd) { return s, nil }
func (s *stubTab) View() string                       { return s.name }
func (s *stubTab) SetSnapshot(cockpit.Snapshot)       { s.calls = append(s.calls, "snap") }
func (s *stubTab) Resize(w, h int)                    { s.calls = append(s.calls, "resize") }
func (s *stubTab) IsCapturingInput() bool             { return false }

// --- registry tests ---------------------------------------------------------

// TestTabRegistryOrder verifies that registerTab sorts by Order, regardless of
// insertion order.
func TestTabRegistryOrder(t *testing.T) {
	// Snapshot and restore the global registry around this test.
	saved := tabRegistry
	tabRegistry = nil
	t.Cleanup(func() { tabRegistry = saved })

	registerTab(TabDef{Name: "C", Order: 2, New: func(Theme, bool) TabModel { return &stubTab{name: "C"} }})
	registerTab(TabDef{Name: "A", Order: 0, New: func(Theme, bool) TabModel { return &stubTab{name: "A"} }})
	registerTab(TabDef{Name: "B", Order: 1, New: func(Theme, bool) TabModel { return &stubTab{name: "B"} }})

	want := []string{"A", "B", "C"}
	for i, def := range tabRegistry {
		if def.Name != want[i] {
			t.Errorf("tabRegistry[%d].Name = %q, want %q", i, def.Name, want[i])
		}
	}
}

// TestTabRegistryStableSort verifies that ties in Order preserve insertion order.
func TestTabRegistryStableSort(t *testing.T) {
	saved := tabRegistry
	tabRegistry = nil
	t.Cleanup(func() { tabRegistry = saved })

	registerTab(TabDef{Name: "first", Order: 1})
	registerTab(TabDef{Name: "second", Order: 1})
	registerTab(TabDef{Name: "third", Order: 1})

	want := []string{"first", "second", "third"}
	for i, def := range tabRegistry {
		if def.Name != want[i] {
			t.Errorf("tabRegistry[%d].Name = %q, want %q", i, def.Name, want[i])
		}
	}
}

// TestNewAppBuildsFromRegistry verifies that NewApp constructs exactly one tab
// model per registered definition.
func TestNewAppBuildsFromRegistry(t *testing.T) {
	saved := tabRegistry
	tabRegistry = nil
	t.Cleanup(func() { tabRegistry = saved })

	var built []string
	for _, name := range []string{"X", "Y"} {
		n := name // capture
		registerTab(TabDef{
			Name:  n,
			Order: len(tabRegistry),
			New:   func(theme Theme, _ bool) TabModel { built = append(built, n); return &stubTab{name: n} },
		})
	}

	p := &stubProvider{id: "test"}
	app := NewApp([]cockpit.Provider{p}, false)

	if len(app.tabs) != 2 {
		t.Fatalf("len(app.tabs) = %d, want 2", len(app.tabs))
	}
	for i, name := range []string{"X", "Y"} {
		if app.tabs[i].View() != name {
			t.Errorf("app.tabs[%d].View() = %q, want %q", i, app.tabs[i].View(), name)
		}
	}
	if len(built) != 2 {
		t.Errorf("built = %v, want [X Y]", built)
	}
}

// TestRenderTabBar verifies that renderTabBar renders all registered tab names.
func TestRenderTabBar(t *testing.T) {
	saved := tabRegistry
	tabRegistry = nil
	t.Cleanup(func() { tabRegistry = saved })

	registerTab(TabDef{Name: "Alpha", Order: 0})
	registerTab(TabDef{Name: "Beta", Order: 1})

	theme := DefaultTheme()
	bar := renderTabBar(0, theme, 80)

	for _, name := range []string{"Alpha", "Beta"} {
		// Bar may include ANSI codes; check that the name text is present.
		if !containsRune(bar, name) {
			t.Errorf("renderTabBar output missing tab name %q", name)
		}
	}
}

// TestHiddenTabExcludedFromBarAndCycle verifies that a Hidden tab (the Detail
// overlay) is omitted from renderTabBar and is never landed on by
// nextVisibleTab cycling, while remaining reachable by index for programmatic
// switches.
func TestHiddenTabExcludedFromBarAndCycle(t *testing.T) {
	saved := tabRegistry
	tabRegistry = nil
	t.Cleanup(func() { tabRegistry = saved })

	registerTab(TabDef{Name: "Alpha", Order: 0})
	registerTab(TabDef{Name: "Beta", Order: 1})
	registerTab(TabDef{Name: "Ghost", Order: 99, Hidden: true})

	// Bar excludes the hidden tab.
	bar := renderTabBar(0, DefaultTheme(), 80)
	if containsRune(bar, "Ghost") {
		t.Errorf("renderTabBar leaked hidden tab: %q", stripANSI(bar))
	}
	for _, name := range []string{"Alpha", "Beta"} {
		if !containsRune(bar, name) {
			t.Errorf("renderTabBar missing visible tab %q", name)
		}
	}

	// Counts: 3 registered, 2 visible.
	registered := len(tabRegistry)
	visible := visibleTabCount()
	if registered != 3 || visible != 2 {
		t.Fatalf("counts: registered=%d visible=%d, want 3/2", registered, visible)
	}

	// A full forward cycle from the first visible tab visits only visible tabs
	// and returns to the start; the hidden index (2) is never produced.
	n := len(tabRegistry)
	cur := firstVisibleTab()
	if cur != 0 {
		t.Fatalf("firstVisibleTab = %d, want 0", cur)
	}
	visited := []int{}
	for i := 0; i < visible; i++ {
		cur = nextVisibleTab(cur, +1, n)
		if tabRegistry[cur].Hidden {
			t.Fatalf("nextVisibleTab landed on hidden tab index %d", cur)
		}
		visited = append(visited, cur)
	}
	// After VisibleTabCount() forward steps we are back at the start (0).
	if cur != 0 {
		t.Errorf("after full cycle cur = %d, want 0 (visited %v)", cur, visited)
	}

	// Reverse cycling likewise skips the hidden tab.
	if got := nextVisibleTab(0, -1, n); got != 1 {
		t.Errorf("nextVisibleTab(0,-1) = %d, want 1 (Beta, skipping hidden Ghost)", got)
	}
}

// visibleTabCount reports how many entries in tabRegistry are not Hidden.
// Test-only helper: production code has no need to count tabs.
func visibleTabCount() int {
	n := 0
	for _, def := range tabRegistry {
		if !def.Hidden {
			n++
		}
	}
	return n
}

// TestRealRegistryHasExactlyOneHiddenTab verifies the production tab
// registry (populated by every tab source file's init(), not the synthetic
// fixtures the other registry tests install) carries exactly one hidden
// overlay — the Detail panel. detail_test.go's TestDetailNotTabReachable
// relies on this invariant holding for the real app.
func TestRealRegistryHasExactlyOneHiddenTab(t *testing.T) {
	hidden := 0
	for _, def := range tabRegistry {
		if def.Hidden {
			hidden++
		}
	}
	if hidden != 1 {
		t.Fatalf("hidden tabs in production registry = %d, want 1 (Detail)", hidden)
	}
}

// containsRune returns true if sub (as a plain string) appears anywhere in s,
// ignoring ANSI escape sequences.
func containsRune(s, sub string) bool {
	// Strip ANSI for a simple contains check.
	plain := stripANSI(s)
	return len(sub) > 0 && contains(plain, sub)
}

func stripANSI(s string) string {
	out := make([]byte, 0, len(s))
	inEsc := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\x1b' {
			inEsc = true
			continue
		}
		if inEsc {
			if c == 'm' {
				inEsc = false
			}
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

func contains(s, sub string) bool {
	if len(sub) == 0 {
		return true
	}
	if len(sub) > len(s) {
		return false
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
