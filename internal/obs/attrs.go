// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import "log/slog"

// Canonical attribute keys. Every structured log record, span, and metric
// emitted by koryph uses these key names so signals can be correlated.
const (
	KeyRunID    = "run_id"
	KeyProject  = "project"
	KeyBeadID   = "bead_id"
	KeyAttempt  = "attempt"
	KeyProvider = "provider"
	KeyModel    = "model"
	// KeyModelActual is the model that ACTUALLY served an attempt
	// (koryph-qf6.2, reduced from the result line's modelUsage), as opposed
	// to KeyModel — the tier dispatch requested. The two diverge when the
	// CLI's hardcoded --fallback-model downgrades a session mid-flight.
	KeyModelActual = "model_actual"
	KeyPersona     = "persona"
	KeyPhase       = "phase"
	KeyError       = "error"

	// §O4 client-span keys — forge.api and vault.resolve spans.

	// KeyEndpointClass is the conceptual operation name for a forge API call
	// (e.g. "list_installations", "mint_installation_token"). It is stable and
	// scrubbed of any path parameters that could carry secret material.
	KeyEndpointClass = "endpoint_class"

	// KeyLatencyMS is the wall-clock duration of a sub-operation in
	// milliseconds. Emitted on every forge.api and vault.resolve span event.
	KeyLatencyMS = "latency_ms"

	// KeyStatus is the HTTP response status code for forge API spans.
	// 0 means the call never received an HTTP response (network error, CLI call,
	// or a non-HTTP operation).
	KeyStatus = "status"

	// KeyKeyRef is the vault key reference — the provider-specific URI or path
	// that identifies WHERE the secret lives (e.g. "pass://share/item",
	// "koryph-bot-mybot"). It is NEVER the secret value itself.
	// Logging this key is safe; logging the resolved value is forbidden.
	KeyKeyRef = "key_ref"

	// KeyLifecycle is the bot lifecycle event label (e.g. "create.started",
	// "create.succeeded", "load", "save"). Used on bot.lifecycle log records.
	KeyLifecycle = "lifecycle_event"

	// Section O2 engine + scheduler instrumentation keys.

	// KeyStage is the pipeline stage name (e.g. "docs", "test") for stage
	// duration histograms and pipeline progress lines.
	KeyStage = "stage"

	// KeyCoDispatch is the number of slots co-dispatched in this wave/refill
	// (gauge). Emitted at each refill boundary to track parallelism.
	KeyCoDispatch = "co_dispatch"

	// KeyDeferralToken is the footprint token that caused a bead to be deferred
	// (e.g. "fp:core", "domain:unknown"). Used for deferrals_by_token metric.
	KeyDeferralToken = "deferral_token"

	// KeyDispatchedCount is the number of beads dispatched in a wave/refill.
	KeyDispatchedCount = "dispatched_count"

	// KeyReadyCount is the number of ready beads seen in a frontier scan.
	KeyReadyCount = "ready_count"

	// KeyWave is the wave or refill number within the current run.
	KeyWave = "wave"

	// KeyPID is the OS process id of a dispatched agent.
	KeyPID = "pid"

	// KeyCostUSD is the actual cost of a bead attempt in USD.
	KeyCostUSD = "cost_usd"

	// KeyEstimateUSD is the pre-dispatch bias-corrected cost estimate in USD.
	KeyEstimateUSD = "estimate_usd"

	// Section koryph-77r.1 token telemetry keys (design
	// docs/designs/2026-07-token-economy.md §3 L1).

	// KeyInputTokens/KeyOutputTokens/KeyCacheReadTokens/
	// KeyCacheCreationTokens are the per-attempt token composition parsed
	// from a stream-json result line's usage block (or the session-
	// transcript fallback).
	KeyInputTokens         = "input_tokens"
	KeyOutputTokens        = "output_tokens"
	KeyCacheReadTokens     = "cache_read_tokens"
	KeyCacheCreationTokens = "cache_creation_tokens"

	// KeyCacheRatio is cache_read / (input + cache_read + cache_creation) for
	// one attempt — the I7 cache-hit-ratio tripwire signal.
	KeyCacheRatio = "cache_ratio"

	// KeyTotalTokens is the attempt's total token volume
	// (input+cache_read+cache_creation) the cache-ratio tripwire gates on.
	KeyTotalTokens = "total_tokens"

	// KeyTurns is the number of completed agent turns an attempt ran, and
	// KeyTurnCeiling the per-bead ceiling it crossed (koryph-840 turn-exhausted
	// classification).
	KeyTurns       = "turns"
	KeyTurnCeiling = "turn_ceiling"
)

// RunAttrs returns slog attributes for a run context. Pass "" for any field
// that is not applicable at the call site.
func RunAttrs(runID, project string) []slog.Attr {
	var out []slog.Attr
	if runID != "" {
		out = append(out, slog.String(KeyRunID, runID))
	}
	if project != "" {
		out = append(out, slog.String(KeyProject, project))
	}
	return out
}

// BeadAttrs returns slog attributes for a bead/slot context.
func BeadAttrs(beadID string, attempt int) []slog.Attr {
	var out []slog.Attr
	if beadID != "" {
		out = append(out, slog.String(KeyBeadID, beadID))
	}
	if attempt > 0 {
		out = append(out, slog.Int(KeyAttempt, attempt))
	}
	return out
}

// ForgeAttrs returns slog attributes for a forge (LLM provider) context.
func ForgeAttrs(provider, model, persona string) []slog.Attr {
	var out []slog.Attr
	if provider != "" {
		out = append(out, slog.String(KeyProvider, provider))
	}
	if model != "" {
		out = append(out, slog.String(KeyModel, model))
	}
	if persona != "" {
		out = append(out, slog.String(KeyPersona, persona))
	}
	return out
}

// Err returns a slog.Attr for an error value, or an empty attr when err is nil.
func Err(err error) slog.Attr {
	if err == nil {
		return slog.Attr{}
	}
	return slog.String(KeyError, err.Error())
}

// Phase returns a slog.Attr for a scheduler phase label.
func Phase(phase string) slog.Attr {
	return slog.String(KeyPhase, phase)
}
