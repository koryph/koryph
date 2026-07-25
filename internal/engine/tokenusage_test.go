// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/runtime/codex"
)

// tokenUsageClaudeScript acts like fakeClaudeScript but its result line also
// carries a usage block (koryph-77r.1), the real stream-json shape
// completeSlot's dispatch.ParseResultUsage parses.
const tokenUsageClaudeScript = `#!/bin/sh
` + fakeCompletionFunction + `
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
koryph_test_complete agent-work.txt || exit $?
printf '{"type":"result","total_cost_usd":0.42,"usage":{"input_tokens":1000,"output_tokens":50,"cache_read_input_tokens":8000,"cache_creation_input_tokens":200}}\n'
exit 0
`

// TestCompleteSlotPersistsTokenUsageFromResultLine proves completeSlot's
// dispatch.ParseResultUsage wiring end-to-end (koryph-77r.1): a fake agent
// whose stream-json result line carries a usage block leaves that
// composition on the merged bead's ledger slot.
func TestCompleteSlotPersistsTokenUsageFromResultLine(t *testing.T) {
	f := newFixture(t, fixOpts{claudeScript: tokenUsageClaudeScript})

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 1 {
		t.Fatalf("Outcome = %+v, want 1 merged", got)
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
	if sl.InputTokens != 1000 || sl.OutputTokens != 50 || sl.CacheReadTokens != 8000 || sl.CacheCreationTokens != 200 {
		t.Errorf("slot token composition = %+v, want 1000/50/8000/200", sl)
	}
}

func TestParseRuntimeSignalsPreservesCodexNormalizationAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.jsonl")
	line := []byte(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3}}` + "\n")
	if err := os.WriteFile(path, line, 0o600); err != nil {
		t.Fatal(err)
	}

	signals := parseRuntimeSignals(codex.New(""), path)
	usage, ok := signals.usage()
	if !ok {
		t.Fatal("parseRuntimeSignals returned no usage")
	}
	if usage.InputTokens != 6 || usage.CacheReadTokens != 4 || usage.OutputTokens != 3 {
		t.Fatalf("normalized usage = %+v, want fresh/cache/output 6/4/3", usage)
	}
	if usage.TokenSemantics != runtime.TokenSemanticsDisjointV1 {
		t.Fatalf("TokenSemantics = %q, want %q",
			usage.TokenSemantics, runtime.TokenSemanticsDisjointV1)
	}
	if usage.ProviderTotalInputTokens != 10 || !usage.HasProviderTotalInput {
		t.Fatalf("provider-total audit = %d present:%v, want 10/true",
			usage.ProviderTotalInputTokens, usage.HasProviderTotalInput)
	}
	if total := totalAttemptTokens(usage); total != 13 {
		t.Fatalf("normalized attempt total = %d, want 6+4+3=13", total)
	}
}

// modelFallbackClaudeScript emits a result line whose modelUsage says the
// session was dominantly served by a haiku model id even though dispatch
// requested sonnet — the --fallback-model downgrade shape (koryph-qf6.2).
const modelFallbackClaudeScript = `#!/bin/sh
` + fakeCompletionFunction + `
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
koryph_test_complete agent-work.txt || exit $?
printf '{"type":"result","total_cost_usd":0.10,"modelUsage":{"claude-haiku-4-5-20251001":{"outputTokens":900},"claude-sonnet-4-5":{"outputTokens":100}}}\n'
exit 0
`

// TestCompleteSlotRecordsActualModel proves the koryph-qf6.2 wiring
// end-to-end: the result line's modelUsage keys are reduced to the dominant
// model, normalized to a tier, and persisted as Slot.ModelActual (mirrored
// onto the manifest) — so a --fallback-model downgrade is visible instead of
// the outcome being silently attributed to the requested tier.
func TestCompleteSlotRecordsActualModel(t *testing.T) {
	f := newFixture(t, fixOpts{claudeScript: modelFallbackClaudeScript})

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.Merged != 1 {
		t.Fatalf("Outcome = %+v, want 1 merged", got)
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
	if sl.Model != "sonnet" {
		t.Fatalf("requested model = %q, want sonnet (implement stage default)", sl.Model)
	}
	if sl.ModelActual != "haiku" {
		t.Errorf("ModelActual = %q, want haiku (dominant modelUsage id, normalized to its tier)", sl.ModelActual)
	}
	if m, err := store.LoadManifest(run.RunID, "tb1"); err == nil {
		if m.ModelActual != "haiku" {
			t.Errorf("manifest ModelActual = %q, want haiku (checkpoint mirror)", m.ModelActual)
		}
	} else {
		t.Errorf("LoadManifest: %v", err)
	}
}

// TestCacheRatioWarnPureLogic exercises the I7 cache-ratio tripwire's
// threshold arithmetic (koryph-77r.1, design
// docs/designs/2026-07-token-economy.md §2 I7) in isolation from logging.
func TestCacheRatioWarnPureLogic(t *testing.T) {
	cases := []struct {
		name      string
		u         dispatch.TokenUsage
		wantWarn  bool
		wantRatio float64
	}{
		{
			name:     "below volume floor never warns even with a terrible ratio",
			u:        dispatch.TokenUsage{InputTokens: 100, CacheReadTokens: 0, CacheCreationTokens: 100},
			wantWarn: false,
		},
		{
			name:      "material volume, healthy ratio does not warn",
			u:         dispatch.TokenUsage{InputTokens: 1000, CacheReadTokens: 30000, CacheCreationTokens: 1000},
			wantWarn:  false,
			wantRatio: 30000.0 / 32000.0,
		},
		{
			name:      "material volume, collapsed ratio warns",
			u:         dispatch.TokenUsage{InputTokens: 15000, CacheReadTokens: 5000, CacheCreationTokens: 5000},
			wantWarn:  true,
			wantRatio: 5000.0 / 25000.0,
		},
		{
			name:     "exactly at the floor does not warn (strict less-than)",
			u:        dispatch.TokenUsage{InputTokens: 10000, CacheReadTokens: 10000, CacheCreationTokens: 0},
			wantWarn: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ratio, total, warn := cacheRatioWarn(tc.u)
			if warn != tc.wantWarn {
				t.Errorf("warn = %v, want %v (ratio=%g total=%d)", warn, tc.wantWarn, ratio, total)
			}
			if tc.wantWarn && (ratio < tc.wantRatio-1e-9 || ratio > tc.wantRatio+1e-9) {
				t.Errorf("ratio = %g, want %g", ratio, tc.wantRatio)
			}
		})
	}
}

// TestApplyTokenUsageAccumulates proves applyTokenUsage ADDs one attempt's
// token composition onto the slot's persisted totals rather than overwriting
// them (koryph-77r.1, mirroring CostUSD's accumulation across requeues).
func TestApplyTokenUsageAccumulates(t *testing.T) {
	f := newFixture(t, fixOpts{})
	r := runnerFromFixture(t, f)

	sl := &ledger.Slot{
		PhaseID: "tb1", Status: ledger.SlotRunning,
		InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CacheCreationTokens: 5,
		ProviderTotalInputTokens: 150, HasProviderTotalInput: true,
	}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	r.applyTokenUsage("tb1", dispatch.TokenUsage{
		InputTokens: 20, OutputTokens: 2, CacheReadTokens: 10, CacheCreationTokens: 1,
		TokenSemantics:           runtime.TokenSemanticsDisjointV1,
		ProviderTotalInputTokens: 30, HasProviderTotalInput: true,
	})

	got, err := r.store.LoadRun(r.run.RunID)
	if err != nil {
		t.Fatalf("LoadRun: %v", err)
	}
	gotSlot := got.Slots["tb1"]
	if gotSlot == nil {
		t.Fatal("slot tb1 missing after applyTokenUsage")
	}
	if gotSlot.InputTokens != 120 || gotSlot.OutputTokens != 12 || gotSlot.CacheReadTokens != 60 || gotSlot.CacheCreationTokens != 6 {
		t.Errorf("slot tokens = %+v, want accumulated 120/12/60/6", gotSlot)
	}
	if gotSlot.ProviderTotalInputTokens != 180 || !gotSlot.HasProviderTotalInput {
		t.Errorf("provider-total audit = %d present:%v, want accumulated 180/true",
			gotSlot.ProviderTotalInputTokens, gotSlot.HasProviderTotalInput)
	}
	if normalized := gotSlot.InputTokens + gotSlot.OutputTokens +
		gotSlot.CacheReadTokens + gotSlot.CacheCreationTokens; normalized != 198 {
		t.Errorf("normalized total = %d, want 198 without provider-total audit", normalized)
	}
}

func TestProviderTotalAuditExcludedFromRuntimeDecisions(t *testing.T) {
	u := dispatch.TokenUsage{
		InputTokens: 6, OutputTokens: 2, CacheReadTokens: 4,
		TokenSemantics:           runtime.TokenSemanticsDisjointV1,
		ProviderTotalInputTokens: 1_000_000, HasProviderTotalInput: true,
	}
	if got := totalAttemptTokens(u); got != 12 {
		t.Errorf("totalAttemptTokens = %d, want 12 normalized tokens", got)
	}
	ratio, total, warn := cacheRatioWarn(u)
	if ratio != 0 || total != 10 || warn {
		t.Errorf("cacheRatioWarn = ratio:%v total:%d warn:%v, want 0/10/false", ratio, total, warn)
	}
}

func TestAllRequeuePathsPreserveNormalizedAndProviderTokenCounters(t *testing.T) {
	cases := []struct {
		name    string
		requeue func(context.Context, *runner, *ledger.Slot)
	}{
		{
			name: "generic crash gate review merge and conflict path",
			requeue: func(ctx context.Context, r *runner, sl *ledger.Slot) {
				r.requeueSlot(ctx, sl, "", "agent died with commits")
			},
		},
		{
			name: "rate limit path",
			requeue: func(ctx context.Context, r *runner, sl *ledger.Slot) {
				r.requeueRateLimited(ctx, sl)
			},
		},
		{
			name: "budget kill path",
			requeue: func(ctx context.Context, r *runner, sl *ledger.Slot) {
				r.requeueBudgetKilled(ctx, sl, 1, dispatch.TokenUsage{})
			},
		},
		{
			name: "turn ceiling path",
			requeue: func(ctx context.Context, r *runner, sl *ledger.Slot) {
				r.requeueTurnExhausted(ctx, sl)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixOpts{})
			r, backend := escalationRunner(t, f)
			sl := escalationSlot(t, r, "tb1", 1)
			sl.InputTokens = 6
			sl.OutputTokens = 2
			sl.CacheReadTokens = 4
			sl.ProviderTotalInputTokens = 10
			sl.HasProviderTotalInput = true
			if err := r.store.SaveRun(r.run); err != nil {
				t.Fatalf("SaveRun seeded counters: %v", err)
			}

			tc.requeue(t.Context(), r, sl)

			if len(backend.specs) != 1 {
				t.Fatalf("dispatches = %d, want exactly one replacement", len(backend.specs))
			}
			got := r.run.Slots["tb1"]
			if got == nil {
				t.Fatal("replacement slot tb1 missing")
			}
			if got.InputTokens != 6 || got.OutputTokens != 2 ||
				got.CacheReadTokens != 4 || got.CacheCreationTokens != 0 {
				t.Errorf("normalized counters = %+v, want fresh/output/cache-read/cache-create 6/2/4/0", got)
			}
			if got.ProviderTotalInputTokens != 10 || !got.HasProviderTotalInput {
				t.Errorf("provider-total audit = %d present:%v, want 10/true",
					got.ProviderTotalInputTokens, got.HasProviderTotalInput)
			}
			if r.run.TokenSemantics != ledger.CurrentTokenSemantics {
				t.Errorf("run token semantics = %q, want %q",
					r.run.TokenSemantics, ledger.CurrentTokenSemantics)
			}
			normalized := got.InputTokens + got.OutputTokens +
				got.CacheReadTokens + got.CacheCreationTokens
			if normalized != 12 {
				t.Errorf("normalized total = %d, want 12; provider-total audit must not be added", normalized)
			}
		})
	}
}
