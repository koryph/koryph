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
	KeyPersona  = "persona"
	KeyPhase    = "phase"
	KeyError    = "error"

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
