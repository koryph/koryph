// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/koryph/koryph/internal/beads"
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
func (p *staticProvider) RepoRoot() string  { return "/tmp/test-" + p.id }
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
	app := tui.NewApp([]cockpit.Provider{p}, false)

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
	app := tui.NewApp([]cockpit.Provider{p}, false)

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
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(60, 20))
	defer func() { _ = tm.Quit() }()

	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "too small")
	})
}

// TestAppBurndownTab verifies that tab-switching reaches the Burndown tab and
// renders the expected section headers.
func TestAppBurndownTab(t *testing.T) {
	// Populate a snapshot with enough burndown data to exercise the sections.
	now := time.Now()
	snap := newTestSnap()
	snap.Burndown = cockpit.BurndownSnapshot{
		ComputedAt: now,
		Backlog: cockpit.BacklogBurndown{
			Ready:               3,
			TotalRemaining:      3,
			CriticalPathHops:    2,
			ObservedParallelism: 1.5,
			ParallelismN:        3,
			InsufficientHistory: false,
			HistoryN:            7,
			DrainETA_P50:        now.Add(5 * 24 * time.Hour),
			DrainETA_P90:        now.Add(10 * 24 * time.Hour),
			Sparkline:           "   ▃▅▇█",
		},
		Cost: cockpit.CostBurndown{
			RemainingBeads:  3,
			AvgCostPerBead:  2.50,
			ProjectedP50USD: 7.50,
			ProjectedP90USD: 11.25,
			Fit:             cockpit.FitGreen,
		},
		DurationStats: []cockpit.DurationStat{
			{Tier: "sonnet", N: 8, Mean: 18 * time.Minute, P50: 15 * time.Minute, P90: 42 * time.Minute},
			{Tier: "haiku", N: 2, Mean: 8 * time.Minute, P50: 7 * time.Minute, P90: 12 * time.Minute, Sparse: true},
		},
	}

	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for initial render (Threads tab).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})

	// Tab to the Burndown tab.
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	// Burndown tab should render section headers.
	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "Burndown") &&
			strings.Contains(s, "Backlog")
	})
}

// TestAppQueueTab verifies the Queue tab renders with the section header
// when a QueueSnapshot is populated.
func TestAppQueueTab(t *testing.T) {
	now := time.Now()
	snap := newTestSnap()
	snap.Queue = cockpit.QueueSnapshot{
		Roots: []cockpit.QueueNode{
			{
				Issue: beads.Issue{ID: "e1", Title: "Build TUI cockpit", IssueType: "epic", Status: "open"},
				State: cockpit.QueueStateContainer,
				Children: []cockpit.QueueNode{
					{
						Issue: beads.Issue{ID: "t1", Title: "Add widget support", IssueType: "task", Status: "open"},
						State: cockpit.QueueStateRunning,
					},
					{
						Issue:  beads.Issue{ID: "t2", Title: "Queue view", IssueType: "task", Status: "open"},
						State:  cockpit.QueueStateDepBlocked,
						Reason: "on t1",
					},
				},
			},
			{
				Issue: beads.Issue{ID: "t3", Title: "Standalone ready task", IssueType: "task", Status: "open"},
				State: cockpit.QueueStateReady,
			},
		},
		NodeCount:  4,
		ComputedAt: now,
	}

	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p})

	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Navigate to the Queue tab (tab 4×: Threads→Burndown→Detail→Efficiency→Queue).
	// Tab order: Threads(0) Burndown(1) Detail(2) Efficiency(3) Queue(4).
	waitFor(t, tm, func(bts []byte) bool {
		return strings.Contains(string(bts), "Threads")
	})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})
	tm.Send(tea.KeyMsg{Type: tea.KeyTab})

	waitFor(t, tm, func(bts []byte) bool {
		s := string(bts)
		return strings.Contains(s, "Queue")
	})
}

// TestAppHelp verifies the help overlay renders on "?".
func TestAppHelp(t *testing.T) {
	p := &staticProvider{id: "proj-1", snap: newTestSnap()}
	app := tui.NewApp([]cockpit.Provider{p}, false)

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
