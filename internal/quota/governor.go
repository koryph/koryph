// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"fmt"
	"math"
	"path/filepath"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// configPath returns the on-disk location for an account's governor state.
func configPath(account string) string {
	return filepath.Join(paths.QuotaDir(), account+".json")
}

// LoadConfig reads the persisted governor config for account. A missing file
// yields an uncalibrated DefaultConfig (ceilings 0) rather than an error, so a
// fresh install is usable. Estimator fields absent from a hand-edited file are
// backfilled from the defaults; ceilings and calibration are preserved.
func LoadConfig(account string) (*Config, error) {
	path := configPath(account)
	if !fsx.Exists(path) {
		return DefaultConfig(account), nil
	}
	var cfg Config
	if err := fsx.ReadJSON(path, &cfg); err != nil {
		return nil, err
	}
	if cfg.Account == "" {
		cfg.Account = account
	}
	def := DefaultConfig(account)
	if cfg.PerTierUSD == nil {
		cfg.PerTierUSD = def.PerTierUSD
	}
	if cfg.SizeMultiplier == nil {
		cfg.SizeMultiplier = def.SizeMultiplier
	}
	if cfg.SafetyMargin == 0 {
		cfg.SafetyMargin = def.SafetyMargin
	}
	if cfg.PerAgentMaxUSD == 0 {
		cfg.PerAgentMaxUSD = def.PerAgentMaxUSD
	}
	return &cfg, nil
}

// SaveConfig writes cfg atomically to ~/.koryph/quota/<account>.json.
func SaveConfig(cfg *Config) error {
	return fsx.WriteJSONAtomic(configPath(cfg.Account), cfg)
}

// isCalibrated reports whether at least one ceiling has been calibrated (from
// either the live usage windows or the config). Both-zero == never calibrated.
func isCalibrated(u Usage, cfg *Config) bool {
	if cfg != nil && (cfg.WindowCeilingUSD > 0 || cfg.WeeklyCeilingUSD > 0) {
		return true
	}
	return u.Window5h.CeilingUSD > 0 || u.Weekly.CeilingUSD > 0
}

// maxFraction is the worse of the two window fractions (fail-closed windows
// report 1.0).
func maxFraction(u Usage) float64 {
	return math.Max(u.Window5h.Fraction(), u.Weekly.Fraction())
}

// levelFor maps a fraction of the ceiling onto a governor level.
func levelFor(frac float64) Level {
	switch {
	case frac >= StopFraction:
		return LevelStop
	case frac >= DrainFraction:
		return LevelDrain
	case frac >= WarnFraction:
		return LevelWarn
	default:
		return LevelOK
	}
}

// State returns the governor verdict for a usage snapshot: the worse of the 5h
// and weekly fractions against the warn/drain/stop thresholds. A window with an
// unmeasured or unavailable source reports Fraction 1.0 and therefore stops
// (fail closed).
//
// Special case: when the account has never been calibrated (both ceilings 0),
// State returns (LevelOK, false) so a fresh install is not deadlocked. The bool
// is the "calibrated" flag — false means the verdict is advisory only.
func State(u Usage, cfg *Config) (Level, bool) {
	if !isCalibrated(u, cfg) {
		return LevelOK, false
	}
	return levelFor(maxFraction(u)), true
}

// ScaleSlots scales a desired parallelism (max) down as usage climbs: full max
// at or below the warn fraction, linearly interpolated max→1 across
// [Warn, Drain), and 0 at or above the drain fraction. The result never dips
// below 1 while below drain.
func ScaleSlots(u Usage, max int) int {
	if max <= 0 {
		return 0
	}
	frac := maxFraction(u)
	if frac >= DrainFraction {
		return 0
	}
	if frac <= WarnFraction {
		return max
	}
	// t in (0,1): 0 at Warn, →1 approaching Drain.
	t := (frac - WarnFraction) / (DrainFraction - WarnFraction)
	scaled := float64(max) - t*(float64(max)-1.0)
	n := int(math.Round(scaled))
	if n < 1 {
		n = 1
	}
	return n
}

// Preflight decides whether a loop wave with an estimated cost may dispatch. It
// projects (spent+estimate)/ceiling for the 5h window; a wave that would cross
// the drain fraction is refused with a precise reason. An uncalibrated account
// is always allowed with reason "uncalibrated" (advisory only). An unavailable
// window fails closed.
func Preflight(u Usage, waveEstimateUSD float64, cfg *Config) (ok bool, reason string) {
	if !isCalibrated(u, cfg) {
		return true, "uncalibrated: governor advisory only"
	}
	w := u.Window5h
	ceiling := w.CeilingUSD
	if ceiling <= 0 && cfg != nil {
		ceiling = cfg.WindowCeilingUSD
	}
	if ceiling <= 0 {
		return false, "5h window ceiling uncalibrated — failing closed"
	}
	if w.Source == "unavailable" {
		return false, "5h usage unavailable — failing closed"
	}
	projected := (w.SpentUSD + waveEstimateUSD) / ceiling
	if projected >= DrainFraction {
		return false, fmt.Sprintf(
			"wave ($%.2f est) would put the 5h window at %.0f%% ($%.2f/$%.2f) — crosses drain (%.0f%%)",
			waveEstimateUSD, projected*100, w.SpentUSD+waveEstimateUSD, ceiling, DrainFraction*100)
	}
	return true, fmt.Sprintf(
		"5h window projected %.0f%% after wave (drain at %.0f%%)",
		projected*100, DrainFraction*100)
}
