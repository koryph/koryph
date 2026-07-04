// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package quota is the per-account usage governor. Loop-mode policy:
// warn at 80%, graceful drain at 90% (finish current beads, dispatch nothing
// new), hard stop at 95% (block dispatch, wait for the current turn — never
// interrupt mid-turn). Manual dispatch is exempt from stops (still logged).
//
// Usage sources (usage.go), in order:
//  1. ccusage CLI run with the account's CLAUDE_CONFIG_DIR in env
//     (`ccusage blocks --json --active` for the 5h window; `ccusage daily
//     --json` summed 7 days for the weekly window; npx fallback honoring
//     KORYPH_NO_NPX).
//  2. Local transcript scan: <configDir||~/.claude>/projects/*/*.jsonl,
//     fixed UTC 5h grid, approximate per-model pricing (Go port of
//     usage-window.py). Marked approximate in the snapshot.
//
// Any source failure → treat the window as AT ceiling (fail closed) and say
// so in the snapshot.
//
// Governor state persists at ~/.koryph/quota/<account>.json (calibration:
// ceilings are ccusage-USD proxies calibrated from /usage percentages — the
// user reads /usage; ceiling = observed$/observed%).
//
// Implementation contract (governor.go, usage.go, estimate.go):
//   - Snapshot(ctx, profile) (Usage, error)
//   - State(u, cfg) Level (ok|warn|drain|stop) using the max of window and
//     weekly fractions.
//   - ScaleSlots(u, cfg, max) int — linear scale-down between warn and stop,
//     min 1 below drain, 0 at/above drain.
//   - Preflight(u, waveEstimateUSD, cfg) (ok bool, reason string) — a loop
//     wave that would cross drain (90%) does not dispatch.
//   - EstimateWave / EstimateItem — per-tier base cost x size multiplier x
//     safety margin, EWMA-calibrated per tier from observed slot costs
//     (Record(tier, size, actualUSD)).
package quota

// Level is the governor verdict.
type Level string

const (
	LevelOK    Level = "ok"
	LevelWarn  Level = "warn"  // >= 0.80
	LevelDrain Level = "drain" // >= 0.90
	LevelStop  Level = "stop"  // >= 0.95
)

// Thresholds (fractions of the calibrated ceiling).
const (
	WarnFraction  = 0.80
	DrainFraction = 0.90
	StopFraction  = 0.95
)

// Window is one measured usage window.
type Window struct {
	Hours      int     `json:"hours"`
	SpentUSD   float64 `json:"spent_usd"`
	CeilingUSD float64 `json:"ceiling_usd"`
	Source     string  `json:"source"` // ccusage|jsonl-scan|unavailable
	Approx     bool    `json:"approx"`
}

// Fraction returns spent/ceiling (1.0 when unmeasurable — fail closed).
func (w Window) Fraction() float64 {
	if w.CeilingUSD <= 0 || w.Source == "unavailable" {
		return 1.0
	}
	return w.SpentUSD / w.CeilingUSD
}

// Usage is a per-account snapshot.
type Usage struct {
	Account  string `json:"account"`
	At       string `json:"at"`
	Window5h Window `json:"window_5h"`
	Weekly   Window `json:"weekly"`
}

// ConfigSchemaVersion is the current on-disk schema for the per-account quota
// config. Files without it (pre-versioning) still load and are backfilled.
const ConfigSchemaVersion = 1

// ErrorStat tracks per-(model, size-class) estimator accuracy across all
// observed dispatches (koryph-6bl). It is stored under Config.ErrorStats
// keyed by "<tier>:<size>" (the same key as Config.Calibration).
//
// Bias is the rolling mean of (actual/estimate) ratios — 1.0 means perfect,
// > 1.0 means the estimator is systematically under-estimating, < 1.0 means
// over-estimating. MAPE is the rolling mean absolute percentage error
// ((|actual-estimate|/estimate)*100). Both use the same 0.7/0.3 EWMA as
// the base calibration so recent observations carry more weight.
//
// Additive: absent from old configs (nil map → no-op in readers).
type ErrorStat struct {
	N    int     `json:"n"`    // total observations (not EWMA-decayed)
	Bias float64 `json:"bias"` // EWMA of actual/estimate ratio
	MAPE float64 `json:"mape"` // EWMA of |actual−estimate|/estimate * 100
}

// GuardModeOn is the default enforced billing-guard mode. GuardModeAdvisory
// and GuardModeOff both disable throttling (usage is still measured and
// logged). "off" is a synonym for "advisory" accepted by the CLI — both
// store as "advisory" after normalisation by SetGuardMode.
const (
	GuardModeOn       = "on"
	GuardModeAdvisory = "advisory"
	GuardModeOff      = "off"
)

// Config is per-account governor configuration + calibration state,
// persisted at ~/.koryph/quota/<account>.json.
type Config struct {
	SchemaVersion    int                   `json:"schema_version,omitempty"`
	Account          string                `json:"account"`
	WindowCeilingUSD float64               `json:"window_ceiling_usd"`
	WeeklyCeilingUSD float64               `json:"weekly_ceiling_usd"`
	PlanTier         string                `json:"plan_tier,omitempty"` // e.g. max20x, teams
	PerAgentMaxUSD   float64               `json:"per_agent_max_usd"`   // --max-budget-usd kill switch
	PerTierUSD       map[string]float64    `json:"per_tier_usd"`        // estimator base
	SizeMultiplier   map[string]float64    `json:"size_multiplier"`     // S/M/L
	SafetyMargin     float64               `json:"safety_margin"`
	Calibration      map[string]float64    `json:"calibration,omitempty"` // "<tier>:<size>" → EWMA USD
	ErrorStats       map[string]*ErrorStat `json:"error_stats,omitempty"` // "<tier>:<size>" → accuracy stats (koryph-6bl)

	// GuardMode is the live billing-guard toggle written by
	// `koryph quota guard`. "" or "on" = enforced (default).
	// "advisory"/"off" = guard advisory for this account: usage is still
	// measured and logged but never blocks dispatch. The engine re-reads
	// this field at every wave boundary via governor() → LoadConfig(),
	// so a toggle takes effect on the very next wave without a restart.
	// (koryph-i25)
	GuardMode string `json:"guard_mode,omitempty"`

	// GuardUntil is an optional RFC3339 timestamp set by
	// `koryph quota guard --until`. When non-empty the guard override
	// expires at that time and reverts to enforced automatically; the
	// engine treats an expired override identically to GuardMode="on".
	// (koryph-i25)
	GuardUntil string `json:"guard_until,omitempty"`
}

// DefaultConfig returns uncalibrated defaults for a new account profile.
// Equivalent to DefaultConfigForRuntime(account, "claude").
func DefaultConfig(account string) *Config {
	return DefaultConfigForRuntime(account, "claude")
}

// DefaultConfigForRuntime returns uncalibrated defaults for a new account
// profile dispatching under runtimeName (koryph-v8u.12): PerTierUSD seeds
// from that runtime's own tierUSDTables entry (see estimate.go) instead of
// always Anthropic's haiku/sonnet/opus/fable table, so onboarding a
// non-claude account's governor config starts from base prices at least
// shaped for that runtime's own tier vocabulary. For runtimeName == "claude"
// this is byte-for-byte DefaultConfig's pre-koryph-v8u.12 literal.
func DefaultConfigForRuntime(account, runtimeName string) *Config {
	table := tierUSDTableForRuntime(runtimeName)
	perTier := make(map[string]float64, len(table.perTier))
	for k, v := range table.perTier {
		perTier[k] = v
	}
	return &Config{
		SchemaVersion:  ConfigSchemaVersion,
		Account:        account,
		PerAgentMaxUSD: 25,
		PerTierUSD:     perTier,
		SizeMultiplier: map[string]float64{"S": 0.5, "M": 1.0, "L": 2.0},
		SafetyMargin:   1.5,
	}
}
