// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package engine obs.go -- structured logging helpers for the engine component.
//
// All engine progress events emit both a human-readable line (via
// runner.progress, which writes to opts.Out for console/test capture) AND a
// structured slog record via the helpers here. The two channels are byte-
// identical at INFO level so golden console tests are stable.
//
// Key naming follows the canonical obs attribute keys in internal/obs/attrs.go.
// Section O2 bead: engine + scheduler instrumentation.
package engine

import (
	"log/slog"

	"github.com/koryph/koryph/internal/obs"
)

// log is the package-level logger for the engine component. Safe to use at
// package-init time because obs.For performs lazy bootstrap.
var log = obs.For("engine")

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

// logRequeueEvent emits an INFO record for a requeue, including accumulated cost.
func logRequeueEvent(beadID, reason string, attempt int, accumulatedCostUSD float64) {
	log.Info("engine.slot.requeue_event",
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
