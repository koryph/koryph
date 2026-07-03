// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
)

// rateLimitedClaudeScript never commits and always reports a rate-limit
// error on stream-json stdout, so ParseRateLimited classifies every death as
// rate-limited (koryph-2im.4) regardless of how many times it is requeued.
const rateLimitedClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"error","message":"rate_limit_error: 429 too many requests"}\n'
exit 1
`

// ordinaryFailureClaudeScript never commits and dies with an ordinary
// (non-rate-limit) error — the negative control proving classification does
// not false-positive and the existing Attempts-budget path is unaffected.
const ordinaryFailureClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"result","total_cost_usd":0.01,"is_error":true,"subtype":"error_max_turns"}\n'
exit 1
`

// TestRateLimitedDeathRequeuesWithoutBurningAttempt proves koryph-2im.4's core
// contract: a death classified as rate-limited requeues via the
// RateLimitRequeues budget (5) instead of ledger.MaxAttempts, reports the
// signal to the machine-wide governor every time, and blocks with a clear
// note once its own budget is exhausted — all while Attempts never moves off
// its initial value.
func TestRateLimitedDeathRequeuesWithoutBurningAttempt(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, rateLimitedClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Blocked != 1 || got.Merged != 0 {
		t.Errorf("Outcome = %+v, want 1 blocked / 0 merged", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "rate-limited requeues exhausted") {
		t.Errorf("slot note = %q, want it to name the exhausted rate-limit budget", sl.Note)
	}
	if sl.RateLimitRequeues != rateLimitedRequeueBudget {
		t.Errorf("RateLimitRequeues = %d, want %d (budget exhausted)", sl.RateLimitRequeues, rateLimitedRequeueBudget)
	}
	// The whole point: Attempts must NEVER move for an environmental failure.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (unchanged by rate-limit requeues)", sl.Attempts)
	}

	// Every rate-limited death (including the one that ultimately blocked) was
	// reported to the shared governor.
	gs := govern.NewStore()
	status, err := gs.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := rateLimitedRequeueBudget + 1 // initial dispatch's death + each requeue's death
	if status.RateLimitEvents != wantEvents {
		t.Errorf("governor RateLimitEvents = %d, want %d", status.RateLimitEvents, wantEvents)
	}
}

// TestOrdinaryDeathStillUsesAttemptsBudget is the negative control: a death
// with no rate-limit marker in its stream is unaffected by koryph-2im.4 and
// still exhausts the normal ledger.MaxAttempts budget before blocking.
func TestOrdinaryDeathStillUsesAttemptsBudget(t *testing.T) {
	f := newFixture(t, fixOpts{})
	claudeBin := os.Getenv("KORYPH_CLAUDE_BIN")
	writeFile(t, claudeBin, ordinaryFailureClaudeScript, 0o755)

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Blocked != 1 {
		t.Errorf("Outcome = %+v, want 1 blocked", got)
	}

	store := ledger.NewStore(f.repo)
	run, err := store.LoadLatest()
	if err != nil {
		t.Fatalf("LoadLatest: %v", err)
	}
	sl := run.Slots["tb1"]
	if sl == nil {
		t.Fatalf("no slot tb1 in run: %+v", run.Slots)
	}
	if sl.Status != ledger.SlotBlocked {
		t.Errorf("slot status = %q, want blocked", sl.Status)
	}
	if !strings.Contains(sl.Note, "attempts exhausted") {
		t.Errorf("slot note = %q, want the ordinary attempts-exhausted note", sl.Note)
	}
	if sl.Attempts != ledger.MaxAttempts {
		t.Errorf("Attempts = %d, want %d (ordinary failure still burns attempts)", sl.Attempts, ledger.MaxAttempts)
	}
	if sl.RateLimitRequeues != 0 {
		t.Errorf("RateLimitRequeues = %d, want 0 (never classified as rate-limited)", sl.RateLimitRequeues)
	}

	gs := govern.NewStore()
	status, err := gs.AIMDStatus()
	if err != nil {
		t.Fatal(err)
	}
	if status.RateLimitEvents != 0 {
		t.Errorf("governor RateLimitEvents = %d, want 0 (ordinary failure never reports rate-limit)", status.RateLimitEvents)
	}
}
