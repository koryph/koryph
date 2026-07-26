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
// not false-positive and commitless work is preserved without blind retries.
const ordinaryFailureClaudeScript = `#!/bin/sh
cat > /dev/null
printf '{"type":"result","total_cost_usd":0.01,"is_error":true,"subtype":"error_max_turns"}\n'
exit 1
`

// TestRateLimitedDeathRequeuesWithoutBurningAttempt proves koryph-2im.4's core
// contract: a death classified as rate-limited requeues via the
// typed runtime-transient budget (2) instead of ledger.MaxAttempts, reports
// the signal to the machine-wide governor every time, and parks with a clear
// typed outcome once its budget is exhausted — all while Attempts never moves
// off its initial value.
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
	if !strings.Contains(sl.Note, "runtime-transient-exhausted") {
		t.Errorf("slot note = %q, want typed transient exhaustion", sl.Note)
	}
	if sl.RateLimitRequeues != 2 || sl.Retry.TransientRetries != 2 {
		t.Errorf("typed transient counters = legacy %d / typed %d, want 2 / 2",
			sl.RateLimitRequeues, sl.Retry.TransientRetries)
	}
	// The whole point: Attempts must NEVER move for an environmental failure.
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (unchanged by rate-limit requeues)", sl.Attempts)
	}

	// Every rate-limited death (including the one that ultimately blocked) was
	// reported to the shared governor.
	gs := govern.NewStore()
	status, err := gs.AIMDStatus(fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	wantEvents := 3 // initial dispatch's death + two typed retries' deaths
	if status.RateLimitEvents != wantEvents {
		t.Errorf("governor RateLimitEvents = %d, want %d", status.RateLimitEvents, wantEvents)
	}
}

// TestOrdinaryCommitlessDeathParksImmediately is the negative control: a death
// with no rate-limit marker in its stream is unaffected by transient recovery
// and parks immediately because there is no changed evidence to repair.
func TestOrdinaryCommitlessDeathParksImmediately(t *testing.T) {
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
	if !strings.Contains(sl.Note, "code-repair-unchanged") {
		t.Errorf("slot note = %q, want unchanged-evidence code-defect park", sl.Note)
	}
	if sl.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1 (commitless failure parks immediately)", sl.Attempts)
	}
	if sl.Retry.CodeRepairs != 0 {
		t.Errorf("CodeRepairs = %d, want 0 (unchanged evidence is not blindly retried)", sl.Retry.CodeRepairs)
	}
	if sl.RateLimitRequeues != 0 {
		t.Errorf("RateLimitRequeues = %d, want 0 (never classified as rate-limited)", sl.RateLimitRequeues)
	}

	gs := govern.NewStore()
	status, err := gs.AIMDStatus(fixtureAccount)
	if err != nil {
		t.Fatal(err)
	}
	if status.RateLimitEvents != 0 {
		t.Errorf("governor RateLimitEvents = %d, want 0 (ordinary failure never reports rate-limit)", status.RateLimitEvents)
	}
}
