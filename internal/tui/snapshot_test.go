// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"testing"
	"time"

	"github.com/koryph/koryph/internal/cockpit"
)

// baseSnap returns a minimal snapshot suitable for snapshotUnchanged tests.
func baseSnap(at time.Time) cockpit.Snapshot {
	return cockpit.Snapshot{
		RunID:     "run-1",
		RunStatus: "running",
		Wave:      1,
		Burndown:  cockpit.BurndownSnapshot{ComputedAt: at},
		Graph:     cockpit.GraphSnapshot{ComputedAt: at},
		Efficiency: cockpit.EfficiencySnapshot{
			ComputedAt: at,
		},
		Queue: cockpit.QueueSnapshot{ComputedAt: at},
		Governor: cockpit.GovernorSnapshot{
			Pools: map[string]cockpit.PoolSnapshot{
				"anthropic": {Leases: 2, Dynamic: 8},
			},
		},
		Events: cockpit.EventsSnapshot{
			Events: []cockpit.TUIEvent{
				{Time: at, Kind: "dispatch", Message: "dispatch abc"},
			},
		},
	}
}

func TestSnapshotUnchanged_Identical(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	if !snapshotUnchanged(a, b) {
		t.Fatal("identical snapshots should be unchanged")
	}
}

func TestSnapshotUnchanged_RunIDChange(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	b.RunID = "run-2"
	if snapshotUnchanged(a, b) {
		t.Fatal("RunID change should be detected")
	}
}

func TestSnapshotUnchanged_BurndownEpoch(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	b.Burndown.ComputedAt = now.Add(5 * time.Second)
	if snapshotUnchanged(a, b) {
		t.Fatal("Burndown ComputedAt change should be detected")
	}
}

func TestSnapshotUnchanged_EfficiencyEpoch(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	b.Efficiency.ComputedAt = now.Add(5 * time.Second)
	if snapshotUnchanged(a, b) {
		t.Fatal("Efficiency ComputedAt change should be detected")
	}
}

func TestSnapshotUnchanged_QueueEpoch(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	b.Queue.ComputedAt = now.Add(5 * time.Second)
	if snapshotUnchanged(a, b) {
		t.Fatal("Queue ComputedAt change should be detected")
	}
}

func TestSnapshotUnchanged_GraphEpoch(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	b.Graph.ComputedAt = now.Add(5 * time.Second)
	if snapshotUnchanged(a, b) {
		t.Fatal("Graph ComputedAt change should be detected")
	}
}

func TestSnapshotUnchanged_GovernorLeaseChange(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	// A new agent leases a slot.
	b.Governor.Pools["anthropic"] = cockpit.PoolSnapshot{Leases: 3, Dynamic: 8}
	if snapshotUnchanged(a, b) {
		t.Fatal("Governor lease change should be detected")
	}
}

func TestSnapshotUnchanged_GovernorDynamicChange(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	// AIMD reduced the cap.
	b.Governor.Pools["anthropic"] = cockpit.PoolSnapshot{Leases: 2, Dynamic: 6}
	if snapshotUnchanged(a, b) {
		t.Fatal("Governor dynamic cap change should be detected")
	}
}

// TestSnapshotUnchanged_EventsAtCapRotation verifies that when the events ring
// is at eventsRingMax capacity, a rotation (same length, new newest entry)
// is detected rather than misclassified as unchanged (review finding).
func TestSnapshotUnchanged_EventsAtCapRotation(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)

	// Fill both rings to capacity (500 = cockpit.eventsRingMax).
	const cap = 500
	events := make([]cockpit.TUIEvent, cap)
	for i := range events {
		events[i] = cockpit.TUIEvent{Time: now.Add(time.Duration(i) * time.Second), Kind: "dispatch"}
	}
	a.Events.Events = events

	// b has the same length but the LAST event is different (oldest was dropped,
	// newest is new) — the at-capacity rotation scenario.
	rotated := make([]cockpit.TUIEvent, cap)
	copy(rotated, events[1:]) // shift: drop oldest
	rotated[cap-1] = cockpit.TUIEvent{
		Time: now.Add(time.Duration(cap) * time.Second),
		Kind: "merge",
	}
	b.Events.Events = rotated

	if snapshotUnchanged(a, b) {
		t.Fatal("events rotation at capacity should be detected as changed")
	}
}

// TestSnapshotUnchanged_SlotStatusChange verifies that a slot's stage change
// (e.g. running → merged) is detected.
func TestSnapshotUnchanged_SlotStatusChange(t *testing.T) {
	now := time.Now()
	a := baseSnap(now)
	b := baseSnap(now)
	slot := cockpit.SlotSnapshot{PhaseID: "abc-1", Stage: "running", Attempt: 1}
	a.Slots = []cockpit.SlotSnapshot{slot}
	slotDone := slot
	slotDone.Stage = "merged"
	b.Slots = []cockpit.SlotSnapshot{slotDone}
	if snapshotUnchanged(a, b) {
		t.Fatal("slot stage change should be detected")
	}
}
