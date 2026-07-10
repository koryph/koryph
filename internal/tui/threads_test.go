// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/cockpit"
)

// TestThreadFilterMatches verifies the active/all/terminal filter predicate.
func TestThreadFilterMatches(t *testing.T) {
	active := cockpit.SlotSnapshot{Stage: "running", Terminal: false}
	terminal := cockpit.SlotSnapshot{Stage: "merged", Terminal: true}

	cases := []struct {
		filter          threadFilter
		wantActiveShown bool
		wantTermShown   bool
	}{
		{threadFilterActive, true, false},
		{threadFilterAll, true, true},
		{threadFilterDone, false, true},
	}
	for _, c := range cases {
		if got := c.filter.matches(active); got != c.wantActiveShown {
			t.Errorf("filter %s matches(active) = %v, want %v", c.filter.label(), got, c.wantActiveShown)
		}
		if got := c.filter.matches(terminal); got != c.wantTermShown {
			t.Errorf("filter %s matches(terminal) = %v, want %v", c.filter.label(), got, c.wantTermShown)
		}
	}
}

// TestThreadsFilterHidesTerminal verifies the default active filter hides a
// merged (terminal) slot, and cycling reveals it — the core of "why is a merged
// thread showing as active" being fixed.
func TestThreadsFilterHidesTerminal(t *testing.T) {
	m := newThreadsModel(DefaultTheme())
	m.Resize(120, 20)
	m.SetSnapshot(cockpit.Snapshot{Slots: []cockpit.SlotSnapshot{
		{BeadID: "live-1", Stage: "running", Terminal: false},
		{BeadID: "done-1", Stage: "merged", Terminal: true},
	}})

	// Default filter is active: only the running slot is visible.
	if len(m.visible) != 1 || m.visible[0].BeadID != "live-1" {
		t.Fatalf("active filter: expected only live-1 visible, got %v", visibleIDs(m.visible))
	}
	// Cycle to "all": both visible.
	m.filter = threadFilterAll
	m.rebuild()
	if len(m.visible) != 2 {
		t.Fatalf("all filter: expected 2 visible, got %v", visibleIDs(m.visible))
	}
	// Cycle to "terminal": only the merged slot.
	m.filter = threadFilterDone
	m.rebuild()
	if len(m.visible) != 1 || m.visible[0].BeadID != "done-1" {
		t.Fatalf("terminal filter: expected only done-1, got %v", visibleIDs(m.visible))
	}
}

func visibleIDs(sl []cockpit.SlotSnapshot) []string {
	ids := make([]string, len(sl))
	for i, s := range sl {
		ids[i] = s.BeadID
	}
	return ids
}

// TestRetrySummary verifies the retry/requeue breakdown column.
func TestRetrySummary(t *testing.T) {
	cases := []struct {
		name string
		sl   cockpit.SlotSnapshot
		want string
	}{
		{"clean first attempt", cockpit.SlotSnapshot{Attempt: 1}, "—"},
		{"one plain retry", cockpit.SlotSnapshot{Attempt: 2}, "×1"},
		{"gate retries", cockpit.SlotSnapshot{Attempt: 3, GateRequeues: 2}, "×2 g2"},
		{"rate-limit only", cockpit.SlotSnapshot{Attempt: 1, RateLimitRequeues: 1}, "×0 rl1"},
		{"mixed causes", cockpit.SlotSnapshot{Attempt: 3, GateRequeues: 1, MergeRequeues: 1}, "×2 g1,m1"},
		{"budget kill", cockpit.SlotSnapshot{Attempt: 2, BudgetKillRequeues: 1}, "×1 bk1"},
	}
	for _, c := range cases {
		if got := retrySummary(c.sl); got != c.want {
			t.Errorf("%s: retrySummary = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestModelWithEscalation verifies the escalation marker only appears when the
// ledger rationale records an escalation.
func TestModelWithEscalation(t *testing.T) {
	plain := cockpit.SlotSnapshot{Model: "claude-sonnet-4-5"}
	if got := modelWithEscalation(plain); got != "sonnet-4-5" {
		t.Errorf("plain model: got %q, want sonnet-4-5", got)
	}
	esc := cockpit.SlotSnapshot{Model: "claude-opus-4-8", ModelWhy: "escalated after gate failures"}
	if got := modelWithEscalation(esc); !strings.HasSuffix(got, "↑") {
		t.Errorf("escalated model: got %q, want trailing ↑", got)
	}
	// A non-escalation rationale must NOT add the marker.
	other := cockpit.SlotSnapshot{Model: "claude-opus-4-8", ModelWhy: "operator override"}
	if got := modelWithEscalation(other); strings.Contains(got, "↑") {
		t.Errorf("non-escalation rationale: got %q, should not have ↑", got)
	}
}

// TestThreadsBeadColumnNarrow verifies the Bead column is a narrow id column, a
// Description column is present, and the Status column still takes the bulk of
// the width (issue #5 + the short-description request).
func TestThreadsBeadColumnNarrow(t *testing.T) {
	cols := threadColumns(140)
	var beadW, descW, statusW int
	for _, c := range cols {
		switch c.Title {
		case "Bead":
			beadW = c.Width
		case "Description":
			descW = c.Width
		case "Status":
			statusW = c.Width
		}
	}
	if beadW > 18 {
		t.Errorf("Bead column too wide: %d (want ≤18)", beadW)
	}
	if descW <= 0 {
		t.Error("Description column missing")
	}
	if statusW <= descW {
		t.Errorf("Status column (%d) should be wider than Description (%d)", statusW, descW)
	}
}

// TestSlotToRow_Description verifies the Description cell shows the bead's short
// title, and blanks the id-fallback so the id is not repeated from the Bead
// column.
func TestSlotToRow_Description(t *testing.T) {
	// Real title present.
	row := slotToRow(cockpit.SlotSnapshot{BeadID: "koryph-2im.9", Title: "footprint persistence"}, 30, 30)
	if row[0] != "koryph-2im.9" {
		t.Errorf("Bead cell = %q, want the id", row[0])
	}
	if row[1] != "footprint persistence" {
		t.Errorf("Description cell = %q, want the title", row[1])
	}
	// Id-fallback title (provider could not resolve a real title) → blank.
	row = slotToRow(cockpit.SlotSnapshot{BeadID: "x-1", Title: "x-1"}, 30, 30)
	if row[1] != "" {
		t.Errorf("Description cell = %q, want blank when title == id", row[1])
	}
}
