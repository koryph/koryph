// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package quota is the per-account usage governor. Loop-mode policy:
// warn at 90%, slot-scale at 90–94% (linear scale-down of parallelism),
// graceful stop at 97% (no new dispatch; in-flight beads finish), hard stop
// at 99% (in-flight agents receive SIGTERM; worktrees preserved for resume).
// Manual dispatch is exempt from stops (still logged). All four thresholds
// are configurable per-account via Config.Ladder; zero fields use package
// defaults (DefaultWarnFraction etc.).
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
//   - State(u, cfg) Level (ok|warn|throttle|drain|stop) using the max of window
//     and weekly fractions.
//   - ScaleSlots(u, cfg, max) int — linear scale-down between throttle and
//     graceful_stop, min 1 below graceful_stop, 0 at/above graceful_stop.
//   - Preflight(u, waveEstimateUSD, cfg) (ok bool, reason string) — a loop
//     wave that would cross graceful_stop (97%) does not dispatch.
//   - EstimateWave / EstimateItem — per-tier base cost x size multiplier x
//     safety margin, EWMA-calibrated per tier from observed slot costs
//     (Record(tier, size, actualUSD)).
//
// Rolling-$ accounting (koryph-i3b.3, design docs/designs/2026-07-api-key-auth.md
// §7): the ladder above is shaped for subscription/oauth-token accounts, which
// have a real plan window to measure a percentage against. A pay-per-token
// (auth_mode=api-key) account has no such window — Window.Fraction() returns
// 1.0 when CeilingUSD<=0, which would read a pure-API account as permanently
// at stop, backwards. StateForAuthMode / PreflightForAuthMode /
// ScaleSlotsForAuthMode branch on the caller's auth_mode string (mirrored as
// AuthMode* below to avoid a quota->registry import cycle — registry already
// imports quota): subscription and oauth-token delegate to State/Preflight/
// ScaleSlots verbatim (byte-for-byte unchanged); api-key reads the same
// warn/throttle/graceful_stop/hard_stop ladder off
// spentUSD/Config.RollingCeilingUSD instead, where spentUSD is the caller's
// own tracked pay-per-token spend (settled total_cost_usd, projected run
// cost, etc — quota does not itself track it). RollingCeilingUSD==0 (not
// configured) is advisory (LevelOK, false), reusing the existing
// uncalibrated-is-advisory posture rather than a second escape hatch.
package quota

import "fmt"

// Auth-mode string values, mirrored from registry.AuthMode* (koryph-i3b.3).
// quota cannot import package registry (registry already imports quota, e.g.
// for SetCalibrationStaleAt — importing back would cycle), so callers pass
// registry.Record.EffectiveAuthMode()'s plain string return value directly;
// these constants exist so quota's own call sites and tests don't hardcode
// the literal strings a second time.
const (
	AuthModeSubscription = "subscription"
	AuthModeAPIKey       = "api-key"
	AuthModeOAuthToken   = "oauth-token"
)

// Level is the governor verdict.
type Level string

const (
	LevelOK       Level = "ok"
	LevelWarn     Level = "warn"     // >= DefaultWarnFraction
	LevelThrottle Level = "throttle" // >= DefaultThrottleFraction; slot scaling starts
	LevelDrain    Level = "drain"    // >= DefaultGracefulStopFraction; no new dispatch
	LevelStop     Level = "stop"     // >= DefaultHardStopFraction; interrupt in-flight
)

// Deprecated: use DefaultWarnFraction / DefaultThrottleFraction /
// DefaultGracefulStopFraction / DefaultHardStopFraction. These
// constants remain for any caller that has not yet migrated.
const (
	WarnFraction  = 0.80
	DrainFraction = 0.90
	StopFraction  = 0.95
)

// Default ladder thresholds (koryph-ivk). All configurable per-account via
// Config.Ladder; zero config fields fall back to these defaults.
const (
	DefaultWarnFraction         = 0.90
	DefaultThrottleFraction     = 0.94
	DefaultGracefulStopFraction = 0.97
	DefaultHardStopFraction     = 0.99
)

// Ladder holds the configurable governor thresholds for one account.
// All fields are fractions in (0,1]. Zero values use package defaults
// (see DefaultWarnFraction etc.). Fields must be strictly ascending.
//
// Ladder is embedded in Config.Ladder and re-read at every preflight
// (governor() re-calls LoadConfig each wave), so changes take effect
// without a restart.
type Ladder struct {
	Warn         float64 `json:"warn,omitempty"`
	Throttle     float64 `json:"throttle,omitempty"`
	GracefulStop float64 `json:"graceful_stop,omitempty"`
	HardStop     float64 `json:"hard_stop,omitempty"`
}

// Effective returns the ladder with zero fields filled in from package
// defaults. The receiver is never mutated.
func (l Ladder) Effective() Ladder {
	if l.Warn == 0 {
		l.Warn = DefaultWarnFraction
	}
	if l.Throttle == 0 {
		l.Throttle = DefaultThrottleFraction
	}
	if l.GracefulStop == 0 {
		l.GracefulStop = DefaultGracefulStopFraction
	}
	if l.HardStop == 0 {
		l.HardStop = DefaultHardStopFraction
	}
	return l
}

// Validate checks that effective thresholds are strictly ascending in (0,1].
// Returns nil when the ladder is valid (including the all-zero default ladder).
func (l Ladder) Validate() error {
	el := l.Effective()
	if el.Warn <= 0 || el.Warn > 1 {
		return fmt.Errorf("quota: ladder.warn %.4g out of (0,1]", el.Warn)
	}
	if el.Throttle <= el.Warn || el.Throttle > 1 {
		return fmt.Errorf("quota: ladder.throttle %.4g must be > warn %.4g and <= 1", el.Throttle, el.Warn)
	}
	if el.GracefulStop <= el.Throttle || el.GracefulStop > 1 {
		return fmt.Errorf("quota: ladder.graceful_stop %.4g must be > throttle %.4g and <= 1", el.GracefulStop, el.Throttle)
	}
	if el.HardStop <= el.GracefulStop || el.HardStop > 1 {
		return fmt.Errorf("quota: ladder.hard_stop %.4g must be > graceful_stop %.4g and <= 1", el.HardStop, el.GracefulStop)
	}
	return nil
}

// IsDefault reports whether the ladder is all-zero (using package defaults).
func (l Ladder) IsDefault() bool {
	return l.Warn == 0 && l.Throttle == 0 && l.GracefulStop == 0 && l.HardStop == 0
}

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

	// Ladder is the configurable governor threshold set for this account.
	// Zero fields fall back to package defaults (DefaultWarnFraction etc.).
	// Validated strictly ascending in (0,1] at load time; invalid values are
	// silently reset to zero (use defaults). Re-read at every wave preflight
	// without a restart.
	Ladder Ladder `json:"ladder,omitempty"`

	// CalibrationStale is set when the proxy config has changed since the last
	// calibration run (koryph-3l1.2). When true, the governor still operates
	// using the existing slope but the doctor emits a WARN and prompts a
	// `koryph quota calibrate` re-run, because the ccusage-USD vs /usage-%
	// slope is not proven invariant under a compression change (design §2 I1/I5,
	// §3 L5). Cleared by `koryph quota calibrate` on completion.
	CalibrationStale bool `json:"calibration_stale,omitempty"`
	// CalibrationStaleReason is the human-readable cause for CalibrationStale.
	CalibrationStaleReason string `json:"calibration_stale_reason,omitempty"`

	// RollingCeilingUSD is the per-account rolling-$ ceiling for pay-per-token
	// (auth_mode=api-key) accounts (koryph-i3b.3, design
	// docs/designs/2026-07-api-key-auth.md §7). See the package doc's
	// "Rolling-$ accounting" section for why this exists alongside
	// WindowCeilingUSD/WeeklyCeilingUSD instead of reusing them: an api-key
	// account has no subscription plan window to calibrate a percentage
	// against, so StateForAuthMode / PreflightForAuthMode /
	// ScaleSlotsForAuthMode read the ladder off spent-USD/RollingCeilingUSD
	// instead of Window.Fraction().
	//
	// 0 (the zero value) means "not configured": the api-key ladder is then
	// advisory only (LevelOK, false) — the same "uncalibrated, don't
	// deadlock" posture as an unconfigured subscription account. MVP scope
	// enforces spend via the existing per-run --budget and PerAgentMaxUSD
	// caps in the meantime (design §7 MVP note; a persisted rolling window
	// is a fast follow, design Open Questions #2).
	RollingCeilingUSD float64 `json:"rolling_ceiling_usd,omitempty"`

	// MaxThreads is this account's persisted DEFAULT concurrency-pool seed
	// (koryph-1o2.3): "koryph quota set-threads --account X N" writes it here,
	// alongside the rest of this account's already-per-account governor
	// config, rather than in the registry (registry.Record is a project-scoped
	// label shared across projects — a Record field would duplicate/conflict
	// across every project using this account).
	//
	// This is the SEED half of a two-tier cap: internal/engine resolves the
	// concurrency pool's effective cap with strict precedence — (1) an
	// explicit `governor set --account X` operator override on the account's
	// govern.Config.MaxGlobalAgents always wins; (2) else this MaxThreads seed;
	// (3) else the "anthropic" default pool's own cap, for migration
	// continuity from a pre-per-account-pools setup; (4) else
	// govern.DefaultMaxGlobalAgents. The two halves are deliberately NOT
	// collapsed into governor.json: a later change to this seed must not be
	// silently shadowed by (nor silently overwrite) a stale operator override
	// recorded there. govern must not import quota (layering) — the engine
	// wires this value into govern.Store.SeedCap so govern itself never reads
	// this field directly.
	//
	// 0 (the zero value, and every config predating koryph-1o2.3) means
	// "unset" — no seed; precedence falls through to level 3.
	MaxThreads int `json:"max_threads,omitempty"`
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
