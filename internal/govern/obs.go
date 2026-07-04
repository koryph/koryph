// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"log/slog"

	"github.com/koryph/koryph/internal/obs"
)

// log is the package-level logger for the govern component. It is safe to use
// at package init time because obs.For performs a lazy bootstrap.
var log = obs.For("govern")

// logGranted emits an INFO record when a slot is granted.
func logGranted(pool, project, bead string, cap, active int) {
	log.Info("govern.slot.granted",
		slog.String("pool", pool),
		slog.String("project", project),
		slog.String("bead", bead),
		slog.Int("cap", cap),
		slog.Int("active", active),
	)
}

// logDenied emits a DEBUG record when a slot is denied.
func logDenied(pool, project, bead string, cap, active int) {
	log.Debug("govern.slot.denied",
		slog.String("pool", pool),
		slog.String("project", project),
		slog.String("bead", bead),
		slog.Int("cap", cap),
		slog.Int("active", active),
	)
}

// logCapDecreased emits a WARN record when a rate-limit event decreases the cap.
func logCapDecreased(pool, project, bead string, oldCap, newCap int) {
	log.Warn("govern.aimd.cap.decreased",
		slog.String("pool", pool),
		slog.String("project", project),
		slog.String("bead", bead),
		slog.Int("old_cap", oldCap),
		slog.Int("new_cap", newCap),
	)
}

// logBreakerOpened emits a WARN record when the circuit breaker opens.
func logBreakerOpened(pool, project, bead string) {
	log.Warn("govern.breaker.opened",
		slog.String("pool", pool),
		slog.String("project", project),
		slog.String("bead", bead),
	)
}

// logBreakerClosed emits a WARN record when the circuit breaker closes
// after a successful probe.
func logBreakerClosed(pool string) {
	log.Warn("govern.breaker.closed",
		slog.String("pool", pool),
	)
}

// logCrashedProbeReopened emits a WARN record when pruneCrashedProbe
// re-opens a breaker because the probe timed out.
func logCrashedProbeReopened(pool string) {
	log.Warn("govern.breaker.crashed_probe.reopened",
		slog.String("pool", pool),
	)
}

// logBreakerHalfOpen emits an INFO record when a breaker is promoted from
// open to half-open.
func logBreakerHalfOpen(pool string) {
	log.Info("govern.breaker.half_open",
		slog.String("pool", pool),
	)
}

// logProbeAdvanced emits a DEBUG record when the AIMD additive probe advances
// the dynamic cap.
func logProbeAdvanced(pool string, oldCap, newCap int) {
	log.Debug("govern.aimd.probe.advanced",
		slog.String("pool", pool),
		slog.Int("old_cap", oldCap),
		slog.Int("new_cap", newCap),
	)
}
