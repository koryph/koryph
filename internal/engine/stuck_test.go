// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
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
