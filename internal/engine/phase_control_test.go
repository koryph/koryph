// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/phasecontrol"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/runtimecanary"
)

func phaseControlRunner(t *testing.T) (*runner, *fakeSource, *ledger.Slot, string) {
	t.Helper()
	store := &ledger.Store{KoryphRoot: t.TempDir()}
	run := &ledger.Run{RunID: "run-1", ProjectID: "demo", Slots: map[string]*ledger.Slot{}}
	source := &fakeSource{}
	sl := &ledger.Slot{PhaseID: "bead-1", BeadID: "bead-1"}
	r := &runner{
		opts:    Options{ProjectID: "demo"},
		adapter: source,
		store:   store,
		run:     run,
		issues:  map[string]beads.Issue{"bead-1": {ID: "bead-1"}},
	}
	return r, source, sl, store.PhaseDir(run.RunID, sl.PhaseID)
}

func TestPhaseControlAddsCurrentBeadLabelOnce(t *testing.T) {
	r, source, sl, phaseDir := phaseControlRunner(t)
	req, err := phasecontrol.NewRequest(sl.PhaseID, phasecontrol.OperationLabelAdd)
	if err != nil {
		t.Fatal(err)
	}
	req.Label = "area:docs"
	if err := phasecontrol.Submit(phaseDir, req); err != nil {
		t.Fatal(err)
	}
	r.processPhaseRequests(context.Background(), sl)
	r.processPhaseRequests(context.Background(), sl)
	if len(source.addLabels) != 1 || source.addLabels[0] != [2]string{"bead-1", "area:docs"} {
		t.Fatalf("AddLabel calls = %v", source.addLabels)
	}
	resp, err := phasecontrol.WaitResponse(context.Background(), phaseDir, req)
	if err != nil || resp.State != phasecontrol.ResponseApplied {
		t.Fatalf("response=%+v err=%v", resp, err)
	}
}

func TestPhaseControlRejectsRoutingLabelAndPhaseMismatch(t *testing.T) {
	r, source, sl, phaseDir := phaseControlRunner(t)
	for _, tc := range []struct {
		phase string
		label string
	}{
		{phase: sl.PhaseID, label: "model:frontier"},
		{phase: "another-bead", label: "area:docs"},
	} {
		req, err := phasecontrol.NewRequest(tc.phase, phasecontrol.OperationLabelAdd)
		if err != nil {
			t.Fatal(err)
		}
		req.Label = tc.label
		if err := phasecontrol.Submit(phaseDir, req); err != nil {
			t.Fatal(err)
		}
		r.processPhaseRequests(context.Background(), sl)
		resp, err := phasecontrol.WaitResponse(context.Background(), phaseDir, req)
		if err != nil || resp.State != phasecontrol.ResponseRejected {
			t.Fatalf("response=%+v err=%v", resp, err)
		}
	}
	if len(source.addLabels) != 0 {
		t.Fatalf("unexpected AddLabel calls: %v", source.addLabels)
	}
}

func TestPhaseControlRunsTargetCanaryAsynchronouslyAndReplays(t *testing.T) {
	r, _, sl, phaseDir := phaseControlRunner(t)
	sl.Worktree = t.TempDir()
	r.rec = &registry.Record{
		Name:             "demo",
		Root:             sl.Worktree,
		AccountProfile:   "work",
		ClaudeConfigDir:  "/target/claude",
		ExpectedIdentity: "owner@example.com",
	}
	r.cfg = &project.Config{}
	calls := 0
	r.runtimeCanaryRun = func(_ context.Context, o runtimecanary.Options) runtimecanary.Result {
		calls++
		if o.Profile.ConfigDir != "/target/claude" || o.Model != "sonnet" {
			t.Errorf("canary options = %+v", o)
		}
		return runtimecanary.Result{OK: true, Runtime: "claude", Kind: "ok", Detail: "authenticated headless canary passed"}
	}
	req, err := phasecontrol.NewRequest(sl.PhaseID, phasecontrol.OperationRuntimeCanary)
	if err != nil {
		t.Fatal(err)
	}
	req.Runtime = "claude"
	if err := phasecontrol.Submit(phaseDir, req); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for {
		r.processPhaseRequests(context.Background(), sl)
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		resp, waitErr := phasecontrol.WaitResponse(ctx, phaseDir, req)
		cancel()
		if waitErr == nil {
			if resp.State != phasecontrol.ResponseApplied {
				t.Fatalf("response = %+v", resp)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("canary response did not arrive: %v", waitErr)
		}
		time.Sleep(time.Millisecond)
	}
	r.processPhaseRequests(context.Background(), sl)
	if calls != 1 {
		t.Fatalf("canary calls = %d, want one durable execution", calls)
	}
}

func TestRuntimeCanaryRetriesTransientInsideOrchestratorBudget(t *testing.T) {
	r, _, sl, _ := phaseControlRunner(t)
	sl.Worktree = t.TempDir()
	r.rec = &registry.Record{
		Name:             "demo",
		Root:             sl.Worktree,
		AccountProfile:   "work",
		ClaudeConfigDir:  "/target/claude",
		ExpectedIdentity: "owner@example.com",
	}
	r.cfg = &project.Config{}
	calls := 0
	r.runtimeCanaryRun = func(_ context.Context, _ runtimecanary.Options) runtimecanary.Result {
		calls++
		if calls == 1 {
			return runtimecanary.Result{Runtime: "claude", Kind: "transient", Detail: "temporary timeout"}
		}
		return runtimecanary.Result{OK: true, Runtime: "claude", Kind: "ok", Detail: "passed"}
	}
	oldBackoff := runtimeCanaryRetryBackoff
	runtimeCanaryRetryBackoff = time.Millisecond
	defer func() { runtimeCanaryRetryBackoff = oldBackoff }()

	result := r.executeRuntimeCanary(context.Background(), sl, "claude")
	if !result.OK || calls != runtimeCanaryAttempts {
		t.Fatalf("result=%+v calls=%d", result, calls)
	}
}
