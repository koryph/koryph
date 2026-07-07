// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/ledger"
)

// tokenUsageClaudeScript acts like fakeClaudeScript but its result line also
// carries a usage block (koryph-77r.1), the real stream-json shape
// completeSlot's dispatch.ParseResultUsage parses.
const tokenUsageClaudeScript = `#!/bin/sh
cat > /dev/null
echo "work" > agent-work.txt
git add agent-work.txt
git commit -q --no-verify -m "feat(tb1): work"
printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
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

	sl := &ledger.Slot{PhaseID: "tb1", Status: ledger.SlotRunning, InputTokens: 100, OutputTokens: 10, CacheReadTokens: 50, CacheCreationTokens: 5}
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatalf("SetSlot: %v", err)
	}

	r.applyTokenUsage("tb1", dispatch.TokenUsage{InputTokens: 20, OutputTokens: 2, CacheReadTokens: 10, CacheCreationTokens: 1})

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
}
