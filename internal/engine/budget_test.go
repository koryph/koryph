// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

// TestRunOnlyDispatchesNamedBead proves --only narrows the frontier: the named
// ready bead dispatches, and a name absent from the frontier drains with nothing
// dispatched.
func TestRunOnlyDispatchesNamedBead(t *testing.T) {
	t.Run("named bead dispatches", func(t *testing.T) {
		newFixture(t, fixOpts{})
		var out bytes.Buffer
		opts := baseOptions(&out)
		opts.Only = "tb1"
		got, err := Run(context.Background(), opts)
		t.Logf("output:\n%s", out.String())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got.Dispatched != 1 || got.Merged != 1 {
			t.Errorf("Outcome = %+v, want 1 dispatched / 1 merged", got)
		}
	})

	t.Run("absent bead drains", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		var out bytes.Buffer
		opts := baseOptions(&out)
		opts.Only = "nope"
		got, err := Run(context.Background(), opts)
		t.Logf("output:\n%s", out.String())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		if got.Dispatched != 0 || !got.Drained {
			t.Errorf("Outcome = %+v, want 0 dispatched / drained", got)
		}
		if log := f.bdLog(t); strings.Contains(log, "--claim") {
			t.Errorf("a bead was claimed despite --only miss:\n%s", log)
		}
	})
}

// twoWaveBD serves tb1 on the first `ready` and tb2 on every later one — a
// stateful-frontier stand-in: new work exists after the first bead's cost has
// been recorded. tb2 persists (rather than the frontier going empty) because
// the rolling loop scans more often than wave mode's exactly-two calls; a
// call-counted empty frontier made the budget exit legitimately report
// "drained" instead of "budget-cap" (koryph-2im.8). Both beads share fp:core,
// so in-flight footprint gating keeps tb2 undispatched while tb1 runs in
// either mode.
const twoWaveBD = `#!/bin/sh
dir="$FAKE_BD_DIR"
printf '%s\n' "$*" >> "$dir/bd.log"
case "$1" in
  ready)
    n=$(cat "$dir/ready_n" 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > "$dir/ready_n"
    if [ "$n" = "1" ]; then
      echo '[{"id":"tb1","title":"one","description":"x","status":"open","priority":1,"issue_type":"task","labels":["fp:core"]}]'
    else
      echo '[{"id":"tb2","title":"two","description":"x","status":"open","priority":1,"issue_type":"task","labels":["fp:core"]}]'
    fi
    ;;
  version) echo "bd version 1.0.5" ;;
  update|close|comment) exit 0 ;;
  show) exit 1 ;;
  *) exit 1 ;;
esac
`

// TestRunBudgetPausesWhenExceeded proves the per-run --budget ceiling: after the
// first bead's cost (0.42) exceeds a 0.10 cap, the next wave's ready bead is not
// dispatched and the run pauses with a budget-cap reason.
func TestRunBudgetPausesWhenExceeded(t *testing.T) {
	f := newFixture(t, fixOpts{bdScript: twoWaveBD})
	var out bytes.Buffer
	opts := baseOptions(&out)
	opts.Once = false     // let it reach a second wave
	opts.BudgetUSD = 0.10 // below the fake agent's reported 0.42

	got, err := Run(context.Background(), opts)
	t.Logf("output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Dispatched != 1 {
		t.Errorf("Dispatched = %d, want 1 (budget stops the second bead)", got.Dispatched)
	}
	if got.Reason != "budget-cap" {
		t.Errorf("Reason = %q, want budget-cap", got.Reason)
	}
	if !strings.Contains(out.String(), "run budget reached") {
		t.Errorf("expected a budget-reached log line; got:\n%s", out.String())
	}
	log := f.bdLog(t)
	if !strings.Contains(log, "update tb1 --claim") {
		t.Errorf("tb1 was not dispatched:\n%s", log)
	}
	if strings.Contains(log, "update tb2 --claim") {
		t.Errorf("tb2 dispatched despite the budget cap:\n%s", log)
	}
}

// TestRunCostUSD sums recorded slot costs — the figure --budget measures against.
func TestRunCostUSD(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)
	r.run.Slots = map[string]*ledger.Slot{
		"a": {PhaseID: "a", CostUSD: 0.25},
		"b": {PhaseID: "b", CostUSD: 0.50},
		"c": nil,
	}
	if got := r.runCostUSD(); got != 0.75 {
		t.Errorf("runCostUSD = %v, want 0.75", got)
	}
}
