// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/obs"
)

// runEndReason scans captured slog records for an engine.run.end record whose
// run_id matches and returns its reason attribute (ok=false when none found).
func runEndReason(capH *capturingHandler, runID string) (string, bool) {
	capH.mu.Lock()
	defer capH.mu.Unlock()
	for _, rec := range capH.recs {
		if rec.Message != "engine.run.end" {
			continue
		}
		var gotRun, reason string
		rec.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case obs.KeyRunID:
				gotRun = a.Value.String()
			case "reason":
				reason = a.Value.String()
			}
			return true
		})
		if gotRun == runID {
			return reason, true
		}
	}
	return "", false
}

// TestSIGTERMInterruptedFinalizesAndEmitsRunEnd verifies part (3) of
// koryph-oixo: a SIGTERM to a live engine — delivered as ctx cancellation,
// exactly what cmdRun's signal.NotifyContext now wires — reaches the engine's
// graceful interrupted() path even with an agent in flight. The run is left
// status=running and its active slot is checkpointed (recoverable by --resume
// or `koryph ops reconcile`), and engine.run.end is emitted with the
// "interrupted" reason. An abrupt SIGKILL can never be handled; that is what
// the read-side liveness derivation (parts 1/2) is the backstop for.
func TestSIGTERMInterruptedFinalizesAndEmitsRunEnd(t *testing.T) {
	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	orig := log
	log = obs.For("engine")
	t.Cleanup(func() {
		log = orig
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	specs := []beadSpec{{id: "tb1", fp: "a"}}
	f := newFixture(t, fixOpts{bdScript: multiBeadBDScript(specs), claudeScript: slowClaudeScript})
	t.Setenv("KORYPH_TEST_SLOW_PHASE", "tb1")
	t.Setenv("KORYPH_TEST_SLOW_SECONDS", "10") // still in flight when we cancel

	out := &syncBuf{}
	opts := baseOptions(nil)
	opts.Out = out
	opts.Once = false
	opts.DispatchMode = "rolling"
	opts.PollSec = 1

	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		got Outcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		got, err := Run(ctx, opts)
		done <- result{got, err}
	}()

	store := ledger.NewStore(f.repo)
	dispatched := waitForCondition(10*time.Second, func() bool {
		run, err := store.LoadLatest()
		return err == nil && run != nil && run.Slots["tb1"] != nil && run.Slots["tb1"].DispatchedAt != ""
	})
	if !dispatched {
		cancel()
		t.Fatalf("tb1 never dispatched; log:\n%s", out.String())
	}

	// The operator/loop SIGTERM. cmdRun translates it into this cancel().
	cancel()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Run: %v", r.err)
		}
		if r.got.Reason != "interrupted" || r.got.Code != ExitOK {
			t.Errorf("Outcome = %+v, want reason=interrupted Code=%d", r.got, ExitOK)
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("Run did not return after cancel; log:\n%s", out.String())
	}

	// Left recoverable: status still running (for --resume), the in-flight slot
	// checkpointed and non-terminal.
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	if run.Status != ledger.RunRunning {
		t.Errorf("run status = %q, want %q (left running for resume)", run.Status, ledger.RunRunning)
	}
	if sl := run.Slots["tb1"]; sl == nil || ledger.Terminal(sl.Status) {
		t.Errorf("tb1 slot = %+v, want present and non-terminal (checkpointed in flight)", sl)
	}

	// engine.run.end must fire on the interrupted path — the structured record
	// that a killed-without-a-handler engine (the original bug) never wrote.
	if reason, ok := runEndReason(capH, run.RunID); !ok {
		t.Errorf("no engine.run.end record for run %s — the SIGTERM path must finalize the run log", run.RunID)
	} else if reason != "interrupted" {
		t.Errorf("engine.run.end reason = %q, want %q", reason, "interrupted")
	}
}
