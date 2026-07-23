// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
)

// TestPollIntervalPrecedence exercises pollInterval's four-way resolution
// order (koryph-2im.2): KORYPH_POLL_SEC env > explicit Options.PollSec >
// project config poll_seconds > defaultPollSec. Run() no longer pre-defaults
// opts.PollSec (it used to, which would have shadowed the config value before
// Load ever ran), so pollInterval must resolve all four sources itself.
func TestPollIntervalPrecedence(t *testing.T) {
	cases := []struct {
		name    string
		env     string // "" = unset
		optsSec int
		cfgSec  int
		want    time.Duration
	}{
		{
			name:    "env overrides opts and config",
			env:     "7",
			optsSec: 20,
			cfgSec:  30,
			want:    7 * time.Second,
		},
		{
			name:    "opts overrides config when env unset",
			env:     "",
			optsSec: 20,
			cfgSec:  30,
			want:    20 * time.Second,
		},
		{
			name:    "config used when opts unset",
			env:     "",
			optsSec: 0,
			cfgSec:  30,
			want:    30 * time.Second,
		},
		{
			name:    "default when nothing set",
			env:     "",
			optsSec: 0,
			cfgSec:  0,
			want:    time.Duration(defaultPollSec) * time.Second,
		},
		{
			name:    "unparseable/non-positive env is ignored, falls through to opts",
			env:     "-1",
			optsSec: 5,
			cfgSec:  30,
			want:    5 * time.Second,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envPollSec, tc.env)
			r := &runner{
				opts: Options{PollSec: tc.optsSec},
				cfg:  &project.Config{PollSeconds: tc.cfgSec},
			}
			if got := r.pollInterval(); got != tc.want {
				t.Errorf("pollInterval() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestProgressProbeDue is the pure-function unit test for L3's split probe
// cadence: the git progress probe (branchProgress) runs on tick 1 and every
// 3rd timer tick thereafter (1, 4, 7, ...), independent of the SIGCHLD wake
// path — signal-triggered passes never call progressProbeDue at all (they
// hardcode probeProgress=false in pollUntilIdle), which this table can't
// observe directly, so the SIGCHLD test below covers that half.
func TestProgressProbeDue(t *testing.T) {
	want := map[int]bool{
		1: true, 2: false, 3: false,
		4: true, 5: false, 6: false,
		7: true, 8: false, 9: false,
		10: true,
	}
	for tick, expect := range want {
		if got := progressProbeDue(tick); got != expect {
			t.Errorf("progressProbeDue(%d) = %v, want %v", tick, got, expect)
		}
	}
}

// TestPollPassRefreshesDemand verifies that pollPass refreshes the engine's
// demand heartbeat on every poll tick, not only at dispatch time. Under slot
// saturation (all global slots occupied, no new admissions), the dispatch-time
// refresh in wave/rolling loops never fires; without this fix, doctor's
// 10-minute TTL would false-trigger on a healthy, fully-loaded pipeline
// (koryph-p42).
func TestPollPassRefreshesDemand(t *testing.T) {
	f := newFixture(t, fixOpts{})

	// Demand file for project "proj" in the default (anthropic) pool lives at
	// {KORYPH_HOME}/slots/demand/proj.json — see govern.Store.demandPath.
	demandFile := filepath.Join(f.home, "slots", "demand", "proj.json")
	if _, err := os.Stat(demandFile); !os.IsNotExist(err) {
		t.Fatalf("demand file unexpectedly present before test: stat=%v", err)
	}

	before := time.Now().Add(-time.Second) // generous buffer against sub-second clocks

	// Construct a minimal runner with just the fields pollPass needs:
	//   gov  → refreshDemand writes the heartbeat
	//   run  → activePhaseIDs iterates run.Slots (empty: no slots to poll)
	//   store → SaveRun persists the ledger (fsx.WriteJSONAtomic creates dirs)
	gs := govern.NewStore()
	r := &runner{
		opts:  Options{ProjectID: "proj"},
		gov:   gs,
		run:   &ledger.Run{RunID: "test-run-p42", Slots: map[string]*ledger.Slot{}},
		store: &ledger.Store{KoryphRoot: filepath.Join(f.repo, ".plan-logs", "koryph")},
	}

	r.pollPass(context.Background(), false)

	// pollPass must have written a fresh demand heartbeat via refreshDemand.
	data, err := os.ReadFile(demandFile)
	if err != nil {
		t.Fatalf("demand file not created by pollPass: %v\n(fix: pollPass must call refreshDemand)", err)
	}
	var d govern.Demand
	if err := json.Unmarshal(data, &d); err != nil {
		t.Fatalf("demand file malformed: %v", err)
	}
	ts, err := time.Parse(time.RFC3339, d.UpdatedAt)
	if err != nil {
		t.Fatalf("demand UpdatedAt not parseable: %v", err)
	}
	if ts.Before(before) {
		t.Errorf("demand UpdatedAt = %v, want >= %v (pollPass did not refresh demand heartbeat)",
			ts, before)
	}
	if d.Project != "proj" {
		t.Errorf("demand Project = %q, want \"proj\"", d.Project)
	}
}

// TestBeadClosedMidFlightBoundedOnHungBd proves the koryph-1dg guarantee at the
// engine boundary: the loop-critical bd read pollPass makes before every requeue
// (beadClosedMidFlight → adapter.Show) cannot stall the poll pass past the
// adapter's timeout when the bd binary is wedged (a dolt lock held by another
// process). Without the adapter timeout this call would block for the stub's
// full 300s sleep, freezing liveness reaping, completeSlot, and drain.
func TestBeadClosedMidFlightBoundedOnHungBd(t *testing.T) {
	repo := t.TempDir()
	bin := filepath.Join(repo, "bd")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatalf("write hung bd: %v", err)
	}
	adapter := &beads.Adapter{
		RepoRoot: repo,
		BeadsDir: filepath.Join(repo, ".beads"),
		Bin:      bin,
		Timeout:  100 * time.Millisecond, // tight bound for the test
	}
	r := &runner{adapter: adapter}

	done := make(chan bool, 1)
	start := time.Now()
	go func() { done <- r.beadClosedMidFlight(context.Background(), "x-1") }()

	select {
	case closed := <-done:
		// A timed-out Show surfaces an error, which beadClosedMidFlight treats as
		// "not closed — let the requeue proceed" (its documented degraded path).
		if closed {
			t.Fatal("hung bd Show should error and be treated as not-closed, got closed=true")
		}
		if elapsed := time.Since(start); elapsed > 10*time.Second {
			t.Fatalf("beadClosedMidFlight took %s, expected it bounded near the 100ms adapter timeout", elapsed)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("beadClosedMidFlight stalled on a hung bd — the adapter timeout did not bound the loop-critical read")
	}
}

// TestPollUntilIdleSyncsObsConfigEachTick guards the fix for the audit finding
// that obs.Sync/ReloadConfig was wired into the top of waveLoop/rollingLoop
// but NOT into pollUntilIdle's own inner tick loop — so a wave that took
// multiple poll ticks to settle would only pick up a mid-run `koryph obs
// level` change once the WHOLE wave finished, not on the next tick as the
// "no restart needed" contract promises (docs/designs/2026-07-observability.md
// §4). waveLoop calls syncObsConfig exactly once per iteration (see wave.go),
// and a --once run with one ready bead completes exactly one waveLoop
// iteration (koryph-2im.3), so any count above 1 can only have come from
// pollUntilIdle's own loop body — proving the per-tick reload is wired in
// without depending on exact tick counts (which would make the test flaky).
func TestPollUntilIdleSyncsObsConfigEachTick(t *testing.T) {
	newFixture(t, fixOpts{})
	var out bytes.Buffer
	ctx := context.Background()

	syncObsConfigCalls = 0
	opts := baseOptions(&out)

	got, err := Run(ctx, opts)
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	if got.Merged != 1 {
		t.Fatalf("want 1 merged bead, got %+v\noutput:\n%s", got, out.String())
	}
	if syncObsConfigCalls <= 1 {
		t.Errorf("syncObsConfigCalls = %d, want > 1 — a single-wave --once run only calls "+
			"syncObsConfig once from waveLoop's own top-of-loop; a count of 1 means "+
			"pollUntilIdle's tick loop never re-synced the obs config while the wave's "+
			"slot was running", syncObsConfigCalls)
	}
}

// TestRunSIGCHLDFastPath asserts a dispatched fake agent's exit is detected
// far sooner than a large poll interval, via the SIGCHLD wake in
// pollUntilIdle (koryph-2im.2). Without the wake, completion would only be
// noticed on the next 30s timer tick; with it, detection is bounded by the
// actual work (git/bd subprocess calls), not the poll interval.
func TestRunSIGCHLDFastPath(t *testing.T) {
	newFixture(t, fixOpts{})
	var out bytes.Buffer
	ctx := context.Background()

	opts := baseOptions(&out)
	opts.PollSec = 30 // far larger than the fake agent's near-instant exit

	start := time.Now()
	got, err := Run(ctx, opts)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Run: %v\noutput:\n%s", err, out.String())
	}
	if got.Merged != 1 {
		t.Fatalf("want 1 merged bead, got %+v\noutput:\n%s", got, out.String())
	}
	// A generous margin above the observed sub-2s cost of the fixture's git/bd
	// round trips, but a small fraction of the 30s poll interval: this can
	// only pass fast if SIGCHLD (not the timer) detected the agent's exit.
	if elapsed > 10*time.Second {
		t.Fatalf("Run took %v with a 30s poll interval — expected SIGCHLD to detect "+
			"the agent's exit within seconds instead of waiting for the timer backstop", elapsed)
	}
}
