// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/account"
	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/merge"
	"github.com/koryph/koryph/internal/modelroute"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
	"github.com/koryph/koryph/internal/registry"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/review"
	"github.com/koryph/koryph/internal/runtime"
	"github.com/koryph/koryph/internal/timeoutcfg"
	"github.com/koryph/koryph/internal/worktree"
)

// gateRequeueNote and mergeErrorRequeueNote are passed to requeueSlot purely
// for progress/observability (the slot's Note field, surfaced in status/
// roster output) — they are NOT the dedup mechanism. That job now belongs to
// the ledger.Slot.GateRequeues / MergeRequeues counters below (koryph-2im.6):
// a single-shot Note marker could not tell "already requeued once" from
// "requeued twice," so the budget was stuck at 1. Counters let the budget be
// raised (2 each) without losing the ability to cap it.
const gateRequeueNote = "gate-failed requeue"
const mergeErrorRequeueNote = "merge-error requeue"

// conflictRequeueNote marks a slot bounced back to its agent to resolve a
// rebase conflict in-worktree (CONFLICT.md carries the details; koryph-3as).
const conflictRequeueNote = "rebase-conflict requeue"

// commitStyleRequeueNote marks a slot bounced once for non-conventional commit
// subjects; a second commit-style failure blocks instead of looping. Unlike
// gate/merge above, commit-style stays a single-shot Note-marker dedup — its
// budget is unchanged at 1 (koryph-2im.6): a reword bounce either fixes the
// subject or it won't, so a second bounce buys nothing.
const commitStyleRequeueNote = "commit-style requeue"

// resumeRequeueNote marks a slot re-dispatched by the resume backlog after an
// engine restart or a width-gated deferral. This is NOT a bead fault — the
// agent did not fail, the engine was interrupted — so requeueSlot must not
// consume an attempt or drive the final-attempt escalation for it. Counting
// resume re-dispatches as faults let a mid-run restart push otherwise-healthy
// beads to "final attempt, escalated to the recovery tier" having genuinely
// failed zero or one time, spending recovery-tier money on non-faults (D4:
// faults ≠ dispatches). See requeueSlot's `fault` gate and drainResumeBacklog.
const resumeRequeueNote = "resume: width-gated re-dispatch"

// cleanNoCommitExitNote marks a slot whose agent exited cleanly (exit 0) but
// produced no commits and no SUMMARY.md — the agent deliberately concluded it
// could not or need not act (an environment-gated no-op: a resource it needs is
// absent, the host is too contended, work is already done). Re-dispatching a
// higher tier just reproduces the identical no-op, so this requeue is excluded
// from the final-attempt model escalation: it is an environmental signal, not a
// capability fault (D14). It still counts an attempt, so the bead parks with an
// environment reason once the attempt budget is spent rather than looping.
const cleanNoCommitExitNote = "agent exited cleanly with no new commits"

// genericCompletionBlockRequeueNote marks a clean, committed worker that
// self-blocked without the structured host-capability fields. The worker
// contract instructs the next attempt to report any sandbox/host denial with
// `phase block`; therefore this is a deterministic classification-correction
// retry, not evidence that a stronger coding tier is needed. It still consumes
// the normal attempt, but the final correction attempt stays on the frozen
// tier so a repeated generic host block cannot spend the frontier escalation.
const genericCompletionBlockRequeueNote = "agent reported unstructured completion block"

// gateRequeueBudget and mergeRequeueBudget are the per-slot requeue budgets
// for a post-rebase gate failure and a merge error, respectively — each
// raised from a single-shot Note-marker dedup to 2 (koryph-2im.6). A rare
// real race (the base moved twice) now self-heals instead of stranding the
// bead after just one retry. Both remain bounded by ledger.MaxAttempts.
const (
	gateRequeueBudget     = 2
	mergeRequeueBudget    = 2
	conflictRequeueBudget = 2
)

// rateLimitedRequeueBudget bounds how many times a slot may requeue on a
// classified rate-limit/overload death (koryph-2im.4) WITHOUT burning a normal
// attempt — the failure is environmental (the account got throttled), not a
// fault of the bead's work, so it must not count toward ledger.MaxAttempts.
// It is still budgeted independently so a persistently rate-limited account
// cannot loop a slot forever; exhausting it blocks with a clear note.
const rateLimitedRequeueBudget = 5

// mergeErrorRetryable reports whether a slot whose merge just errored should be
// requeued for another attempt rather than blocked. A merge error is usually
// transient — the base moved, a push raced — and requeueSlot Force-rebases the
// landed branch onto current main before resuming, so the retry re-attempts the
// merge from a correct base. It is retried at most mergeRequeueBudget times
// (koryph-3fs, budget raised by koryph-2im.6).
func mergeErrorRetryable(sl *ledger.Slot) bool {
	return sl.MergeRequeues < mergeRequeueBudget && sl.Attempts < ledger.MaxAttempts
}

// gateRequeueRetryable reports whether a slot whose merge gate just failed
// (after a rebase) should be requeued for another attempt rather than
// blocked. Mirrors mergeErrorRetryable (koryph-2im.6).
func gateRequeueRetryable(sl *ledger.Slot) bool {
	return sl.GateRequeues < gateRequeueBudget && sl.Attempts < ledger.MaxAttempts
}

// commitStyleRetryable reports whether a slot bounced for a non-conventional
// commit subject should be requeued for a reword rather than blocked. Budget
// stays 1 (unchanged by koryph-2im.6): a reword either fixes the subject or
// it won't, so this deliberately keeps the single-shot Note-marker dedup
// rather than a counter — Note still doubles as the requeue reason surfaced
// in status/roster output either way.
func commitStyleRetryable(sl *ledger.Slot) bool {
	return sl.Note != commitStyleRequeueNote && sl.Attempts < ledger.MaxAttempts
}

// pollUntilIdle ticks every pollInterval until every slot in the run is
// terminal, waking early on SIGCHLD (koryph-2im.2). A ctx cancellation
// propagates so the caller can checkpoint.
//
// Dispatched agents are direct children of this process (that is why
// slotAlive's Wait4(WNOHANG) below works), so a child's exit raises SIGCHLD
// here immediately — completion latency drops from up-to-pollInterval to
// near-instant. It is a wake HINT only: short-lived git/bd children raise the
// same signal, so a wake does not mean "a slot finished," it means "go find
// out" — the poll pass below makes that call, same as a timer tick would.
// The channel is 1-buffered and non-blocking to send on, so a burst of
// exits (fanned-out git subprocesses, several agents landing at once)
// coalesces into one extra pass rather than one per signal. The timer
// remains the backstop: a missed or coalesced signal just falls back to the
// next tick, never to incorrectness.
func (r *runner) pollUntilIdle(ctx context.Context) error {
	interval := r.pollInterval()

	wake := r.wakeCh
	if wake == nil {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGCHLD)
		defer signal.Stop(ch)
		wake = ch
	}

	tick := 0
	for {
		if r.capabilityBlocked {
			return nil
		}
		// A wave can sit inside this loop for many minutes while its slots run —
		// waveLoop's own syncObsConfig() call only fires once, BEFORE this loop is
		// entered, so without a call here a mid-wave `koryph obs level` change
		// would silently wait out the whole wave instead of taking effect on the
		// next poll tick as the "no restart needed" contract promises (design
		// docs/designs/2026-07-observability.md §4). rollingLoop does not need
		// this: its own for-loop IS the tick loop.
		syncObsConfig()
		// Refresh the heartbeat's active/ready/wave snapshot every tick (koryph-
		// lwnq) — cheap in-memory reads, same goroutine that owns r.run, so this
		// is always safe even though the heartbeat ticker reads it concurrently.
		r.hb.setCounts(r.activeCount(), r.lastReadyCount, r.run.Wave)

		// liveActiveCount, not activeCount: a resume backlog (SlotQueued) reserves
		// width but has no agent to poll, so waiting on it here would deadlock —
		// only the wave-loop boundary's drainResumeBacklog can promote it. When
		// every LIVE slot has settled, return so that boundary regains control and
		// drains the next backlog beads (koryph-bzf). Identical to activeCount when
		// no backlog is present.
		if r.liveActiveCount() == 0 {
			return nil
		}
		timerFired, err := r.waitTick(ctx, wake, interval)
		if err != nil {
			return err
		}
		probeProgress := false
		if timerFired {
			tick++
			// Split probe cost (L3): the git rev-list progress probe is the
			// pricier subprocess, so it runs on the first timer tick and every
			// 3rd one thereafter — same freshness as a 30s poll, a fraction of
			// the churn at a 10s tick. Liveness/stuck detection below is
			// unaffected; it runs on every pass regardless.
			probeProgress = progressProbeDue(tick)
		}
		r.pollPass(ctx, probeProgress)
	}
}

// waitTick blocks until the poll timer fires, a wake hint arrives on wake, or
// ctx is cancelled — the single wait primitive shared by pollUntilIdle (wave
// mode's drain-to-idle poll) and rollingLoop's one-tick-per-iteration refill
// wait (koryph-2im.3, extracted from the pre-koryph-2im.3 pollUntilIdle body
// so the two loops cannot drift on wake semantics). timerFired reports which
// branch fired: true for the timer (progress-probe cadence applies), false
// for a signal wake (liveness only — see pollUntilIdle's probeProgress
// comment) or for a coalesced wake the caller need not distinguish further.
// A ctx cancellation returns ctx.Err() and the caller propagates it so
// interrupted() can checkpoint.
func (r *runner) waitTick(ctx context.Context, wake <-chan os.Signal, interval time.Duration) (timerFired bool, err error) {
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case <-time.After(interval):
		return true, nil
	case <-wake:
		return false, nil
	}
}

// pollPass is one sweep over the run's active slots (koryph-2im.3, extracted
// from pollUntilIdle's per-tick body): refresh liveness/progress for each
// non-terminal slot, then flush this pass's batched progress in one write.
// Terminal transitions (completeSlot) already persisted immediately; this
// only commits the cheap commit-count/heartbeat refresh, which resume
// recomputes from git anyway — so batching it costs no crash safety.
func (r *runner) pollPass(ctx context.Context, probeProgress bool) {
	// Refresh the demand heartbeat on every poll tick so the engine's presence
	// is visible to `koryph doctor` even under slot saturation — when every
	// global slot is occupied no new admissions happen, so the admission-time
	// refresh in wave/rolling loops never fires, and the 10-minute TTL would
	// falsely expire on a healthy, fully-loaded pipeline (koryph-p42).
	r.refreshDemand()
	// One resource-table snapshot per pass, aggregated per slot below (resmon's
	// "one snapshot, many trees" — koryph process-metrics). nil on an
	// unsupported platform or probe failure, in which case sampling is skipped.
	procs := r.sampleProcTable(ctx)
	for _, id := range r.activePhaseIDs() {
		sl := r.run.Slots[id]
		if sl == nil || ledger.Terminal(sl.Status) {
			continue
		}
		// A resume-backlog slot (SlotQueued) has no live agent — its last agent
		// is dead and drainResumeBacklog re-dispatches it at a scheduling
		// boundary, not here. Polling it would drive completeSlot, which would
		// immediately re-dispatch it uncapped: exactly the width bypass koryph-bzf
		// removes. Skip it until the boundary drain promotes it to a live slot.
		if sl.Status == ledger.SlotQueued {
			continue
		}
		if procs != nil {
			r.sampleSlotResources(sl, procs)
		}
		r.pollSlot(ctx, sl, probeProgress)
	}
	_ = r.store.SaveRun(r.run)
}

// resSampleTimeout bounds one process-table probe so a hung `ps` (darwin) or a
// pathologically slow `/proc` scan (linux) can never stall the poll loop's
// liveness, completeSlot, and merge work — the probe is on the critical path.
const resSampleTimeout = 5 * time.Second

// resMinSampleInterval floors the resource-sampling cadence independently of the
// poll interval. Metrics only need coarse resolution, so even when PollSec is
// configured very low (or a burst of SIGCHLD wakes drives pollPass rapidly) the
// engine never forks `ps` / scans /proc more often than this — bounding the
// sampler's subprocess and syscall overhead.
const resMinSampleInterval = 5 * time.Second

// sampleProcTable takes one process-table snapshot for this poll pass, or nil
// (sampling skipped this pass) when disabled, throttled, unsupported, timed out,
// or failed — so resource sampling can never break or stall the poll loop.
//
// Disable: envResmon="off" turns sampling off entirely (tests set it so the
// timing-sensitive wave/pacing loops don't fork `ps`). Throttle: pollPass also
// runs on every SIGCHLD wake, so sampling is limited to at most once per
// max(pollInterval, resMinSampleInterval). Timeout: the probe is bounded by
// resSampleTimeout regardless of the long-lived run ctx.
func (r *runner) sampleProcTable(ctx context.Context) *resmon.ProcTable {
	if os.Getenv(envResmon) == "off" {
		return nil
	}
	interval := max(r.pollInterval(), resMinSampleInterval)
	now := time.Now()
	if !r.lastResSampleAt.IsZero() && now.Sub(r.lastResSampleAt) < interval {
		return nil
	}
	probe := r.resProbe
	if probe == nil {
		probe = resmon.Snapshot
	}
	pctx, cancel := context.WithTimeout(ctx, resSampleTimeout)
	defer cancel()
	tbl, err := probe(pctx)
	if err != nil {
		return nil
	}
	r.lastResSampleAt = now
	return tbl
}

// sampleSlotResources folds this pass's resource reading for sl's agent process
// cohort into the slot's running Usage and mirrors the derived aggregates onto
// the ledger slot. A slot whose process has already exited (Aggregate not
// found) contributes no sample. All units are converted to the ledger's MB /
// seconds. Best-effort: never returns an error and never blocks completion.
func (r *runner) sampleSlotResources(sl *ledger.Slot, procs *resmon.ProcTable) {
	if sl.PID <= 0 {
		return
	}
	sample, ok := procs.Aggregate(sl.PID)
	if !ok {
		return
	}
	if r.resUsage == nil {
		r.resUsage = make(map[string]*slotResUsage)
	}
	u := r.resUsage[sl.PhaseID]
	if u == nil || u.pid != sl.PID {
		// First sample for this slot, or a requeue installed a new PID: start a
		// fresh per-attempt accumulation aligned with the new DispatchedAt.
		u = &slotResUsage{pid: sl.PID}
		r.resUsage[sl.PhaseID] = u
	}
	u.usage.Add(sample)
	// Implicit CPU heartbeat (koryph-2rf): cumulative CPU advancing since the
	// last pass means the cohort is computing, whatever the status file says.
	if u.usage.CPUSeconds > u.lastCPU {
		u.lastCPU = u.usage.CPUSeconds
		u.lastActiveAt = time.Now()
	}

	peakMB := int(u.usage.PeakRSSKB / 1024)
	avgMB := int(u.usage.AvgRSSKB() / 1024)
	cpuSec := u.usage.CPUSeconds
	ioReadMB := float64(u.usage.IOReadKB) / 1024
	ioWriteMB := float64(u.usage.IOWriteKB) / 1024
	samples := u.usage.Samples
	r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.PeakRSSMB = peakMB
		s.AvgRSSMB = avgMB
		s.CPUSeconds = cpuSec
		s.IOReadMB = ioReadMB
		s.IOWriteMB = ioWriteMB
		s.ResourceSamples = samples
	})
}

// stampFinishedAt records the wall-clock stop time on a slot the moment it
// becomes terminal, if not already stamped. Called after completeSlot so it
// covers every terminal path (merged/blocked/conflict/failed/…) from one place
// rather than threading the stamp through each. A requeued (still non-terminal)
// slot is left untouched. Also drops the slot's in-memory Usage so a later
// same-phase requeue starts a fresh accumulation.
func (r *runner) stampFinishedAt(phaseID string) {
	s := r.run.Slots[phaseID]
	if s == nil || !ledger.Terminal(s.Status) || s.FinishedAt != "" {
		return
	}
	_ = r.store.UpdateSlot(r.run, phaseID, func(sl *ledger.Slot) {
		sl.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	})
	delete(r.resUsage, phaseID)
}

// progressProbeDue reports whether a timer-driven poll pass should run the
// git progress probe: tick 1, and every 3rd tick thereafter (1, 4, 7, ...).
// tick counts timer ticks only — signal-triggered passes never advance it and
// never call this (koryph-2im.2).
func progressProbeDue(tick int) bool {
	return tick%3 == 1
}

// pollSlot refreshes one slot: liveness (always), commit progress (only when
// probeProgress — see progressProbeDue), stuck detection, and — on death —
// completion handling.
func (r *runner) pollSlot(ctx context.Context, sl *ledger.Slot, probeProgress bool) {
	// A worker may be blocked inside `koryph phase request` waiting for an
	// orchestrator-owned action. Service its typed request before checking
	// liveness so the command can return while the agent is still running.
	phaseRequestPending := r.processPhaseRequests(ctx, sl)

	alive := slotAlive(sl.PID)

	// Batch per-tick progress in memory; pollUntilIdle flushes once per tick.
	if probeProgress {
		if commits, head, err := r.branchProgress(ctx, sl.Worktree); err == nil {
			r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Commits = commits
				s.LastCommit = head
			})
		}
	}

	if alive {
		// Turn-ceiling enforcement (koryph-840): a runaway bead that stays under
		// the per-turn cost cap can still accrete 90M+ cache-read tokens by
		// re-reading its whole tool-result history every turn for hundreds of
		// turns. The claude CLI has no --max-turns flag and its --max-budget-usd
		// cap only fires once the whole budget is spent, so the engine counts
		// completed turns from the live stream and, past the ceiling, gracefully
		// interrupts the agent (SIGTERM — it checkpoints, worktree preserved) so
		// completeSlot can requeue it with a FRESH session. Gated on
		// probeProgress so the stream is scanned at most once per timer tick, not
		// on every SIGCHLD wake. This is a deliberate, bounded runaway defense —
		// distinct from the stuck path just below, which never interrupts a
		// quietly-working agent.
		if probeProgress && r.enforceTurnCeiling(sl) {
			return
		}
		// Derive the slot's most-recent activity ONCE and use it for both the
		// status decision and the last_activity_at stamp (distinct from
		// updated_at, which only means "we polled"). Re-derived every tick from
		// ground truth so status is never latched.
		last := r.slotActivityAt(ctx, sl)
		if !last.IsZero() {
			stamp := last.UTC().Format(time.RFC3339)
			r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.LastActivityAt = stamp })
		}
		status := ledger.SlotRunning
		if r.stuckFrom(sl, last) {
			status = ledger.SlotStuck
		}
		if sl.Status != status {
			prev := sl.Status
			r.store.MutateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = status })
			switch {
			case status == ledger.SlotStuck:
				r.progress("bead %s: silent — no heartbeat, commit, stream, or CPU activity for >%ds (process alive; still polling — koryph never interrupts a running agent)",
					sl.PhaseID, int(r.stuckThreshold(sl).Seconds()))
			case prev == ledger.SlotStuck:
				// Re-derived from ground truth, not latched: the agent resumed
				// producing work, so the transient stuck flag clears instead of
				// staying on the board after recovery.
				r.progress("bead %s: activity resumed — clearing stuck", sl.PhaseID)
			}
		}
		r.checkpointSlot(sl, "running")
		return
	}

	if phaseRequestPending {
		const note = "agent exited while an orchestrator-owned capability request is still running"
		if sl.Note != note {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Note = note })
			r.progress("bead %s: waiting for pending capability request before completion classification", sl.PhaseID)
		}
		r.checkpointSlot(sl, "capability-request-pending")
		return
	}

	r.completeSlot(ctx, sl)
	// Stamp the wall-clock stop time if completeSlot drove the slot terminal
	// (as opposed to requeuing it) — one place covering every terminal path.
	r.stampFinishedAt(sl.PhaseID)
}

// slotAlive reports whether the agent process is still running. The engine is
// usually the direct parent of a dispatched (detached, released) agent, so a
// plain kill(pid,0) probe would report a zombie as alive forever — nobody
// waits on it. A non-blocking Wait4 reaps our own dead children first; for
// processes we did not parent (resumed runs) it fails with ECHILD and we fall
// back to the signal probe.
func slotAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	var ws syscall.WaitStatus
	wpid, err := syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
	if err == nil {
		if wpid == pid {
			return false // reaped: definitively dead
		}
		if wpid == 0 {
			return true // our child, still running
		}
	}
	return dispatch.Alive(pid)
}

// streamSignals is the runtime-neutral completion data recovered from an
// agent transcript. Claude keeps its established specialised readers; every
// other adapter supplies the same facts through runtime.ParseEvents. This
// prevents a new runtime from silently bypassing rate-limit recovery or token
// accounting just because its JSONL differs from Claude's.
type streamSignals struct {
	cost        float64
	hasCost     bool
	tokens      dispatch.TokenUsage
	hasTokens   bool
	rateLimited bool
}

func (s streamSignals) costUSD() (float64, bool) { return s.cost, s.hasCost }
func (s streamSignals) usage() (dispatch.TokenUsage, bool) {
	return s.tokens, s.hasTokens
}

func parseRuntimeSignals(rt runtime.Runtime, path string) streamSignals {
	if rt.Name() == "claude" {
		cost, hasCost := dispatch.ParseResultCost(path)
		usage, hasUsage := dispatch.ParseResultUsage(path)
		return streamSignals{cost: cost, hasCost: hasCost, tokens: usage, hasTokens: hasUsage, rateLimited: dispatch.ParseRateLimited(path)}
	}
	f, err := os.Open(path)
	if err != nil {
		return streamSignals{}
	}
	defer f.Close()
	es, err := rt.ParseEvents(f)
	if err != nil {
		return streamSignals{}
	}
	defer es.Close()
	var out streamSignals
	for {
		ev, ok, err := es.Next()
		if err != nil || !ok {
			return out
		}
		if ev.Kind == runtime.EventResult {
			if ev.HasCost {
				out.cost, out.hasCost = ev.CostUSD, true
			}
			if ev.HasUsage {
				out.tokens = dispatch.TokenUsage{
					InputTokens: ev.InputTokens, OutputTokens: ev.OutputTokens,
					CacheReadTokens: ev.CacheReadTokens, CacheCreationTokens: ev.CacheCreationTokens,
				}
				out.hasTokens = true
			}
		}
		if ev.Kind == runtime.EventError && ev.RateLimited {
			out.rateLimited = true
		}
	}
}

// completeSlot handles a dead agent: record cost, then either finish the
// candidate (review + merge policy), block, or requeue.
func (r *runner) completeSlot(ctx context.Context, sl *ledger.Slot) {
	runtimeName := sl.Runtime
	if runtimeName == "" {
		if r.rt != nil {
			runtimeName = r.rt.Name()
		} else {
			projectDefault := ""
			if r.cfg != nil {
				projectDefault = r.cfg.DefaultRuntime
			}
			runtimeName, _ = modelroute.ResolveRuntimeName(nil, projectDefault)
		}
	}
	streamRT, registered := runtimeForName(runtimeName)
	if !registered && r.rt != nil {
		streamRT = r.rt // dispatch already succeeded; preserve legacy recovery behaviour.
	}
	if streamRT == nil {
		r.blockSlot(sl.PhaseID, dispatchReq{issue: r.issueFor(ctx, sl), attempt: sl.Attempts},
			fmt.Sprintf("runtime %q is unavailable while parsing completion signals", runtimeName))
		return
	}
	signals := parseRuntimeSignals(streamRT, sl.Stream)
	// Actual-model ground truth (koryph-qf6.2): every dispatch runs with a
	// hardcoded --fallback-model, so the tier dispatch requested can silently
	// degrade mid-session. The result line's modelUsage keys record what
	// actually ran; stamp the dominant model (normalized to a tier when the
	// id names one) so outcome data never attributes a downgraded session to
	// the requested tier. Snapshotted per attempt, exactly like DeathReason.
	if streamRT.Name() == "claude" {
		if id, ok := dispatch.ParseActualModel(sl.Stream); ok && id != "" {
			actual := id
			if tier := modelroute.TierForModelID(id); tier != "" {
				actual = tier
			}
			sl.ModelActual = actual
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.ModelActual = actual })
			if actual != sl.Model {
				r.progress("bead %s: model fallback — requested %s, actually ran %s (%s)",
					sl.PhaseID, sl.Model, actual, id)
				logModelFallback(sl.PhaseID, sl.Model, actual, id)
			}
		}
	}

	if cost, ok := signals.costUSD(); ok {
		// ADD the new attempt's cost to whatever was accumulated from prior
		// attempts (koryph-6bl: CostUSD accumulates across requeues so total
		// spend per bead is never lost when a slot is replaced on requeue).
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.CostUSD += cost })
		model, size := sl.Model, r.sizeClass(sl.PhaseID)
		// Lock-guarded read-modify-write so concurrent runs on the same account
		// don't clobber each other's EWMA calibration (koryph-8iu.1).
		// Pass sl.EstimateUSD so Record can also update error stats (koryph-6bl);
		// 0 on an old-format slot is treated as "unknown" and skips error stats.
		// sl.ProxyID segments the calibration key by the arm this slot's
		// dispatch was assigned to (koryph-3l1.3, calibKey's proxyID
		// segmentation): the holdout arm and "no proxy configured" share the
		// same "" key by design (see ledger.Slot.ProxyID's doc), so a standing
		// canary's direct-arm observations fold into the SAME baseline
		// population Record always fed, while the proxied arm accumulates
		// separately under "tier:size@proxyID" and can never pollute it.
		if cfg, err := quota.UpdateConfig(r.quotaName(), func(c *quota.Config) error {
			quota.RecordForProxy(c, model, size, sl.ProxyID, cost, sl.EstimateUSD)
			return nil
		}); err == nil {
			r.quotaCfg = cfg
		}
		logBeadCost(sl.PhaseID, model, cost, sl.EstimateUSD)
	}

	// Per-attempt token composition (koryph-77r.1, design
	// docs/designs/2026-07-token-economy.md §3 L1): prefer the stream-json
	// result line's own usage block; when absent, fall back to summing this
	// attempt's session transcript (cheap — SessionTokens globs by the
	// already-unique session id, no new full-tree scan). Leave the slot's
	// token fields untouched (zeros on a fresh slot) when neither source
	// yields a reading, rather than building heavier scan machinery.
	//
	// attemptUsage captures THIS attempt's own composition (not the slot's
	// accumulated total) for the koryph-77r.10 thrash guard below — see
	// totalAttemptTokens' doc for why the distinction matters.
	var attemptUsage dispatch.TokenUsage
	if usage, ok := signals.usage(); ok {
		attemptUsage = usage
		r.applyTokenUsage(sl.PhaseID, usage)
	} else if tc, ok := quota.SessionTokens(r.profile.ConfigDir, sl.SessionID); ok {
		attemptUsage = dispatch.TokenUsage{
			InputTokens:         tc.InputTokens,
			OutputTokens:        tc.OutputTokens,
			CacheReadTokens:     tc.CacheReadTokens,
			CacheCreationTokens: tc.CacheCreationTokens,
		}
		r.applyTokenUsage(sl.PhaseID, attemptUsage)
	} else {
		logBeadTokensUnavailable(sl.PhaseID)
	}

	// koryph-a1x (F1a): an operator SIGTERM via `koryph stop` is a terminal
	// intent, not a death to classify. Park the phase — no retry, and no
	// auto-merge of partial commits (which could open a merge racing the
	// operator's own hand-fix) — before any classification below. This attempt's
	// cost and tokens above are still recorded.
	if r.parkForOperatorStop(ctx, sl) {
		return
	}

	// Turn-ceiling classification (koryph-840) runs upstream of the rate-limit,
	// budget-kill, and finishCandidate checks below. When enforceTurnCeiling
	// interrupted this attempt for running past the turn ceiling, that stamp is
	// the authoritative death reason — it must win even if the SIGTERM'd
	// agent's final API call happened to 429 (rate-limit) or the cost happened
	// to cross the budget cap on the same turn, because only the turn-exhausted
	// path requeues with a FRESH session (the others warm-resume and would
	// re-accrete the very context this bead exists to shed). A self-finished
	// agent is never stamped here regardless of turn count, so an honest
	// long-running bead still flows to finishCandidate below.
	if sl.DeathReason == deathReasonTurnExhausted {
		r.requeueTurnExhausted(ctx, sl)
		return
	}

	// Rate-limit classification runs upstream of the commits/finishCandidate
	// check (koryph-2im.4): a death caused by the API throttling us is not a
	// completed candidate even if some work landed before the 429/overload hit,
	// so it must not fall through to review/merge. Checked before the
	// MaxAttempts gate too — the requeue budget here is RateLimitRequeues, not
	// Attempts (see requeueRateLimited).
	if signals.rateLimited {
		r.requeueRateLimited(ctx, sl)
		return
	}

	// Probe the live branch commit count as a fallback when the progress probe
	// has not yet updated sl.Commits (koryph-ek2): the per-tick progress probe
	// runs only on timer ticks — NOT on every SIGCHLD wake — so a fast
	// commit-then-die leaves sl.Commits == 0 even though the branch has work.
	// Without this fallback the engine classified such deaths as "no commits"
	// and requeued, burning the attempt budget on already-complete branches.
	// The same gap bit resumed agents that exited cleanly after concluding the
	// work was done (their new slot starts at Commits=0 too).
	commits := sl.Commits
	if commits == 0 && sl.Branch != "" {
		if n, err := r.commitCount(ctx, sl.Branch); err == nil && n > 0 {
			commits = n
			// Update the persisted slot so ledger / status output reflects the
			// actual commit count that triggered finishCandidate.
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Commits = n })
		}
	}

	// Budget-kill classification runs upstream of finishCandidate too
	// (koryph-77r.10), mirroring the rate-limit precedent immediately above:
	// the agent was terminated by --max-budget-usd, not by its own choice, so
	// even committed work should not short-circuit straight to review/merge —
	// it gets the same warm-resume-then-park treatment via requeueBudgetKilled
	// (which itself is Attempts-counted, unlike the environmental rate-limit
	// path).
	if dispatch.ParseBudgetKilled(sl.Stream) {
		r.requeueBudgetKilled(ctx, sl, commits, attemptUsage)
		return
	}

	summary := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "SUMMARY.md")
	if commits > 0 || fsx.Exists(summary) {
		r.finishCandidate(ctx, sl)
		return
	}

	// No commits on the branch and no SUMMARY.md: distinguish a clean exit
	// (agent concluded work was done, or finished with an empty result) from an
	// unclean death (crashed / killed before producing a result line).
	deathDesc := "agent died with no commits"
	if sl.Stream != "" && dispatch.ParseCleanExit(sl.Stream) {
		deathDesc = cleanNoCommitExitNote
	}

	if sl.Attempts >= ledger.MaxAttempts {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = fmt.Sprintf("%s; %d attempts exhausted", deathDesc, sl.Attempts)
		})
		r.checkpointSlot(sl, "blocked")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (%s, %d attempts)", sl.PhaseID, deathDesc, sl.Attempts)
		logSlotBlocked(sl.PhaseID, fmt.Sprintf("%s; %d attempts exhausted", deathDesc, sl.Attempts),
			sl.Model, sl.ModelActual, sl.Attempts)
		// Failure write-back (koryph-qf6.5, koryph-84yu): reconcile the bd claim
		// to blocked-with-reason. Without this the attempt count, model, and
		// death summary are stranded in this machine's gitignored ledger AND the
		// bead stays in_progress with no live agent — invisible to every future
		// `bd ready` frontier until an operator resets it by hand. Best-effort; a
		// bd failure never blocks the loop.
		r.reconcileBlockedBead(ctx, sl, fmt.Sprintf("%s; %d attempts exhausted", deathDesc, sl.Attempts))
		return
	}
	r.requeueSlot(ctx, sl, "", deathDesc)
}

// cacheRatioFloor is the cache_read-share floor below which
// checkCacheRatioTripwire WARNs (koryph-77r.1, design
// docs/designs/2026-07-token-economy.md §2 I7, §3 L1): Claude Code resends
// full conversation history every turn, so a healthy multi-turn session's
// cache_read share should dominate input+cache_creation once the prefix is
// established. A collapse below this floor on a material-token-volume
// attempt is I7's named failure signature — a nondeterministic transform
// busting the cached prefix and converting 90%-discounted cache reads into
// 1.25x-cost cache writes, i.e. the quota multiplier this tripwire exists to
// catch at runtime.
const cacheRatioFloor = 0.5

// cacheRatioMinTokens is the minimum attempt token volume (input+cache_read+
// cache_creation) before the cache-ratio tripwire evaluates at all: a
// session's first turn has no cache to read yet by construction, so a small
// early attempt would false-positive on every single dispatch.
const cacheRatioMinTokens = 20_000

// applyTokenUsage accumulates one attempt's token composition onto the
// slot's persisted totals — exactly like CostUSD accumulates across requeues
// (koryph-77r.1, koryph-6bl) — logs it, and evaluates the I7 cache-ratio
// tripwire against the attempt's OWN composition (not the accumulated
// total), so a healthy early attempt can never mask a later attempt's
// cache-prefix collapse.
func (r *runner) applyTokenUsage(beadID string, u dispatch.TokenUsage) {
	_ = r.store.UpdateSlot(r.run, beadID, func(s *ledger.Slot) {
		s.InputTokens += u.InputTokens
		s.OutputTokens += u.OutputTokens
		s.CacheReadTokens += u.CacheReadTokens
		s.CacheCreationTokens += u.CacheCreationTokens
	})
	logBeadTokens(beadID, u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheCreationTokens)
	r.checkCacheRatioTripwire(beadID, u)
}

// cacheRatioWarn evaluates the I7 cache-ratio tripwire's pure decision logic
// for one attempt's token composition — split out from
// checkCacheRatioTripwire so the threshold arithmetic is unit-testable
// without capturing log output. total is input+cache_read+cache_creation;
// warn is true iff total >= cacheRatioMinTokens and cache_read's share of
// total is below cacheRatioFloor. ratio/total are always returned (0 when
// below the volume floor) for the caller's log record.
func cacheRatioWarn(u dispatch.TokenUsage) (ratio float64, total int64, warn bool) {
	total = u.InputTokens + u.CacheReadTokens + u.CacheCreationTokens
	if total < cacheRatioMinTokens {
		return 0, total, false
	}
	ratio = float64(u.CacheReadTokens) / float64(total)
	return ratio, total, ratio < cacheRatioFloor
}

// checkCacheRatioTripwire WARNs when one attempt's cache_read share collapses
// below cacheRatioFloor on a session with at least cacheRatioMinTokens total
// volume (koryph-77r.1, design §2 I7). Observability only — it never changes
// dispatch behavior.
func (r *runner) checkCacheRatioTripwire(beadID string, u dispatch.TokenUsage) {
	if ratio, total, warn := cacheRatioWarn(u); warn {
		logCacheRatioTripwire(beadID, ratio, total)
	}
}

// requeueRateLimited re-dispatches a slot that died with a classified
// rate-limit/overload marker in its stream (koryph-2im.4): it reports the
// signal to the machine-wide governor (so the AIMD overlay backs off admission
// across every engine on the host) and then requeues WITHOUT incrementing
// Attempts — the failure is environmental, not the bead's — bounded instead by
// the independent RateLimitRequeues budget. I5 holds: this never touches a
// running agent, only gates the NEXT dispatch's admission via the governor.
func (r *runner) requeueRateLimited(ctx context.Context, sl *ledger.Slot) {
	// Same closed-bead guard as requeueSlot: drop cleanly if the operator
	// retired the bead while the rate-limited agent was running.
	if r.beadClosedMidFlight(ctx, sl.PhaseID) {
		return
	}
	r.reportRateLimit(sl.PhaseID)

	// Drain active: park, don't requeue (koryph-z0x, F1b).
	if r.parkForDrain(ctx, sl) {
		return
	}
	// Run --budget exhausted: park instead of re-dispatching (koryph-u7q), even
	// though a rate-limit requeue burns no attempt — over the cap is over the cap.
	if r.parkForRunBudget(ctx, sl) {
		return
	}

	if sl.RateLimitRequeues >= rateLimitedRequeueBudget {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = fmt.Sprintf("rate-limited requeues exhausted (%d)", rateLimitedRequeueBudget)
		})
		r.checkpointSlot(sl, "rate-limit-exhausted")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (rate-limited %d times; requeue budget exhausted)", sl.PhaseID, sl.RateLimitRequeues)
		logSlotBlocked(sl.PhaseID, fmt.Sprintf("rate-limited requeues exhausted (%d)", rateLimitedRequeueBudget),
			sl.Model, sl.ModelActual, sl.Attempts)
		r.reconcileBlockedBead(ctx, sl, fmt.Sprintf("rate-limited requeues exhausted (%d)", rateLimitedRequeueBudget))
		return
	}

	requeues := sl.RateLimitRequeues + 1
	r.progress("bead %s: rate-limited — requeueing without burning an attempt (%d/%d)",
		sl.PhaseID, requeues, rateLimitedRequeueBudget)
	r.backoffSleep(ctx, requeues)

	// Rate-limit requeues never preserve a no-commit worktree (koryph-77r.10
	// scopes that behavior to budget-kill deaths only, which is bead-specific
	// rather than environmental) — the WIP snapshot is still captured and
	// threaded through so a rebuilt worktree's resume can cite it.
	wipSnapshot := r.refreshWorktreeForRequeue(ctx, sl, false)

	resumeSession := ""
	if m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID); err == nil &&
		m.SessionID != "" && sl.Worktree != "" && fsx.Exists(sl.Worktree) {
		resumeSession = m.SessionID
	}

	r.dispatchBead(ctx, dispatchReq{
		issue:           r.issueFor(ctx, sl),
		epicID:          sl.EpicID,
		attempt:         sl.Attempts, // unchanged: environmental failure, not a bead attempt
		resumeSHA:       r.branchHead(ctx, sl.Branch),
		resumeSessionID: resumeSession,
		reviewIters:     sl.ReviewIters,
		note:            "rate-limited requeue",
		// Its own budget increments; every other spent budget carries forward
		// unchanged (koryph-qf6.1 — see requeueSlot's counter comment).
		rateLimitRequeues:     requeues,
		gateRequeues:          sl.GateRequeues,
		mergeRequeues:         sl.MergeRequeues,
		conflictRequeues:      sl.ConflictRequeues,
		budgetKillRequeues:    sl.BudgetKillRequeues,
		turnExhaustedRequeues: sl.TurnExhaustedRequeues,
		wipSnapshotPath:       wipSnapshot,
		// Freeze the model resolution from the first attempt (koryph-ehx) —
		// see requeueSlot's identical comment. A rate-limit requeue is the
		// same attempt continuing, so it must re-run the same model.
		frozenModel:    sl.Model,
		frozenPersona:  sl.Agent,
		frozenModelWhy: sl.ModelWhy,
		frozenEffort:   sl.Effort,
		// Carry the persisted footprint forward (koryph-2im.3): a requeue is
		// the SAME bead attempt continuing, not a relabeled re-evaluation, so
		// in-flight gating must stay exact across it rather than falling back
		// to a recompute that could have drifted from what was actually
		// admitted.
		footprint: sl.Footprint,
		// Carry the frozen resource claim forward too (koryph-4ql.3): a requeue
		// re-attaches the SAME kinds + reservation the slot was admitted with, so
		// a relabel or vocabulary edit mid-run cannot re-price a live slot (I8).
		resources: resourcesFromSlot(sl),
		// Carry the frozen similarity features forward too (koryph-qf6.3) —
		// same freeze rationale as footprint/resources.
		features: featuresFromSlot(sl),
		// Carry accumulated cost forward (koryph-6bl) — same reasoning as
		// requeueSlot: rate-limited agents may have spent tokens before being
		// throttled; that cost must not be lost across the requeue.
		accumulatedCostUSD: sl.CostUSD,
		// Carry accumulated token composition forward too (koryph-77r.1) —
		// same reasoning as accumulatedCostUSD.
		accumulatedTokens: dispatch.TokenUsage{
			InputTokens:         sl.InputTokens,
			OutputTokens:        sl.OutputTokens,
			CacheReadTokens:     sl.CacheReadTokens,
			CacheCreationTokens: sl.CacheCreationTokens,
		},
	})
}

// budgetKillRequeueBudget bounds how many warm-resume requeues a slot may
// spend on a classified budget-kill death (koryph-77r.10, design
// docs/designs/2026-07-token-economy.md recovery-economics follow-up)
// before parking needs-attention instead of trying a third time. Unlike
// rateLimitedRequeueBudget (5, an account-wide throttle that self-heals), a
// budget-kill is bead-specific: every dispatch already runs under a
// per-agent cap (dispatchBead's MaxBudgetUSD: r.quotaCfg.PerAgentMaxUSD), so
// a SECOND consecutive kill on the SAME bead means the bead itself needs
// more budget or a human's attention — a third blind cold-restart would
// just burn another whole cap for nothing. Unlike RateLimitRequeues, a
// budget-kill requeue DOES still count toward Attempts (see
// requeueBudgetKilled), so this only bounds the warm-resume leg of that
// normal attempt accounting.
const budgetKillRequeueBudget = 1

// thrashGuardTokenFloor is the per-ATTEMPT total token volume (input +
// output + cache_read + cache_creation — see totalAttemptTokens) above
// which a ZERO-commit budget-kill death is treated as thrashing rather than
// an honest budget-starved attempt worth a warm resume (koryph-77r.10):
// burning this many tokens with nothing committed is the token-economy
// signature of an agent looping large tool-call cycles (Read/Bash) rather
// than making progress — see design docs/designs/2026-07-token-economy.md
// §3 L1's measured fleet baseline (94.7% of raw tokens are cache reads on a
// healthy multi-turn session) — so a warm resume would likely just re-loop
// and burn a second cap for nothing. A documented heuristic, not a proven
// threshold; tune here if it over/under-fires in practice.
const thrashGuardTokenFloor = 150_000

// deathReasonBudgetKilled is the ledger.Slot.DeathReason value stamped when
// completeSlot classifies a death as killed by --max-budget-usd
// (koryph-77r.10).
const deathReasonBudgetKilled = "budget-killed"

// deathReasonTurnExhausted is the ledger.Slot.DeathReason value enforceTurnCeiling
// stamps (BEFORE it SIGTERMs the agent) when a slot runs past the per-bead
// turn ceiling (koryph-840). Unlike deathReasonBudgetKilled, this stamp is
// written by the LIVE poll (not by completeSlot's post-mortem classification):
// it is the mid-flight signal that survives to the next tick's completeSlot,
// which reads it to route the death to requeueTurnExhausted (a fresh-session
// requeue) rather than the warm-resume rate-limit/budget paths.
const deathReasonTurnExhausted = "turn-exhausted"

// turnExhaustedRequeueBudget bounds how many FRESH-session requeues a slot may
// spend on the turn ceiling before parking needs-attention (koryph-840). A
// fresh restart drops the accreted in-context history but keeps committed
// work (via resumeSHA), so a bead that genuinely converges in chunks can make
// progress across a couple of restarts; but a bead that keeps blowing past the
// ceiling with a clean context each time is not converging and needs a human
// (split it, or raise PerAgentMaxTurns), not an endless string of cold
// restarts. Set slightly above budgetKillRequeueBudget (1) because a fresh
// session is a genuinely different attempt shape — not the same bloated
// context re-warmed — so a second try is more likely to help here than there.
const turnExhaustedRequeueBudget = 2

// totalAttemptTokens sums one attempt's token composition — the thrash
// guard's volume signal (koryph-77r.10). Deliberately the ATTEMPT's own
// usage (dispatch.ParseResultUsage's/quota.SessionTokens' reading for THIS
// death, not the slot's accumulated total), mirroring cacheRatioWarn's
// identical reasoning: an early, healthy attempt must never mask a later
// attempt's pathological one.
func totalAttemptTokens(u dispatch.TokenUsage) int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadTokens + u.CacheCreationTokens
}

// requeueBudgetKilled re-dispatches (or parks) a slot classified as killed
// by --max-budget-usd (koryph-77r.10). Every dispatch already runs under a
// per-agent budget cap, so this death shape is routine, not exceptional —
// see events.go's budgetKillMarkers doc for the empirically pinned
// stream-json shape. The dominant case (zero commits) previously restarted
// cold: refreshWorktreeForRequeue's no-commit branch snapshotted WIP,
// removed the worktree, and dropped the branch BEFORE the session-resume
// precondition (m.SessionID != "" && fsx.Exists(sl.Worktree)) could ever
// hold, re-paying the entire exploration from an empty context on every
// requeue. This preserves the worktree and branch instead so --resume
// --fork-session fires warm, but bounds the warm-resume budget tightly
// (budgetKillRequeueBudget, far under rateLimitedRequeueBudget) and guards
// against thrashing: a bead that keeps dying from budget with nothing
// committed needs a human, not a third cap. Unlike requeueRateLimited, this
// DOES increment Attempts (via the normal dispatchReq.attempt =
// sl.Attempts+1) — a budget-kill is bead-specific, not an environmental
// throttle — so it can never itself exceed ledger.MaxAttempts; it just
// stops well short of that ceiling (park after budgetKillRequeueBudget)
// rather than blindly spending every remaining attempt cold.
func (r *runner) requeueBudgetKilled(ctx context.Context, sl *ledger.Slot, commits int, usage dispatch.TokenUsage) {
	// Same closed-bead guard as requeueSlot/requeueRateLimited: drop cleanly
	// if the operator retired the bead while the budget-killed agent was
	// running.
	if r.beadClosedMidFlight(ctx, sl.PhaseID) {
		return
	}

	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.DeathReason = deathReasonBudgetKilled })
	logBudgetKilled(sl.PhaseID, sl.Attempts, sl.CostUSD, sl.Model, sl.ModelActual)

	// Drain active: park, don't warm-resume requeue (koryph-z0x, F1b).
	if r.parkForDrain(ctx, sl) {
		return
	}
	// Run --budget exhausted: park instead of a warm-resume requeue (koryph-u7q).
	// The per-agent budget kill already fired; spending another attempt would
	// also breach the whole-run ceiling.
	if r.parkForRunBudget(ctx, sl) {
		return
	}

	thrashing := commits == 0 && totalAttemptTokens(usage) >= thrashGuardTokenFloor
	if sl.BudgetKillRequeues >= budgetKillRequeueBudget || thrashing {
		why := "budget-killed twice in a row"
		switch {
		case thrashing && sl.BudgetKillRequeues < budgetKillRequeueBudget:
			why = fmt.Sprintf("budget-killed with zero commits and %d tokens burned this attempt (thrash guard)",
				totalAttemptTokens(usage))
		case thrashing:
			why = fmt.Sprintf("budget-killed twice in a row, and zero commits with %d tokens burned this attempt (thrash guard)",
				totalAttemptTokens(usage))
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = fmt.Sprintf(
				"needs-attention: %s — parked instead of spending another --max-budget-usd attempt (accumulated cost $%.2f so far); raise the account's per-agent budget or split the bead",
				why, sl.CostUSD)
		})
		r.checkpointSlot(sl, "budget-killed-parked")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked, needs-attention (%s)", sl.PhaseID, why)
		logSlotBlocked(sl.PhaseID, why, sl.Model, sl.ModelActual, sl.Attempts)
		// Needs-attention write-back (koryph-qf6.5, koryph-84yu): reconcile the
		// bd claim to blocked so the parked bead is visible, not stranded
		// in_progress — see the attempts-exhausted block for why this lands on
		// the bead itself.
		r.reconcileBlockedBead(ctx, sl, fmt.Sprintf(
			"needs-attention: %s — parked instead of spending another --max-budget-usd attempt (accumulated cost $%.2f); raise the account's per-agent budget or split the bead",
			why, sl.CostUSD))
		return
	}

	requeues := sl.BudgetKillRequeues + 1
	attempt := sl.Attempts + 1
	r.progress("bead %s: budget-killed — warm-resume requeue, attempt %d (budget-kill %d/%d)",
		sl.PhaseID, attempt, requeues, budgetKillRequeueBudget)
	logSlotRequeue(sl.PhaseID, "budget-killed requeue", attempt)
	logRequeueEvent(r.run.RunID, r.opts.ProjectID, sl.PhaseID, "budget-killed requeue", attempt, sl.CostUSD)
	r.backoffSleep(ctx, sl.Attempts)

	// Preserve the worktree/branch on a zero-commit death so the
	// session-resume precondition holds (koryph-77r.10) — see
	// refreshWorktreeForRequeue's preserveNoCommitWorktree doc. The flag is a
	// no-op when commits > 0 (the existing rebase path applies unchanged).
	wipSnapshot := r.refreshWorktreeForRequeue(ctx, sl, true)

	resumeSession := ""
	if m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID); err == nil &&
		m.SessionID != "" && sl.Worktree != "" && fsx.Exists(sl.Worktree) {
		resumeSession = m.SessionID
	}

	r.dispatchBead(ctx, dispatchReq{
		issue:           r.issueFor(ctx, sl),
		epicID:          sl.EpicID,
		attempt:         attempt,
		resumeSHA:       r.branchHead(ctx, sl.Branch),
		resumeSessionID: resumeSession,
		reviewIters:     sl.ReviewIters,
		gateRequeues:    sl.GateRequeues,
		mergeRequeues:   sl.MergeRequeues,
		// Every other spent budget carries forward unchanged (koryph-qf6.1 —
		// see requeueSlot's counter comment); only budgetKillRequeues below
		// increments.
		conflictRequeues:      sl.ConflictRequeues,
		rateLimitRequeues:     sl.RateLimitRequeues,
		note:                  "budget-killed requeue",
		budgetKillRequeues:    requeues,
		turnExhaustedRequeues: sl.TurnExhaustedRequeues,
		wipSnapshotPath:       wipSnapshot,
		// Freeze the model resolution from the first attempt (koryph-ehx) —
		// see requeueSlot's identical comment. A budget-kill warm-resume must
		// re-run the same model the bead was originally dispatched with.
		frozenModel:    sl.Model,
		frozenPersona:  sl.Agent,
		frozenModelWhy: sl.ModelWhy,
		frozenEffort:   sl.Effort,
		// Carry the persisted footprint forward (koryph-2im.3) — see
		// requeueRateLimited's identical comment.
		footprint: sl.Footprint,
		// Carry the frozen resource claim forward (koryph-4ql.3) — see
		// requeueRateLimited's identical comment.
		resources: resourcesFromSlot(sl),
		// Carry the frozen similarity features forward (koryph-qf6.3) — see
		// requeueRateLimited's identical comment.
		features: featuresFromSlot(sl),
		// Carry accumulated cost forward (koryph-6bl): the budget-killed
		// attempt's own cost was already added to sl.CostUSD at the top of
		// completeSlot, so this is the correct running total.
		accumulatedCostUSD: sl.CostUSD,
		// Carry accumulated token composition forward too (koryph-77r.1) —
		// same reasoning as accumulatedCostUSD.
		accumulatedTokens: dispatch.TokenUsage{
			InputTokens:         sl.InputTokens,
			OutputTokens:        sl.OutputTokens,
			CacheReadTokens:     sl.CacheReadTokens,
			CacheCreationTokens: sl.CacheCreationTokens,
		},
	})
}

// enforceTurnCeiling counts a still-running agent's completed turns and, past
// the per-bead ceiling, gracefully interrupts it (koryph-840). It returns true
// when it tripped — the caller then returns, leaving the SIGTERM'd agent to
// checkpoint and exit so the next tick's completeSlot routes it (via the
// stamped DeathReason) to requeueTurnExhausted. It returns false (a no-op)
// when the ceiling is disabled (PerAgentMaxTurns <= 0), not yet reached, or
// already tripped this attempt — the DeathReason stamp makes the SIGTERM fire
// exactly once even though the poll re-enters every tick until the process
// actually dies. Turn count comes from the live stream via dispatch.CountTurns
// (num_turns is only emitted on the terminal result line, which a running
// agent has not written yet); it is an approximate completed-turn proxy, which
// the ceiling's wide headroom (default 150, far above healthy beads) absorbs.
func (r *runner) enforceTurnCeiling(sl *ledger.Slot) bool {
	ceiling := r.quotaCfg.PerAgentMaxTurns
	if ceiling <= 0 {
		return false // disabled for this account (negative) or unresolved
	}
	if sl.DeathReason == deathReasonTurnExhausted {
		return true // already tripped; the process is on its way down — don't re-signal
	}
	if sl.Stream == "" {
		return false
	}
	turns := dispatch.CountTurns(sl.Stream)
	if turns < ceiling {
		return false
	}
	// Stamp BEFORE signalling so the mark is durable even if this engine process
	// dies between the SIGTERM and the next tick: completeSlot keys the
	// fresh-session requeue off DeathReason, so a lost stamp would misroute the
	// death onto a warm-resume path and re-accrete the very context we are
	// shedding. Also updated in-memory so a re-entry this same tick short-circuits.
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.DeathReason = deathReasonTurnExhausted })
	sl.DeathReason = deathReasonTurnExhausted
	r.progress("bead %s: turn ceiling hit — %d turns >= %d; interrupting for a fresh-session requeue (worktree preserved)",
		sl.PhaseID, turns, ceiling)
	logTurnExhausted(sl.PhaseID, turns, ceiling, sl.Attempts, sl.Model, sl.ModelActual)
	// Graceful SIGTERM only (never SIGKILL) — agents checkpoint on it and their
	// worktree/branch are preserved, exactly like the hard-stop path
	// (interruptActiveSlots). A failed signal is non-fatal: the agent may have
	// already exited, in which case completeSlot reclassifies it on the next
	// tick off the stamp we just wrote.
	if err := dispatch.StopGraceful(sl.PID); err != nil {
		r.progress("bead %s: turn-ceiling SIGTERM failed (%v) — reclassifying on exit", sl.PhaseID, err)
	}
	return true
}

// requeueTurnExhausted re-dispatches (or parks) a slot enforceTurnCeiling
// interrupted for running past the per-bead turn ceiling (koryph-840). The
// defining difference from requeueBudgetKilled is the FRESH session: this
// re-dispatch passes NO resumeSessionID, so the new attempt starts with an
// empty context instead of --resume-ing the bloated one whose every-turn
// re-read is exactly what tripped the ceiling. Committed work still survives
// (resumeSHA = branch head; refreshWorktreeForRequeue rebases the worktree
// rather than discarding it when commits landed), so a fresh restart resumes
// from real progress with a clean context. Like requeueBudgetKilled it counts
// toward Attempts (bead-specific, not environmental) and bounds itself tightly
// (turnExhaustedRequeueBudget) before parking needs-attention.
func (r *runner) requeueTurnExhausted(ctx context.Context, sl *ledger.Slot) {
	// Same closed-bead guard as the other requeue paths: drop cleanly if the
	// operator retired the bead while the turn-capped agent was winding down.
	if r.beadClosedMidFlight(ctx, sl.PhaseID) {
		return
	}

	// Drain / run-budget parks take precedence over any retry, exactly as in
	// requeueBudgetKilled.
	if r.parkForDrain(ctx, sl) {
		return
	}
	if r.parkForRunBudget(ctx, sl) {
		return
	}

	if sl.TurnExhaustedRequeues >= turnExhaustedRequeueBudget {
		why := fmt.Sprintf("hit the turn ceiling %d times — a fresh session still isn't converging",
			turnExhaustedRequeueBudget+1)
		note := fmt.Sprintf(
			"needs-attention: %s — parked instead of another fresh restart (accumulated cost $%.2f); split the bead or raise per_agent_max_turns",
			why, sl.CostUSD)
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = note
		})
		r.checkpointSlot(sl, "turn-exhausted-parked")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked, needs-attention (%s)", sl.PhaseID, why)
		logSlotBlocked(sl.PhaseID, why, sl.Model, sl.ModelActual, sl.Attempts)
		r.reconcileBlockedBead(ctx, sl, note)
		return
	}

	requeues := sl.TurnExhaustedRequeues + 1
	attempt := sl.Attempts + 1
	r.progress("bead %s: turn-exhausted — fresh-session requeue, attempt %d (turn-exhausted %d/%d)",
		sl.PhaseID, attempt, requeues, turnExhaustedRequeueBudget)
	logSlotRequeue(sl.PhaseID, "turn-exhausted requeue", attempt)
	logRequeueEvent(r.run.RunID, r.opts.ProjectID, sl.PhaseID, "turn-exhausted requeue", attempt, sl.CostUSD)
	r.backoffSleep(ctx, sl.Attempts)

	// Rebuild the worktree the ordinary way (preserveNoCommitWorktree=false): a
	// fresh session must NOT reuse the old checkout's live session, so there is
	// no warm-resume precondition to protect. Committed work is rebased onto
	// current main; a zero-commit attempt is snapshotted to WIP and the branch
	// reset — exactly like the default requeue path.
	wipSnapshot := r.refreshWorktreeForRequeue(ctx, sl, false)

	r.dispatchBead(ctx, dispatchReq{
		issue:     r.issueFor(ctx, sl),
		epicID:    sl.EpicID,
		attempt:   attempt,
		resumeSHA: r.branchHead(ctx, sl.Branch),
		// resumeSessionID deliberately left "" — a FRESH session is the whole
		// point of this path (koryph-840); a --resume would re-read exactly the
		// accreted context the turn ceiling exists to shed.
		reviewIters:           sl.ReviewIters,
		gateRequeues:          sl.GateRequeues,
		mergeRequeues:         sl.MergeRequeues,
		conflictRequeues:      sl.ConflictRequeues,
		rateLimitRequeues:     sl.RateLimitRequeues,
		budgetKillRequeues:    sl.BudgetKillRequeues,
		turnExhaustedRequeues: requeues,
		note:                  "turn-exhausted requeue",
		wipSnapshotPath:       wipSnapshot,
		// Freeze the model resolution from the first attempt (koryph-ehx) — a
		// requeue re-runs the same model/persona/effort the bead was dispatched
		// with, exactly like every other requeue path.
		frozenModel:    sl.Model,
		frozenPersona:  sl.Agent,
		frozenModelWhy: sl.ModelWhy,
		frozenEffort:   sl.Effort,
		// Carry the frozen footprint/resources/features forward (koryph-2im.3/
		// 4ql.3/qf6.3) — see requeueRateLimited's identical comments.
		footprint: sl.Footprint,
		resources: resourcesFromSlot(sl),
		features:  featuresFromSlot(sl),
		// Carry accumulated cost and token composition forward (koryph-6bl/
		// 77r.1): the turn-capped attempt's spend was already folded into
		// sl.CostUSD / the slot's token fields at the top of completeSlot.
		accumulatedCostUSD: sl.CostUSD,
		accumulatedTokens: dispatch.TokenUsage{
			InputTokens:         sl.InputTokens,
			OutputTokens:        sl.OutputTokens,
			CacheReadTokens:     sl.CacheReadTokens,
			CacheCreationTokens: sl.CacheCreationTokens,
		},
	})
}

// beadClosedMidFlight checks whether the bead has been closed or deferred by
// the operator while the agent was running. When true it marks the slot blocked
// (without burning an attempt), releases the global slot, logs the event, and
// returns true so every requeue path can return early.
//
// A Show error is treated as "not closed": if we cannot confirm the bead's
// state we let the requeue proceed rather than silently dropping work on a
// transient bd failure.
func (r *runner) beadClosedMidFlight(ctx context.Context, id string) bool {
	iss, err := r.adapter.Show(ctx, id)
	if err != nil {
		return false // bd unavailable or bead not found — let the requeue proceed
	}
	if iss.Status != "closed" && iss.Status != "deferred" {
		return false
	}
	_ = r.store.UpdateSlot(r.run, id, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = "bead closed while in flight — releasing slot"
	})
	if sl := r.run.Slots[id]; sl != nil {
		r.checkpointSlot(sl, "closed-mid-flight")
	}
	r.releaseGlobalSlot(id)
	r.progress("bead %s: bead closed while in flight — releasing slot", id)
	return true
}

// proxyBaseURLForSlot resolves the ANTHROPIC_BASE_URL a secondary spawn tied
// to slot sl (post-implement pipeline stage, review) should use
// (koryph-3l1.3, design §3 L6): it follows the SAME arm sl's own main
// dispatch was already assigned (registry.AgentProxy.ArmFor, computed once in
// dispatchBead and stamped into sl.ProxyID) rather than recomputing or
// defaulting to the project's live config — a stage/review spawned for a
// holdout-arm bead must stay direct too, or proxied stage/review traffic
// would leak into what is supposed to be the "no interception" control
// population's telemetry, corrupting the comparison. sl.ProxyID=="" covers
// both "no agent_proxy configured" and "this bead's holdout arm" identically
// (see ledger.Slot.ProxyID's doc) — exactly the case where no
// ANTHROPIC_BASE_URL override belongs in ChildEnvSpec.ProxyBaseURL.
func (r *runner) proxyBaseURLForSlot(sl *ledger.Slot) string {
	if sl.ProxyID == "" {
		return ""
	}
	return r.rec.ProxyBaseURL()
}

// finishCandidate runs the configured post-implement pipeline stages, the
// optional review pass, and then applies the merge policy to a completed slot.
func (r *runner) finishCandidate(ctx context.Context, sl *ledger.Slot) {
	policy := r.mergePolicy(ctx, sl.EpicID)

	// Completion is runtime-neutral: every adapter ultimately produces a git
	// branch/worktree plus the shared status document. Refuse incomplete
	// candidates before pipeline/review can create noise or merge can mistake
	// the unchanged base commit for agent work.
	assessment := r.assessCandidate(ctx, sl)
	if !assessment.eligible {
		if assessment.capabilityBlock {
			r.parkCapabilityBlock(ctx, sl, assessment.capability, assessment.capabilityDetail)
			return
		}
		if assessment.retryableBlock {
			r.progress("bead %s: clean committed candidate self-blocked without structured capability fields; retrying for classification correction", sl.PhaseID)
			r.requeueSlot(ctx, sl, "", genericCompletionBlockRequeueNote)
			return
		}
		r.parkIncompleteCandidate(ctx, sl, assessment.reason)
		return
	}

	// --direct is the owner override: skip the PR flow and merge straight to the
	// default branch, even on a merge:pr epic. The push to a protected default
	// branch still requires the identity to hold a branch-protection bypass —
	// koryph does not gate on org role. A blocking review can still downgrade
	// this to manual below, so the safety path is not bypassed (koryph-ufy.5).
	if r.opts.Direct {
		policy = project.PolicyAuto
	}

	// Post-implement stages (docs, test, ...) run in the worktree before review
	// and merge (koryph-a14). A required stage failure blocks the slot —
	// never auto-merge past incomplete pipeline work.
	if ok, failed := r.runPipelineStages(ctx, sl); !ok {
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "pipeline stage failed: " + failed
		})
		r.checkpointSlot(sl, "stage-failed")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (pipeline stage %q failed)", sl.PhaseID, failed)
		return
	}

	if r.opts.Review {
		runtimeName := sl.Runtime
		if runtimeName == "" {
			runtimeName = r.rt.Name()
		}
		reviewRT, ok := runtimeForName(runtimeName)
		if !ok {
			r.blockSlot(sl.PhaseID, dispatchReq{issue: beads.Issue{ID: sl.PhaseID}}, "review runtime is no longer registered")
			return
		}
		ra := r.rec.AccountFor(runtimeName)
		reviewProfile := account.Profile{Name: r.rec.AccountProfile, ConfigDir: ra.ConfigDir}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = ledger.SlotReview })
		outPath := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "review.json")
		reviewPersona := modelroute.PersonaFor(modelroute.StageReview, r.cfg.Stages)
		// The reviewer's model tier stays hardcoded opus (quality-critical, never
		// auto-downgraded — koryph-77r.8 audit). Effort was previously never
		// threaded through at all, so the persona's own frontmatter `effort:`
		// hint (koryph-security-reviewer: xhigh) was silently dropped; resolve it
		// here the same way wave.go's main-dispatch path does, so the already
		// declared effort actually takes effect.
		reviewEffort := ""
		if _, metaEffort, _, err := modelroute.PersonaMeta(r.rec.Root, reviewPersona); err == nil {
			reviewEffort = metaEffort
		}
		// Unified reviewer timeout (koryph-w82i): the single wall-clock budget is
		// the bead > project > system > built-in winner. The bead's
		// `timeout:<seconds>` label wins, else the project review.timeout_seconds
		// (EffectiveReview), else the machine-wide default, else the built-in
		// 1200s. No escalation, no hard cap.
		issue := r.issueFor(ctx, sl)
		beadTimeout, _ := timeoutcfg.BeadTimeout(issue.Labels)
		reviewTimeout := timeoutcfg.Resolve(beadTimeout, r.cfg.EffectiveReview().TimeoutSeconds, r.systemTimeoutSec)
		reviewModel, rerr := r.resolveModelForRuntime(modelroute.StageReview, issue, "", runtimeName)
		if rerr != nil {
			r.blockSlot(sl.PhaseID, dispatchReq{issue: beads.Issue{ID: sl.PhaseID}}, "review model resolution: "+rerr.Error())
			return
		}
		v := review.Review(ctx, review.Opts{
			RepoRoot:  r.rec.Root,
			Worktree:  sl.Worktree,
			Branch:    sl.Branch,
			Base:      r.rec.DefaultBranch,
			Persona:   reviewPersona,
			Model:     reviewModel.Model,
			Effort:    reviewEffort,
			Profile:   reviewProfile,
			OutPath:   outPath,
			ClaudeBin: os.Getenv(envClaudeBin),
			Runtime:   reviewRT,
			Contract: review.Contract{
				ID: issue.ID, Title: issue.Title, Description: issue.Description,
				AcceptanceCriteria: issue.AcceptanceCriteria, Labels: issue.Labels,
				Runtime: runtimeName, CompletionState: func() string {
					state, _ := completionState(sl.StatusPath)
					return state
				}(),
			},
			TimeoutSec:   reviewTimeout,
			ProxyBaseURL: r.proxyBaseURLForSlot(sl),
		})
		if v.Degraded {
			// Fail CLOSED: --review was explicitly requested, so a review we
			// could not obtain (even after in-reviewer retries) must never wave
			// the merge through. Block the slot and surface the reason rather
			// than silently auto-merging unreviewed work (koryph-b2h).
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotBlocked
				s.Note = fmt.Sprintf("review degraded after %d attempt(s), NOT merged: %s", v.Attempts, v.Reason)
			})
			r.checkpointSlot(sl, "review-degraded")
			r.releaseGlobalSlot(sl.PhaseID) // terminal
			r.progress("bead %s: BLOCKED — review could not complete after %d attempt(s) (%s); refusing to auto-merge unreviewed work",
				sl.PhaseID, v.Attempts, v.Reason)
			r.reconcileBlockedBead(ctx, sl, "review degraded: "+v.Reason)
			return
		}
		if v.Blocking {
			if sl.ReviewIters < 2 {
				_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.ReviewIters++ })
				r.progress("bead %s: blocking review findings (iteration %d) — bouncing back to the implementer",
					sl.PhaseID, sl.ReviewIters)
				r.requeueSlot(ctx, sl, outPath, "blocking review findings")
				return
			}
			// Iterations exhausted: unresolved findings are a terminal block,
			// not a weaker merge policy. "Manual merge" was interpreted as
			// ready-for-merge and let wrong-scope or unsafe work advance.
			note := fmt.Sprintf("blocking review findings persist after %d iteration(s) — branch/worktree preserved; resolve findings before retrying", sl.ReviewIters)
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
				s.Status = ledger.SlotBlocked
				s.Note = note
			})
			r.checkpointSlot(sl, "review-blocking")
			r.releaseGlobalSlot(sl.PhaseID)
			r.progress("bead %s: blocked (%s)", sl.PhaseID, note)
			r.auditBlocked(ctx, sl, "review-blocking", v.Raw)
			return
		}
	}

	// merge_policy pr never touches the protected default branch directly:
	// push the branch and open a PR for a later fast-forward landing step.
	// This is the safe path for protected branches, so — unlike auto-merge —
	// it is not gated on --auto-merge (koryph-ufy.1).
	if policy == project.PolicyPR {
		r.openPRSlot(ctx, sl)
		return
	}

	if policy == project.PolicyAuto && (r.opts.AutoMerge || r.opts.Direct) {
		r.mergeSlot(ctx, sl)
		return
	}

	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.Status = ledger.SlotMergePending })
	r.checkpointSlot(sl, "merge-pending")
	r.releaseGlobalSlot(sl.PhaseID) // agent done; free the slot (operator merges)
	_ = r.adapter.Comment(ctx, sl.PhaseID,
		fmt.Sprintf("ready for merge: branch %s, run %s", sl.Branch, r.run.RunID))
	r.progress("bead %s: merge-pending (policy %s, branch %s) — merge via the CLI", sl.PhaseID, policy, sl.Branch)
}

// mergeSlot lands a review-clean candidate on the default branch.
// mergeReconcilers maps the project's generated-file reconciler vocabulary to
// the merge package's type. merge deliberately does not import project, so the
// two types are kept in sync by this one-line copy (like Gate []string).
func mergeReconcilers(cfg *project.Config) []merge.Reconciler {
	if cfg == nil || len(cfg.MergeReconcilers) == 0 {
		return nil
	}
	out := make([]merge.Reconciler, len(cfg.MergeReconcilers))
	for i, rc := range cfg.MergeReconcilers {
		out[i] = merge.Reconciler{Path: rc.Path, Command: rc.Command}
	}
	return out
}

func (r *runner) mergeSlot(ctx context.Context, sl *ledger.Slot) {
	res, err := merge.Merge(ctx, merge.Opts{
		RepoRoot:            r.rec.Root,
		Branch:              sl.Branch,
		DefaultBranch:       r.rec.DefaultBranch,
		Gate:                r.cfg.Gate,
		Extra:               r.cfg.ProtectedPaths,
		Push:                true, // merge itself skips push when no remote exists
		SlotOwner:           r.owner,
		SlotRetries:         3,
		Slot:                r.slotLocker(ctx),
		RequireSigned:       r.requireSigned(),
		RequireConventional: r.cfg.EnforceConventional(),
		Reconcilers:         mergeReconcilers(r.cfg),
		Prepare:             r.cfg.MergePrepare,
	})
	if err != nil {
		// A merge error is usually transient (base moved, push raced). Self-heal
		// by requeueing — requeueSlot Force-rebases the landed branch onto
		// current main and resumes — rather than stranding the bead. Only after
		// mergeRequeueBudget requeues does a failure block. Mirrors the
		// gate-failed path below (koryph-3fs, budget koryph-2im.6).
		if mergeErrorRetryable(sl) {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.MergeRequeues++ })
			r.progress("bead %s: merge error (%v) — requeueing (%d/%d) to retry the merge",
				sl.PhaseID, err, sl.MergeRequeues, mergeRequeueBudget)
			r.requeueSlot(ctx, sl, "", mergeErrorRequeueNote)
			return
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge error after requeue: " + err.Error()
		})
		r.checkpointSlot(sl, "merge-error")
		r.releaseGlobalSlot(sl.PhaseID) // terminal
		r.progress("bead %s: blocked (merge error after requeue: %v)", sl.PhaseID, err)
		return
	}

	if res.Status == merge.StatusMerged {
		now := time.Now().UTC().Format(time.RFC3339)
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotMerged
			s.MergedAt = now
		})
		r.checkpointSlot(sl, "merged")
		_ = r.adapter.Close(ctx, sl.PhaseID, "merged: "+res.MergedSHA)
		r.writeBackEscalatedMerge(ctx, sl.PhaseID)
		r.noteEpicCandidate(ctx, sl.PhaseID)
		_ = r.reg.Audit(registry.Event{
			Kind:      "merge",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead": sl.PhaseID, "branch": sl.Branch, "sha": res.MergedSHA,
			},
		})
		r.progress("bead %s: merged (%s)", sl.PhaseID, shortSHA(res.MergedSHA))
		if len(res.Reconciled) > 0 {
			// A healed rebase is not a silent success: surface it so a rising
			// heal rate flags a missing footprint label upstream (the cheaper
			// fix). See docs/designs/2026-07-merge-reconcilers.md L7.
			r.progress("bead %s: rebase conflict auto-healed (%d generated file(s), %d round(s)): %s",
				sl.PhaseID, len(res.Reconciled), res.ReconcileRounds, strings.Join(res.Reconciled, ", "))
			_ = r.reg.Audit(registry.Event{
				Kind:      "merge-reconcile",
				ProjectID: r.opts.ProjectID,
				Actor:     r.owner,
				Detail: map[string]string{
					"bead": sl.PhaseID, "branch": sl.Branch,
					"paths":  strings.Join(res.Reconciled, ","),
					"rounds": strconv.Itoa(res.ReconcileRounds),
				},
			})
		}
		if res.Prepared {
			// merge_prepare normalized the rebased tree (e.g. renumbered a
			// migration to tip) and koryph committed it before the gate.
			r.progress("bead %s: merge-prepare normalized the rebased tree before merge", sl.PhaseID)
			_ = r.reg.Audit(registry.Event{
				Kind:      "merge-prepare",
				ProjectID: r.opts.ProjectID,
				Actor:     r.owner,
				Detail:    map[string]string{"bead": sl.PhaseID, "branch": sl.Branch},
			})
		}
		if sl2 := r.run.Slots[sl.PhaseID]; sl2 != nil {
			logSlotMerged(r.run.RunID, r.opts.ProjectID, sl.PhaseID, shortSHA(res.MergedSHA), sl2.CostUSD,
				sl2.Model, sl2.ModelActual, sl2.Attempts)
		}
		r.releaseGlobalSlot(sl.PhaseID)
		return
	}

	// The gate-failed requeue keeps its slot and returns early; every other
	// (terminal) failure frees the global slot.
	if r.handleMergeFailure(ctx, sl, res) {
		return
	}
	r.releaseGlobalSlot(sl.PhaseID)
}

// handleMergeFailure records the non-success outcomes shared by the ff-merge
// and PR-open paths (gate-failed, conflict, protected, unsigned, and any
// unknown status). It returns true when it requeued the slot instead of
// blocking it (the gate-failed single retry): the caller must then return
// WITHOUT releasing the global slot, because a requeue keeps it.
// auditBlocked records a TERMINAL merge refusal durably: the canonical
// engine.slot.blocked telemetry event (which other consumers filter on) plus a
// registry audit.jsonl entry — mirroring the pr-open success path (openPRSlot).
// Before this, the safety-relevant refusals (protected paths touched, unsigned
// commit rejected, gate failed) left only a per-run ledger Note and a free-text
// progress line: no filterable event, no durable cross-run audit. reason is a
// stable token (gate-failed, unsigned, protected, commit-style, pr-error,
// merge-error) consumers key on.
func (r *runner) auditBlocked(ctx context.Context, sl *ledger.Slot, reason, detail string) {
	logSlotBlocked(sl.PhaseID, reason, sl.Model, sl.ModelActual, sl.Attempts)
	// koryph-84yu: every auditBlocked caller is a TERMINAL block that keeps the
	// bead claimed (the conflict path, which resets to open instead, never lands
	// here). Reconcile the bd claim to blocked-with-reason so a merge-refused
	// bead is visible for operator resolution, not silently stranded in_progress.
	r.reconcileBlockedBead(ctx, sl, reason)
	audit := map[string]string{
		"bead":   sl.PhaseID,
		"reason": reason,
		"branch": sl.Branch,
		"detail": tailOf(detail, 400),
	}
	// Persist the full failure detail to a discoverable file so a gate/signing
	// failure is diagnosable post-hoc: the ledger Note and the audit entry keep
	// only a 400-char tail, and merge itself does no logging. The phase dir is
	// the same place session.log/stderr.log live, so `koryph tail` and the TUI
	// find it. Best-effort — a write failure must not mask the block.
	if detail != "" {
		outPath := filepath.Join(r.store.PhaseDir(r.run.RunID, sl.PhaseID), "gate-output.log")
		if err := os.WriteFile(outPath, []byte(detail+"\n"), 0o644); err == nil {
			audit["output_file"] = outPath
		}
	}
	_ = r.reg.Audit(registry.Event{
		Kind:      "blocked",
		ProjectID: r.opts.ProjectID,
		Actor:     r.owner,
		Detail:    audit,
	})
}

func (r *runner) handleMergeFailure(ctx context.Context, sl *ledger.Slot, res merge.Result) (requeued bool) {
	switch res.Status {
	case merge.StatusGateFailed:
		if gateRequeueRetryable(sl) {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.GateRequeues++ })
			r.progress("bead %s: gate failed after rebase — requeueing (%d/%d)",
				sl.PhaseID, sl.GateRequeues, gateRequeueBudget)
			r.requeueSlot(ctx, sl, "", gateRequeueNote)
			return true
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "gate failed after requeue: " + tailOf(res.GateOutput, 400)
		})
		r.checkpointSlot(sl, "gate-failed")
		r.progress("bead %s: blocked (gate failed after requeue)", sl.PhaseID)
		r.auditBlocked(ctx, sl, "gate-failed", res.GateOutput)

	case merge.StatusCommitStyle:
		if commitStyleRetryable(sl) {
			r.progress("bead %s: non-conventional commit subject(s) — bouncing to the implementer to reword",
				sl.PhaseID)
			r.requeueSlot(ctx, sl, "", commitStyleRequeueNote)
			return true
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "commit-style: non-conventional commit subject(s) persist after a reword requeue — " +
				"reword each to 'type(scope): subject' (type ∈ feat|fix|docs|chore|refactor|revert|test|ci|build|perf|style) " +
				"then re-dispatch with koryph run. Offending: " + tailOf(res.GateOutput, 300)
		})
		r.checkpointSlot(sl, "commit-style")
		r.progress("bead %s: blocked (non-conventional commit subjects persist after requeue)", sl.PhaseID)
		r.auditBlocked(ctx, sl, "commit-style", res.GateOutput)

	case merge.StatusDirty:
		note := "candidate worktree has uncommitted changes — preserved for recovery; commit the intended work before retrying"
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = note
		})
		r.checkpointSlot(sl, "dirty")
		r.progress("bead %s: blocked (%s)", sl.PhaseID, note)
		r.auditBlocked(ctx, sl, "dirty", res.GateOutput)

	case merge.StatusNoChanges:
		note := "candidate branch has no commits beyond the dispatch base — refusing false merge"
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = note
			s.Commits = 0
			s.LastCommit = ""
		})
		r.checkpointSlot(sl, "no-changes")
		r.progress("bead %s: blocked (%s)", sl.PhaseID, note)
		r.auditBlocked(ctx, sl, "no-changes", res.GateOutput)

	case merge.StatusConflict:
		// A rebase conflict is the most agent-resolvable merge failure: requeue
		// so the agent resumes on its branch with CONFLICT.md and resolves it
		// (koryph-3as). Terminal SlotConflict previously stranded the bead
		// in_progress — invisible to bd ready — when the run later drained.
		if sl.ConflictRequeues < conflictRequeueBudget && sl.Attempts < ledger.MaxAttempts {
			_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) { s.ConflictRequeues++ })
			r.progress("bead %s: rebase conflict — requeueing (%d/%d) for in-worktree resolution (see CONFLICT.md)",
				sl.PhaseID, sl.ConflictRequeues, conflictRequeueBudget)
			r.requeueSlot(ctx, sl, "", conflictRequeueNote)
			return true
		}
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotConflict
			s.Note = "rebase conflict after requeue: " + res.ConflictMD
		})
		r.checkpointSlot(sl, "conflict")
		// Reset the bead so the frontier can re-adopt it in a later run — a
		// terminal conflict slot must never strand the bead in_progress.
		if err := r.adapter.SetStatus(ctx, sl.PhaseID, "open"); err == nil {
			_ = r.adapter.Comment(ctx, sl.PhaseID,
				"engine: rebase conflict unresolved after "+conflictRequeueNote+" budget; bead reset to open — worktree and CONFLICT.md preserved for the next attempt")
		}
		r.progress("bead %s: rebase conflict after requeue budget — bead reset to open (details: %s)",
			sl.PhaseID, res.ConflictMD)
		logSlotConflict(sl.PhaseID, res.ConflictMD)

	case merge.StatusProtected:
		note := "protected paths touched: " + strings.Join(res.Protected, ", ") +
			" — " + r.protectedResolutionHint(res.Protected, sl.Branch, sl.PhaseID)
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = note
		})
		r.checkpointSlot(sl, "protected")
		r.progress("bead %s: blocked (%s)", sl.PhaseID, note)
		r.auditBlocked(ctx, sl, "protected", note)

	case merge.StatusUnsigned:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge refused: " + tailOf(res.GateOutput, 400)
		})
		r.checkpointSlot(sl, "unsigned")
		r.progress("bead %s: blocked (unsigned commits on %s — signing is required; nothing merged)",
			sl.PhaseID, sl.Branch)
		r.auditBlocked(ctx, sl, "unsigned", res.GateOutput)

	default:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotFailed
			s.Note = "merge status " + string(res.Status)
		})
		r.checkpointSlot(sl, "merge-"+string(res.Status))
		r.progress("bead %s: merge failed (%s)", sl.PhaseID, string(res.Status))
		r.auditBlocked(ctx, sl, "merge-"+string(res.Status), res.GateOutput)
	}
	return false
}

// openPRSlot handles merge_policy pr: it reuses the merge preflight (protected
// paths, signing, sync, rebase, gate) and then pushes the branch and opens a
// pull request instead of fast-forward merging. The worktree/branch are kept
// for a later landing step, so the bead parks in pr-opened rather than merged.
func (r *runner) openPRSlot(ctx context.Context, sl *ledger.Slot) {
	iss := r.issueFor(ctx, sl)
	res, err := merge.Merge(ctx, merge.Opts{
		RepoRoot:            r.rec.Root,
		Branch:              sl.Branch,
		DefaultBranch:       r.rec.DefaultBranch,
		Gate:                r.cfg.Gate,
		Extra:               r.cfg.ProtectedPaths,
		SlotOwner:           r.owner,
		SlotRetries:         3,
		Slot:                r.slotLocker(ctx),
		RequireSigned:       r.requireSigned(),
		RequireConventional: r.cfg.EnforceConventional(),
		Reconcilers:         mergeReconcilers(r.cfg),
		Prepare:             r.cfg.MergePrepare,
		OpenPR:              true,
		KeepWorktree:        true, // the branch parks for a later landing step
		PRTitle:             prTitle(iss),
		PRBody:              prBody(iss, r.run.RunID),
	})
	if err != nil {
		// A push or gh error is usually config/auth (not a transient rebase
		// race), so block with the reason rather than looping. The branch is
		// kept, so a fixed remote/gh lets a --resume retry.
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "pr error: " + err.Error()
		})
		r.checkpointSlot(sl, "pr-error")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (pr error: %v)", sl.PhaseID, err)
		r.auditBlocked(ctx, sl, "pr-error", err.Error())
		return
	}

	switch res.Status {
	case merge.StatusPROpened:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotPROpened
			s.Note = fmt.Sprintf("PR #%d opened: %s", res.PRNumber, res.PRURL)
		})
		r.checkpointSlot(sl, "pr-opened")
		_ = r.reg.Audit(registry.Event{
			Kind:      "pr-open",
			ProjectID: r.opts.ProjectID,
			Actor:     r.owner,
			Detail: map[string]string{
				"bead": sl.PhaseID, "branch": sl.Branch, "pr": res.PRURL,
			},
		})
		_ = r.adapter.Comment(ctx, sl.PhaseID,
			fmt.Sprintf("PR opened for merge: %s (branch %s, run %s)", res.PRURL, sl.Branch, r.run.RunID))
		r.progress("bead %s: pr-opened (%s, branch %s) — land the PR to complete", sl.PhaseID, res.PRURL, sl.Branch)
		r.releaseGlobalSlot(sl.PhaseID)

	case merge.StatusPRNoRemote:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge_policy pr requires a git remote; none configured"
		})
		r.checkpointSlot(sl, "pr-no-remote")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (merge_policy pr needs a git remote; branch %s kept)", sl.PhaseID, sl.Branch)

	case merge.StatusPRNoGH:
		_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
			s.Status = ledger.SlotBlocked
			s.Note = "merge_policy pr requires an authenticated gh CLI; unavailable"
		})
		r.checkpointSlot(sl, "pr-no-gh")
		r.releaseGlobalSlot(sl.PhaseID)
		r.progress("bead %s: blocked (merge_policy pr needs an authenticated gh; branch %s kept)", sl.PhaseID, sl.Branch)

	default:
		if r.handleMergeFailure(ctx, sl, res) {
			return
		}
		r.releaseGlobalSlot(sl.PhaseID)
	}
}

// prTitle renders a conventional-commit-shaped PR title from a bead: the type
// derives from the bead type (bug→fix, chore→chore, else feat), the scope is
// the bead id, and the subject is the bead title's first line.
func prTitle(iss beads.Issue) string {
	typ := "feat"
	switch iss.IssueType {
	case "bug":
		typ = "fix"
	case "chore":
		typ = "chore"
	}
	subject := iss.Title
	if i := strings.IndexByte(subject, '\n'); i >= 0 {
		subject = subject[:i]
	}
	if subject = strings.TrimSpace(subject); subject == "" {
		subject = iss.ID
	}
	return fmt.Sprintf("%s(%s): %s", typ, iss.ID, subject)
}

// prBody renders a PR body carrying the bead id, title, and acceptance text
// (the bead description), plus a note that the PR must land fast-forward-only.
func prBody(iss beads.Issue, runID string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Bead **%s**: %s\n\n", iss.ID, iss.Title)
	if desc := strings.TrimSpace(iss.Description); desc != "" {
		b.WriteString(desc)
		b.WriteString("\n\n")
	}
	fmt.Fprintf(&b, "---\nOpened by `koryph run` (merge_policy=pr, run %s). Land fast-forward-only; do not push straight to the default branch.\n", runID)
	return b.String()
}

// parkForRunBudget refuses a requeue when the per-run --budget cap is exhausted
// (koryph-u7q): re-dispatching would spend past a ceiling the operator set, and
// requeues run INSIDE pollUntilIdle where the wave-boundary budget gate never
// sees them — that hole let a run sail past --budget one retry at a time.
// Instead it parks the slot terminal (needs-attention) and returns true. false
// means the budget still has room and the caller should proceed with the
// requeue. Mirrors the budget-KILL park (SlotBlocked + note + release), so the
// bead stays claimed for operator resolution rather than being silently retried.
func (r *runner) parkForRunBudget(ctx context.Context, sl *ledger.Slot) bool {
	if !r.budgetExhausted() {
		return false
	}
	note := fmt.Sprintf(
		"needs-attention: run --budget cap reached ($%.2f projected >= $%.2f) — parked without re-dispatch (accumulated cost $%.2f); raise --budget or resume the run",
		r.projectedRunCostUSD(), r.opts.BudgetUSD, sl.CostUSD)
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = note
	})
	r.releaseGlobalSlot(sl.PhaseID) // terminal: free the reserved/held slot
	r.progress("bead %s: not requeued — %s", sl.PhaseID, note)
	logSlotBlocked(sl.PhaseID, note, sl.Model, sl.ModelActual, sl.Attempts)
	r.reconcileBlockedBead(ctx, sl, note) // koryph-84yu: never strand the claim in_progress
	return true
}

// parkForOperatorStop parks a phase an operator explicitly stopped via `koryph
// stop` (koryph-a1x, F1a). An operator stop is a terminal intent, not a death
// to classify: never auto-retry it, and never auto-merge its partial commits —
// a redispatch (or an auto-merge of half-finished work) can race the operator's
// own hand-fix on the same files. Consumes the sentinel so a later deliberate
// re-dispatch (koryph run) is not blocked. Returns true when it parked.
func (r *runner) parkForOperatorStop(ctx context.Context, sl *ledger.Slot) bool {
	if !r.store.StopRequested(sl.PhaseID) {
		return false
	}
	r.store.ConsumeStop(sl.PhaseID)
	const note = "operator-stopped via koryph stop — not auto-retried; re-dispatch explicitly with koryph run"
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = note
	})
	r.releaseGlobalSlot(sl.PhaseID) // terminal
	r.progress("bead %s: not requeued — %s", sl.PhaseID, note)
	logSlotBlocked(sl.PhaseID, note, sl.Model, sl.ModelActual, sl.Attempts)
	r.reconcileBlockedBead(ctx, sl, note) // koryph-84yu: never strand the claim in_progress
	return true
}

// parkForDrain parks a death that arrived while an operator drain is active
// (koryph-z0x, F1b). Drain's contract is "start no new work", which must cover
// retries, not only fresh pulls from bd ready — the requeue path had no drain
// check, so a death during drain silently admitted a new attempt. finishing an
// active slot's committed work (finishCandidate) is unaffected; only requeues
// park here. Returns true when it parked.
func (r *runner) parkForDrain(ctx context.Context, sl *ledger.Slot) bool {
	if !r.store.DrainRequested() {
		return false
	}
	const note = "drain active — not requeued; re-dispatch after the drain completes"
	_ = r.store.UpdateSlot(r.run, sl.PhaseID, func(s *ledger.Slot) {
		s.Status = ledger.SlotBlocked
		s.Note = note
	})
	r.releaseGlobalSlot(sl.PhaseID) // terminal for this run
	r.progress("bead %s: not requeued — %s", sl.PhaseID, note)
	logSlotBlocked(sl.PhaseID, note, sl.Model, sl.ModelActual, sl.Attempts)
	r.reconcileBlockedBead(ctx, sl, note) // koryph-84yu: never strand the claim in_progress
	return true
}

// protectedResolutionHint composes the operator's next step for a protected-
// path block (koryph-zfg, F2). When every touched path is in the liftable
// subset (.github/, Makefile) it names the one-command sanctioned landing;
// otherwise it says manual review is required because --allow-protected will
// not lift governance defaults or the project's own protected_paths — so the
// operator does not waste an attempt on a flag that will still refuse.
func (r *runner) protectedResolutionHint(hits []string, branch, phaseID string) string {
	if merge.AllLiftable(hits) {
		return fmt.Sprintf(
			"routine CI/build paths — land with: koryph merge --project %s %s --allow-protected --push --close-bead %s --reason <why>",
			r.opts.ProjectID, branch, phaseID)
	}
	return "includes governance or project-policy paths — requires manual review; --allow-protected will not lift these"
}

// slotEscalated reports whether a slot's model rationale records an in-run
// escalation (koryph-qf6.4) — the same substring the TUI's ↑ marker keys on
// (tui/threads.go), so ledger, display, and write-back agree on what counts.
func slotEscalated(sl *ledger.Slot) bool {
	return sl != nil && strings.Contains(strings.ToLower(sl.ModelWhy), "escalat")
}

// blockedModelDesc renders the model a blocked slot's failure write-back
// should cite (koryph-qf6.5): the tier, expanded with the escalation
// rationale when the final attempt escalated ("opus" alone would hide that
// the first attempts ran sonnet) and with the actual model when the CLI's
// fallback diverged from the request (koryph-qf6.2).
func blockedModelDesc(sl *ledger.Slot) string {
	desc := sl.Model
	if desc == "" {
		desc = "unknown model"
	}
	if slotEscalated(sl) {
		desc = fmt.Sprintf("%s (%s)", desc, sl.ModelWhy)
	}
	if sl.ModelActual != "" && sl.ModelActual != sl.Model {
		desc += fmt.Sprintf(" — actually ran %s", sl.ModelActual)
	}
	return desc
}

// writeBackEscalatedMerge leaves durable provenance ON THE BEAD when it only
// merged after its final attempt escalated (koryph-qf6.5): a
// model-observed:<tier> label, which syncs through the beads DB across
// machines — unlike the gitignored run ledger — and is the evidence the
// similarity learner (koryph-qf6.6) and humans read. Deliberately NOT a
// model:<tier> routing label: observations and routing are separate
// vocabularies, and routing labels stay owned by humans and the learner.
// Best-effort, like the Close preceding it — a bd failure never blocks the
// loop.
func (r *runner) writeBackEscalatedMerge(ctx context.Context, phaseID string) {
	sl := r.run.Slots[phaseID]
	if !slotEscalated(sl) || sl.Model == "" {
		return
	}
	if err := r.adapter.AddLabel(ctx, phaseID, "model-observed:"+sl.Model); err != nil {
		r.progress("bead %s: model-observed label write-back failed: %v", phaseID, err)
		return
	}
	r.progress("bead %s: labeled model-observed:%s (merged after escalation)", phaseID, sl.Model)
}

// requeueSlot re-dispatches a slot: backoff, refresh the worktree onto current
// main, then the same dispatch flow with the branch HEAD as ResumeSHA and (when
// the manifest carries a session id and the worktree survives) a native session
// resume.
func (r *runner) requeueSlot(ctx context.Context, sl *ledger.Slot, reviewPath, why string) {
	// Before re-dispatching for any reason (agent death, gate-fail, review
	// bounce, merge error) re-validate that the bead is still open. The
	// operator may have closed or deferred it while the previous agent was
	// running; without this check the engine would waste a full re-dispatch on
	// a bead the operator has already retired (live repro koryph-pln).
	if r.beadClosedMidFlight(ctx, sl.PhaseID) {
		return
	}
	// Drain active: park, don't requeue (koryph-z0x, F1b) — drain must suppress
	// retries, not only fresh dispatch.
	if r.parkForDrain(ctx, sl) {
		return
	}
	// Run --budget exhausted: park instead of re-dispatching (koryph-u7q).
	if r.parkForRunBudget(ctx, sl) {
		return
	}
	// A resume re-dispatch is not a bead fault (the engine restarted, or a lower
	// width deferred the bead), so it must neither consume an attempt nor drive
	// the escalation ladder — track faults, not dispatches (D4). Every other
	// requeue reason IS a fault and increments as before.
	fault := why != resumeRequeueNote
	attempt := sl.Attempts
	if fault {
		attempt = sl.Attempts + 1
	}
	r.progress("bead %s: requeueing, attempt %d (%s)", sl.PhaseID, attempt, why)
	logSlotRequeue(sl.PhaseID, why, attempt)
	logRequeueEvent(r.run.RunID, r.opts.ProjectID, sl.PhaseID, why, attempt, sl.CostUSD)

	// In-run escalation (koryph-qf6.4): the FINAL attempt of a bead-fault
	// requeue runs on the recovery tier instead of burning the last attempt
	// on a model that has already failed twice — the one deliberate exception
	// to the koryph-ehx freeze (a policy decision recorded in the rationale,
	// not a drifting re-resolution; see resolveModel). Merge errors are
	// excluded (usually transient — the base moved, a push raced — not a
	// model-capability failure), as are the rate-limit and budget-kill paths,
	// which never reach this function. RecoveryModel resolves the selected
	// runtime's concrete frontier model and enforces the same fail-closed
	// policy as initial routing; the frozen-model path otherwise bypasses it.
	// A generic worker self-block is deliberately excluded: its retry exists to
	// correct the portable host-capability classification, not to retry coding
	// on a stronger tier.
	frozenModel, frozenWhy := sl.Model, sl.ModelWhy
	if fault && attempt >= ledger.MaxAttempts && why != mergeErrorRequeueNote && why != cleanNoCommitExitNote && why != genericCompletionBlockRequeueNote {
		runtimeName := sl.Runtime
		if runtimeName == "" {
			runtimeName = "claude"
		}
		modelMap := r.modelRequestForRuntime(modelroute.StageImplement, nil, "", runtimeName).ModelMap
		if up := modelroute.RecoveryModel(sl.Model, runtimeName, modelMap, r.rec.AllowedModels); up != "" {
			frozenModel = up
			frozenWhy = fmt.Sprintf("escalated from %s after %d bead-fault attempts (%s)", sl.Model, sl.Attempts, why)
			r.progress("bead %s: escalating final attempt %d to %s (was %s — %s)",
				sl.PhaseID, attempt, up, sl.Model, why)
			logModelEscalated(sl.PhaseID, sl.Model, up, attempt, why)
		}
	}

	r.backoffSleep(ctx, sl.Attempts)

	// Never re-run an agent against a checkout that predates a main-side fix
	// (koryph-137): rebuild a no-commit worktree from current main, or rebase
	// one with landed work onto the advanced base, before re-dispatch. This
	// path never preserves a no-commit worktree (that is reserved for a
	// budget-kill death — see requeueBudgetKilled, koryph-77r.10); the WIP
	// snapshot is still captured and threaded through so a rebuilt worktree's
	// resume can cite it.
	wipSnapshot := r.refreshWorktreeForRequeue(ctx, sl, false)

	resumeSession := ""
	if m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID); err == nil &&
		m.SessionID != "" && sl.Worktree != "" && fsx.Exists(sl.Worktree) {
		resumeSession = m.SessionID
	}

	r.dispatchBead(ctx, dispatchReq{
		issue:           r.issueFor(ctx, sl),
		epicID:          sl.EpicID,
		attempt:         attempt,
		resumeSHA:       r.branchHead(ctx, sl.Branch),
		resumeSessionID: resumeSession,
		reviewPath:      reviewPath,
		reviewIters:     sl.ReviewIters,
		wipSnapshotPath: wipSnapshot,
		// Carry ALL the requeue counters forward: dispatchBead builds a
		// brand-new ledger.Slot rather than mutating this one, so any counter
		// omitted here resets to zero on the new slot and its budget silently
		// refills (koryph-2im.6; conflict/rate-limit/budget-kill threading
		// added by koryph-qf6.1 — causes interleave, so each path preserves
		// the others' spent budgets, not just its own).
		gateRequeues:          sl.GateRequeues,
		mergeRequeues:         sl.MergeRequeues,
		conflictRequeues:      sl.ConflictRequeues,
		rateLimitRequeues:     sl.RateLimitRequeues,
		budgetKillRequeues:    sl.BudgetKillRequeues,
		turnExhaustedRequeues: sl.TurnExhaustedRequeues,
		note:                  why,
		// Freeze the model resolution from the first attempt (koryph-ehx): a
		// requeue re-runs the SAME model/persona/effort the bead was dispatched
		// with, so a `model:*` relabel mid-run (or non-deterministic
		// persona-tier resolution) cannot silently switch a retry to the wrong
		// model. Same freeze rationale as the footprint just below. The one
		// exception is the deliberate final-attempt escalation above
		// (koryph-qf6.4), which replaces the frozen tier with a recorded,
		// allowlist-checked policy decision — never a re-resolution.
		frozenModel:    frozenModel,
		frozenPersona:  sl.Agent,
		frozenModelWhy: frozenWhy,
		frozenEffort:   sl.Effort,
		// Carry the persisted footprint forward too (koryph-2im.3) — see
		// requeueRateLimited's identical comment.
		footprint: sl.Footprint,
		// Carry the frozen resource claim forward too (koryph-4ql.3) — see
		// requeueRateLimited's identical comment.
		resources: resourcesFromSlot(sl),
		// Carry the frozen similarity features forward too (koryph-qf6.3) —
		// see requeueRateLimited's identical comment.
		features: featuresFromSlot(sl),
		// Carry accumulated cost forward so the new slot starts from the total
		// spend so far (koryph-6bl): completeSlot ADDs the next attempt's cost
		// rather than overwriting, so the sum across all attempts is correct.
		accumulatedCostUSD: sl.CostUSD,
		// Carry accumulated token composition forward too (koryph-77r.1) —
		// same reasoning as accumulatedCostUSD.
		accumulatedTokens: dispatch.TokenUsage{
			InputTokens:         sl.InputTokens,
			OutputTokens:        sl.OutputTokens,
			CacheReadTokens:     sl.CacheReadTokens,
			CacheCreationTokens: sl.CacheCreationTokens,
		},
	})
}

// refreshWorktreeForRequeue makes a requeued slot's worktree reflect current
// main before re-dispatch, closing the stale-checkout gap (koryph-137): a
// worktree created before a main-side fix must not carry the old checkout
// across attempts, because dispatchBead's worktree.Ensure attaches to an
// existing tree rather than rebuilding it.
//
//   - No commits to preserve → by default, rebuild from the default branch:
//     snapshot any WIP for forensics, remove the worktree, and drop the
//     branch so Ensure recreates both from current main (a fresh checkout,
//     re-bootstrapped). preserveNoCommitWorktree skips the remove/drop
//     (koryph-77r.10, requeueBudgetKilled): a budget-kill death still has a
//     live session id worth resuming from, and rebuilding would destroy the
//     session-resume precondition (m.SessionID != "" &&
//     fsx.Exists(sl.Worktree)) before it could ever hold. The WIP snapshot is
//     still captured either way.
//   - Landed commits → rebase the branch onto the advanced base via Refresh
//     (Force, so the requeue always re-bases regardless of drift threshold or
//     footprint overlap) and resume on top of current main. Completely
//     unaffected by preserveNoCommitWorktree — that flag only matters in the
//     no-commit branch below.
//
// Every failure is non-fatal: the slot still re-dispatches onto whatever
// checkout survives, matching pre-137 behavior, with the reason logged.
//
// Returns the WIP snapshot patch path when one was captured in the no-commit
// branch (koryph-77r.10, so the caller can cite it in the RESUMING block via
// promptc.Input.WIPSnapshotPath — see compile.go); "" in every other case,
// including the commits>0 rebase branch and any failure path.
func (r *runner) refreshWorktreeForRequeue(ctx context.Context, sl *ledger.Slot, preserveNoCommitWorktree bool) string {
	if sl.Worktree == "" || sl.Branch == "" {
		return ""
	}

	commits := sl.Commits
	if commits == 0 {
		if n, err := r.commitCount(ctx, sl.Branch); err == nil {
			commits = n
		}
	}

	if commits > 0 {
		res, err := worktree.Refresh(ctx, worktree.RefreshOpts{
			RepoRoot: r.rec.Root,
			Path:     sl.Worktree,
			Branch:   sl.Branch,
			Base:     r.rec.DefaultBranch,
			Force:    true, // a requeue always re-bases onto current main
		})
		if err != nil {
			r.progress("bead %s: requeue refresh error (resuming on existing checkout): %v", sl.PhaseID, err)
			return ""
		}
		switch res.Action {
		case "refreshed":
			r.progress("bead %s: worktree rebased onto %s before requeue (was %d behind)",
				sl.PhaseID, r.rec.DefaultBranch, res.Behind)
		case "conflict":
			r.progress("bead %s: requeue rebase conflict — resuming on the un-rebased branch (see CONFLICT.md)", sl.PhaseID)
		case "deferred-dirty":
			r.progress("bead %s: requeue refresh deferred (worktree dirty) — resuming on existing checkout", sl.PhaseID)
		}
		return ""
	}

	// No landed work: snapshot WIP, then either rebuild from current main
	// (default) or preserve the worktree+branch as-is so a native session
	// resume can fire (preserveNoCommitWorktree).
	var wipPatch string
	if fsx.Exists(sl.Worktree) {
		if patch, err := worktree.PatchSnapshot(ctx, sl.Worktree, r.store.PhaseDir(r.run.RunID, sl.PhaseID)); err == nil && patch != "" {
			wipPatch = patch
			r.progress("bead %s: captured WIP snapshot before worktree rebuild: %s", sl.PhaseID, patch)
		}
		if preserveNoCommitWorktree {
			r.progress("bead %s: preserving worktree and branch (no commits, but a live session) for a warm resume", sl.PhaseID)
			return wipPatch
		}
		if err := worktree.Remove(ctx, sl.Worktree, true); err != nil {
			r.progress("bead %s: requeue worktree rebuild skipped, remove failed (dispatch will attach existing): %v",
				sl.PhaseID, err)
			return wipPatch
		}
	} else if preserveNoCommitWorktree {
		return wipPatch
	}
	// Drop the stale branch so Ensure recreates it from the default branch tip
	// rather than re-checking-out the old one. A "not found" error means the
	// operator already deleted the branch — that IS the clean state, so proceed
	// silently instead of warning about an absent branch.
	if err := worktree.DeleteBranch(ctx, r.rec.Root, sl.Branch); err != nil && !strings.Contains(err.Error(), "not found") {
		r.progress("bead %s: requeue branch reset skipped (%v) — dispatch may attach the old tip", sl.PhaseID, err)
	}
	return wipPatch
}

// slotLocker returns a bd-backed merge mutex when the project has a
// <project>-merge-slot bead, else nil (no cross-process locking).
func (r *runner) slotLocker(ctx context.Context) merge.SlotLocker {
	slotID := r.opts.ProjectID + "-merge-slot"
	if _, err := r.adapter.Show(ctx, slotID); err != nil {
		return nil
	}
	return &bdSlotLocker{runner: r, slotID: slotID}
}

// bdSlotLocker satisfies merge.SlotLocker over the beads adapter's
// claim/release lease on the merge-slot bead.
type bdSlotLocker struct {
	runner *runner
	slotID string
}

// Acquire claims the merge-slot bead (3 retries with backoff).
func (l *bdSlotLocker) Acquire(ctx context.Context, owner string) error {
	return l.runner.adapter.MergeSlotAcquire(ctx, l.slotID, owner, 3)
}

// Release reopens the merge-slot bead.
func (l *bdSlotLocker) Release(ctx context.Context) error {
	return l.runner.adapter.MergeSlotRelease(ctx, l.slotID)
}

// checkpointSlot refreshes the slot's manifest v2 (execution state + head
// commit + attempt), rebuilding a minimal manifest when none exists yet.
func (r *runner) checkpointSlot(sl *ledger.Slot, execState string) {
	cur := r.run.Slots[sl.PhaseID]
	if cur == nil {
		cur = sl
	}
	m, err := r.store.LoadManifest(r.run.RunID, sl.PhaseID)
	existed := err == nil
	if err != nil {
		m = &ledger.Manifest{
			ProjectID:       r.opts.ProjectID,
			BeadID:          cur.PhaseID,
			EpicID:          cur.EpicID,
			AccountProfile:  cur.AccountProfile,
			ClaudeConfigDir: cur.ClaudeConfigDir,
			SessionID:       cur.SessionID,
			SessionName:     cur.SessionName,
			Model:           cur.Model,
			WorktreePath:    cur.Worktree,
			Branch:          cur.Branch,
			BillingMode:     cur.BillingMode,
		}
	}
	// Skip the manifest read-modify-write when nothing recovery-relevant moved —
	// a quietly-running slot re-checkpoints identically every tick otherwise.
	if existed && m.ExecutionState == execState && m.HeadCommit == cur.LastCommit && m.Attempt == cur.Attempts {
		return
	}
	m.ExecutionState = execState
	m.HeadCommit = cur.LastCommit
	m.Attempt = cur.Attempts
	m.ModelActual = cur.ModelActual
	_ = r.store.SaveManifest(r.run.RunID, sl.PhaseID, m)
}

// resStuckMultiplier scales the silence threshold for slots that declared
// external resources (res:* labels, persisted on the slot — koryph-2rf):
// cluster bring-up, browser suites, and full-stack e2e journeys routinely
// exceed the default 900 s window while blocking the agent inside a single
// tool call, so flagging them at the plain threshold is mostly false alarm.
const resStuckMultiplier = 4

// stuckThreshold is the silence window for one slot: StuckSec, scaled by
// resStuckMultiplier when the slot holds declared resources (koryph-2rf).
func (r *runner) stuckThreshold(sl *ledger.Slot) time.Duration {
	threshold := time.Duration(r.opts.StuckSec) * time.Second
	if len(sl.Resources) > 0 {
		threshold *= resStuckMultiplier
	}
	return threshold
}

// isStuck reports whether an alive slot shows no sign of life — no stream
// event, agent-written heartbeat, commit, or cohort CPU activity — within its
// silence threshold. Re-derived from ground truth every poll tick (never
// latched), so a slot recovers from a transient stuck flag the moment it
// produces work again. Informational only — polling continues; koryph never
// interrupts a running agent.
func (r *runner) isStuck(ctx context.Context, sl *ledger.Slot) bool {
	// Stuck = no ground-truth activity within the slot's silence threshold,
	// re-derived from scratch every poll tick so a past silence can never latch
	// as a present-tense board label. slotActivityAt folds every signal into one
	// "last did real work" instant.
	return r.stuckFrom(sl, r.slotActivityAt(ctx, sl))
}

// stuckFrom is the pure stuck decision given a slot's most-recent activity
// instant (from slotActivityAt): silent past the threshold, or — when no signal
// is known yet — dispatched longer ago than the threshold. Split out so the poll
// tick derives activity ONCE and reuses it for both the status decision and the
// last_activity_at stamp, rather than paying the git probe twice.
func (r *runner) stuckFrom(sl *ledger.Slot, last time.Time) bool {
	if last.IsZero() {
		return r.sinceDispatched(sl) > r.stuckThreshold(sl)
	}
	return time.Since(last) > r.stuckThreshold(sl)
}

// slotActivityAt returns the most recent instant the slot showed real work —
// the max of every ground-truth signal so the newest one wins:
//
//   - stream.jsonl mtime — the agent emits a thinking/tool/result event; the
//     most direct and finest-grained sign of life, fresh through long operations
//     that write no status heartbeat and burn little leader CPU;
//   - status.json mtime — the agent-authored step heartbeat;
//   - cohort CPU last-advance (koryph-2rf implicit heartbeat);
//   - last commit time.
//
// Zero when none is known. Evaluated fresh each tick — nothing is remembered
// between calls — which is what lets a slot recover from a transient stuck flag
// the instant it produces work again, rather than staying "stuck" on the board
// after it has demonstrably resumed.
func (r *runner) slotActivityAt(ctx context.Context, sl *ledger.Slot) time.Time {
	var latest time.Time
	bump := func(t time.Time) {
		if t.After(latest) {
			latest = t
		}
	}
	if sl.Stream != "" {
		if fi, err := os.Stat(sl.Stream); err == nil {
			bump(fi.ModTime())
		}
	}
	if fi, err := os.Stat(sl.StatusPath); err == nil {
		bump(fi.ModTime())
	}
	if u := r.resUsage[sl.PhaseID]; u != nil && u.pid == sl.PID {
		bump(u.lastActiveAt)
	}
	if res, err := execx.Run(ctx, execx.Cmd{
		Dir: sl.Worktree, Name: "git", Args: []string{"log", "-1", "--format=%ct", "HEAD"},
	}); err == nil && res.ExitCode == 0 {
		if sec, perr := strconv.ParseInt(strings.TrimSpace(res.Stdout), 10, 64); perr == nil {
			bump(time.Unix(sec, 0))
		}
	}
	return latest
}

// sinceDispatched is the age of the slot's dispatch (zero when unparseable).
func (r *runner) sinceDispatched(sl *ledger.Slot) time.Duration {
	t, err := time.Parse(time.RFC3339, sl.DispatchedAt)
	if err != nil {
		return 0
	}
	return time.Since(t)
}

// branchProgress counts commits ahead of the default branch and resolves the
// short HEAD inside a worktree.
func (r *runner) branchProgress(ctx context.Context, wtPath string) (int, string, error) {
	res, err := execx.MustSucceed(ctx, execx.Cmd{
		Dir: wtPath, Name: "git",
		Args: []string{"rev-list", "--count", r.rec.DefaultBranch + "..HEAD"},
	})
	if err != nil {
		return 0, "", err
	}
	n, err := strconv.Atoi(strings.TrimSpace(res.Stdout))
	if err != nil {
		return 0, "", err
	}
	head := ""
	// LastCommit means the candidate's last commit, never the default branch's
	// pre-existing HEAD. Leaving it empty for n==0 prevents an unchanged base
	// from masquerading as merged work in the ledger and Bead close reason.
	if n > 0 {
		if hr, herr := execx.Run(ctx, execx.Cmd{
			Dir: wtPath, Name: "git", Args: []string{"rev-parse", "--short", "HEAD"},
		}); herr == nil && hr.ExitCode == 0 {
			head = strings.TrimSpace(hr.Stdout)
		}
	}
	return n, head, nil
}

// sizeClass buckets a bead for the cost estimator, defaulting to M when the
// issue body is not in memory (e.g. an adopted slot).
func (r *runner) sizeClass(beadID string) string {
	if iss, ok := r.issues[beadID]; ok {
		return quota.SizeOf(len(iss.Description))
	}
	return "M"
}

// tailOf returns the last n bytes of s.
func tailOf(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
