// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
)

// drainResizeDeadlines holds the generous per-phase deadlines used by the
// drain and resize tests. The fake claude sleeps KORYPH_TEST_SLOW_SECONDS
// (nominally 3 s) to hold a slot open; under extreme host load (observed
// load-avg 208 from concurrent gates + live koryph loops) system scheduling
// inflates that sleep and every intermediate git/subprocess step 6x or more.
// The deadlines below are sized for a >10x inflation while remaining fast
// (<10 s total per test) on a quiet machine (koryph-b0k).
const (
	// drainDispatchTimeout is how long to wait for tb1's DispatchedAt to
	// appear in the ledger. Dispatch involves a git-worktree-add + subprocess
	// spawn; inflate the 1–2 s baseline by 15x to tolerate heavy load.
	drainDispatchTimeout = 30 * time.Second
	// drainEffectTimeout is how long to wait for a drain/resize log line or
	// ledger entry to appear after the sentinel/override is written.
	drainEffectTimeout = 30 * time.Second
	// drainFinalTimeout is the backstop for the engine's Run() to return.
	// slowClaudeScript sleeps 3 s; inflate by 30x for extreme-load tolerance.
	drainFinalTimeout = 90 * time.Second
)

// --- operator drain (koryph-57v.1) ------------------------------------------

// TestRollingOperatorDrainFinishesActiveThenExitsImmediately proves the whole
// drain contract in rolling mode: (1) a sentinel written while a bead is
// in-flight does not touch that bead — it lands normally; (2) the moment the
// last slot lands (active hits 0), the very next boundary exits through the
// normal drained-finalize path with a distinct "operator-drain" reason,
// WITHOUT ever scanning/dispatching tb2 (still ready, non-conflicting, and
// would otherwise have been picked up on the next refill) — this is the
// "drain requested when nothing is active exits immediately" requirement:
// operator drain is checked before the frontier scan, so ready work never
// gets a look-in once it fires; (3) the sentinel is consumed so a fresh run
// afterwards starts clean.
func TestRollingOperatorDrainFinishesActiveThenExitsImmediately(t *testing.T) {
	specs := []beadSpec{{id: "tb1", fp: "a"}, {id: "tb2", fp: "b"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "3")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1
	// Width 1: tb1 and tb2 are non-conflicting, so a width-2 batch would
	// dispatch both immediately, leaving nothing to prove the drain gate held
	// tb2 back once tb1 landed.
	opts.Max = 1

	type result struct {
		got Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := Run(context.Background(), opts)
		done <- result{got, err}
	}()

	store := ledger.NewStore(f.repo)
	dispatched := waitForCondition(drainDispatchTimeout, func() bool {
		run, err := store.LoadLatest()
		return err == nil && run != nil && run.Slots["tb1"] != nil && run.Slots["tb1"].DispatchedAt != ""
	})
	if !dispatched {
		t.Fatalf("tb1 never dispatched; log:\n%s", out.String())
	}

	if err := store.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain: %v", err)
	}

	sawDrain := waitForCondition(drainEffectTimeout, func() bool {
		return strings.Contains(out.String(), "operator drain: no new dispatch")
	})
	if !sawDrain {
		t.Fatalf("never observed the operator-drain hold-back log line; log:\n%s", out.String())
	}

	select {
	case r := <-done:
		t.Logf("engine output:\n%s", out.String())
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.got.Dispatched != 1 || r.got.Merged != 1 {
			t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged (tb2 held by drain)", r.got)
		}
		if r.got.Reason != "operator-drain" || !r.got.Drained || r.got.Code != ExitDrained {
			t.Errorf("Outcome = %+v, want reason=operator-drain, Drained=true, Code=%d", r.got, ExitDrained)
		}
		run, err := store.LoadLatest()
		if err != nil {
			t.Fatalf("LoadLatest: %v", err)
		}
		if run.Status != ledger.RunDrained {
			t.Errorf("run status = %q, want %q", run.Status, ledger.RunDrained)
		}
		if sl := run.Slots["tb2"]; sl != nil {
			t.Errorf("tb2 got a slot despite the operator drain: %+v", sl)
		}
		if store.DrainRequested() {
			t.Error("drain sentinel was not consumed on the operator-drain exit")
		}
	case <-time.After(drainFinalTimeout):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}

// TestWaveOperatorDrainFinishesActiveThenExitsImmediately is the wave-mode
// twin of the rolling test above: --max 1 keeps tb2 out of tb1's wave
// entirely, so once tb1's wave settles (pollUntilIdle returns) the loop's
// next governorGate call sees the sentinel with nothing active and exits
// immediately — tb2 is never even scanned into a second wave.
func TestWaveOperatorDrainFinishesActiveThenExitsImmediately(t *testing.T) {
	specs := []beadSpec{{id: "tb1", fp: "a"}, {id: "tb2", fp: "b"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "3")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "wave"
	opts.PollSec = 1
	opts.Max = 1

	type result struct {
		got Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := Run(context.Background(), opts)
		done <- result{got, err}
	}()

	store := ledger.NewStore(f.repo)
	dispatched := waitForCondition(drainDispatchTimeout, func() bool {
		run, err := store.LoadLatest()
		return err == nil && run != nil && run.Slots["tb1"] != nil && run.Slots["tb1"].DispatchedAt != ""
	})
	if !dispatched {
		t.Fatalf("tb1 never dispatched; log:\n%s", out.String())
	}

	if err := store.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain: %v", err)
	}

	select {
	case r := <-done:
		t.Logf("engine output:\n%s", out.String())
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.got.Dispatched != 1 || r.got.Merged != 1 {
			t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged (tb2 held by drain)", r.got)
		}
		if r.got.Reason != "operator-drain" || !r.got.Drained || r.got.Code != ExitDrained {
			t.Errorf("Outcome = %+v, want reason=operator-drain, Drained=true, Code=%d", r.got, ExitDrained)
		}
		run, err := store.LoadLatest()
		if err != nil {
			t.Fatalf("LoadLatest: %v", err)
		}
		if run.Status != ledger.RunDrained {
			t.Errorf("run status = %q, want %q", run.Status, ledger.RunDrained)
		}
		if sl := run.Slots["tb2"]; sl != nil {
			t.Errorf("tb2 got a slot despite the operator drain: %+v", sl)
		}
		if store.DrainRequested() {
			t.Error("drain sentinel was not consumed on the operator-drain exit")
		}
	case <-time.After(drainFinalTimeout):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}

// TestOperatorDrainStaleSentinelClearedAtRunStart proves the run-start clear
// (koryph-57v.1): a sentinel left over from BEFORE this process started (no
// run currently touching it) must not instantly kill a fresh, intentional
// invocation — it is treated as stale, cleared, and logged, and the run
// proceeds and drains normally (reason "drained", not "operator-drain").
func TestOperatorDrainStaleSentinelClearedAtRunStart(t *testing.T) {
	f := newFixture(t, fixOpts{})
	store := ledger.NewStore(f.repo)
	if err := store.RequestDrain(); err != nil {
		t.Fatalf("RequestDrain: %v", err)
	}

	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Once = true // single-bead fixture; --once is sufficient to observe the clear

	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "cleared a stale operator-drain request") {
		t.Errorf("progress output missing the stale-sentinel clear notice:\n%s", out.String())
	}
	if store.DrainRequested() {
		t.Error("sentinel still present after run start — it should have been consumed as stale")
	}
	if got.Reason == "operator-drain" {
		t.Errorf("Outcome = %+v, a stale (pre-start) sentinel must not produce an operator-drain outcome", got)
	}
	// The bead still dispatched and merged normally — the stale sentinel did
	// not block anything.
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged despite the stale sentinel", got)
	}
}

// --- live resize (koryph-57v.1) ---------------------------------------------

// TestRollingResizeOverrideRaisesWidthWithoutRestart proves a resize override
// written mid-run is picked up at the very next refill boundary: with the
// project configured (opts.Max) at width 1, tb2 could not dispatch alongside
// tb1 under the ORIGINAL width — but once the override raises it to 2 while
// tb1 is still sleeping, tb2 must dispatch WHILE tb1 is still active,
// demonstrating the effective width changed without a restart.
func TestRollingResizeOverrideRaisesWidthWithoutRestart(t *testing.T) {
	specs := []beadSpec{{id: "tb1", fp: "a"}, {id: "tb2", fp: "b"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "3")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1
	opts.Max = 1 // configured width: only tb1 fits until an override raises it

	type result struct {
		got Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := Run(context.Background(), opts)
		done <- result{got, err}
	}()

	store := ledger.NewStore(f.repo)
	dispatched := waitForCondition(drainDispatchTimeout, func() bool {
		run, err := store.LoadLatest()
		return err == nil && run != nil && run.Slots["tb1"] != nil && run.Slots["tb1"].DispatchedAt != ""
	})
	if !dispatched {
		t.Fatalf("tb1 never dispatched; log:\n%s", out.String())
	}

	if err := store.SetResize(ledger.ResizeOverride{Max: 2}); err != nil {
		t.Fatalf("SetResize: %v", err)
	}

	var tb1StillActiveWhenTb2Dispatched bool
	ok := waitForCondition(drainEffectTimeout, func() bool {
		run, err := store.LoadLatest()
		if err != nil || run == nil {
			return false
		}
		sl := run.Slots["tb2"]
		if sl == nil || sl.DispatchedAt == "" {
			return false
		}
		tb1 := run.Slots["tb1"]
		tb1StillActiveWhenTb2Dispatched = tb1 != nil && !ledger.Terminal(tb1.Status)
		return true
	})
	if !ok {
		t.Fatalf("tb2 was never dispatched within the deadline (override not honored); log:\n%s", out.String())
	}
	if !tb1StillActiveWhenTb2Dispatched {
		t.Fatalf("tb2 dispatched only after tb1 had already landed — the override did not raise capacity while tb1 was active; log:\n%s", out.String())
	}

	select {
	case r := <-done:
		t.Logf("engine output:\n%s", out.String())
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.got.Dispatched != 2 || r.got.Merged != 2 {
			t.Errorf("Outcome = %+v, want 2 dispatched / 2 merged", r.got)
		}
	case <-time.After(drainFinalTimeout):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}
