// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"strings"
	"sync"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/review"
)

const (
	finalizationReview = "review"
	finalizationMerge  = "merge"
	finalizationPR     = "pr"
)

// finalizationGeneration is the immutable identity of one candidate
// finalization attempt. A result is applied only while all three fields still
// match the live slot, so an operator override or coding requeue cannot receive
// a stale review/merge result from an older generation.
type finalizationGeneration struct {
	phaseID string
	attempt int
	branch  string
}

type finalizationJob struct {
	generation finalizationGeneration
	stage      string
	reviewOpts review.Opts
	mergeOpts  merge.Opts
}

type finalizationResult struct {
	generation finalizationGeneration
	stage      string
	review     review.Verdict
	merge      merge.Result
	err        error
}

// finalizationLane is one process-local FIFO for the two expensive,
// black-box completion operations. The worker owns no runner or ledger
// pointer: it can only execute an immutable review.Opts or merge.Opts value and
// return a value result. The poll goroutine remains the sole owner of runner,
// ledger, Beads, checkpoint, and global-slot mutations.
type finalizationLane struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu      sync.Mutex
	ready   *sync.Cond
	queue   []finalizationJob
	stopped bool

	results chan finalizationResult
	wake    chan struct{}
	done    chan struct{}

	reviewFn func(context.Context, review.Opts) review.Verdict
	mergeFn  func(context.Context, merge.Opts) (merge.Result, error)
}

func newFinalizationLane(parent context.Context) *finalizationLane {
	ctx, cancel := context.WithCancel(parent)
	l := &finalizationLane{
		ctx:      ctx,
		cancel:   cancel,
		results:  make(chan finalizationResult, 1),
		wake:     make(chan struct{}, 1),
		done:     make(chan struct{}),
		reviewFn: review.Review,
		mergeFn:  merge.Merge,
	}
	l.ready = sync.NewCond(&l.mu)
	go l.work()
	return l
}

// enqueue appends without waiting for the worker. The queue is deliberately
// owned under a mutex rather than represented by a bounded channel: a burst of
// completed agents must never block the poll goroutine behind a slow review.
func (l *finalizationLane) enqueue(job finalizationJob) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.stopped {
		return false
	}
	l.queue = append(l.queue, job)
	l.ready.Signal()
	return true
}

func (l *finalizationLane) work() {
	defer close(l.done)
	for {
		l.mu.Lock()
		for len(l.queue) == 0 && !l.stopped {
			l.ready.Wait()
		}
		if l.stopped {
			l.mu.Unlock()
			return
		}
		job := l.queue[0]
		l.queue[0] = finalizationJob{}
		l.queue = l.queue[1:]
		l.mu.Unlock()

		result := l.execute(job)
		select {
		case l.results <- result:
			select {
			case l.wake <- struct{}{}:
			default:
			}
		case <-l.ctx.Done():
			return
		}
	}
}

func (l *finalizationLane) execute(job finalizationJob) finalizationResult {
	out := finalizationResult{generation: job.generation, stage: job.stage}
	switch job.stage {
	case finalizationReview:
		out.review = l.reviewFn(l.ctx, job.reviewOpts)
	case finalizationMerge, finalizationPR:
		out.merge, out.err = l.mergeFn(l.ctx, job.mergeOpts)
	}
	return out
}

func (l *finalizationLane) close() {
	if l == nil {
		return
	}
	l.cancel()
	l.mu.Lock()
	l.stopped = true
	l.queue = nil
	l.ready.Broadcast()
	l.mu.Unlock()
	<-l.done
}

func generationFor(sl *ledger.Slot) finalizationGeneration {
	return finalizationGeneration{
		phaseID: sl.PhaseID,
		attempt: sl.Attempts,
		branch:  sl.Branch,
	}
}

func (r *runner) submitFinalization(ctx context.Context, job finalizationJob) {
	if r.finalizer != nil {
		if r.finalizer.enqueue(job) {
			return
		}
		// A cancelled lane means the run is already stopping. Leave the durable
		// review/merging state for --resume instead of mutating it synchronously
		// after cancellation.
		if ctx.Err() != nil {
			return
		}
	}

	// Narrow unit tests and non-Run callers historically invoke
	// finishCandidate directly. Preserve that synchronous seam while every real
	// engine Run installs the lane before entering its loop.
	l := &finalizationLane{
		ctx:      ctx,
		reviewFn: review.Review,
		mergeFn:  merge.Merge,
	}
	r.applyFinalizationResult(ctx, l.execute(job))
}

func (r *runner) finalizationWake() <-chan struct{} {
	if r.finalizer == nil {
		return nil
	}
	return r.finalizer.wake
}

// drainFinalizationResults is called only by the poll goroutine. It drains the
// worker's value results before examining slot liveness so all runner/store
// side effects stay serialized with ordinary poll transitions.
func (r *runner) drainFinalizationResults(ctx context.Context) {
	if r.finalizer == nil {
		return
	}
	for {
		select {
		case result := <-r.finalizer.results:
			r.applyFinalizationResult(ctx, result)
		default:
			return
		}
	}
}

func (r *runner) applyFinalizationResult(ctx context.Context, result finalizationResult) {
	sl := r.run.Slots[result.generation.phaseID]
	if sl == nil ||
		sl.Attempts != result.generation.attempt ||
		sl.Branch != result.generation.branch {
		r.progress("bead %s: discarded stale %s result for attempt %d branch %s",
			result.generation.phaseID, result.stage, result.generation.attempt, result.generation.branch)
		return
	}

	switch result.stage {
	case finalizationReview:
		if sl.Status != ledger.SlotReview {
			r.progress("bead %s: discarded review result after state changed to %s", sl.PhaseID, sl.Status)
			return
		}
		r.applyReviewResult(ctx, sl, result.review)
	case finalizationMerge, finalizationPR:
		if sl.Status != ledger.SlotMerging {
			r.progress("bead %s: discarded %s result after state changed to %s", sl.PhaseID, result.stage, sl.Status)
			return
		}
		if result.stage == finalizationPR {
			r.applyPRResult(ctx, sl, result.merge, result.err)
		} else {
			r.applyMergeResult(ctx, sl, result.merge, result.err)
		}
	}
}

// resumeFinalization re-enters only the durable completion stage recorded
// before an engine interruption. Review can be safely repeated. A review-clean
// merge is re-enqueued directly; if the prior worker landed it before the
// engine died, the retained candidate branch proves that fact and the poll
// goroutine applies the successful merge outcome without coding redispatch.
func (r *runner) resumeFinalization(ctx context.Context, sl *ledger.Slot) {
	switch sl.FinalizationStage {
	case finalizationMerge:
		if sha, ok := r.alreadyLandedSHA(ctx, sl); ok {
			r.applyMergeResult(ctx, sl, merge.Result{Status: merge.StatusMerged, MergedSHA: sha}, nil)
			return
		}
		r.mergeSlot(ctx, sl)
	case finalizationPR:
		r.openPRSlot(ctx, sl)
	default:
		r.finishCandidate(ctx, sl)
	}
}

func (r *runner) alreadyLandedSHA(ctx context.Context, sl *ledger.Slot) (string, bool) {
	if r.rec == nil || sl == nil || sl.Branch == "" || r.rec.DefaultBranch == "" {
		return "", false
	}
	ancestor, err := execx.Run(ctx, execx.Cmd{
		Dir:  r.rec.Root,
		Name: "git",
		Args: []string{"merge-base", "--is-ancestor", sl.Branch, r.rec.DefaultBranch},
	})
	if err != nil || ancestor.ExitCode != 0 {
		return "", false
	}
	head, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir:  r.rec.Root,
		Name: "git",
		Args: []string{"rev-parse", sl.Branch},
	})
	if err != nil {
		return "", false
	}
	sha := strings.TrimSpace(head.Stdout)
	return sha, sha != ""
}
