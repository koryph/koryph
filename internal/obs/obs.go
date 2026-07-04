// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package obs is koryph's observability foundation (§O1 of the design doc).
//
// It provides:
//
//   - A custom TRACE slog level (below Debug) and canonical level parsing.
//   - Per-component slog.Logger instances with independently-settable minimum
//     levels, all writing to a shared, swappable handler pipeline.
//   - A handler seam: ConsoleHandler (human-readable, INFO by default),
//     JSONHandler/TextHandler (stdlib wrappers with TRACE-aware level names),
//     and a MultiHandler fan-out — ready for an otelslog bridge in §O2.
//   - ~/.koryph/observability.json schema and a Watcher that reloads on
//     demand (no restart required). Env overrides: KORYPH_LOG_LEVEL,
//     KORYPH_LOG_FORMAT, KORYPH_OTEL_ENDPOINT.
//   - A central RedactingHandler that scrubs every log record. RedactAttr and
//     RedactValue are exported for use in tests that assert no secret reaches
//     a handler.
//   - Canonical attribute keys and helper functions: RunAttrs, BeadAttrs,
//     ForgeAttrs, Err, Phase.
//
// # Quick start
//
//	// At program startup (once):
//	cfg, _ := obs.LoadConfig()
//	obs.Init(cfg, obs.NewTextHandler(os.Stderr, cfg.ComponentLevel("engine")))
//
//	// Per package (package-level var):
//	var log = obs.For("engine")
//	log.Info("tick", obs.RunAttrs("run-123", "myproject")...)
//
//	// Dynamic reload at each scheduler tick:
//	obs.ReloadConfig()
//
// The OTEL SDK is not yet wired in §O1; the handler seam accepts any
// slog.Handler so otelslog.NewOtelHandler can be slotted in during §O2
// without touching call sites.
package obs
