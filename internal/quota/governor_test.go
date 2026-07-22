// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"math"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/schemaver"
)

// TestConfigSchemaVersionMatchesSchemaver keeps quota's own stamp constant in
// lockstep with the schemaver registry that LoadConfig's CheckRead guards
// against. If they drift, LoadConfig would refuse a config this build just
// wrote (or fail to guard a truly-newer one).
func TestConfigSchemaVersionMatchesSchemaver(t *testing.T) {
	if ConfigSchemaVersion != schemaver.Current(schemaver.Quota) {
		t.Fatalf("quota.ConfigSchemaVersion=%d but schemaver.Current(Quota)=%d — bump both together",
			ConfigSchemaVersion, schemaver.Current(schemaver.Quota))
	}
}

// win builds a measured Window.
func win(hours int, spent, ceiling float64, source string) Window {
	return Window{Hours: hours, SpentUSD: spent, CeilingUSD: ceiling, Source: source}
}

// usage assembles a Usage from a 5h and a weekly window.
func usage(w5, weekly Window) Usage {
	return Usage{Account: "acct", Window5h: w5, Weekly: weekly}
}

// calibrated is a config with real ceilings.
func calibratedCfg() *Config {
	c := DefaultConfig("acct")
	c.WindowCeilingUSD = 100
	c.WeeklyCeilingUSD = 1000
	return c
}

func TestConfigRoundtrip(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())

	// Missing file → uncalibrated default.
	got, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got.WindowCeilingUSD != 0 || got.WeeklyCeilingUSD != 0 {
		t.Fatalf("fresh config should be uncalibrated, got %+v", got)
	}
	if got.PerAgentMaxUSD != 25 {
		t.Fatalf("default PerAgentMaxUSD = %g, want 25", got.PerAgentMaxUSD)
	}

	got.WindowCeilingUSD = 123.5
	got.Calibration = map[string]float64{"opus:L": 9.9}
	if err := SaveConfig(got); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	got2, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig 2: %v", err)
	}
	if got2.WindowCeilingUSD != 123.5 {
		t.Fatalf("roundtrip ceiling = %g, want 123.5", got2.WindowCeilingUSD)
	}
	if got2.Calibration["opus:L"] != 9.9 {
		t.Fatalf("roundtrip calibration = %v", got2.Calibration)
	}
}

// TestSetMaxThreads covers koryph-1o2.3's per-account concurrency-pool seed
// setter: setting, clearing (0), and negative-rejects-to-zero, all under the
// exclusive per-account flock (via UpdateConfig), and that it persists.
func TestSetMaxThreads(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())

	got, err := SetMaxThreads("acct", 6)
	if err != nil {
		t.Fatalf("SetMaxThreads(6): %v", err)
	}
	if got.MaxThreads != 6 {
		t.Fatalf("MaxThreads = %d, want 6", got.MaxThreads)
	}

	reloaded, err := LoadConfig("acct")
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if reloaded.MaxThreads != 6 {
		t.Fatalf("reloaded MaxThreads = %d, want 6", reloaded.MaxThreads)
	}

	// Clearing with 0 reverts to "unset".
	got, err = SetMaxThreads("acct", 0)
	if err != nil {
		t.Fatalf("SetMaxThreads(0): %v", err)
	}
	if got.MaxThreads != 0 {
		t.Fatalf("MaxThreads after clear = %d, want 0", got.MaxThreads)
	}

	// A negative value clamps to 0 rather than persisting garbage.
	got, err = SetMaxThreads("acct", -5)
	if err != nil {
		t.Fatalf("SetMaxThreads(-5): %v", err)
	}
	if got.MaxThreads != 0 {
		t.Fatalf("MaxThreads after negative set = %d, want clamped to 0", got.MaxThreads)
	}
}

func TestState(t *testing.T) {
	cal := calibratedCfg()
	uncal := DefaultConfig("acct") // ceilings 0

	cases := []struct {
		name       string
		u          Usage
		cfg        *Config
		wantLevel  Level
		wantCalibd bool
	}{
		{
			name:       "uncalibrated is advisory ok",
			u:          usage(win(5, 0, 0, ""), win(168, 0, 0, "")),
			cfg:        uncal,
			wantLevel:  LevelOK,
			wantCalibd: false,
		},
		{
			name:       "healthy",
			u:          usage(win(5, 10, 100, "ccusage"), win(168, 50, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelOK,
			wantCalibd: true,
		},
		{
			name:       "warn at 0.90",
			u:          usage(win(5, 90, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelWarn,
			wantCalibd: true,
		},
		{
			name:       "throttle at 0.94",
			u:          usage(win(5, 94, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelThrottle,
			wantCalibd: true,
		},
		{
			name:       "drain at 0.97",
			u:          usage(win(5, 97, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelDrain,
			wantCalibd: true,
		},
		{
			name:       "stop at 0.99",
			u:          usage(win(5, 99, 100, "ccusage"), win(168, 0, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelStop,
			wantCalibd: true,
		},
		{
			name:       "weekly dominates at throttle",
			u:          usage(win(5, 10, 100, "ccusage"), win(168, 950, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelThrottle,
			wantCalibd: true,
		},
		{
			name:       "unavailable fails closed when calibrated",
			u:          usage(win(5, 0, 100, "unavailable"), win(168, 10, 1000, "ccusage")),
			cfg:        cal,
			wantLevel:  LevelStop,
			wantCalibd: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, calibd := State(tc.u, tc.cfg)
			if lvl != tc.wantLevel || calibd != tc.wantCalibd {
				t.Fatalf("State = (%s, %v), want (%s, %v)", lvl, calibd, tc.wantLevel, tc.wantCalibd)
			}
		})
	}
}

func TestScaleSlots(t *testing.T) {
	const max = 9
	cases := []struct {
		frac float64
		want int
	}{
		{0.93, max}, // below throttle → full
		{0.94, max}, // at throttle → full
		{0.955, 5},  // midpoint of [0.94, 0.97): 9 - 0.5*(9-1) = 5
		{0.969, 1},  // near graceful_stop → clamps to 1
		{0.97, 0},   // at graceful_stop → 0
		{0.99, 0},   // above graceful_stop → 0
	}
	for _, tc := range cases {
		// window carries the fraction; weekly is idle.
		u := usage(win(5, tc.frac*100, 100, "ccusage"), win(168, 0, 1000, "ccusage"))
		if got := ScaleSlots(u, nil, max); got != tc.want {
			t.Fatalf("ScaleSlots(frac=%.3f) = %d, want %d", tc.frac, got, tc.want)
		}
	}
}

func TestScaleSlotsCustomLadder(t *testing.T) {
	const max = 9
	cfg := DefaultConfig("acct")
	cfg.Ladder = Ladder{Throttle: 0.80, GracefulStop: 0.90}
	cases := []struct {
		frac float64
		want int
	}{
		{0.79, max}, // below throttle → full
		{0.80, max}, // at throttle → full
		{0.85, 5},   // midpoint → scaled
		{0.895, 1},  // near graceful_stop → 1
		{0.90, 0},   // at graceful_stop → 0
	}
	for _, tc := range cases {
		u := usage(win(5, tc.frac*100, 100, "ccusage"), win(168, 0, 1000, "ccusage"))
		if got := ScaleSlots(u, cfg, max); got != tc.want {
			t.Fatalf("ScaleSlots(frac=%.3f,custom) = %d, want %d", tc.frac, got, tc.want)
		}
	}
}

func TestPreflight(t *testing.T) {
	cal := calibratedCfg() // window ceiling 100
	base := usage(win(5, 50, 100, "ccusage"), win(168, 100, 1000, "ccusage"))

	// (50+45)/100 = 0.95, below graceful_stop=0.97 → should pass
	if ok, reason := Preflight(base, 45, cal); !ok {
		t.Fatalf("wave to 95%% should pass (graceful-stop is 97%%), got not-ok: %s", reason)
	}
	// (50+48)/100 = 0.98 >= graceful_stop=0.97 → should fail
	ok, reason := Preflight(base, 48, cal)
	if ok {
		t.Fatalf("wave crossing graceful-stop should not dispatch")
	}
	if !strings.Contains(reason, "graceful-stop") {
		t.Fatalf("reason should mention graceful-stop, got %q", reason)
	}

	// Uncalibrated → always ok, advisory.
	uncal := DefaultConfig("acct")
	uu := usage(win(5, 0, 0, ""), win(168, 0, 0, ""))
	ok, reason = Preflight(uu, 999, uncal)
	if !ok || !strings.Contains(reason, "uncalibrated") {
		t.Fatalf("uncalibrated Preflight = (%v, %q), want ok + uncalibrated", ok, reason)
	}

	// Unavailable window fails closed even when calibrated.
	un := usage(win(5, 0, 100, "unavailable"), win(168, 10, 1000, "ccusage"))
	if ok, _ := Preflight(un, 0, cal); ok {
		t.Fatalf("unavailable window should fail closed")
	}
}

func TestLadder(t *testing.T) {
	// All-zero (defaults) is valid.
	if err := (Ladder{}).Validate(); err != nil {
		t.Fatalf("default ladder invalid: %v", err)
	}
	// Custom valid ladder.
	custom := Ladder{Warn: 0.85, Throttle: 0.90, GracefulStop: 0.95, HardStop: 0.98}
	if err := custom.Validate(); err != nil {
		t.Fatalf("custom ladder invalid: %v", err)
	}
	// Not strictly ascending → error.
	bad := Ladder{Warn: 0.90, Throttle: 0.85}
	if err := bad.Validate(); err == nil {
		t.Fatal("expected error for non-ascending ladder")
	}
	// Out of range → error.
	outOfRange := Ladder{Warn: 1.1}
	if err := outOfRange.Validate(); err == nil {
		t.Fatal("expected error for warn > 1")
	}
	// Effective() fills defaults.
	el := (Ladder{}).Effective()
	if el.Warn != DefaultWarnFraction {
		t.Fatalf("Effective().Warn = %g, want %g", el.Warn, DefaultWarnFraction)
	}
	// IsDefault.
	if !(Ladder{}).IsDefault() {
		t.Fatal("zero Ladder should be IsDefault")
	}
	if custom.IsDefault() {
		t.Fatal("custom Ladder should not be IsDefault")
	}
}

// rollingCfg is a config with a rolling-$ ceiling configured (api-key mode)
// but no subscription window ceilings.
func rollingCfg(ceiling float64) *Config {
	c := DefaultConfig("acct")
	c.RollingCeilingUSD = ceiling
	return c
}

// TestStateForAuthModeSubscriptionUnchanged asserts subscription/oauth-token
// accounts get byte-for-byte the same verdict from StateForAuthMode as from
// State — the design's "auth_mode: subscription behavior is byte-for-byte
// unchanged" acceptance criterion (§11 AC5), and oauth-token bills/governs
// identically to subscription (design §7 table).
func TestStateForAuthModeSubscriptionUnchanged(t *testing.T) {
	cal := calibratedCfg()
	u := usage(win(5, 94, 100, "ccusage"), win(168, 0, 1000, "ccusage"))

	wantLevel, wantCalibd := State(u, cal)

	for _, mode := range []string{AuthModeSubscription, AuthModeOAuthToken, ""} {
		lvl, calibd := StateForAuthMode(mode, u, 0 /* spentUSD ignored */, cal)
		if lvl != wantLevel || calibd != wantCalibd {
			t.Fatalf("StateForAuthMode(%q) = (%s, %v), want (%s, %v) matching State", mode, lvl, calibd, wantLevel, wantCalibd)
		}
	}
}

// TestStateForAuthModeAPIKeyNoCeilingIsAdvisory is design §11 AC6's first
// half: an api-key account with no rolling-$ ceiling configured must read
// advisory (LevelOK, false), NOT permanently-at-stop — the exact bug
// Window.Fraction() would produce if reused unmodified for this mode.
func TestStateForAuthModeAPIKeyNoCeilingIsAdvisory(t *testing.T) {
	uncal := DefaultConfig("acct") // RollingCeilingUSD unset
	// An empty Usage — api-key mode ignores the subscription windows entirely.
	u := Usage{Account: "acct"}

	lvl, calibd := StateForAuthMode(AuthModeAPIKey, u, 1000 /* huge spend */, uncal)
	if calibd {
		t.Fatalf("api-key with no ceiling should be uncalibrated (advisory), got calibrated=true")
	}
	if lvl != LevelOK {
		t.Fatalf("api-key with no ceiling should read LevelOK (advisory), got %s", lvl)
	}

	// Also nil cfg must not panic and must be advisory.
	lvl, calibd = StateForAuthMode(AuthModeAPIKey, u, 1000, nil)
	if calibd || lvl != LevelOK {
		t.Fatalf("api-key with nil cfg should be advisory LevelOK, got (%s, %v)", lvl, calibd)
	}
}

// TestStateForAuthModeAPIKeyCeilingFiresLadder is design §11 AC6's second
// half: with a ceiling configured, api-key accounts read the standard
// warn/throttle/graceful_stop/hard_stop ladder off spentUSD/ceiling.
func TestStateForAuthModeAPIKeyCeilingFiresLadder(t *testing.T) {
	cfg := rollingCfg(100)
	u := Usage{Account: "acct"} // irrelevant for api-key mode

	cases := []struct {
		name      string
		spentUSD  float64
		wantLevel Level
	}{
		{"healthy", 10, LevelOK},
		{"warn at 0.90", 90, LevelWarn},
		{"throttle at 0.94", 94, LevelThrottle},
		{"drain at 0.97", 97, LevelDrain},
		{"stop at 0.99", 99, LevelStop},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lvl, calibd := StateForAuthMode(AuthModeAPIKey, u, tc.spentUSD, cfg)
			if !calibd {
				t.Fatalf("configured ceiling should report calibrated=true")
			}
			if lvl != tc.wantLevel {
				t.Fatalf("StateForAuthMode(spent=%.0f) = %s, want %s", tc.spentUSD, lvl, tc.wantLevel)
			}
		})
	}
}

func TestScaleSlotsForAuthMode(t *testing.T) {
	const max = 9
	u := Usage{Account: "acct"}

	// subscription/oauth-token: delegates to ScaleSlots verbatim.
	subCfg := calibratedCfg()
	subU := usage(win(5, 95.5, 100, "ccusage"), win(168, 0, 1000, "ccusage"))
	want := ScaleSlots(subU, subCfg, max)
	if got := ScaleSlotsForAuthMode(AuthModeSubscription, subU, 0, subCfg, max); got != want {
		t.Fatalf("ScaleSlotsForAuthMode(subscription) = %d, want %d (== ScaleSlots)", got, want)
	}

	// api-key, no ceiling: advisory, never scales down.
	uncal := DefaultConfig("acct")
	if got := ScaleSlotsForAuthMode(AuthModeAPIKey, u, 1000, uncal, max); got != max {
		t.Fatalf("ScaleSlotsForAuthMode(api-key, uncalibrated) = %d, want %d (advisory, no scaling)", got, max)
	}

	// api-key, with ceiling: scales down using the same curve as ScaleSlots.
	cfg := rollingCfg(100)
	cases := []struct {
		spentUSD float64
		want     int
	}{
		{93, max},
		{94, max},
		{95.5, 5},
		{96.9, 1},
		{97, 0},
		{99, 0},
	}
	for _, tc := range cases {
		if got := ScaleSlotsForAuthMode(AuthModeAPIKey, u, tc.spentUSD, cfg, max); got != tc.want {
			t.Fatalf("ScaleSlotsForAuthMode(api-key, spent=%.1f) = %d, want %d", tc.spentUSD, got, tc.want)
		}
	}
}

func TestPreflightForAuthMode(t *testing.T) {
	u := Usage{Account: "acct"}

	// subscription: delegates to Preflight verbatim.
	subCfg := calibratedCfg()
	subU := usage(win(5, 50, 100, "ccusage"), win(168, 100, 1000, "ccusage"))
	wantOK, wantReason := Preflight(subU, 45, subCfg)
	if ok, reason := PreflightForAuthMode(AuthModeSubscription, subU, 0, 45, subCfg); ok != wantOK || reason != wantReason {
		t.Fatalf("PreflightForAuthMode(subscription) = (%v,%q), want (%v,%q)", ok, reason, wantOK, wantReason)
	}

	// api-key, no ceiling: always allowed, advisory.
	uncal := DefaultConfig("acct")
	if ok, reason := PreflightForAuthMode(AuthModeAPIKey, u, 0, 999, uncal); !ok || !strings.Contains(reason, "uncalibrated") {
		t.Fatalf("PreflightForAuthMode(api-key, uncalibrated) = (%v,%q), want ok + uncalibrated", ok, reason)
	}

	// api-key, with ceiling: (50+45)/100 = 0.95 < graceful_stop 0.97 → ok.
	cfg := rollingCfg(100)
	if ok, reason := PreflightForAuthMode(AuthModeAPIKey, u, 50, 45, cfg); !ok {
		t.Fatalf("rolling-$ wave to 95%% should pass (graceful-stop is 97%%), got not-ok: %s", reason)
	}
	// (50+48)/100 = 0.98 >= graceful_stop=0.97 → refused.
	ok, reason := PreflightForAuthMode(AuthModeAPIKey, u, 50, 48, cfg)
	if ok {
		t.Fatalf("rolling-$ wave crossing graceful-stop should not dispatch")
	}
	if !strings.Contains(reason, "graceful-stop") {
		t.Fatalf("reason should mention graceful-stop, got %q", reason)
	}
}

func TestWindowFraction(t *testing.T) {
	if f := win(5, 10, 0, "ccusage").Fraction(); f != 1.0 {
		t.Fatalf("zero ceiling should be 1.0, got %g", f)
	}
	if f := win(5, 50, 100, "unavailable").Fraction(); f != 1.0 {
		t.Fatalf("unavailable should be 1.0, got %g", f)
	}
	if f := win(5, 25, 100, "ccusage").Fraction(); math.Abs(f-0.25) > 1e-9 {
		t.Fatalf("measured fraction = %g, want 0.25", f)
	}
}
