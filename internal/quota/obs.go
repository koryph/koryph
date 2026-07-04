// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"log/slog"

	"github.com/koryph/koryph/internal/obs"
)

// log is the package-level logger for the quota component. It is safe to use
// at package init time because obs.For performs a lazy bootstrap.
var log = obs.For("quota")

// LogUsage logs the current usage snapshot at INFO level: 5h window burn
// fraction, weekly window burn fraction, and the current governor level.
func LogUsage(u Usage, cfg *Config) {
	level, calibrated := State(u, cfg)

	log.Info("quota.usage.snapshot",
		slog.String("account", u.Account),
		slog.Float64("window_5h_fraction", u.Window5h.Fraction()),
		slog.Float64("window_5h_spent_usd", u.Window5h.SpentUSD),
		slog.Float64("window_5h_ceiling_usd", u.Window5h.CeilingUSD),
		slog.String("window_5h_source", u.Window5h.Source),
		slog.Float64("weekly_fraction", u.Weekly.Fraction()),
		slog.Float64("weekly_spent_usd", u.Weekly.SpentUSD),
		slog.Float64("weekly_ceiling_usd", u.Weekly.CeilingUSD),
		slog.String("weekly_source", u.Weekly.Source),
		slog.String("governor_level", string(level)),
		slog.Bool("calibrated", calibrated),
	)
}
