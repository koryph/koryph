// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

// --- shared rolling-dispatch test fixtures ----------------------------------

// beadSpec describes one synthetic bd frontier entry for the rolling-dispatch
// tests below.
type beadSpec struct {
	id, fp string
	// revealAfterClaim, when set, hides this bead from `bd ready` until the
	// named bead has been claimed — used to force a candidate to appear on a
	// LATER refill iteration than an already-dispatched blocker, which is
	// what actually exercises the in-flight (opts.Active) gate rather than
	// same-batch intra-wave coloring (both already covered by
	// wave_footprint_test.go).
	revealAfterClaim string
}

// multiBeadBDScript renders a fake `bd` shell script serving a fixed synthetic
// frontier. Unlike fakeBDScript's touch-file/call-counter approach, `ready`
// here derives the current frontier straight from bd.log (grepping for
// "update <id> --claim" / "close <id>" lines already recorded by prior
// calls), so it behaves like real bd: a claimed or closed bead drops out of
// ready() on the very next call — required for a rolling loop, which may call
// `ready` many times over the life of one run.
func multiBeadBDScript(specs []beadSpec) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("dir=\"$FAKE_BD_DIR\"\n")
	b.WriteString("printf '%s\\n' \"$*\" >> \"$dir/bd.log\"\n")
	b.WriteString("log=\"$dir/bd.log\"\n")
	b.WriteString("case \"$1\" in\n")
	b.WriteString("  ready)\n")
	b.WriteString("    items=\"\"\n")
	for _, s := range specs {
		fmt.Fprintf(&b, "    if ! grep -q '^update %s --claim$' \"$log\" 2>/dev/null && ! grep -q '^close %s ' \"$log\" 2>/dev/null; then\n", s.id, s.id)
		indent := "      "
		if s.revealAfterClaim != "" {
			fmt.Fprintf(&b, "      if grep -q '^update %s --claim$' \"$log\" 2>/dev/null; then\n", s.revealAfterClaim)
			indent = "        "
		}
		entry := fmt.Sprintf(`{\"id\":\"%s\",\"title\":\"%s\",\"description\":\"x\",\"status\":\"open\",\"priority\":1,\"issue_type\":\"task\",\"labels\":[\"fp:%s\"]}`, s.id, s.id, s.fp)
		fmt.Fprintf(&b, "%sif [ -n \"$items\" ]; then items=\"$items,\"; fi\n", indent)
		fmt.Fprintf(&b, "%sitems=\"$items%s\"\n", indent, entry)
		if s.revealAfterClaim != "" {
			b.WriteString("      fi\n")
		}
		b.WriteString("    fi\n")
	}
	b.WriteString("    echo \"[$items]\"\n")
	b.WriteString("    ;;\n")
	b.WriteString("  version) echo \"bd version 1.0.5\" ;;\n")
	b.WriteString("  update|close|comment) exit 0 ;;\n")
	b.WriteString("  show) exit 1 ;;\n")
	b.WriteString("  *) exit 1 ;;\n")
	b.WriteString("esac\n")
	return b.String()
}

// slowClaudeScript is fakeClaudeScript generalized to (a) name its commit
// after the ACTUAL dispatched bead (via $KORYPH_PHASE_ID, set by
// dispatch.CLIBackend into launch.sh's environment) instead of hardcoding
// "tb1", and (b) sleep KORYPH_TEST_SLOW_SECONDS before committing when
// $KORYPH_PHASE_ID matches $KORYPH_TEST_SLOW_PHASE — used to hold a slot open
// long enough for the test to observe a sibling refill happening around it.
const slowClaudeScript = `#!/bin/sh
cat > /dev/null
if [ -n "$KORYPH_TEST_SLOW_PHASE" ] && [ "$KORYPH_PHASE_ID" = "$KORYPH_TEST_SLOW_PHASE" ]; then
  sleep "$KORYPH_TEST_SLOW_SECONDS"
fi
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat($KORYPH_PHASE_ID): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
printf '{"type":"result","total_cost_usd":0.10}\n'
exit 0
`

// syncBuf is a mutex-guarded io.Writer so a test can safely read a run's
// progress output WHILE Run() is still executing in another goroutine (a
// plain bytes.Buffer is not safe for concurrent read/write, and several tests
// below need to observe an intermediate log line, not just the final state).
type syncBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// waitForCondition polls cond every 30ms until it reports true or deadline
// elapses, returning whether it succeeded.
func waitForCondition(deadline time.Duration, cond func() bool) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(30 * time.Millisecond)
	}
	return cond()
}

// --- L1: the wave-barrier-breaking property ---------------------------------

// TestRollingRefillBreaksWaveBarrier is the barrier test (koryph-2im.3's
// whole point): in a 2-wide rolling run with 3 conflict-free beads, the first
// slot's fake agent exits quickly while the second sleeps; the third bead
// must dispatch shortly after the FIRST slot frees — WITHOUT waiting for the
// second (still-running) slot to land. Under wave-mode's pollUntilIdle this
// would be impossible: the loop blocks until BOTH tb1 and tb2 are terminal
// before it would even look at tb3.
func TestRollingRefillBreaksWaveBarrier(t *testing.T) {
	specs := []beadSpec{{id: "tb1", fp: "a"}, {id: "tb2", fp: "b"}, {id: "tb3", fp: "c"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb2")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "4")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1

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
	var tb2StillRunningWhenTb3Dispatched bool
	ok := waitForCondition(8*time.Second, func() bool {
		run, err := store.LoadLatest()
		if err != nil || run == nil {
			return false
		}
		sl := run.Slots["tb3"]
		if sl == nil || sl.DispatchedAt == "" {
			return false
		}
		tb2 := run.Slots["tb2"]
		tb2StillRunningWhenTb3Dispatched = tb2 != nil && !ledger.Terminal(tb2.Status)
		return true
	})
	if !ok {
		t.Fatalf("tb3 was never dispatched within the deadline; log:\n%s", out.String())
	}
	if !tb2StillRunningWhenTb3Dispatched {
		t.Fatalf("tb3 dispatched only after tb2 had already landed — the wave barrier was not broken; log:\n%s", out.String())
	}

	select {
	case r := <-done:
		t.Logf("engine output:\n%s", out.String())
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.got.Dispatched != 3 || r.got.Merged != 3 {
			t.Errorf("Outcome = %+v, want 3 dispatched / 3 merged", r.got)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}

// --- L2: in-flight footprint gating across refill iterations ----------------

// TestRollingInFlightFootprintHeldAcrossIterations proves the in-flight
// (opts.Active) gate — not the intra-batch one wave_footprint_test.go already
// covers — holds across SEPARATE rolling iterations: tb2 (same footprint as
// tb1) is invisible to `bd ready` until tb1 has already been claimed, so it
// can only ever be considered on a LATER iteration while tb1 is in flight. It
// must be deferred with the "(in-flight)" reason while tb1 runs, then
// dispatch once tb1 lands.
func TestRollingInFlightFootprintHeldAcrossIterations(t *testing.T) {
	specs := []beadSpec{
		{id: "tb1", fp: "x"},
		{id: "tb2", fp: "x", revealAfterClaim: "tb1"},
	}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "3")

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1

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
	// While tb1 is still active, tb2 must have been observed and deferred as
	// an in-flight conflict — never dispatched.
	held := waitForCondition(6*time.Second, func() bool {
		return strings.Contains(out.String(), "footprint conflict with tb1 (in-flight)")
	})
	if !held {
		t.Fatalf("tb2 was never deferred as an in-flight conflict with tb1; log:\n%s", out.String())
	}
	if run, err := store.LoadLatest(); err == nil && run != nil {
		if sl := run.Slots["tb2"]; sl != nil {
			t.Fatalf("tb2 has a slot (%+v) while tb1 is still in flight — the in-flight gate did not hold", sl)
		}
		if tb1 := run.Slots["tb1"]; tb1 == nil || ledger.Terminal(tb1.Status) {
			t.Fatalf("tb1 was expected to still be active when tb2's deferral was observed; got %+v", tb1)
		}
	}

	select {
	case r := <-done:
		t.Logf("engine output:\n%s", out.String())
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		// Both eventually land once tb1 frees the conflicting footprint.
		if r.got.Dispatched != 2 || r.got.Merged != 2 {
			t.Errorf("Outcome = %+v, want 2 dispatched / 2 merged", r.got)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}

// --- governor behavior in rolling mode --------------------------------------

// TestRollingGovernorStopWithNoneActivePauses mirrors
// TestRunBillingGuardEnforcedStops (guard_test.go) in rolling mode: a
// calibrated governor already reading hard STOP before any dispatch pauses
// the run immediately (paused-quota), exactly like wave mode.
func TestRollingGovernorStopWithNoneActivePauses(t *testing.T) {
	f := newFixture(t, fixOpts{})
	calibrateStopped(t, "work")

	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Once = false
	opts.DispatchMode = "rolling"

	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 0 || got.Merged != 0 {
		t.Errorf("Outcome = %+v, want 0 dispatched / 0 merged", got)
	}
	if !strings.HasPrefix(got.Reason, "quota-") {
		t.Errorf("Reason = %q, want a quota-* reason", got.Reason)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if run.Status != ledger.RunPausedQuota {
		t.Errorf("run status = %q, want %q", run.Status, ledger.RunPausedQuota)
	}
}

// TestRollingGovernorDrainStopsRefillButActiveFinish proves I4/I5 in rolling
// mode: the governor moves from OK to DRAIN mid-run (usage crosses the drain
// band while a bead is already in flight); no NEW bead is dispatched once
// draining, but the already-active bead is never interrupted — it completes
// and merges normally — and the run pauses (quota-drain) once nothing is left
// active, rather than misreporting "drained" (the frontier still has tb2
// genuinely ready, just deferred by the governor).
func TestRollingGovernorDrainStopsRefillButActiveFinish(t *testing.T) {
	specs := []beadSpec{{id: "tb1", fp: "a"}, {id: "tb2", fp: "b"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "3")

	// Calibrated governor, initially well under warn (10% of a $100 window) so
	// the first refill dispatches under enforced (non-advisory) governance —
	// then bumped into the drain band ([0.90,0.95)) while tb1 sleeps. Both
	// scans of the SAME transcript file (5h and weekly windows) pick up the
	// same cost; a generous weekly ceiling keeps the weekly fraction
	// negligible so the 5h window is what drives the level. The ceiling is
	// $100 (not $1): per-item cost ESTIMATES (quota.EstimateItem's default
	// per-tier table, independent of this transcript) run a few dollars, so a
	// too-small ceiling would trip the preflight refusal path instead of
	// exercising the drain-mid-run path this test targets.
	//
	// This assumes ccusage is not on PATH in the test environment — the same
	// assumption calibrateStopped (guard_test.go) already relies on for its
	// "unavailable ⇒ fail-closed stop" fixture; if that assumption ever
	// breaks, that test breaks first.
	cfg := quota.DefaultConfig("work")
	cfg.WindowCeilingUSD = 100.0
	cfg.WeeklyCeilingUSD = 10000.0
	if err := quota.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	transcript := writeSonnetTranscript(t, f.idDir, 666667) // ≈$10 → OK (10%)

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1
	// Width 1: tb1 and tb2 are mutually non-conflicting, so a width-2 batch
	// would dispatch BOTH on the very first (pre-drain) scan, leaving nothing
	// for the drain check to actually hold back. Width 1 forces tb2 to still
	// be pending when drain kicks in.
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

	// Wait for tb1 to actually dispatch under OK, then push usage into drain.
	store := ledger.NewStore(f.repo)
	dispatched := waitForCondition(5*time.Second, func() bool {
		run, err := store.LoadLatest()
		return err == nil && run != nil && run.Slots["tb1"] != nil && run.Slots["tb1"].DispatchedAt != ""
	})
	if !dispatched {
		t.Fatalf("tb1 never dispatched under OK governance; log:\n%s", out.String())
	}
	writeSonnetTranscriptAt(t, transcript, 6133333) // ≈$92 → drain band [0.90,0.95)

	// While draining, tb2 (ready, non-conflicting, capacity available) must
	// NEVER get a slot.
	sawDrain := waitForCondition(6*time.Second, func() bool {
		return strings.Contains(out.String(), "governor drain")
	})
	if !sawDrain {
		t.Fatalf("never observed a governor-drain log line; log:\n%s", out.String())
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
		if r.got.Reason != "quota-drain" {
			t.Errorf("Reason = %q, want quota-drain", r.got.Reason)
		}
		run, err := store.LoadLatest()
		if err != nil {
			t.Fatalf("LoadLatest: %v", err)
		}
		if run.Status != ledger.RunPausedQuota {
			t.Errorf("run status = %q, want %q", run.Status, ledger.RunPausedQuota)
		}
		if sl := run.Slots["tb2"]; sl != nil {
			t.Errorf("tb2 got a slot despite draining: %+v", sl)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("Run did not complete in time; log:\n%s", out.String())
	}
}

// writeSonnetTranscript writes a single-line JSONL transcript under
// <configDir>/projects/x/y.jsonl reporting outputTokens of sonnet output (the
// only cost quota.JSONLScan will see), timestamped now, and returns its path
// so the test can rewrite it later at a different cost.
func writeSonnetTranscript(t *testing.T, configDir string, outputTokens int) string {
	t.Helper()
	path := transcriptPath(configDir)
	writeSonnetTranscriptAt(t, path, outputTokens)
	return path
}

func transcriptPath(configDir string) string {
	return configDir + "/projects/x/y.jsonl"
}

// writeSonnetTranscriptAt (re)writes the transcript at path with a fresh
// "now" timestamp — quota.JSONLScan is called fresh on every governor check,
// so overwriting this file changes what the NEXT check sees.
func writeSonnetTranscriptAt(t *testing.T, path string, outputTokens int) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf(`{"timestamp":%q,"model":"claude-sonnet-4-5","usage":{"output_tokens":%d}}`, now, outputTokens)
	writeFile(t, path, line+"\n", 0o644)
}

// --- drained exit in rolling mode -------------------------------------------

// TestRollingDrainedExitFinalizesAndDropsDemand proves the drained exit path
// in rolling mode: once the (single-bead) frontier is exhausted and nothing
// is active, the run finalizes as drained, withdraws its governor demand, and
// reports the same Outcome contract (ExitDrained/Drained=true) as wave mode.
func TestRollingDrainedExitFinalizesAndDropsDemand(t *testing.T) {
	f := newFixture(t, fixOpts{})
	gs := govern.NewStore()

	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1

	got, err := Run(context.Background(), opts)
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Code != ExitDrained || !got.Drained {
		t.Errorf("Outcome = %+v, want ExitDrained/Drained=true", got)
	}
	if got.Dispatched != 1 || got.Merged != 1 {
		t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged before draining", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if run.Status != ledger.RunDrained {
		t.Errorf("run status = %q, want %q", run.Status, ledger.RunDrained)
	}

	_, _, demand, err := gs.Snapshot(govern.DefaultPool)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	for _, d := range demand {
		if d.Project == "proj" {
			t.Errorf("demand for proj still present after drain: %+v", d)
		}
	}
}

// --- --once parity between modes --------------------------------------------

// TestRunOnceParityAcrossDispatchModes proves --once is byte-for-byte
// identical in both modes (design compatibility contract, §4): the same
// fixture and options, differing only in DispatchMode, must produce the same
// Outcome.
func TestRunOnceParityAcrossDispatchModes(t *testing.T) {
	for _, mode := range []string{"wave", "rolling"} {
		t.Run(mode, func(t *testing.T) {
			newFixture(t, fixOpts{})
			var out bytes.Buffer
			opts := baseOptions(&out)
			opts.Once = true // both modes must run today's wave semantics
			opts.DispatchMode = mode

			got, err := Run(context.Background(), opts)
			t.Logf("engine output (%s):\n%s", mode, out.String())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if got.Code != ExitOK || got.Dispatched != 1 || got.Merged != 1 || got.Failed != 0 || got.Blocked != 0 {
				t.Errorf("mode %s: Outcome = %+v, want 1 dispatched / 1 merged", mode, got)
			}
		})
	}
}
