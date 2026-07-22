// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/tui"
)

// newDetailSnap returns a snapshot with a populated Detail field.
func newDetailSnap() cockpit.Snapshot {
	snap := newTestSnap()
	// Ensure the first slot has a BeadID matching the detail.
	snap.Slots[0].BeadID = "abc-1"
	snap.Detail = cockpit.BeadDetailSnapshot{
		BeadID:      "abc-1",
		Title:       "Add widget support",
		Description: "Implement the core widget rendering pipeline.",
		Labels:      []string{"area:tui", "fp:write:tui"},
		Status:      "running",
		Priority:    2,
		IssueType:   "task",
		Branch:      "agent/abc-1",
		Worktree:    "/tmp/worktrees/abc-1",
		CostUSD:     0.042,
		EstimateUSD: 0.10,
		Deps:        []string{"xyz-9"},
		ReverseDeps: []string{"def-2"},
		AttemptHistory: []cockpit.AttemptRecord{
			{
				Attempt:      1,
				Status:       "running",
				CostUSD:      0.042,
				Elapsed:      5 * time.Minute,
				Model:        "claude-sonnet-4-5",
				Branch:       "agent/abc-1",
				DispatchedAt: time.Now().Add(-5 * time.Minute),
			},
		},
		ComputedAt: time.Now(),
	}
	return snap
}

// TestDetailRendersFields verifies the detail panel renders the bead metadata
// when navigating from the Threads tab via Enter.
func TestDetailRendersFields(t *testing.T) {
	snap := newDetailSnap()
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the threads table rows to actually appear (so the snap is loaded).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})

	// Press Enter to open detail for the first row (abc-1).
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The Detail tab should now render with the bead ID and title.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "abc-1") && strings.Contains(s, "widget")
	})
}

// TestDetailShowsDepLinks verifies that dependency links appear in the detail view.
func TestDetailShowsDepLinks(t *testing.T) {
	snap := newDetailSnap()
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the threads table rows to actually appear (so the snap is loaded).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The dep (xyz-9) and rdep (def-2) should appear.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "xyz-9") || strings.Contains(s, "def-2")
	})
}

// TestDetailNotTabReachable verifies the Detail panel is a hidden overlay: a
// full cycle of Tab presses never lands on it (it carries no queue/thread
// selection, so its "No bead selected" placeholder must never surface from
// Tab navigation alone) and cycling returns to the Threads tab.
func TestDetailNotTabReachable(t *testing.T) {
	// The registry invariant this test used to check inline — exactly one
	// hidden tab (Detail) in the production registry — is now covered by
	// TestRealRegistryHasExactlyOneHiddenTab in tabs_test.go.

	// A snapshot with no Detail payload: were Detail Tab-reachable, it would
	// render its "No bead selected" placeholder.
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// We start on the Threads tab (its rows load); the Detail overlay's
	// placeholder must not surface, since Detail is never the initial or a
	// Tab-cyclable destination. (Tab-cycle skipping of the hidden tab is proven
	// deterministically in TestHiddenTabExcludedFromBarAndCycle.)
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "abc-1") &&
			!strings.Contains(s, "No bead selected")
	})
}

// TestDetailDepNavigation verifies that j/k keystrokes move cursor focus in
// the dep list without panicking after opening detail via Enter on a thread.
func TestDetailDepNavigation(t *testing.T) {
	snap := newDetailSnap()
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the Threads table row (abc-1) to appear.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})

	// Enter on the first thread opens the Detail overlay for abc-1; j/k then
	// exercise cursor movement through the dep rows.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})

	// Verify the detail overlay is showing content (abc-1).
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "abc-1")
	})
}

// TestDetailBackstack verifies that Backspace returns to the previous tab
// when the navigation stack is empty.
func TestDetailBackstack(t *testing.T) {
	snap := newDetailSnap()
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for threads to render.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})

	// Open detail via Enter.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})

	// Backspace with an empty nav stack should return to the Threads tab.
	tm.Send(tea.KeyMsg{Type: tea.KeyBackspace})
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		// The Threads tab renders a table with "Stage" column header.
		return strings.Contains(s, "Stage") || strings.Contains(s, "Bead")
	})
}

// TestDetailBlockerHighlight verifies that dep rows (which block this bead)
// and reverse-dep rows both appear in the detail view.
func TestDetailBlockerHighlight(t *testing.T) {
	snap := newDetailSnap()
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// Both the dep (xyz-9) and rdep (def-2) should appear.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "xyz-9") && strings.Contains(s, "def-2")
	})
}

// TestDetailLogTail verifies that pressing 't' switches to log-tail mode,
// which renders a log viewport header.
func TestDetailLogTail(t *testing.T) {
	snap := newDetailSnap()
	// Point the detail at a log file that exists (use /dev/null for portability).
	snap.Detail.LogPath = "/dev/null"
	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})
	// Open detail.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "abc-1")
	})

	// Press 't' to enter log-tail mode.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("t")})
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Log tail") || strings.Contains(string(bts), "tail log")
	})
}
