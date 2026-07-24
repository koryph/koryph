// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package engine obs.go -- structured logging helpers for the engine component.
//
// Engine observability has two distinct channels that must not be conflated:
// runner.progress writes a human-readable line to opts.Out (the console/run
// log), while the helpers here emit structured, queryable slog records
// (engine.*) for log pipelines and cost rollups. progress does NOT also mirror
// its line into slog — doing so doubled every line once stdout and stderr were
// merged into one run log (D8); it falls back to slog only when there is no
// console sink at all (headless).
//
// Key naming follows the canonical obs attribute keys in internal/obs/attrs.go.
// Section O2 bead: engine + scheduler instrumentation.
package engine

import (
	"log/slog"

	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/phasecontrol"
)

// log is the package-level logger for the engine component. Safe to use at
// package-init time because obs.For performs lazy bootstrap.
var log = obs.For("engine")

// syncObsConfig re-reads ~/.koryph/observability.json so a mid-run
// `koryph obs level|enable|disable` takes effect on the NEXT scheduler tick
// without restarting the loop — the "no restart needed" contract the obs design
// (docs/designs/2026-07-observability.md §4) promised but nothing honored
// (obs.ReloadConfig had no caller). Called at the top of every wave/rolling
// iteration AND every pollUntilIdle tick (koryph-mes): a wave can sit inside
// pollUntilIdle for many minutes while its slots run, so syncing only once
// per wave (wave.go's call, before pollUntilIdle is entered) left a mid-wave
// level change waiting out the whole wave instead of landing on the next
// poll tick. Best-effort: a transient read/parse error leaves the previously
// loaded config in place (obs.ReloadConfig retains it on error).
func syncObsConfig() {
	syncObsConfigCalls++ // test-only counter; see TestPollUntilIdleSyncsObsConfigEachTick
	_, _ = obs.ReloadConfig()
}

// syncObsConfigCalls counts syncObsConfig invocations across the process
// lifetime. It exists solely so a test can assert the per-tick reload
// actually fires inside pollUntilIdle (not just once per wave/rolling
// iteration); production code never reads it.
var syncObsConfigCalls int

// logRunStart emits an INFO record when an engine run starts.
func logRunStart(runID, project, mode string) {
	log.Info("engine.run.start",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String("mode", mode),
	)
}

// logRunEnd emits an INFO record when an engine run ends.
func logRunEnd(runID, project, reason string, drained bool, dispatched, merged int) {
	log.Info("engine.run.end",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String("reason", reason),
		slog.Bool("drained", drained),
		slog.Int("dispatched", dispatched),
		slog.Int("merged", merged),
	)
}

// logRefillDispatched emits an INFO record after a wave/refill dispatches beads.
func logRefillDispatched(runID, project string, wave, dispatchedCount int) {
	log.Info("engine.refill.dispatched",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.Int(obs.KeyWave, wave),
		slog.Int(obs.KeyDispatchedCount, dispatchedCount),
	)
}

// logDeferral emits a DEBUG record for a deferred bead, carrying the token
// that caused the deferral. Used to drive deferrals_by_token metric.
func logDeferral(beadID, reason, token string) {
	log.Debug("engine.bead.deferred",
		slog.String(obs.KeyBeadID, beadID),
		slog.String("reason", reason),
		slog.String(obs.KeyDeferralToken, token),
	)
}

// logSlotDispatched emits an INFO record when a slot is successfully dispatched.
func logSlotDispatched(runID, project, beadID string, attempt int, model string, pid int) {
	log.Info("engine.slot.dispatched",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, beadID),
		slog.Int(obs.KeyAttempt, attempt),
		slog.String(obs.KeyModel, model),
		slog.Int(obs.KeyPID, pid),
	)
}

// logSlotRequeue emits an INFO record when a slot is requeued.
// Used to drive requeues_by_reason metric.
func logSlotRequeue(beadID, reason string, attempt int) {
	log.Info("engine.slot.requeued",
		slog.String(obs.KeyBeadID, beadID),
		slog.String("reason", reason),
		slog.Int(obs.KeyAttempt, attempt),
	)
}

// logRequeueEvent emits an INFO record for a requeue, including accumulated
// cost. run_id/project are carried so per-project cost rollups include
// requeued-attempt spend — without them, requeue cost is invisible to
// project-filtered accounting (unlike engine.slot.merged, which has always
// carried both), understating true run cost by ~20-30% (koryph-x5d).
func logRequeueEvent(runID, project, beadID, reason string, attempt int, accumulatedCostUSD float64) {
	log.Info("engine.slot.requeue_event",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, beadID),
		slog.String("reason", reason),
		slog.Int(obs.KeyAttempt, attempt),
		slog.Float64(obs.KeyCostUSD, accumulatedCostUSD),
	)
}

// logSlotMerged emits an INFO record when a slot is merged. model/modelActual/
// attempt (koryph-qf6.2) make the outcome event self-contained for per-attempt
// reconstruction from telemetry: without them, the durable JSONL recorded which
// model DISPATCHED (engine.slot.dispatched) but not which model the outcome
// belongs to.
func logSlotMerged(runID, project, beadID, sha string, costUSD float64, model, modelActual string, attempt int) {
	log.Info("engine.slot.merged",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, beadID),
		slog.String("sha", sha),
		slog.Float64(obs.KeyCostUSD, costUSD),
		slog.String(obs.KeyModel, model),
		slog.String(obs.KeyModelActual, modelActual),
		slog.Int(obs.KeyAttempt, attempt),
	)
}

// logEpicValidation emits the epic_validation lifecycle event (started,
// met-closed, met-pending-docs, gaps-filed, degraded, parked,
// closed-after-docs) — the TUI events tab and `koryph obs tail` consume it
// with no extra plumbing (design §3, koryph-wo0.4).
func logEpicValidation(runID, project, epicID string, round int, outcome string) {
	log.Info("engine.epic_validation",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, epicID),
		slog.Int("round", round),
		slog.String("outcome", outcome),
	)
}

// logSlotBlocked emits a WARN record when a slot is blocked — see
// logSlotMerged for why the outcome events carry model/modelActual/attempt
// (koryph-qf6.2). model/modelActual may be empty on a block that precedes any
// dispatch (e.g. worktree failure before a model was resolved).
func logSlotBlocked(beadID, reason, model, modelActual string, attempt int) {
	log.Warn("engine.slot.blocked",
		slog.String(obs.KeyBeadID, beadID),
		slog.String("reason", reason),
		slog.String(obs.KeyModel, model),
		slog.String(obs.KeyModelActual, modelActual),
		slog.Int(obs.KeyAttempt, attempt),
	)
}

// logPhaseRequest records a sanitized phase-control outcome. Rejected and
// failed operations are WARN so a zero-token watcher sees missing host
// capability without waiting for a coding-agent retry.
func logPhaseRequest(runID, project, beadID, requestID, operation, state, detail string) {
	attrs := []any{
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, beadID),
		slog.String("request_id", requestID),
		slog.String("operation", operation),
		slog.String("state", state),
		slog.String("detail", obs.RedactValue(detail)),
	}
	if state == phasecontrol.ResponseRejected || state == phasecontrol.ResponseFailed {
		log.Warn("engine.phase.request", attrs...)
		return
	}
	log.Info("engine.phase.request", attrs...)
}

func logCapabilityBlocked(runID, project, beadID, capability, detail, model string, attempt int) {
	log.Error("engine.slot.capability_blocked",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.String(obs.KeyBeadID, beadID),
		slog.String("capability", capability),
		slog.String("detail", obs.RedactValue(detail)),
		slog.String(obs.KeyModel, model),
		slog.Int(obs.KeyAttempt, attempt),
	)
}

// logModelFallback emits a WARN record when an attempt's actual model
// (result-line modelUsage) diverges from the requested tier (koryph-qf6.2) —
// the CLI's hardcoded --fallback-model silently downgraded the session.
func logModelFallback(beadID, requested, actual, rawID string) {
	log.Warn("engine.slot.model_fallback",
		slog.String(obs.KeyBeadID, beadID),
		slog.String(obs.KeyModel, requested),
		slog.String(obs.KeyModelActual, actual),
		slog.String("model_id", rawID),
	)
}

// logModelEscalated emits an INFO record when a bead's final attempt is
// escalated to the recovery tier (koryph-qf6.4). This is the durable
// escalation event the similarity learner (koryph-qf6.6) mines: from/to
// carry the tier transition, reason the bead-fault cause that triggered it.
func logModelEscalated(beadID, from, to string, attempt int, reason string) {
	log.Info("engine.slot.escalated",
		slog.String(obs.KeyBeadID, beadID),
		slog.String("from", from),
		slog.String("to", to),
		slog.Int(obs.KeyAttempt, attempt),
		slog.String("reason", reason),
	)
}

// logModelLearnApplied emits an INFO record when the wave-boundary learner
// stamps a learned model label onto a frontier bead (koryph-qf6.6).
func logModelLearnApplied(beadID, tier, area, size string) {
	log.Info("engine.bead.model_learned",
		slog.String(obs.KeyBeadID, beadID),
		slog.String(obs.KeyModel, tier),
		slog.String("area", area),
		slog.String("size", size),
	)
}

// logSlotConflict emits a WARN record when a slot hits a merge conflict.
func logSlotConflict(beadID, details string) {
	log.Warn("engine.slot.conflict",
		slog.String(obs.KeyBeadID, beadID),
		slog.String("details", details),
	)
}

// logCoDispatch emits a DEBUG record tracking the co-dispatch gauge at each
// refill boundary. active is the number of currently running slots; width is
// the effective concurrency ceiling for this wave/refill.
func logCoDispatch(runID, project string, wave, active, width int) {
	log.Debug("engine.co_dispatch",
		slog.String(obs.KeyRunID, runID),
		slog.String(obs.KeyProject, project),
		slog.Int(obs.KeyWave, wave),
		slog.Int(obs.KeyCoDispatch, active),
		slog.Int("width", width),
	)
}

// logStageDuration emits a DEBUG record for a pipeline stage duration.
// Used to populate stage duration histograms. err is nil on success.
func logStageDuration(beadID, stage string, ms int64, err error) {
	attrs := []any{
		slog.String(obs.KeyBeadID, beadID),
		slog.String(obs.KeyStage, stage),
		slog.Int64(obs.KeyLatencyMS, ms),
	}
	if err != nil {
		attrs = append(attrs, slog.String(obs.KeyError, err.Error()))
	}
	log.Debug("engine.stage.duration", attrs...)
}

// logBeadCost emits a DEBUG record with actual vs estimated cost for a bead
// attempt. Used to populate per-bead cost and estimator accuracy signals.
func logBeadCost(beadID, model string, costUSD, estimateUSD float64) {
	log.Debug("engine.bead.cost",
		slog.String(obs.KeyBeadID, beadID),
		slog.String(obs.KeyModel, model),
		slog.Float64(obs.KeyCostUSD, costUSD),
		slog.Float64(obs.KeyEstimateUSD, estimateUSD),
	)
}

// logBeadTokens emits a DEBUG record with one attempt's token composition
// (koryph-77r.1, design docs/designs/2026-07-token-economy.md §3 L1). Used to
// populate per-bead token-composition and cache-hit-ratio signals.
func logBeadTokens(beadID string, input, output, cacheRead, cacheCreation int64) {
	log.Debug("engine.bead.tokens",
		slog.String(obs.KeyBeadID, beadID),
		slog.Int64(obs.KeyInputTokens, input),
		slog.Int64(obs.KeyOutputTokens, output),
		slog.Int64(obs.KeyCacheReadTokens, cacheRead),
		slog.Int64(obs.KeyCacheCreationTokens, cacheCreation),
	)
}

// logBeadTokensUnavailable emits a DEBUG record when neither the stream-json
// result line nor the session-transcript fallback yielded a token
// composition for a bead attempt (koryph-77r.1) — the slot's token fields
// stay at their prior (possibly zero) value.
func logBeadTokensUnavailable(beadID string) {
	log.Debug("engine.bead.tokens_unavailable", slog.String(obs.KeyBeadID, beadID))
}

// logBudgetKilled emits a WARN record when an attempt is classified as
// killed by --max-budget-usd (koryph-77r.10, design docs/designs/2026-07-
// token-economy.md recovery-economics follow-up). costUSD is the slot's
// ACCUMULATED cost across all attempts so far (including this one) — the
// AC2 requirement — so a dashboard can total real dollars burned by
// budget-kills per bead without re-deriving it from per-attempt deltas.
func logBudgetKilled(beadID string, attempt int, costUSD float64, model, modelActual string) {
	log.Warn("engine.slot.budget_killed",
		slog.String(obs.KeyBeadID, beadID),
		slog.Int(obs.KeyAttempt, attempt),
		slog.Float64(obs.KeyCostUSD, costUSD),
		slog.String(obs.KeyModel, model),
		slog.String(obs.KeyModelActual, modelActual),
	)
}

// logTurnExhausted emits a WARN record when enforceTurnCeiling interrupts an
// attempt for running past the per-bead turn ceiling (koryph-840). turns is
// the completed-turn count observed, ceiling the configured limit it crossed;
// attempt and model mirror logBudgetKilled so the two runaway-defense events
// share a dashboard shape.
func logTurnExhausted(beadID string, turns, ceiling, attempt int, model, modelActual string) {
	log.Warn("engine.slot.turn_exhausted",
		slog.String(obs.KeyBeadID, beadID),
		slog.Int(obs.KeyTurns, turns),
		slog.Int(obs.KeyTurnCeiling, ceiling),
		slog.Int(obs.KeyAttempt, attempt),
		slog.String(obs.KeyModel, model),
		slog.String(obs.KeyModelActual, modelActual),
	)
}

// logCacheRatioTripwire emits a WARN record when one attempt's cache_read
// share collapses below cacheRatioFloor on a session with material token
// volume (koryph-77r.1, design §2 I7): the quota-multiplier failure
// signature — a nondeterministic transform busting the cached prefix and
// converting 90%-discounted cache reads into 1.25x-cost cache writes. This
// is observability only; it never changes dispatch behavior.
func logCacheRatioTripwire(beadID string, ratio float64, totalTokens int64) {
	log.Warn("engine.bead.cache_ratio_tripwire",
		slog.String(obs.KeyBeadID, beadID),
		slog.Float64(obs.KeyCacheRatio, ratio),
		slog.Int64(obs.KeyTotalTokens, totalTokens),
	)
}
