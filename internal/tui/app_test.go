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

// staticProvider is a test Provider that always returns the same snapshot.
type staticProvider struct {
	id   string
	snap cockpit.Snapshot
	err  error
}

func (p *staticProvider) ProjectID() string { return p.id }
func (p *staticProvider) Refresh() (cockpit.Snapshot, error) {
	if p.err != nil {
		return cockpit.Snapshot{}, p.err
	}
	return p.snap, nil
}

// newTestSnap returns a snapshot suitable for assertions in tests.
func newTestSnap() cockpit.Snapshot {
	now := time.Now()
	return cockpit.Snapshot{
		ProjectID: "test-project",
		RunID:     "20260704-100000",
		RunStatus: "running",
		Wave:      3,
		Slots: []cockpit.SlotSnapshot{
			{
				PhaseID:      "abc-1",
				Title:        "Add widget support",
				Stage:        "running",
				Model:        "claude-sonnet-4-5",
				Attempt:      1,
				PID:          12345,
				CostUSD:      0.042,
				EstimateUSD:  0.1,
				Elapsed:      5 * time.Minute,
				DispatchedAt: now.Add(-5 * time.Minute),
				StatusLine:   "writing tests",
				StatusJSON:   "implementing",
			},
			{
				PhaseID: "def-2",
				Title:   "Fix edge case in parser",
				Stage:   "merged",
				Model:   "claude-haiku-3-5",
				Attempt: 1,
				CostUSD: 0.011,
			},
		},
		Governor: cockpit.GovernorSnapshot{
			Pools: map[string]cockpit.PoolSnapshot{
				"anthropic": {
					Provider: "anthropic",
					Cap:      8,
					Dynamic:  6,
					Adaptive: true,
					Leases:   1,
				},
			},
		},
		CapturedAt: now,
	}
}

// waitFor is a helper that polls the model output until condition is true
// or a timeout expires. It fails the test on timeout.
func waitFor(t *testing.T, tm *teatest.TestModel, condition func([]byte) bool) {
	t.Helper()
	teatest.WaitFor(t, tm.Output(), condition,
		teatest.WithCheckInterval(10*time.Millisecond),
		teatest.WithDuration(3*time.Second),
	)
}

// TestAppQuit verifies q exits cleanly and the final output contains the header.
func TestAppQuit(t *testing.T) {
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))

	// Wait until the header renders.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "proj-1")
	})

	// Quit.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	tm.WaitFinished(t, teatest.WithFinalTimeout(3*time.Second))
}

// TestAppRendersHeader verifies the header renders with the project name and run info.
func TestAppRendersHeader(t *testing.T) {
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Header should contain the project name and wave number.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "proj-1") && strings.Contains(s, "wave 3")
	})
}

// TestAppMinSize verifies the too-small warning is shown when the terminal
// is below 80×24.
func TestAppMinSize(t *testing.T) {
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(60, 20))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "too small")
	})
}

// TestAppHelp verifies the help overlay renders on "?".
func TestAppHelp(t *testing.T) {
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for initial render.
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})

	// Send "?" to toggle help.
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("?")})

	// The help overlay should mention quit or the key binding.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "quit") || strings.Contains(s, "help")
	})
}
