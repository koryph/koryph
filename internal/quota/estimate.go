// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"log/slog"
	"math"
	"strings"
)

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

// tierUSDTable is one runtime's per-tier USD estimator-base table plus an
// explicit fallback base for a tier name the table has no entry for
// (koryph-v8u.12). It mirrors usage.go's runtimePriceTable shape
// (rules+fallback, koryph-v8u.3 item 4) at the coarser "$/dispatch"
// granularity EstimateItemForRuntime works at, rather than usage.go's
// per-MTok token pricing.
type tierUSDTable struct {
	perTier  map[string]float64
	fallback float64
}

// tierUSDTables namespaces the estimator's default per-tier USD base price
// by runtime name (koryph-v8u.12), mirroring how usage.go's pricingTables
// namespaces per-MTok pricing and internal/runtime namespaces ModelMap: each
// runtime's dispatched-model vocabulary is its own (a Codex tier is never
// named "sonnet"), so the governor's estimator seed must be looked up by
// runtime name too, not assumed to be Anthropic's haiku/sonnet/opus/fable
// forever. Only "claude" carries real numbers today — preserved byte-for-
// byte from the pre-koryph-v8u.12 hardcoded DefaultConfig literal and the
// unrecognized-tier cfg.PerTierUSD["sonnet"] fallback the estimator has
// always used — a future runtime adapter bead adds its own entry here
// exactly as usage.go's pricingTables documents for token-level pricing.
var tierUSDTables = map[string]tierUSDTable{
	"claude": {
		perTier:  map[string]float64{"haiku": 0.4, "sonnet": 3.0, "opus": 9.0, "fable": 15.0},
		fallback: 3.0, // sonnet rate: the unrecognized-tier fallback the estimator has always used
	},
}

// tierUSDTableForRuntime returns runtimeName's estimator-base table, falling
// back to claude's when runtimeName is unregistered here — mirroring
// priceForRuntime's graceful degrade (a cost ESTIMATE is advisory governor
// input, never a fail-closed dispatch gate, unlike modelroute.Resolve's
// deliberately fail-closed unknown-runtime error).
func tierUSDTableForRuntime(runtimeName string) tierUSDTable {
	if t, ok := tierUSDTables[runtimeName]; ok {
		return t
	}
	return tierUSDTables["claude"]
}

// EstimateItemForRuntime estimates the USD cost of one dispatch for the given
// runtime (koryph-v8u.12). A calibrated "<tier>:<size>" observation wins;
// otherwise it is the tier base times the size multiplier times the safety
// margin. When cfg.PerTierUSD has no entry for tier (an unrecognized or
// not-yet-configured tier name), the fallback base comes from runtimeName's
// OWN default table instead of a hardcoded claude-specific literal — so a
// non-claude runtime's unrecognized tier degrades to ITS OWN base price,
// never Anthropic's sonnet rate. For runtimeName=="claude" (or ""), the
// fallback is tierUSDTables["claude"].fallback (3.0). Calibration keys
// ("<tier>:<size>") are NOT runtime-namespaced (back-compat decision,
// koryph-v8u.12: only claude dispatches have ever recorded calibration, and
// Record's key shape is unchanged) — a calibrated observation always wins
// regardless of runtime. Equivalent to EstimateItemForRuntimeProxy(cfg,
// runtimeName, tier, size, "") — see that function for the proxy-identity
// segmentation seam (koryph-77r.1).
func EstimateItemForRuntime(cfg *Config, runtimeName, tier, size string) float64 {
	return EstimateItemForRuntimeProxy(cfg, runtimeName, tier, size, "")
}

// calibKey builds the Config.Calibration / Config.ErrorStats key for one
// (tier, size) observation, segmented by proxy identity (koryph-77r.1,
// design docs/designs/2026-07-token-economy.md §2 I5, §3 L1). proxyID==""
// is "direct" — the only identity that has ever existed (no agent_proxy is
// wired yet; see design §L5) — and yields the exact pre-existing "tier:size"
// key shape byte-for-byte, so every key ever persisted before this bead, and
// every key any pre-koryph-77r.1 caller writes, is unaffected: no migration,
// nothing orphaned. A future non-empty proxyID (stamped once koryph-3l1.1's
// agent_proxy seam lands) keys to "tier:size@proxyID" instead — a population
// Record/EstimateItemForRuntimeProxy deliberately never blend with the direct
// population's EWMA, so an experimental proxy's bias/MAPE readings can never
// contaminate the baseline calibration the governor already trusts (I5:
// "Estimator/calibration state is segmented by proxy identity so populations
// never pollute each other").
func calibKey(tier, size, proxyID string) string {
	if proxyID == "" {
		return tier + ":" + size
	}
	return tier + ":" + size + "@" + proxyID
}

// ParseCalibKey is calibKey's inverse: it splits a Config.Calibration/
// Config.ErrorStats key back into (tier, size, proxyID). Exported for the
// two display paths that read raw keys off cfg.Calibration/cfg.ErrorStats
// (koryph-3l1.3 carried contract from koryph-3l1.1's operator notes):
// cmd/koryph/quota.go's cmdMetricsEstimator and internal/cockpit/
// efficiency.go's splitBucket. Both previously assumed the legacy "tier:size"
// shape and parsed it themselves (one by scanning for the LAST ':', the
// other the first) — either approach corrupts on a "@proxyID" suffix once
// RecordForProxy starts writing non-empty proxyIDs, because proxyID is
// itself "<base_url>[#pin]" and a base_url like "http://127.0.0.1:8787"
// contains colons of its own. This is the one place that split is done
// correctly: tier and size are drawn from a closed, colon-free vocabulary
// (model tier names, S/M/L), so proxyID is stripped FIRST by finding the
// first '@' (never a valid tier/size character), and only the remainder is
// split on ITS first ':' — never last, and never on the (already-removed)
// proxyID's own colons.
func ParseCalibKey(key string) (tier, size, proxyID string) {
	ts := key
	if i := strings.IndexByte(key, '@'); i >= 0 {
		ts, proxyID = key[:i], key[i+1:]
	}
	if i := strings.IndexByte(ts, ':'); i >= 0 {
		return ts[:i], ts[i+1:], proxyID
	}
	return ts, "", proxyID
}

// EstimateItemForRuntimeProxy is EstimateItemForRuntime generalized by proxy
// identity (koryph-77r.1); see calibKey's doc for the segmentation contract.
func EstimateItemForRuntimeProxy(cfg *Config, runtimeName, tier, size, proxyID string) float64 {
	if cfg.Calibration != nil {
		if v, ok := cfg.Calibration[calibKey(tier, size, proxyID)]; ok {
			return v
		}
	}
	base, ok := cfg.PerTierUSD[tier]
	if !ok {
		base = tierUSDTableForRuntime(runtimeName).fallback
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

// EstimateWaveForRuntime sums the per-item cost estimates for a wave
// dispatched on the given runtime (koryph-v8u.12); see EstimateItemForRuntime.
func EstimateWaveForRuntime(cfg *Config, runtimeName string, items []struct{ Tier, Size string }) float64 {
	var total float64
	for _, it := range items {
		total += EstimateItemForRuntime(cfg, runtimeName, it.Tier, it.Size)
	}
	return total
}

// Record folds an observed dispatch cost into the per-"<tier>:<size>"
// calibration via an EWMA (0.7*old + 0.3*new), seeding with the first
// observation. estimateUSD is the dispatch-time estimate for this slot —
// when > 0 it is also folded into the per-key ErrorStats (bias + MAPE) for
// the bias-correction path (koryph-6bl). Pass 0 when the estimate is
// unknown (old ledger slots, requeues without a fresh estimate) to skip
// error-stat updates while still updating the base calibration. The caller
// is responsible for persisting cfg with SaveConfig. Equivalent to
// RecordForProxy(cfg, tier, size, "", actualUSD, estimateUSD) — see that
// function for the proxy-identity segmentation seam (koryph-77r.1).
func Record(cfg *Config, tier, size string, actualUSD, estimateUSD float64) {
	RecordForProxy(cfg, tier, size, "", actualUSD, estimateUSD)
}

// RecordForProxy is Record generalized by proxy identity (koryph-77r.1,
// design §2 I5): proxyID segments the Calibration/ErrorStats key (see
// calibKey) so a future agent-proxy experiment's observations never blend
// into the direct (proxyID=="") population Record itself still updates.
func RecordForProxy(cfg *Config, tier, size, proxyID string, actualUSD, estimateUSD float64) {
	if cfg.Calibration == nil {
		cfg.Calibration = map[string]float64{}
	}
	key := calibKey(tier, size, proxyID)
	if old, ok := cfg.Calibration[key]; ok {
		cfg.Calibration[key] = 0.7*old + 0.3*actualUSD
	} else {
		cfg.Calibration[key] = actualUSD
	}

	log.Debug("quota.calibration.update",
		slog.String("key", key),
		slog.Float64("actual_usd", actualUSD),
		slog.Float64("new_ewma", cfg.Calibration[key]),
	)

	// Update error statistics when both actual and a valid estimate are
	// available — estimateUSD == 0 means "unknown"; skip gracefully.
	if estimateUSD > 0 {
		if cfg.ErrorStats == nil {
			cfg.ErrorStats = map[string]*ErrorStat{}
		}
		// Winsorize this observation before folding: a near-zero estimate (or an
		// anomalous actual) yields an astronomical ratio/APE that, unbounded,
		// poisons the EWMA for every future estimate (see the bias-bound
		// constants). clampBias on the folded result additionally heals a
		// legacy-poisoned value loaded from disk on this first new observation.
		ratio := clampBias(actualUSD / estimateUSD)
		ape := math.Min(math.Abs(actualUSD-estimateUSD)/estimateUSD*100, maxObservedAPE)
		if es, ok := cfg.ErrorStats[key]; ok {
			es.N++
			es.Bias = clampBias(0.7*es.Bias + 0.3*ratio)
			es.MAPE = math.Min(0.7*es.MAPE+0.3*ape, maxObservedAPE)
		} else {
			cfg.ErrorStats[key] = &ErrorStat{N: 1, Bias: ratio, MAPE: ape}
		}
		if es, ok := cfg.ErrorStats[key]; ok {
			log.Debug("quota.estimate.bias",
				slog.String("key", key),
				slog.Int("n", es.N),
				slog.Float64("bias", es.Bias),
				slog.Float64("mape", es.MAPE),
			)
		}
	}
}

// BiasCorrectionThreshold is the minimum sample count before the learned
// bias factor is applied to the estimate. Below this threshold the estimate
// is uncorrected — there is not enough evidence to trust the ratio.
// Exported so the metrics/CLI layer can annotate rows that have reached it.
const BiasCorrectionThreshold = 5

// The bias factor is a slowly-varying multiplicative correction applied on top
// of an already-calibrated base estimate, so a healthy value sits close to 1.
// A value far outside a small band is not signal — it is the fallout of a
// degenerate observation (an actual recorded against a near-zero estimate makes
// actual/estimate astronomically large). Left unbounded, a single such
// observation drove ErrorStats.Bias to ~8.8M on a live account; the estimator
// then reported a ~$70M "wave estimate" that tripped the 5h governor's
// graceful-stop and killed a wave loop on a phantom cost. These bounds clamp
// the correction at three layers: the per-observation ratio folded into the
// EWMA (poison can't get in), the stored EWMA result (a legacy-poisoned value
// on disk self-heals on the next observation), and the applied factor (any
// not-yet-refolded legacy value is neutralized at read time). A correction on
// an already-calibrated base is never legitimately a 10x swing.
const (
	minBiasFactor = 0.1
	maxBiasFactor = 10.0
	// maxObservedAPE caps one observation's absolute-percentage error before it
	// folds into the MAPE EWMA — same near-zero-estimate blow-up as the ratio —
	// keeping the displayed confidence hint finite.
	maxObservedAPE = 1000.0
)

// clampBias bounds a bias factor to the sane band. Applied both to the stored
// EWMA (so a legacy-poisoned value heals) and to the factor at apply time (so
// a legacy value not yet refolded cannot produce a phantom estimate).
func clampBias(b float64) float64 {
	switch {
	case b < minBiasFactor:
		return minBiasFactor
	case b > maxBiasFactor:
		return maxBiasFactor
	default:
		return b
	}
}

// EstimateItemCorrected returns the bias-corrected and raw base estimates
// for one dispatch (koryph-6bl). Once ErrorStats[key].N >= 5, the returned
// corrected value is base * bias; below the threshold corrected == base.
// Equivalent to EstimateItemCorrectedForRuntime(cfg, "claude", tier, size).
func EstimateItemCorrected(cfg *Config, tier, size string) (corrected, base float64) {
	return EstimateItemCorrectedForRuntime(cfg, "claude", tier, size)
}

// EstimateItemCorrectedForRuntime is EstimateItemCorrected generalized
// across runtimes. It applies the learned bias factor once enough samples
// have accumulated, so systematic under/over-estimation self-corrects
// instead of persisting (koryph-6bl). When the bias-corrected path is
// active the returned base is the pre-correction value so callers can
// surface both ("est $1.65 base $1.20"). Equivalent to
// EstimateItemCorrectedForRuntimeProxy(cfg, runtimeName, tier, size, "") —
// see calibKey's doc for the proxy-identity segmentation seam (koryph-77r.1).
func EstimateItemCorrectedForRuntime(cfg *Config, runtimeName, tier, size string) (corrected, base float64) {
	return EstimateItemCorrectedForRuntimeProxy(cfg, runtimeName, tier, size, "")
}

// EstimateItemCorrectedForRuntimeProxy is EstimateItemCorrectedForRuntime
// generalized by proxy identity (koryph-77r.1); see calibKey's doc.
func EstimateItemCorrectedForRuntimeProxy(cfg *Config, runtimeName, tier, size, proxyID string) (corrected, base float64) {
	base = EstimateItemForRuntimeProxy(cfg, runtimeName, tier, size, proxyID)
	corrected = base
	if cfg.ErrorStats != nil {
		key := calibKey(tier, size, proxyID)
		if es, ok := cfg.ErrorStats[key]; ok && es.N >= BiasCorrectionThreshold {
			corrected = base * clampBias(es.Bias)
		}
	}
	return corrected, base
}

// EstimateWaveCorrected sums per-item corrected cost estimates for a wave.
// Equivalent to EstimateWaveCorrectedForRuntime(cfg, "claude", items).
func EstimateWaveCorrected(cfg *Config, items []struct{ Tier, Size string }) float64 {
	return EstimateWaveCorrectedForRuntime(cfg, "claude", items)
}

// EstimateWaveCorrectedForRuntime is EstimateWaveCorrected generalized
// across runtimes (koryph-6bl).
func EstimateWaveCorrectedForRuntime(cfg *Config, runtimeName string, items []struct{ Tier, Size string }) float64 {
	var total float64
	for _, it := range items {
		c, _ := EstimateItemCorrectedForRuntime(cfg, runtimeName, it.Tier, it.Size)
		total += c
	}
	return total
}
