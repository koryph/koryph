// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/review"
)

func TestFinalizationLaneRunsBoundedReviewsInParallelAndWakesPoller(t *testing.T) {
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

	gotStarted := map[string]bool{}
	for range 2 {
		select {
		case got := <-started:
			gotStarted[got] = true
		case <-time.After(time.Second):
			t.Fatal("bounded review worker did not start")
		}
	}
	if !gotStarted["first"] || !gotStarted["second"] {
		t.Fatalf("started reviews = %v, want first and second", gotStarted)
	}
	close(release)

	gotResults := map[string]bool{}
	for range 2 {
		select {
		case result := <-lane.results:
			gotResults[result.generation.phaseID] = true
		case <-time.After(time.Second):
			t.Fatal("review result did not arrive")
		}
	}
	if !gotResults["first"] || !gotResults["second"] {
		t.Fatalf("review results = %v, want first and second", gotResults)
	}
	select {
	case <-lane.wake:
	default:
		t.Fatal("review completion did not wake poller")
	}
}

func TestFinalizationLanesOverlapOneGateTwoReviewsAndOneLanding(t *testing.T) {
	lane := newFinalizationLane(t.Context())
	t.Cleanup(lane.close)

	gateStarted := make(chan struct{}, 1)
	mergeStarted := make(chan struct{}, 1)
	reviewsStarted := make(chan string, 2)
	release := make(chan struct{})
	lane.mergeFn = func(ctx context.Context, opts merge.Opts) (merge.Result, error) {
		if opts.ValidateOnly {
			gateStarted <- struct{}{}
		} else {
			mergeStarted <- struct{}{}
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return merge.Result{}, nil
	}
	lane.reviewFn = func(ctx context.Context, opts review.Opts) review.Verdict {
		reviewsStarted <- opts.Branch
		select {
		case <-release:
		case <-ctx.Done():
		}
		return review.Verdict{}
	}

	jobs := []finalizationJob{
		{stage: finalizationGate, mergeOpts: merge.Opts{ValidateOnly: true}},
		{stage: finalizationReview, reviewOpts: review.Opts{Branch: "review-1"}},
		{stage: finalizationReview, reviewOpts: review.Opts{Branch: "review-2"}},
		{stage: finalizationMerge, mergeOpts: merge.Opts{Validated: &merge.GateEvidence{}}},
	}
	for _, job := range jobs {
		if !lane.enqueue(job) {
			t.Fatalf("enqueue %q failed", job.stage)
		}
	}

	select {
	case <-gateStarted:
	case <-time.After(time.Second):
		t.Fatal("gate lane did not start")
	}
	select {
	case <-mergeStarted:
	case <-time.After(time.Second):
		t.Fatal("short landing lane did not overlap")
	}
	started := map[string]bool{}
	for range 2 {
		select {
		case branch := <-reviewsStarted:
			started[branch] = true
		case <-time.After(time.Second):
			t.Fatal("bounded review lanes did not overlap")
		}
	}
	if !started["review-1"] || !started["review-2"] {
		t.Fatalf("started reviews = %v", started)
	}
	close(release)
}

func TestStaleLandingGenerationIsRejectedBeforeMergeSideEffects(t *testing.T) {
	calls := 0
	current := finalizationGeneration{
		phaseID: "bead", stage: finalizationMerge, attempt: 2, branch: "agent/new",
		dispatchGeneration: "dispatch-new",
	}
	stale := finalizationGeneration{
		phaseID: "bead", stage: finalizationMerge, attempt: 1, branch: "agent/old",
		dispatchGeneration: "dispatch-old",
	}
	lane := &finalizationLane{
		ctx:        t.Context(),
		authorized: map[string]finalizationGeneration{"bead": current},
		mergeFn: func(context.Context, merge.Opts) (merge.Result, error) {
			calls++
			return merge.Result{Status: merge.StatusMerged}, nil
		},
	}
	result := lane.execute(finalizationJob{
		generation: stale,
		stage:      finalizationMerge,
	})
	if !errors.Is(result.err, errStaleFinalizationJob) {
		t.Fatalf("stale landing error = %v", result.err)
	}
	if calls != 0 {
		t.Fatalf("merge side effects called %d times, want 0", calls)
	}
}

func TestLandingAuthorizationIncludesExactStageAndFullGeneration(t *testing.T) {
	current := finalizationGeneration{
		phaseID: "bead", stage: finalizationSecurityReview,
		attempt: 1, branch: "agent/x", dispatchGeneration: "dispatch",
		candidateSHA: "candidate", baseSHA: "base", evidenceKey: "evidence",
		evidenceDigest: "sha256:evidence",
		revalidation:   2,
	}
	calls := 0
	lane := &finalizationLane{
		ctx:        t.Context(),
		authorized: map[string]finalizationGeneration{"bead": current},
		mergeFn: func(context.Context, merge.Opts) (merge.Result, error) {
			calls++
			return merge.Result{Status: merge.StatusMerged}, nil
		},
	}
	job := finalizationJob{
		stage: finalizationMerge,
		generation: finalizationGeneration{
			phaseID: "bead", attempt: 1, branch: "agent/x",
			dispatchGeneration: "dispatch", candidateSHA: "candidate",
			baseSHA: "base", evidenceKey: "evidence", evidenceDigest: "sha256:evidence",
			revalidation: 2,
		},
	}
	result := lane.execute(job)
	if !errors.Is(result.err, errStaleFinalizationJob) || calls != 0 {
		t.Fatalf("stage-stale landing = err %v, calls %d", result.err, calls)
	}
}

func TestFinalizationInfrastructureErrorsRetryWithoutModel(t *testing.T) {
	old := finalizationRetryBackoff
	finalizationRetryBackoff = time.Millisecond
	t.Cleanup(func() { finalizationRetryBackoff = old })

	for _, failure := range []string{"fetch origin main", "push origin main", "acquire merge slot", "gate worker exited"} {
		t.Run(failure, func(t *testing.T) {
			calls := 0
			lane := &finalizationLane{
				ctx: t.Context(),
				mergeFn: func(context.Context, merge.Opts) (merge.Result, error) {
					calls++
					if calls < finalizationInfraAttempts {
						return merge.Result{Status: merge.StatusError}, errors.New(failure)
					}
					return merge.Result{Status: merge.StatusValidated}, nil
				},
			}
			result := lane.execute(finalizationJob{
				stage:      finalizationGate,
				generation: finalizationGeneration{phaseID: "bead"},
				mergeOpts:  merge.Opts{ValidateOnly: true},
			})
			if result.err != nil || calls != finalizationInfraAttempts ||
				result.infraTries != finalizationInfraAttempts {
				t.Fatalf("result=%+v calls=%d", result, calls)
			}
		})
	}
}

func TestFinalizationQueuePreservesCandidateAge(t *testing.T) {
	lane := &finalizationLane{}
	lane.gates.ready = sync.NewCond(&lane.gates.mu)
	now := time.Now()
	for _, job := range []finalizationJob{
		{
			generation:  finalizationGeneration{phaseID: "younger"},
			stage:       finalizationGate,
			candidateAt: now.Add(time.Minute),
		},
		{
			generation:  finalizationGeneration{phaseID: "older"},
			stage:       finalizationGate,
			candidateAt: now,
		},
	} {
		if !lane.enqueue(job) {
			t.Fatalf("enqueue %s", job.generation.phaseID)
		}
	}
	if got := lane.gates.queue[0].generation.phaseID; got != "older" {
		t.Fatalf("first queued candidate = %q, want oldest", got)
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
	lane.mergeFn = func(ctx context.Context, _ merge.Opts) (merge.Result, error) {
		<-ctx.Done()
		return merge.Result{}, ctx.Err()
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
		r.validateSlot(context.Background(), sl)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("completed sibling could not enter durable merging while lane was busy")
	}
	if got := r.run.Slots[sl.PhaseID]; got.Status != ledger.SlotMerging || got.FinalizationStage != finalizationGate {
		t.Fatalf("reaped sibling = status %q stage %q, want merging/gate", got.Status, got.FinalizationStage)
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

func TestFinalizationResultStageAndRevalidationMismatchAreIgnored(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      string
		slotStage   string
		resultStage string
		slotReval   int
		resultReval int
	}{
		{"stale gate stage", ledger.SlotMerging, finalizationMerge, finalizationGate, 2, 2},
		{"stale review stage", ledger.SlotReview, finalizationSecurityReview, finalizationReview, 2, 2},
		{"stale revalidation", ledger.SlotReview, finalizationReview, finalizationReview, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sl := &ledger.Slot{
				PhaseID: "bead", Branch: "agent/x", Attempts: 1,
				DispatchGeneration: "dispatch", Status: tc.status,
				FinalizationStage: tc.slotStage,
				Retry:             ledger.RetryCounters{MergeRevalidations: tc.slotReval},
			}
			r := &runner{run: &ledger.Run{Slots: map[string]*ledger.Slot{"bead": sl}}}
			r.applyFinalizationResult(t.Context(), finalizationResult{
				stage: tc.resultStage,
				generation: finalizationGeneration{
					phaseID: "bead", stage: tc.resultStage,
					branch: "agent/x", attempt: 1,
					dispatchGeneration: "dispatch",
					revalidation:       tc.resultReval,
					candidateSHA:       "candidate", baseSHA: "base", evidenceKey: "evidence",
					evidenceDigest: "sha256:evidence",
				},
				review: review.Verdict{Blocking: false},
			})
			if sl.FinalizationStage != tc.slotStage || sl.Status != tc.status {
				t.Fatalf("stale result mutated slot: %+v", sl)
			}
		})
	}
}

func TestResumeMergeFinalizationRecognizesAlreadyLandedBranch(t *testing.T) {
	r, sl, wt := candidateFixture(t)
	writeFile(t, wt+"/landed.txt", "landed\n", 0o644)
	runGit(t, wt, "add", "landed.txt")
	runGit(t, wt, "commit", "--no-verify", "-m", "feat(candidate): landed")

	r.adapter = &fakeSource{}
	r.reg = registry.NewStore()
	r.cfg = &project.Config{}
	r.opts = Options{ProjectID: "proj"}
	opts, err := r.qualityMergeOpts(sl)
	if err != nil {
		t.Fatal(err)
	}
	opts.ValidateOnly = true
	opts.EvidencePath = r.gateEvidencePathFor(sl, generationForStage(sl, finalizationGate))
	opts.SlotOwner = r.owner
	opts.SlotRetries = 1
	opts.Slot = r.slotLocker(t.Context())
	validated, err := merge.Merge(t.Context(), opts)
	if err != nil || validated.Status != merge.StatusValidated {
		t.Fatalf("validate before partial landing = (%+v, %v)", validated, err)
	}
	installTestGateEvidence(t, r, sl, *validated.Evidence)
	runGit(t, r.rec.Root, "merge", "--ff-only", sl.Branch)

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
