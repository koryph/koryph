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
