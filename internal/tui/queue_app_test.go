// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/cockpit"
)

// queueTreeSnap returns a snapshot whose queue carries two epics with
// children plus a standalone task.
func queueTreeSnap() cockpit.Snapshot {
	child := func(id, title string) cockpit.QueueNode {
		return cockpit.QueueNode{
			Issue: beads.Issue{ID: id, Title: title, IssueType: "task"},
			State: cockpit.QueueStateReady,
		}
	}
	return cockpit.Snapshot{
		ProjectID:  "proj-1",
		RunID:      "20260710-000000",
		RunStatus:  "running",
		CapturedAt: time.Now(),
		Queue: cockpit.QueueSnapshot{
			Roots: []cockpit.QueueNode{
				{
					Issue: beads.Issue{ID: "epic-aaa", Title: "First epic", IssueType: "epic"},
					State: cockpit.QueueStateContainer,
					Children: []cockpit.QueueNode{
						child("epic-aaa.1", "child a1"),
						child("epic-aaa.2", "child a2"),
					},
				},
				{
					Issue: beads.Issue{ID: "epic-bbb", Title: "Second epic", IssueType: "epic"},
					State: cockpit.QueueStateContainer,
					Children: []cockpit.QueueNode{
						child("epic-bbb.1", "child b1"),
					},
				},
				child("task-zzz", "standalone task"),
			},
			NodeCount:  6,
			ComputedAt: time.Now(),
		},
	}
}

var queueTestANSI = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

// TestQueueTabFullAppRender drives the REAL App synchronously — resize,
// snapshot delivery, tab keys — and asserts the two reported Queue-tab
// defects stay fixed: the header/tab bar must survive switching to Queue,
// and each epic's children must render directly under their epic.
func TestQueueTabFullAppRender(t *testing.T) {
	app := NewApp([]cockpit.Provider{&stubProvider{id: "proj-1"}}, true)
	var m tea.Model = *app

	step := func(msg tea.Msg) {
		next, _ := m.Update(msg)
		m = next
	}
	step(tea.WindowSizeMsg{Width: 120, Height: 40})
	step(snapshotMsg(queueTreeSnap()))

	// Tab until the Queue tab is active (bounded — fail loudly if never).
	var frame string
	found := false
	for i := 0; i < 10; i++ {
		step(tea.KeyMsg{Type: tea.KeyTab})
		frame = queueTestANSI.ReplaceAllString(m.View(), "")
		if strings.Contains(frame, "─ Queue") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("never reached the Queue tab; last frame:\n%s", frame)
	}

	// (1) Header and tab bar visible on the Queue tab.
	if !strings.Contains(frame, "koryph tui") {
		t.Errorf("header missing from the Queue frame:\n%s", frame)
	}
	if !strings.Contains(frame, "Threads") || !strings.Contains(frame, "Burndown") {
		t.Errorf("tab bar missing from the Queue frame:\n%s", frame)
	}

	// (2) The tree renders, interleaved parent→children.
	if strings.Contains(frame, "queue refreshing") {
		t.Fatalf("Queue rendered the empty view despite a populated snapshot:\n%s", frame)
	}
	idx := func(s string) int { return strings.Index(frame, s) }
	a, a1, a2, b, b1, z := idx("epic-aaa"), idx("epic-aaa.1"), idx("epic-aaa.2"),
		idx("epic-bbb"), idx("epic-bbb.1"), idx("task-zzz")
	for name, i := range map[string]int{
		"epic-aaa": a, "epic-aaa.1": a1, "epic-aaa.2": a2,
		"epic-bbb": b, "epic-bbb.1": b1, "task-zzz": z,
	} {
		if i < 0 {
			t.Fatalf("row %s missing from frame:\n%s", name, frame)
		}
	}
	if !(a < a1 && a1 < a2 && a2 < b && b < b1 && b1 < z) {
		t.Errorf("rows not interleaved parent→children (aaa=%d aaa.1=%d aaa.2=%d bbb=%d bbb.1=%d zzz=%d):\n%s",
			a, a1, a2, b, b1, z, frame)
	}

	// (3) Root rows carry NO tree connector — a connector on a root made the
	// standalone-task block after the last epic read as that epic's children.
	for _, line := range strings.Split(frame, "\n") {
		if strings.Contains(line, "task-zzz") || strings.Contains(line, "epic-aaa ") {
			if strings.Contains(line, "├─") || strings.Contains(line, "└─") {
				t.Errorf("root row draws a connector as if it had a parent: %q", line)
			}
		}
		if strings.Contains(line, "epic-aaa.1") && !strings.Contains(line, "├─") {
			t.Errorf("child row missing its connector: %q", line)
		}
	}
}

// TestQueueTabNeverScrollsChromeOff is the regression guard for the
// disappearing header/tab bar: a queue TALLER than the terminal must lose its
// own bottom rows, never the app chrome. Before the fix the App budgeted only
// the top chrome, so the Queue tab (the one tab that always fills its full
// allotment) overflowed the terminal and scrolled the header and tab bar off.
func TestQueueTabNeverScrollsChromeOff(t *testing.T) {
	snap := queueTreeSnap()
	// Inflate the queue far past the terminal height.
	for i := 0; i < 60; i++ {
		snap.Queue.Roots = append(snap.Queue.Roots, cockpit.QueueNode{
			Issue: beads.Issue{ID: fmt.Sprintf("task-%03d", i), Title: "filler", IssueType: "task"},
			State: cockpit.QueueStateReady,
		})
		snap.Queue.NodeCount++
	}

	app := NewApp([]cockpit.Provider{&stubProvider{id: "proj-1"}}, true)
	var m tea.Model = *app
	step := func(msg tea.Msg) {
		next, _ := m.Update(msg)
		m = next
	}
	const termH = 24
	step(tea.WindowSizeMsg{Width: 120, Height: termH})
	step(snapshotMsg(snap))

	var frame string
	for i := 0; i < 10; i++ {
		step(tea.KeyMsg{Type: tea.KeyTab})
		frame = queueTestANSI.ReplaceAllString(m.View(), "")
		if strings.Contains(frame, "─ Queue") {
			break
		}
	}

	lines := strings.Split(frame, "\n")
	if len(lines) > termH {
		t.Errorf("app rendered %d lines into a %d-row terminal — the top chrome scrolls off", len(lines), termH)
	}
	if !strings.Contains(lines[0], "koryph tui") {
		t.Errorf("first line is not the header: %q", lines[0])
	}
	if !strings.Contains(frame, "Threads") {
		t.Errorf("tab bar missing:\n%s", frame)
	}
	if !strings.Contains(lines[len(lines)-1], "q quit") {
		t.Errorf("last line is not the status bar: %q", lines[len(lines)-1])
	}
}
