// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
)

// silentSlot returns a slot that trips every pre-koryph-2rf stuck signal:
// no status file, dispatched long ago, and a worktree that is NOT a git repo
// — an empty Worktree would run isStuck's `git log` probe in the test
// process's CWD (this repository!) and key the outcome to how recently the
// developer last committed.
func silentSlot(t *testing.T, id string, age time.Duration, resources []string) *ledger.Slot {
	t.Helper()
	return &ledger.Slot{
		PhaseID:      id,
		PID:          1,
		Status:       ledger.SlotRunning,
		DispatchedAt: time.Now().Add(-age).UTC().Format(time.RFC3339),
		StatusPath:   "/nonexistent/status.json",
		Worktree:     t.TempDir(),
		Resources:    resources,
	}
}

// TestStuckSuppressedByCPUActivity proves the koryph-2rf implicit CPU
// heartbeat: an agent blocked inside one long tool call cannot write the
// JSON heartbeat or commit, but its cohort burns CPU — and that counts as
// life. A cohort with no recorded activity still trips (the hung-at-0%-CPU
// case is exactly what deserves the flag).
func TestStuckSuppressedByCPUActivity(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.opts.StuckSec = 900

	sl := silentSlot(t, "cpu1", 2*time.Hour, nil)
	if !r.isStuck(t.Context(), sl) {
		t.Fatal("baseline: a silent slot with no CPU record must trip stuck")
	}

	r.resUsage = map[string]*slotResUsage{
		"cpu1": {pid: 1, lastCPU: 42, lastActiveAt: time.Now().Add(-time.Minute)},
	}
	if r.isStuck(t.Context(), sl) {
		t.Error("a cohort with recent CPU activity must NOT be stuck (blocked in a long tool call, not wedged)")
	}

	// Activity older than the threshold no longer counts.
	r.resUsage["cpu1"].lastActiveAt = time.Now().Add(-time.Hour)
	if !r.isStuck(t.Context(), sl) {
		t.Error("stale CPU activity beyond the threshold must trip stuck again")
	}

	// A requeue's new PID invalidates the old attempt's activity record.
	r.resUsage["cpu1"] = &slotResUsage{pid: 999, lastActiveAt: time.Now()}
	if !r.isStuck(t.Context(), sl) {
		t.Error("activity recorded for a different pid must not vouch for this attempt")
	}
}

// TestStuckSuppressedByStreamActivity proves the stream heartbeat: an agent
// that is streaming thinking/tool events — the most direct sign of life — is
// never stuck even when it has written no status.json heartbeat, made no
// commit, and its cohort CPU looks idle (the false-stuck a status latch left on
// the board for ~20 min after the agent had demonstrably resumed). Because
// isStuck is re-derived every tick, a fresh stream mtime also clears a transient
// stuck flag rather than latching it.
func TestStuckSuppressedByStreamActivity(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.opts.StuckSec = 900

	sl := silentSlot(t, "s1", 2*time.Hour, nil)
	if !r.isStuck(t.Context(), sl) {
		t.Fatal("baseline: a silent slot with no stream must trip stuck")
	}

	// A stream.jsonl the agent just wrote to → producing output now → not stuck.
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(stream, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sl.Stream = stream
	if r.isStuck(t.Context(), sl) {
		t.Error("a slot whose stream.jsonl was just written must NOT be stuck")
	}

	// Backdate the stream mtime past the threshold → the signal goes stale and
	// the slot trips stuck again (ground-truth re-derivation, not a latch).
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(stream, old, old); err != nil {
		t.Fatal(err)
	}
	if !r.isStuck(t.Context(), sl) {
		t.Error("a stream idle beyond the threshold must trip stuck")
	}
}

// TestSlotActivityAtTakesFreshestSignal proves slotActivityAt returns the max
// across signals, so the newest sign of life wins.
func TestSlotActivityAtTakesFreshestSignal(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	sl := silentSlot(t, "a1", 3*time.Hour, nil)
	if !r.slotActivityAt(t.Context(), sl).IsZero() {
		t.Error("no signals: want zero activity time")
	}

	// A recent stream mtime becomes the activity time.
	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(stream, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	sl.Stream = stream
	if got := r.slotActivityAt(t.Context(), sl); got.IsZero() || time.Since(got) > time.Minute {
		t.Errorf("stream activity time = %v, want ~now", got)
	}

	// A cohort-CPU instant newer than the stream wins (freshest signal).
	future := time.Now().Add(2 * time.Second)
	r.resUsage = map[string]*slotResUsage{"a1": {pid: 1, lastActiveAt: future}}
	if got := r.slotActivityAt(t.Context(), sl); got.Before(future) {
		t.Errorf("activity time = %v, want the fresher cohort-CPU time %v", got, future)
	}
}

// TestPollSlotClearsStuckOnActivity is the finding end-to-end: a slot the board
// shows "stuck" whose agent is alive and streaming is re-derived to "running"
// and stamped with a fresh last_activity_at — status is ground truth, not a
// latch.
func TestPollSlotClearsStuckOnActivity(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.opts.StuckSec = 900

	stream := filepath.Join(t.TempDir(), "stream.jsonl")
	if err := os.WriteFile(stream, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	sl := &ledger.Slot{
		PhaseID: "p1", BeadID: "p1", PID: os.Getpid(), // a live pid
		Status: ledger.SlotStuck, Stream: stream,
		StatusPath:   "/nonexistent/status.json",
		Worktree:     t.TempDir(),
		DispatchedAt: time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}

	r.pollSlot(t.Context(), r.run.Slots["p1"], false)

	got := r.run.Slots["p1"]
	if got.Status != ledger.SlotRunning {
		t.Errorf("status = %q, want running (agent is streaming — stuck must clear, not latch)", got.Status)
	}
	if got.LastActivityAt == "" {
		t.Error("last_activity_at was not stamped")
	}
}

// TestStuckThresholdScalesForResources proves declared-resource beads
// (res:* — cluster bring-up, browser suites) get resStuckMultiplier headroom
// while plain beads keep the exact 900s default.
func TestStuckThresholdScalesForResources(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.opts.StuckSec = 900

	age := 1200 * time.Second // past 900s, well inside 3600s
	if !r.isStuck(t.Context(), silentSlot(t, "plain", age, nil)) {
		t.Error("a plain silent slot past StuckSec must trip stuck (default unchanged)")
	}
	if r.isStuck(t.Context(), silentSlot(t, "e2e", age, []string{"kind-cluster"})) {
		t.Error("a declared-resource slot inside the scaled threshold must not trip stuck")
	}
	if !r.isStuck(t.Context(), silentSlot(t, "e2e2", 5*time.Hour, []string{"kind-cluster"})) {
		t.Error("a declared-resource slot past the scaled threshold must still trip stuck")
	}
}
