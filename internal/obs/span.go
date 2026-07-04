// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"context"
	"log/slog"
	"time"
)

// Span records the start of a sub-operation and emits a structured log record
// when End is called. It is the §O4 placeholder for a full OTel span — when
// the OTEL SDK is wired in §O2, Span.End can forward to a real span.End()
// call without touching any instrumentation call site.
//
// Usage (forge API call):
//
//	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
//	    slog.String(obs.KeyProvider, "github"),
//	    slog.String(obs.KeyEndpointClass, "list_installations"),
//	)
//	resp, err := client.Do(req)
//	sp.End(resp.StatusCode, err)   // emits latency_ms, status, error if any
//
// Usage (vault resolution):
//
//	sp := obs.StartSpan(ctx, obs.For("vault"), slog.LevelDebug, "vault.resolve",
//	    slog.String(obs.KeyProvider, provider),
//	    slog.String(obs.KeyKeyRef, ref),  // reference only, NEVER the value
//	)
//	secret, err := doFetch(...)
//	sp.End(0, err)   // 0 = no HTTP status for vault operations
//
// Redaction contract: attrs passed to StartSpan MUST contain only safe,
// non-secret metadata: provider names, operation class names, key references.
// NEVER pass token values, PEM content, passwords, or response bodies.
type Span struct {
	ctx   context.Context
	start time.Time
	log   *slog.Logger
	level slog.Level
	msg   string
	attrs []slog.Attr
}

// StartSpan creates a Span whose timing starts now. ctx is forwarded to the
// underlying slog call for trace-context propagation (relevant once §O2 wires
// the OTEL SDK). attrs must contain ONLY safe metadata — see redaction contract
// on [Span].
func StartSpan(ctx context.Context, logger *slog.Logger, level slog.Level, msg string, attrs ...slog.Attr) *Span {
	return &Span{
		ctx:   ctx,
		start: time.Now(),
		log:   logger,
		level: level,
		msg:   msg,
		attrs: attrs,
	}
}

// End emits the span log record with latency_ms, an optional HTTP status code,
// and the error (if any). statusCode == 0 means no HTTP status (non-HTTP
// operation or a network-level failure before a response was received).
func (s *Span) End(statusCode int, err error) {
	ms := time.Since(s.start).Milliseconds()

	// Pre-allocate: base attrs + latency + optional status + optional error.
	attrs := make([]slog.Attr, 0, len(s.attrs)+3)
	attrs = append(attrs, s.attrs...)
	attrs = append(attrs, slog.Int64(KeyLatencyMS, ms))
	if statusCode > 0 {
		attrs = append(attrs, slog.Int(KeyStatus, statusCode))
	}
	if err != nil {
		attrs = append(attrs, Err(err))
	}

	s.log.LogAttrs(s.ctx, s.level, s.msg, attrs...)
}

// EndOK is a convenience wrapper for End(0, nil) — use when the operation
// succeeded and there is no HTTP status to record.
func (s *Span) EndOK() {
	s.End(0, nil)
}

// ReInitRaw replaces the package-level registry with one backed directly by
// root, WITHOUT the standard RedactingHandler wrapper. This must ONLY be
// called from tests that need to assert that instrumentation code never
// attempts to log secret-shaped values even before the safety net fires.
//
// cfg controls the minimum level gate for each component; pass a Config with
// DefaultLevel "debug" to capture DEBUG-level span events. Restore normal
// behaviour with [ReInit] after the test.
func ReInitRaw(cfg Config, root slog.Handler) {
	w := NewWatcher(cfg)
	reg := NewRegistry(root, w) // root stored directly — no RedactingHandler
	defaultMu.Lock()
	defaultRegistry = reg
	defaultMu.Unlock()
}
