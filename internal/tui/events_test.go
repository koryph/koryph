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

// newEventsSnap returns a snapshot with a populated events feed.
func newEventsSnap() cockpit.Snapshot {
	now := time.Now()
	snap := newTestSnap()
	snap.Events = cockpit.EventsSnapshot{
		Events: []cockpit.TUIEvent{
			{
				Time:    now.Add(-10 * time.Minute),
				Kind:    "dispatch",
				Level:   "info",
				BeadID:  "abc-1",
				Message: "dispatch  abc-1  model sonnet-4",
			},
			{
				Time:    now.Add(-5 * time.Minute),
				Kind:    "merge",
				Level:   "info",
				BeadID:  "def-2",
				Message: "merged    def-2  $0.011",
			},
			{
				Time:    now.Add(-1 * time.Minute),
				Kind:    "requeue",
				Level:   "warn",
				BeadID:  "ghi-3",
				Message: "requeued  ghi-3  → conflict",
			},
		},
	}
	return snap
}

// tabToEvents navigates to the Events tab (Order 2) from the default Threads tab.
// Threads(0) → Burndown(1) → Events(2).
func tabToEvents(tm *teatest.TestModel) {
	tm.Send(tea.KeyMsg{Type: tea.KeyTab}) // Threads → Burndown
	tm.Send(tea.KeyMsg{Type: tea.KeyTab}) // Burndown → Events
}

// TestEventsTabRenders verifies the Events tab is reachable and renders its header.
func TestEventsTabRenders(t *testing.T) {
	p := &staticProvider{id: "proj-events", snap: newEventsSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for initial render.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})

	// Navigate to the Events tab.
	tabToEvents(tm)

	// The Events tab header and at least one event should appear.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "Events")
	})
}

// TestEventsTabShowsFeed verifies that events from the snapshot appear in the feed.
func TestEventsTabShowsFeed(t *testing.T) {
	p := &staticProvider{id: "proj-events", snap: newEventsSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})
	tabToEvents(tm)

	// At least one of the event kinds or bead IDs should appear in the feed.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "dispatch") ||
			strings.Contains(s, "merge") ||
			strings.Contains(s, "requeue")
	})
}

// TestEventsTabReadOnly verifies that read-only mode labels are shown.
func TestEventsTabReadOnly(t *testing.T) {
	p := &staticProvider{id: "proj-ro", snap: newEventsSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, true /* readOnly */)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})
	tabToEvents(tm)

	// The read-only indicator should appear in the footer.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "read-only")
	})
}

// TestEventsDrainModal verifies that pressing D opens the drain confirmation modal.
func TestEventsDrainModal(t *testing.T) {
	p := &staticProvider{id: "proj-drain", snap: newEventsSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})
	tabToEvents(tm)

	// Wait for Events tab.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Events")
	})

	// Press D to open drain modal.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("D")})

	// Drain modal should show a confirmation message.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "confirm drain") ||
			strings.Contains(s, "dispatch") && strings.Contains(s, "Drain")
	})

	// Press Esc to cancel — should return to normal without crashing.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})

	// Should still be on Events tab.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Events")
	})
}

// TestEventsFilterMode verifies that pressing / enters filter mode.
func TestEventsFilterMode(t *testing.T) {
	p := &staticProvider{id: "proj-filter", snap: newEventsSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})
	tabToEvents(tm)

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Events")
	})

	// Press / to enter filter mode.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})

	// Filter input should appear.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "filter")
	})

	// Press Esc to exit filter mode.
	tm.Send(tea.KeyMsg{Type: tea.KeyEsc})
}
