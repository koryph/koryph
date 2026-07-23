// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Requeue-policy regression test for koryph-840: the per-bead turn ceiling.
// enforceTurnCeiling counts a live agent's completed "assistant" turns and,
// past Config.PerAgentMaxTurns, SIGTERMs it; completeSlot then routes the
// stamped death to requeueTurnExhausted, which re-dispatches with a FRESH
// session (no --resume) so the runaway stops re-reading its accreted context.

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/quota"
)

// turnCeilingClaudeScript trips the turn ceiling on its FIRST session: it emits
// four completed "assistant" turns (well past the ceiling of 2 the test sets),
// commits real work plus a .done marker, then hangs so the engine observes it
// alive past the ceiling and SIGTERMs it (the trap exits cleanly — the work is
// already committed). On the FRESH-session requeue the rebased worktree still
// carries the committed .done marker, so the second invocation finishes
// immediately instead of tripping again.
const turnCeilingClaudeScript = `#!/bin/sh
cat > /dev/null
if [ -f .done ]; then
  printf '{"type":"result","total_cost_usd":0.05,"is_error":false,"num_turns":1}\n'
  exit 0
fi
printf '{"type":"assistant","message":{"stop_reason":"tool_use"}}\n'
printf '{"type":"assistant","message":{"stop_reason":"tool_use"}}\n'
printf '{"type":"assistant","message":{"stop_reason":"tool_use"}}\n'
printf '{"type":"assistant","message":{"stop_reason":"tool_use"}}\n'
echo work > agent-work.txt
touch .done
git add -A
git commit -q --no-verify -m "feat(tb1): work"
trap 'exit 0' TERM
i=0
while [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done
exit 0
`

// TestTurnCeilingInterruptsAndFreshRequeues is the AC2 demonstration: a bead
// that runs past the per-bead turn ceiling is interrupted mid-flight and
// requeued with a fresh session rather than being left to accrete, and the
// fresh attempt (resuming from committed work with a clean context) completes
// and merges.
func TestTurnCeilingInterruptsAndFreshRequeues(t *testing.T) {
	f := newFixture(t, fixOpts{claudeScript: turnCeilingClaudeScript})

	// Lower the ceiling well below what the script emits so a small fixture run
	// trips it deterministically. The engine re-reads the account config each
	// wave, so this takes effect on the first dispatch.
	if _, err := quota.UpdateConfig(fixtureAccount, func(c *quota.Config) error {
		c.PerAgentMaxTurns = 2
		return nil
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	t.Logf("engine output:\n%s", out.String())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got.Merged != 1 || got.Blocked != 0 {
		t.Errorf("Outcome = %+v, want 1 merged / 0 blocked", got)
	}
	if !strings.Contains(out.String(), "turn ceiling hit") {
		t.Errorf("engine output missing the turn-ceiling interruption line:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "fresh-session requeue") {
		t.Errorf("engine output missing the fresh-session requeue line:\n%s", out.String())
	}

	sl := slotFor(t, f.repo, "tb1")
	if sl.Status != ledger.SlotMerged {
		t.Errorf("slot status = %q, want merged", sl.Status)
	}
	if sl.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2 (initial trip + one fresh requeue)", sl.Attempts)
	}
	if sl.TurnExhaustedRequeues != 1 {
		t.Errorf("TurnExhaustedRequeues = %d, want 1", sl.TurnExhaustedRequeues)
	}
}
