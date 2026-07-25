// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
)

const capabilityAndHealthyClaudeScript = `#!/bin/sh
` + fakeCompletionFunction + `
cat > /dev/null
echo "work for $KORYPH_PHASE_ID" > "agent-$KORYPH_PHASE_ID.txt"
git add "agent-$KORYPH_PHASE_ID.txt"
git commit -q --no-verify -m "feat($KORYPH_PHASE_ID): work"
if [ "$KORYPH_PHASE_ID" = "blocked" ]; then
  printf '{"state":"blocked","block_kind":"capability","capability":"network","detail":"proxy unavailable"}\n' > "$KORYPH_STATUS_PATH"
else
  printf 'status: ready-for-merge\n' > "$KORYPH_SUMMARY_PATH"
  koryph_test_complete "agent-$KORYPH_PHASE_ID.txt" || exit $?
fi
printf '{"type":"result","total_cost_usd":0.10}\n'
exit 0
`

func TestRunOnceCapabilityBlockIsSlotLocal(t *testing.T) {
	specs := []beadSpec{{id: "blocked", fp: "blocked"}, {id: "healthy", fp: "healthy"}}
	f := newFixture(t, fixOpts{
		bdScript:     multiBeadBDScript(specs),
		claudeScript: capabilityAndHealthyClaudeScript,
	})
	var out bytes.Buffer
	got, err := Run(context.Background(), baseOptions(&out))
	if err != nil {
		t.Fatalf("Run: %v\n%s", err, out.String())
	}
	if got.Code != ExitOK || got.Dispatched != 2 || got.Merged != 1 || got.Blocked != 1 {
		t.Fatalf("outcome = %+v, want ExitOK with one merged and one blocked\n%s", got, out.String())
	}
	if !strings.Contains(runGit(t, f.repo, "log", "--format=%s", "main"), "feat(healthy): work") {
		t.Fatalf("healthy sibling did not merge\n%s", out.String())
	}
	hold, ok, err := ledger.NewStore(f.repo).LoadCapabilityHold("blocked")
	if err != nil || !ok || hold.Capability != "network" {
		t.Fatalf("hold = %+v, %v, %v", hold, ok, err)
	}
}

func TestCapabilityHoldIgnoresStatusOnlyReopenAndAllowsNoteOnce(t *testing.T) {
	r, _, _ := candidateFixture(t)
	r.cfg = &project.Config{}
	issue := beads.Issue{ID: "candidate", Status: "blocked", UpdatedAt: "first"}
	hold := ledger.CapabilityHold{BeadID: issue.ID, Capability: "network", RetryLimit: 1}
	hold.EvidenceHash = r.capabilityEvidenceHash(context.Background(), issue, hold)
	if err := r.store.SetCapabilityHold(hold); err != nil {
		t.Fatal(err)
	}

	reopened := issue
	reopened.Status = "open"
	reopened.UpdatedAt = "second"
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{reopened}); len(got) != 0 {
		t.Fatalf("status-only reopen escaped hold: %+v", got)
	}

	reopened.Notes = "operator repaired the proxy"
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{reopened}); len(got) != 1 {
		t.Fatalf("durable note did not change evidence: %+v", got)
	}
	if !r.consumeCapabilityRetry(issue.ID) {
		t.Fatal("changed evidence was not consumed")
	}
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{reopened}); len(got) != 0 {
		t.Fatalf("consumed evidence escaped hold again: %+v", got)
	}

	hold, _, err := r.store.LoadCapabilityHold(issue.ID)
	if err != nil || hold.RetryCount != 1 {
		t.Fatalf("hold = %+v, %v", hold, err)
	}
}

func TestCapabilityHoldAllowsOnlyPassingProbe(t *testing.T) {
	r, _, _ := candidateFixture(t)
	r.cfg = &project.Config{}
	issue := beads.Issue{ID: "candidate"}
	hold := ledger.CapabilityHold{BeadID: issue.ID, Capability: "network", RetryLimit: 1}
	hold.EvidenceHash = r.capabilityEvidenceHash(context.Background(), issue, hold)
	if err := r.store.SetCapabilityHold(hold); err != nil {
		t.Fatal(err)
	}
	if err := r.store.RecordCapabilityProbe(issue.ID, "network", "still down", false); err != nil {
		t.Fatal(err)
	}
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{issue}); len(got) != 0 {
		t.Fatalf("failed probe changed eligibility: %+v", got)
	}
	if err := r.store.RecordCapabilityProbe(issue.ID, "network", "reachable", true); err != nil {
		t.Fatal(err)
	}
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{issue}); len(got) != 1 {
		t.Fatalf("passing probe did not change eligibility: %+v", got)
	}
}

func TestCapabilityHoldAllowsBranchHeadChange(t *testing.T) {
	r, _, wt := candidateFixture(t)
	r.cfg = &project.Config{}
	issue := beads.Issue{ID: "candidate"}
	hold := ledger.CapabilityHold{BeadID: issue.ID, Capability: "toolchain", RetryLimit: 1}
	hold.EvidenceHash = r.capabilityEvidenceHash(context.Background(), issue, hold)
	if err := r.store.SetCapabilityHold(hold); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "repair.txt"), "repair\n", 0o644)
	runGit(t, wt, "add", "repair.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "fix(toolchain): repair capability")
	if got := r.filterCapabilityHolds(context.Background(), []beads.Issue{issue}); len(got) != 1 {
		t.Fatalf("branch change did not restore eligibility: %+v", got)
	}
}

func TestDispatchBeadHeldUnchangedUsesZeroBackendCalls(t *testing.T) {
	r, _, _ := candidateFixture(t)
	r.backend = &capturingBackend{}
	if err := r.store.SetCapabilityHold(ledger.CapabilityHold{
		BeadID: "candidate", Capability: "network", EvidenceHash: "unchanged", RetryLimit: 1,
	}); err != nil {
		t.Fatal(err)
	}
	r.dispatchBead(context.Background(), dispatchReq{
		issue: beads.Issue{ID: "candidate"}, attempt: 1,
	})
	if got := len(r.backend.(*capturingBackend).specs); got != 0 {
		t.Fatalf("backend dispatches = %d, want zero", got)
	}
	if sl := r.run.Slots["candidate"]; sl == nil || sl.Status != ledger.SlotBlocked {
		t.Fatalf("slot = %+v, want blocked", sl)
	}
}
