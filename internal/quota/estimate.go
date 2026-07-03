// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

// SizeOf buckets a work item by its description length: S (<400), M (<2000),
// otherwise L.
func SizeOf(descLen int) string {
	switch {
	case descLen < 400:
		return "S"
	case descLen < 2000:
		return "M"
	default:
		return "L"
	}
}

// EstimateItem estimates the USD cost of one dispatch. A calibrated
// "<tier>:<size>" observation wins; otherwise it is the tier base times the
// size multiplier times the safety margin. An unknown tier falls back to the
// sonnet base.
func EstimateItem(cfg *Config, tier, size string) float64 {
	if cfg.Calibration != nil {
		if v, ok := cfg.Calibration[tier+":"+size]; ok {
			return v
		}
	}
	base, ok := cfg.PerTierUSD[tier]
	if !ok {
		base = cfg.PerTierUSD["sonnet"]
	}
	mult, ok := cfg.SizeMultiplier[size]
	if !ok {
		mult = 1.0
	}
	margin := cfg.SafetyMargin
	if margin == 0 {
		margin = 1.0
	}
	return base * mult * margin
}

// EstimateWave sums the per-item estimates for a wave.
func EstimateWave(cfg *Config, items []struct{ Tier, Size string }) float64 {
	var total float64
	for _, it := range items {
		total += EstimateItem(cfg, it.Tier, it.Size)
	}
	return total
}

// Record folds an observed dispatch cost into the per-"<tier>:<size>"
// calibration via an EWMA (0.7*old + 0.3*new), seeding with the first
// observation. The caller is responsible for persisting cfg with SaveConfig.
func Record(cfg *Config, tier, size string, actualUSD float64) {
	if cfg.Calibration == nil {
		cfg.Calibration = map[string]float64{}
	}
	key := tier + ":" + size
	if old, ok := cfg.Calibration[key]; ok {
		cfg.Calibration[key] = 0.7*old + 0.3*actualUSD
	} else {
		cfg.Calibration[key] = actualUSD
	}
}
