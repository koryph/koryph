// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package merge

import (
	"context"
	"log/slog"
	"time"

	"github.com/koryph/koryph/internal/obs"
)

// log is the package-level logger for the merge component. It is safe to use
// at package init time because obs.For performs a lazy bootstrap.
var log = obs.For("merge")

// logResult emits ONE structured "merge.result" event per Merge call,
// regardless of which of mergeInner's many exit paths produced the Result —
// see the Merge doc comment for why the wrapper, not each call site, is the
// instrumentation point. The level/attrs computation lives in
// resultLogAttrs so it is unit-testable without going through the obs
// registry (the package-level log var binds to whatever registry existed at
// this package's init time — before any test gets a chance to swap it in —
// so asserting on captured log OUTPUT from a test is not reliable here; see
// obs_test.go).
func logResult(o Opts, res Result, err error, dur time.Duration) {
	level, attrs := resultLogAttrs(o, res, err, dur)
	log.LogAttrs(context.Background(), level, "merge.result", attrs...)
}

// resultLogAttrs computes the level and attrs for one merge.result event.
// Attrs are kept small and safe: the branch name, the outcome status,
// latency, and counts — never gate/rebase output, which can carry raw
// subprocess stderr (the engine already persists that in full to
// <phase-dir>/gate-output.log via auditBlocked, and callers that skip the
// engine, like `koryph merge`, still get status+error here without risking a
// giant or secret-shaped log line).
func resultLogAttrs(o Opts, res Result, err error, dur time.Duration) (slog.Level, []slog.Attr) {
	attrs := make([]slog.Attr, 0, 8)
	attrs = append(attrs,
		slog.String("branch", o.Branch),
		slog.String("status", string(res.Status)),
		slog.Int64(obs.KeyLatencyMS, dur.Milliseconds()),
	)
	if res.MergedSHA != "" {
		attrs = append(attrs, slog.String("sha", res.MergedSHA))
	}
	if len(res.Protected) > 0 {
		attrs = append(attrs, slog.Int("protected_count", len(res.Protected)))
	}
	if len(res.Reconciled) > 0 {
		attrs = append(attrs, slog.Int("reconciled_count", len(res.Reconciled)))
	}
	if err != nil {
		attrs = append(attrs, obs.Err(err))
	}

	level := slog.LevelInfo
	switch res.Status {
	case StatusMerged, StatusPROpened:
		// success — INFO, unless the wrapper also carries a non-nil err (a
		// merge that landed but then failed to push, for instance).
		if err != nil {
			level = slog.LevelWarn
		}
	default:
		level = slog.LevelWarn
	}

	return level, attrs
}
