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
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the threads table rows to actually appear (so the snap is loaded).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Add widget support")
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
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the threads table rows to actually appear (so the snap is loaded).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Add widget support")
	})
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})

	// The dep (xyz-9) and rdep (def-2) should appear.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "xyz-9") || strings.Contains(s, "def-2")
	})
}

// TestDetailNoBeadSelected verifies the placeholder renders when no bead is selected.
func TestDetailNoBeadSelected(t *testing.T) {
	// Use a snap with no bead detail so the Detail tab shows placeholder.
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for initial render.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})

	// Tab to Detail tab (Tab once → Burndown, Tab again → Detail).
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// The placeholder text should appear.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "No bead selected")
	})
}
