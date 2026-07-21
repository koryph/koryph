// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/cockpit"
)

// TestEpicRowWidthFitsTerminal is a regression test for the epic-burndown
// column-overflow bug: epicW's overhead subtraction previously undercounted
// the header/row separator chars (2-space leading indent + four 2-space gaps
// between 5 columns = 10, not 4), so every rendered line was 6 characters
// wider than the terminal — the epic title column ate the slack and pushed
// Remaining/Vel/Trend/ETA off the right edge. Asserts every rendered line's
// VISIBLE (ANSI-stripped) width never exceeds the terminal width, across a
// spread of widths and a long epic title / large day-count ETA that would
// have exposed both the header overhead bug and the unbounded ETA string.
func TestEpicRowWidthFitsTerminal(t *testing.T) {
	longTitle := strings.Repeat("a very long epic title that keeps going ", 4)
	snap := cockpit.Snapshot{
		Burndown: cockpit.BurndownSnapshot{
			Epics: []cockpit.EpicBurndown{
				{
					EpicID:         "epic-1",
					Title:          longTitle,
					Total:          100,
					Merged:         10,
					Remaining:      90,
					VelocityPerDay: 0.1, // slow — pushes the ETA day-count into the hundreds
					VelocityN:      10,
					ETAP50:         time.Now().Add(400 * 24 * time.Hour),
					ETAP90:         time.Now().Add(900 * 24 * time.Hour),
					Sparkline:      "▁▂▃▄▅▆▇█▁▂▃▄",
				},
			},
			AllEpicsSummary: cockpit.EpicBurndown{
				Title:     "all epics",
				Total:     100,
				Merged:    10,
				Remaining: 90,
			},
		},
	}

	for _, w := range []int{80, 100, 120, 160, 200} {
		m := newBurndownModel(DefaultTheme())
		m.Resize(w, 30)
		m.SetSnapshot(snap)

		// Scoped to the epic table itself (header + epic rows + summary row);
		// TestBacklogAndCostLinesFitTerminal covers the sibling free-text
		// sections separately.
		section := m.renderEpicSection(m.snap.Burndown, 20)
		for i, line := range strings.Split(section, "\n") {
			visible := stripANSI(line)
			if got := len([]rune(visible)); got > w {
				t.Errorf("width=%d line %d exceeds terminal width: got %d chars\n  %q", w, i, got, visible)
			}
		}
	}
}

// TestBacklogAndCostLinesFitTerminal is a regression test for the Backlog
// and Cost Burndown sections: both build a single free-text summary line by
// string concatenation with no width budget, so a long sparkline, a large
// day-count ETA, or large dollar figures could push a line past the
// terminal's right edge with nothing to clip it — the same class of bug as
// the epic-table overflow, but for prose lines instead of table columns.
// Both sections now route their output through clipLine (lipgloss.MaxWidth,
// ANSI-safe) before returning it.
func TestBacklogAndCostLinesFitTerminal(t *testing.T) {
	bd := cockpit.BurndownSnapshot{
		Backlog: cockpit.BacklogBurndown{
			Ready:               42,
			TotalRemaining:      999,
			CriticalPathHops:    123,
			ObservedParallelism: 7.5,
			ParallelismN:        30,
			DrainETA_P50:        time.Now().Add(400 * 24 * time.Hour),
			DrainETA_P90:        time.Now().Add(900 * 24 * time.Hour),
			InsufficientHistory: false,
			HistoryN:            30,
			Sparkline:           "▁▂▃▄▅▆▇█▁▂▃▄▁▂▃▄▅▆▇█▁▂▃▄",
		},
		Cost: cockpit.CostBurndown{
			RemainingBeads:      999,
			AvgCostPerBead:      1234.5678,
			ProjectedP50USD:     123456.78,
			ProjectedP90USD:     987654.32,
			WindowRemainingUSD:  12.34,
			WindowCeilingUSD:    56.78,
			Fit:                 cockpit.FitRed,
			InsufficientHistory: false,
		},
	}

	for _, w := range []int{80, 100, 120} {
		m := newBurndownModel(DefaultTheme())
		m.Resize(w, 30)

		for _, section := range []string{
			m.renderBacklogSection(bd),
			m.renderCostSection(bd),
		} {
			for i, line := range strings.Split(section, "\n") {
				visible := stripANSI(line)
				if got := len([]rune(visible)); got > w {
					t.Errorf("width=%d line %d exceeds terminal width: got %d chars\n  %q", w, i, got, visible)
				}
			}
		}
	}
}
