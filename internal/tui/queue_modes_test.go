// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/cockpit"
)

// modesSnap builds a queue with two epics (nested children of mixed priority
// and labels) plus a standalone task, for mode/filter assertions.
func modesSnap() cockpit.Snapshot {
	node := func(id, typ string, prio int, state cockpit.QueueNodeState, labels []string, kids ...cockpit.QueueNode) cockpit.QueueNode {
		return cockpit.QueueNode{
			Issue:    beads.Issue{ID: id, Title: "title " + id, IssueType: typ, Priority: prio, Labels: labels},
			State:    state,
			Children: kids,
		}
	}
	return cockpit.Snapshot{
		CapturedAt: time.Now(),
		Queue: cockpit.QueueSnapshot{
			Roots: []cockpit.QueueNode{
				node("epic-aaa", "epic", 1, cockpit.QueueStateContainer, nil,
					node("aaa.1", "task", 2, cockpit.QueueStateReady, []string{"area:engine"}),
					node("aaa.2", "task", 0, cockpit.QueueStateReady, nil),
				),
				node("epic-bbb", "epic", 3, cockpit.QueueStateContainer, nil,
					node("bbb.1", "task", 1, cockpit.QueueStateReady, []string{"area:cli"}),
				),
				node("task-zzz", "task", 2, cockpit.QueueStateReady, nil),
			},
			NodeCount:  6,
			ComputedAt: time.Now(),
		},
	}
}

func newQueueForTest(t *testing.T) *queueModel {
	t.Helper()
	m := newQueueModel(DefaultTheme())
	m.Resize(120, 30)
	m.SetSnapshot(modesSnap())
	return m
}

func rowIDs(m *queueModel) []string {
	ids := make([]string, 0, len(m.rows))
	for _, r := range m.rows {
		ids = append(ids, r.node.Issue.ID)
	}
	return ids
}

func press(m *queueModel, key string) {
	var msg tea.KeyMsg
	switch key {
	case "space":
		msg = tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		msg = tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		msg = tea.KeyMsg{Type: tea.KeyEsc}
	default:
		msg = tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)}
	}
	m.Update(msg)
}

func typeString(m *queueModel, s string) {
	for _, r := range s {
		m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
}

// TestQueueModes proves the koryph-vy8 grouping modes: epics (tree),
// priority (flat, sorted), issues (flat, containers dropped) — cycled by m.
func TestQueueModes(t *testing.T) {
	m := newQueueForTest(t)

	// Default: epic tree, children nested under their parents.
	want := []string{"epic-aaa", "aaa.1", "aaa.2", "epic-bbb", "bbb.1", "task-zzz"}
	if got := rowIDs(m); !equalStrings(got, want) {
		t.Fatalf("epics mode rows = %v, want %v", got, want)
	}

	// m → priority: flat, sorted by priority then id; epics included as rows.
	press(m, "m")
	want = []string{"aaa.2", "bbb.1", "epic-aaa", "aaa.1", "task-zzz", "epic-bbb"}
	if got := rowIDs(m); !equalStrings(got, want) {
		t.Fatalf("priority mode rows = %v, want %v", got, want)
	}
	for _, r := range m.rows {
		if r.hasChildren {
			t.Errorf("flat mode row %s still foldable", r.node.Issue.ID)
		}
	}

	// m → issues: same flat sort minus container rows.
	press(m, "m")
	want = []string{"aaa.2", "bbb.1", "aaa.1", "task-zzz"}
	if got := rowIDs(m); !equalStrings(got, want) {
		t.Fatalf("issues mode rows = %v, want %v", got, want)
	}

	// m wraps back to the epic tree.
	press(m, "m")
	if got := rowIDs(m); !equalStrings(got, []string{"epic-aaa", "aaa.1", "aaa.2", "epic-bbb", "bbb.1", "task-zzz"}) {
		t.Fatalf("mode cycle did not wrap to epics: %v", got)
	}
}

// TestQueueFoldAllToggle proves F collapses everything to the epic level and
// expands it back, while space still folds one node at a time.
func TestQueueFoldAllToggle(t *testing.T) {
	m := newQueueForTest(t)

	press(m, "F") // nothing collapsed → collapse all
	if got := rowIDs(m); !equalStrings(got, []string{"epic-aaa", "epic-bbb", "task-zzz"}) {
		t.Fatalf("collapse-all rows = %v, want epic level only", got)
	}

	press(m, "F") // something collapsed → expand all
	if got := rowIDs(m); len(got) != 6 {
		t.Fatalf("expand-all rows = %v, want all 6", got)
	}

	// space folds only the selected epic (cursor at row 0 = epic-aaa).
	press(m, "space")
	if got := rowIDs(m); !equalStrings(got, []string{"epic-aaa", "epic-bbb", "bbb.1", "task-zzz"}) {
		t.Fatalf("single fold rows = %v, want epic-aaa folded only", got)
	}
}

// TestQueueMetadataQuery proves the koryph-166 / search: term kinds, AND
// composition, ancestor retention in the tree, input capture, esc cancel.
func TestQueueMetadataQuery(t *testing.T) {
	m := newQueueForTest(t)

	press(m, "/")
	if !m.IsCapturingInput() {
		t.Fatal("/ must focus the search input (IsCapturingInput true)")
	}
	typeString(m, "label:area:engine")
	press(m, "enter")
	if m.IsCapturingInput() {
		t.Fatal("enter must apply and blur the search input")
	}
	// aaa.1 matches; its ancestor epic-aaa stays for grouping; all else hides.
	if got := rowIDs(m); !equalStrings(got, []string{"epic-aaa", "aaa.1"}) {
		t.Fatalf("label query rows = %v, want [epic-aaa aaa.1]", got)
	}

	// Priority term, AND-composed with type.
	press(m, "/")
	m.queryInput.SetValue("p:1 type:task")
	press(m, "enter")
	if got := rowIDs(m); !equalStrings(got, []string{"epic-bbb", "bbb.1"}) {
		t.Fatalf("p:1 type:task rows = %v, want [epic-bbb bbb.1]", got)
	}

	// Bare text matches id/title.
	press(m, "/")
	m.queryInput.SetValue("zzz")
	press(m, "enter")
	if got := rowIDs(m); !equalStrings(got, []string{"task-zzz"}) {
		t.Fatalf("text query rows = %v, want [task-zzz]", got)
	}

	// esc cancels the edit and keeps the applied query.
	press(m, "/")
	m.queryInput.SetValue("something-else")
	press(m, "esc")
	if m.query != "zzz" {
		t.Errorf("esc must keep the previously applied query, got %q", m.query)
	}

	// Empty query clears the filter.
	press(m, "/")
	m.queryInput.SetValue("")
	press(m, "enter")
	if got := rowIDs(m); len(got) != 6 {
		t.Fatalf("cleared query rows = %v, want all 6", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
