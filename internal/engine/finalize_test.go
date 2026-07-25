// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

func TestFinalizationLaneIsFIFOAndWakesPoller(t *testing.T) {
	lane := newFinalizationLane(t.Context())
	t.Cleanup(lane.close)

	started := make(chan string, 2)
	release := make(chan struct{})
	lane.reviewFn = func(ctx context.Context, opts review.Opts) review.Verdict {
		started <- opts.Branch
		if opts.Branch == "first" {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return review.Verdict{}
	}

	for i, branch := range []string{"first", "second"} {
		if !lane.enqueue(finalizationJob{
			generation: finalizationGeneration{phaseID: branch, attempt: i + 1, branch: branch},
			stage:      finalizationReview,
			reviewOpts: review.Opts{Branch: branch},
		}) {
			t.Fatalf("enqueue %s failed", branch)
		}
	}

	if got := <-started; got != "first" {
		t.Fatalf("first job started = %q, want first", got)
	}
	select {
	case got := <-started:
		t.Fatalf("second job %q started while first was blocked", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)

	for _, want := range []string{"first", "second"} {
		select {
		case <-lane.wake:
		case <-time.After(time.Second):
			t.Fatalf("no poll wake for %s result", want)
		}
		select {
		case got := <-lane.results:
			if got.generation.phaseID != want {
				t.Fatalf("result phase = %q, want %q", got.generation.phaseID, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("no %s result", want)
		}
		if want == "first" {
			if got := <-started; got != "second" {
				t.Fatalf("second job started = %q, want second", got)
			}
		}
	}
}

func TestSlowFinalizationDoesNotBlockSiblingReaping(t *testing.T) {
	r, sl, _ := candidateFixture(t)
	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	r.opts = Options{ProjectID: "proj"}

	lane := newFinalizationLane(t.Context())
	r.finalizer = lane
	started := make(chan struct{})
	lane.reviewFn = func(ctx context.Context, _ review.Opts) review.Verdict {
		close(started)
		<-ctx.Done()
		return review.Verdict{Degraded: true, Reason: ctx.Err().Error()}
	}
	if !lane.enqueue(finalizationJob{
		generation: finalizationGeneration{phaseID: "slow", attempt: 1, branch: "slow"},
		stage:      finalizationReview,
	}) {
		t.Fatal("enqueue slow review")
	}
	<-started

	done := make(chan struct{})
	go func() {
		r.mergeSlot(context.Background(), sl)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("completed sibling could not enter durable merging while lane was busy")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotMerging || got.FinalizationStage != finalizationMerge {
		t.Fatalf("reaped sibling = status %q stage %q, want merging/merge", got.Status, got.FinalizationStage)
	}
	lane.close()
	r.finalizer = nil
}

func TestFinalizationResultGenerationMismatchIsIgnored(t *testing.T) {
	var out bytes.Buffer
	sl := &ledger.Slot{
		PhaseID: "bead", Branch: "agent/new", Attempts: 2,
		Status: ledger.SlotMerging, FinalizationStage: finalizationMerge,
	}
	r := &runner{
		opts: Options{Out: &out},
		run:  &ledger.Run{Slots: map[string]*ledger.Slot{"bead": sl}},
	}
	r.applyFinalizationResult(context.Background(), finalizationResult{
		generation: finalizationGeneration{phaseID: "bead", attempt: 1, branch: "agent/old"},
		stage:      finalizationMerge,
		merge:      merge.Result{Status: merge.StatusMerged, MergedSHA: "stale"},
	})
	if sl.Status != ledger.SlotMerging {
		t.Fatalf("stale result changed status to %q", sl.Status)
	}
}

func TestResumeMergeFinalizationRecognizesAlreadyLandedBranch(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, wt+"/landed.txt", "landed\n", 0o644)
	runGit(t, wt, "add", "landed.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): landed")
	runGit(t, r.rec.Root, "merge", "--ff-only", sl.Branch)

	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	r.opts = Options{ProjectID: "proj"}
	sl.Status = ledger.SlotFinalizing
	sl.CompletionAccounted = true
	sl.FinalizationStage = finalizationMerge
	if err := r.store.SetSlot(r.run, sl); err != nil {
		t.Fatal(err)
	}
	beforeAttempts := sl.Attempts

	r.resumeFinalization(context.Background(), sl)

	if sl.Status != ledger.SlotMerged {
		t.Fatalf("already-landed slot status = %q, want merged", sl.Status)
	}
	if sl.Attempts != beforeAttempts || r.dispatched != 0 {
		t.Fatalf("attempts=%d dispatched=%d, want unchanged %d/0", sl.Attempts, r.dispatched, beforeAttempts)
	}
}
