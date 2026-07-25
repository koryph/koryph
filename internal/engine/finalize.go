// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/review"
)

const (
	finalizationGate           = "gate"
	finalizationReview         = "review"
	finalizationSecurityReview = "security-review"
	finalizationMerge          = "merge"
	finalizationPR             = "pr"
	finalizationReviewWorkers  = 2
	finalizationInfraAttempts  = 3
)

var finalizationRetryBackoff = 250 * time.Millisecond

// finalizationGeneration is the immutable identity of one candidate
// finalization attempt. A result is applied only while all three fields still
// match the live slot, so an operator override or coding requeue cannot receive
// a stale review/merge result from an older generation.
type finalizationGeneration struct {
	phaseID            string
	stage              string
	attempt            int
	branch             string
	dispatchGeneration string
	candidateSHA       string
	baseSHA            string
	evidenceKey        string
	evidenceDigest     string
	revalidation       int
}

type finalizationJob struct {
	generation  finalizationGeneration
	stage       string
	reviewOpts  review.Opts
	mergeOpts   merge.Opts
	candidateAt time.Time
	queuedAt    time.Time
}

type finalizationResult struct {
	generation finalizationGeneration
	stage      string
	review     review.Verdict
	merge      merge.Result
	err        error
	queueWait  time.Duration
	service    time.Duration
	infraTries int
}

type finalizationQueue struct {
	mu      sync.Mutex
	ready   *sync.Cond
	queue   []finalizationJob
	stopped bool
}

// finalizationLane separates the one expensive gate lane, bounded semantic
// review lanes, and the short landing lane. Workers own no runner or ledger
// pointer: they execute immutable values and return value results. The poll
// goroutine remains the sole owner of durable lifecycle mutations.
type finalizationLane struct {
	ctx    context.Context
	cancel context.CancelFunc

	gates   finalizationQueue
	reviews finalizationQueue
	merges  finalizationQueue

	results chan finalizationResult
	wake    chan struct{}
	wg      sync.WaitGroup

	authorizationMu sync.Mutex
	authorized      map[string]finalizationGeneration

	reviewFn func(context.Context, review.Opts) review.Verdict
	mergeFn  func(context.Context, merge.Opts) (merge.Result, error)
}

func newFinalizationLane(parent context.Context) *finalizationLane {
	ctx, cancel := context.WithCancel(parent)
	l := &finalizationLane{
		ctx:        ctx,
		cancel:     cancel,
		results:    make(chan finalizationResult, 16),
		wake:       make(chan struct{}, 1),
		reviewFn:   review.Review,
		mergeFn:    merge.Merge,
		authorized: make(map[string]finalizationGeneration),
	}
	for _, q := range []*finalizationQueue{&l.gates, &l.reviews, &l.merges} {
		q.ready = sync.NewCond(&q.mu)
	}
	l.startWorkers(&l.gates, 1)
	l.startWorkers(&l.reviews, finalizationReviewWorkers)
	l.startWorkers(&l.merges, 1)
	return l
}

// enqueue appends without waiting for a worker. Each queue is deliberately
// owned under a mutex rather than represented by a bounded channel: a burst of
// completed agents must never block the poll goroutine behind a slow review.
func (l *finalizationLane) enqueue(job finalizationJob) bool {
	if l == nil {
		return false
	}
	q := l.queueFor(job.stage)
	if q == nil {
		return false
	}
	if job.queuedAt.IsZero() {
		job.queuedAt = time.Now()
	}
	if job.candidateAt.IsZero() {
		job.candidateAt = job.queuedAt
	}
	job.generation.stage = job.stage
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopped {
		return false
	}
	l.authorize(job.generation)
	q.queue = append(q.queue, job)
	sort.SliceStable(q.queue, func(i, j int) bool {
		return q.queue[i].candidateAt.Before(q.queue[j].candidateAt)
	})
	q.ready.Signal()
	return true
}

func (l *finalizationLane) authorize(generation finalizationGeneration) {
	l.authorizationMu.Lock()
	defer l.authorizationMu.Unlock()
	if l.authorized == nil {
		l.authorized = make(map[string]finalizationGeneration)
	}
	l.authorized[generation.phaseID] = generation
}

func (l *finalizationLane) invalidate(phaseID string) {
	if l == nil {
		return
	}
	l.authorizationMu.Lock()
	delete(l.authorized, phaseID)
	l.authorizationMu.Unlock()
}

func (l *finalizationLane) queueFor(stage string) *finalizationQueue {
	switch stage {
	case finalizationGate:
		return &l.gates
	case finalizationReview, finalizationSecurityReview:
		return &l.reviews
	case finalizationMerge, finalizationPR:
		return &l.merges
	default:
		return nil
	}
}

func (l *finalizationLane) startWorkers(q *finalizationQueue, count int) {
	for range count {
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.work(q)
		}()
	}
}

func (l *finalizationLane) work(q *finalizationQueue) {
	for {
		q.mu.Lock()
		for len(q.queue) == 0 && !q.stopped {
			q.ready.Wait()
		}
		if q.stopped {
			q.mu.Unlock()
			return
		}
		job := q.queue[0]
		q.queue[0] = finalizationJob{}
		q.queue = q.queue[1:]
		q.mu.Unlock()

		started := time.Now()
		result := l.execute(job)
		result.queueWait = started.Sub(job.queuedAt)
		result.service = time.Since(started)
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
	job.generation.stage = job.stage
	out := finalizationResult{generation: job.generation, stage: job.stage}
	switch job.stage {
	case finalizationReview, finalizationSecurityReview:
		out.review = l.reviewFn(l.ctx, job.reviewOpts)
	case finalizationGate:
		out.merge, out.infraTries, out.err = l.executeMerge(job, false)
	case finalizationMerge, finalizationPR:
		out.merge, out.infraTries, out.err = l.executeMerge(job, true)
	}
	return out
}

func (l *finalizationLane) executeMerge(
	job finalizationJob,
	landing bool,
) (merge.Result, int, error) {
	var result merge.Result
	var err error
	for attempt := 1; attempt <= finalizationInfraAttempts; attempt++ {
		if landing {
			// Landing is the only worker action that mutates the protected
			// default branch. Hold authorization from the exact generation
			// check through one transaction. Release during retry backoff so a
			// newer generation can revoke a partially completed old landing.
			l.authorizationMu.Lock()
			authorized, tracked := l.authorized[job.generation.phaseID]
			if !tracked || authorized != job.generation {
				l.authorizationMu.Unlock()
				return result, attempt - 1, errStaleFinalizationJob
			}
			result, err = l.mergeFn(l.ctx, job.mergeOpts)
			l.authorizationMu.Unlock()
		} else {
			result, err = l.mergeFn(l.ctx, job.mergeOpts)
		}
		if result.ValidationCheckpoint != nil {
			// A successful authoritative gate is a process-local checkpoint.
			// Preserve it across bounded post-gate infrastructure retries so
			// signature/evidence failures cannot rerun the expensive gate.
			job.mergeOpts.ValidationCheckpoint = result.ValidationCheckpoint
		}
		if err == nil || errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) || attempt == finalizationInfraAttempts {
			return result, attempt, err
		}
		delay := finalizationRetryBackoff << (attempt - 1)
		select {
		case <-l.ctx.Done():
			return result, attempt, l.ctx.Err()
		case <-time.After(delay):
		}
	}
	return result, finalizationInfraAttempts, err
}

var errStaleFinalizationJob = errors.New("stale finalization job was not authorized to land")

func (l *finalizationLane) close() {
	if l == nil {
		return
	}
	l.cancel()
	for _, q := range []*finalizationQueue{&l.gates, &l.reviews, &l.merges} {
		q.mu.Lock()
		q.stopped = true
		q.queue = nil
		if q.ready != nil {
			q.ready.Broadcast()
		}
		q.mu.Unlock()
	}
	l.wg.Wait()
}

func generationFor(sl *ledger.Slot) finalizationGeneration {
	return finalizationGeneration{
		phaseID: sl.PhaseID, attempt: sl.Attempts, branch: sl.Branch,
		dispatchGeneration: sl.DispatchGeneration,
		revalidation:       sl.Retry.MergeRevalidations,
	}
}

func generationForStage(sl *ledger.Slot, stage string) finalizationGeneration {
	generation := generationFor(sl)
	generation.stage = stage
	return generation
}

func generationForEvidence(
	sl *ledger.Slot,
	evidence *merge.GateEvidence,
	stage string,
) finalizationGeneration {
	generation := generationForStage(sl, stage)
	if evidence != nil {
		generation.candidateSHA = evidence.CandidateSHA
		generation.baseSHA = evidence.BaseSHA
		generation.evidenceKey = gateEvidenceKey(evidence)
		generation.evidenceDigest = strings.TrimSpace(sl.GateEvidenceDigest)
	}
	return generation
}

func (r *runner) submitFinalization(ctx context.Context, job finalizationJob) {
	now := time.Now()
	job.queuedAt = now
	if sl := r.run.Slots[job.generation.phaseID]; sl != nil {
		if candidateAt, err := time.Parse(time.RFC3339Nano, sl.FinalizationQueuedAt); err == nil {
			job.candidateAt = candidateAt
		} else if candidateAt, err := time.Parse(time.RFC3339, sl.FinalizationQueuedAt); err == nil {
			job.candidateAt = candidateAt
		}
	}
	if job.candidateAt.IsZero() {
		job.candidateAt = now
	}
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
	job.generation.stage = job.stage
	l.authorize(job.generation)
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
	if result.queueWait > 0 || result.service > 0 {
		logFinalizationStage(result.generation.phaseID, result.stage, result.queueWait, result.service)
	}
	sl := r.run.Slots[result.generation.phaseID]
	if sl == nil ||
		result.generation.stage != result.stage ||
		sl.Attempts != result.generation.attempt ||
		sl.Branch != result.generation.branch ||
		sl.DispatchGeneration != result.generation.dispatchGeneration ||
		sl.Retry.MergeRevalidations != result.generation.revalidation ||
		sl.FinalizationStage != result.stage {
		r.progress("bead %s: discarded stale %s result for attempt %d branch %s",
			result.generation.phaseID, result.stage, result.generation.attempt, result.generation.branch)
		return
	}
	if result.stage != finalizationGate &&
		(result.generation.candidateSHA == "" ||
			result.generation.baseSHA == "" ||
			result.generation.evidenceKey == "" ||
			result.generation.evidenceDigest == "") {
		r.progress("bead %s: discarded %s result with incomplete finalization generation",
			result.generation.phaseID, result.stage)
		return
	}
	if result.generation.candidateSHA != "" &&
		!r.finalizationEvidenceCurrent(ctx, sl, result.generation) {
		r.progress("bead %s: discarded stale %s result after candidate/gate evidence changed",
			result.generation.phaseID, result.stage)
		return
	}

	switch result.stage {
	case finalizationGate:
		if sl.Status != ledger.SlotMerging {
			r.progress("bead %s: discarded gate result after state changed to %s", sl.PhaseID, sl.Status)
			return
		}
		r.applyGateResult(ctx, sl, result.merge, result.err)
	case finalizationReview:
		if sl.Status != ledger.SlotReview {
			r.progress("bead %s: discarded review result after state changed to %s", sl.PhaseID, sl.Status)
			return
		}
		r.applyReviewResult(ctx, sl, result.review, false)
	case finalizationSecurityReview:
		if sl.Status != ledger.SlotReview {
			r.progress("bead %s: discarded security review result after state changed to %s", sl.PhaseID, sl.Status)
			return
		}
		r.applyReviewResult(ctx, sl, result.review, true)
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
	case finalizationGate:
		if _, ok := r.resumeGateEvidence(ctx, sl); ok {
			if r.opts.Review {
				r.startReview(ctx, sl)
			} else {
				policy := r.mergePolicy(ctx, sl.EpicID)
				if r.opts.Direct {
					policy = project.PolicyAuto
				}
				r.finishAfterReview(ctx, sl, policy)
			}
			return
		}
		r.validateSlot(ctx, sl)
	case finalizationMerge:
		if _, ok := r.resumeGateEvidence(ctx, sl); !ok {
			r.validateSlot(ctx, sl)
			return
		}
		r.mergeSlot(ctx, sl)
	case finalizationPR:
		if _, ok := r.resumeGateEvidence(ctx, sl); !ok {
			r.validateSlot(ctx, sl)
			return
		}
		r.openPRSlot(ctx, sl)
	case finalizationSecurityReview:
		if _, ok := r.resumeGateEvidence(ctx, sl); ok {
			r.startSecurityReview(ctx, sl, "resuming risk-triggered security review")
		} else {
			r.validateSlot(ctx, sl)
		}
	case finalizationReview:
		if _, ok := r.resumeGateEvidence(ctx, sl); ok {
			r.startReview(ctx, sl)
		} else {
			r.validateSlot(ctx, sl)
		}
	default:
		// Legacy review/merge states predate gate evidence. Re-enter validation
		// rather than trusting or repeating their old semantic stage.
		r.validateSlot(ctx, sl)
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
