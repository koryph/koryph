// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package quota

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// configPath returns the on-disk location for an account's governor state.
func configPath(account string) string {
	return filepath.Join(paths.QuotaDir(), account+".json")
}

// UpdateConfig applies mutate to the account's quota config under an exclusive
// per-account flock: it re-reads the config FRESH from disk inside the lock,
// applies mutate, and writes it back. This is the only safe way to update the
// EWMA calibration — the config is shared across every project on the account,
// so two concurrent runs (or a run vs. `koryph quota calibrate`) that each
// Load→mutate→Save their own in-memory copy would clobber each other's writes.
// Returns the updated config so a caller can refresh its in-memory copy. A
// mutate that returns an error aborts the write (nothing is saved).
func UpdateConfig(account string, mutate func(*Config) error) (*Config, error) {
	var out *Config
	err := withConfigLock(account, func() error {
		cfg, err := LoadConfig(account)
		if err != nil {
			return err
		}
		if err := mutate(cfg); err != nil {
			return err
		}
		if err := SaveConfig(cfg); err != nil {
			return err
		}
		out = cfg
		return nil
	})
	return out, err
}

// withConfigLock runs fn while holding an exclusive flock on the account's quota
// lock file. The OS releases the flock if the process dies, so a crash cannot
// wedge quota accounting.
func withConfigLock(account string, fn func() error) error {
	if err := os.MkdirAll(paths.QuotaDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(paths.QuotaDir(), account+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
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
	if cfg.SchemaVersion == 0 {
		cfg.SchemaVersion = ConfigSchemaVersion
	}
	// Validate the ladder; reset invalid rungs to zero (falls back to defaults).
	if err := cfg.Ladder.Validate(); err != nil {
		cfg.Ladder = Ladder{} // reset to defaults
	}
	return &cfg, nil
}

// SaveConfig writes cfg atomically to ~/.koryph/quota/<account>.json (0600 —
// private account state).
func SaveConfig(cfg *Config) error {
	return fsx.WriteJSONAtomicPerm(configPath(cfg.Account), cfg, 0o600)
}

// SetGuardMode writes a billing-guard override into the account's quota config
// under an exclusive flock. mode must be one of GuardModeOn, GuardModeAdvisory,
// or GuardModeOff (the CLI also accepts "off" as a synonym for "advisory";
// SetGuardMode normalises "off" → "advisory" in the stored value so readers
// need only test for "" / "on" vs "advisory"). until is an optional expiry
// time (zero = permanent). The caller is responsible for audit logging.
//
// Calling SetGuardMode with mode == GuardModeOn (or "") clears both fields so
// the JSON stays compact.
func SetGuardMode(account, mode string, until time.Time) (*Config, error) {
	return UpdateConfig(account, func(c *Config) error {
		switch mode {
		case GuardModeOn, "":
			c.GuardMode = ""
			c.GuardUntil = ""
		case GuardModeOff, GuardModeAdvisory:
			c.GuardMode = GuardModeAdvisory // normalise "off" → "advisory"
			if !until.IsZero() {
				c.GuardUntil = until.UTC().Format(time.RFC3339)
			} else {
				c.GuardUntil = ""
			}
		default:
			return fmt.Errorf("quota: unknown guard mode %q (want on|advisory|off)", mode)
		}
		return nil
	})
}

// ConfigGuardAdvisory reports whether the stored guard mode is advisory right
// now, taking the optional GuardUntil expiry into account. now is injected for
// testing; pass time.Now() in production callers.
//
// Returns (false, "") when the mode is enforced (or has expired).
// Returns (true, reason) when advisory, where reason is a human-readable
// description suitable for log lines and doctor findings.
func ConfigGuardAdvisory(cfg *Config, now time.Time) (advisory bool, reason string) {
	if cfg == nil || cfg.GuardMode == "" || cfg.GuardMode == GuardModeOn {
		return false, ""
	}
	// Check expiry.
	if cfg.GuardUntil != "" {
		t, err := time.Parse(time.RFC3339, cfg.GuardUntil)
		if err == nil && now.After(t) {
			// Expired — treat as enforced.
			return false, ""
		}
	}
	if cfg.GuardUntil != "" {
		return true, fmt.Sprintf("quota guard advisory (live toggle; expires %s)", cfg.GuardUntil)
	}
	return true, "quota guard advisory (live toggle)"
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

// levelFor maps a fraction of the ceiling onto a governor level using the
// effective ladder thresholds.
func levelFor(frac float64, l Ladder) Level {
	el := l.Effective()
	switch {
	case frac >= el.HardStop:
		return LevelStop
	case frac >= el.GracefulStop:
		return LevelDrain
	case frac >= el.Throttle:
		return LevelThrottle
	case frac >= el.Warn:
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
	var l Ladder
	if cfg != nil {
		l = cfg.Ladder
	}
	return levelFor(maxFraction(u), l), true
}

// ScaleSlots scales a desired parallelism (max) down as usage climbs:
// full max at or below the throttle fraction, linearly interpolated max→1
// across [Throttle, GracefulStop), and 0 at or above the graceful_stop
// fraction. The result never dips below 1 while below graceful_stop.
// cfg may be nil (uses package defaults).
func ScaleSlots(u Usage, cfg *Config, max int) int {
	if max <= 0 {
		return 0
	}
	var l Ladder
	if cfg != nil {
		l = cfg.Ladder
	}
	el := l.Effective()
	frac := maxFraction(u)
	if frac >= el.GracefulStop {
		return 0
	}
	if frac <= el.Throttle {
		return max
	}
	// t in (0,1): 0 at Throttle, →1 approaching GracefulStop.
	t := (frac - el.Throttle) / (el.GracefulStop - el.Throttle)
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
	var l Ladder
	if cfg != nil {
		l = cfg.Ladder
	}
	el := l.Effective()

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
	if projected >= el.GracefulStop {
		return false, fmt.Sprintf(
			"wave ($%.2f est) would put the 5h window at %.0f%% ($%.2f/$%.2f) — crosses graceful-stop (%.0f%%)",
			waveEstimateUSD, projected*100, w.SpentUSD+waveEstimateUSD, ceiling, el.GracefulStop*100)
	}
	return true, fmt.Sprintf(
		"5h window projected %.0f%% after wave (graceful-stop at %.0f%%)",
		projected*100, el.GracefulStop*100)
}
