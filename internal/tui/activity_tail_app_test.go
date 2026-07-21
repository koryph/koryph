// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"

	"github.com/koryph/koryph/internal/cockpit"
	"github.com/koryph/koryph/internal/tui"
)

func thinkingBlock(text string) string {
	return strings.Join([]string{
		`{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}},"parent_tool_use_id":null}`,
		fmt.Sprintf(`{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":%q}},"parent_tool_use_id":null}`, text),
		`{"type":"stream_event","event":{"type":"content_block_stop","index":0},"parent_tool_use_id":null}`,
	}, "\n") + "\n"
}

// TestAppActivityTailFollowsLiveStream is the regression test for the
// "can't tail the train of thought" bug (koryph-xvk follow-up): while an agent
// runs, its stream.jsonl grows continuously but its status.json — the field the
// App's snapshotUnchanged dedup keys on — stays quiet between steps, so most
// per-tick snapshots are suppressed. The activity tail must therefore re-read
// its file on every tick (App.RefreshTail), not only when a fresh snapshot
// happens to arrive. Without that, the tail freezes on whatever was captured
// when 'T' was pressed. This drives the real operator flow (Threads → Enter →
// T) against a provider whose snapshot never changes, appends new reasoning to
// the live stream, and asserts it appears.
func TestAppActivityTailFollowsLiveStream(t *testing.T) {
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(stream, []byte(thinkingBlock("planning the gateway fix")), 0o644); err != nil {
		t.Fatalf("write stream: %v", err)
	}

	now := time.Now()
	snap := cockpit.Snapshot{
		ProjectID: "proj-1",
		RunID:     "20260704-100000",
		RunStatus: "running",
		Wave:      1,
		Slots: []cockpit.SlotSnapshot{{
			PhaseID:      "b1",
			BeadID:       "b1",
			Title:        "Add widget support",
			Stage:        "running",
			Model:        "claude-sonnet-4-5",
			Attempt:      1,
			PID:          12345,
			DispatchedAt: now.Add(-2 * time.Minute),
			StatusLine:   "reasoning",
		}},
		// snap.Detail carries the focused bead so Enter opens Detail without a
		// DetailProvider; its StreamPath drives the tail. The provider returns
		// this SAME snapshot on every Refresh, so snapshotUnchanged suppresses
		// every tick's snapshotMsg — exactly the freeze condition.
		Detail: cockpit.BeadDetailSnapshot{
			BeadID: "b1", Title: "Add widget support", Status: "in_progress",
			StreamPath: stream, ComputedAt: now,
		},
		CapturedAt: now,
	}

	p := &staticProvider{id: "proj-1", snap: snap}
	app := tui.NewApp([]cockpit.Provider{p}, false)
	tm := teatest.NewTestModel(t, app, teatest.WithInitialTermSize(120, 40))
	defer func() { _ = tm.Quit() }()

	// Wait for the thread row to render (snapshot arrived, table populated).
	waitFor(t, tm, func(b []byte) bool { return strings.Contains(string(b), "reasoning") })

	// Enter → Detail, then T → activity tail.
	tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	waitFor(t, tm, func(b []byte) bool {
		return strings.Contains(string(b), "b1") && strings.Contains(string(b), "Add widget support")
	})
	tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("T")})
	waitFor(t, tm, func(b []byte) bool {
		s := string(b)
		return strings.Contains(s, "Activity [") && strings.Contains(s, "planning the gateway fix")
	})

	// The agent keeps reasoning: append a new thinking block to the SAME live
	// stream. Follow is on, so the tail must surface it within a few ticks —
	// with NO change to the slot's status fields (the snapshot is static).
	f, err := os.OpenFile(stream, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("reopen stream: %v", err)
	}
	if _, err := f.WriteString(thinkingBlock("now verifying the retry path")); err != nil {
		t.Fatalf("append: %v", err)
	}
	_ = f.Close()

	waitFor(t, tm, func(b []byte) bool {
		return strings.Contains(string(b), "now verifying the retry path")
	})
}
