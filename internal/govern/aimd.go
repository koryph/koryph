// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"errors"
	"time"

	"github.com/koryph/koryph/internal/fsx"
)

// The AIMD overlay (koryph-2im.4,
// docs/designs/2026-07-scheduler-throughput.md L5) turns the static
// machine-wide cap into a congestion controller: the effective cap floats
// upward on quiet (probing past the operator's starting width to find the
// real ceiling) and halves on a rate-limit signal. It is entirely optional —
// Config.Adaptive off reproduces today's static-cap behavior exactly — and,
// like the rest of this package, coordinates through governor.json under the
// store's flock so every engine on the host shares one backoff state.

const (
	// rateLimitCooldown bounds one multiplicative decrease per window; a burst
	// of rate-limited deaths across engines halves the cap once, not once each.
	// Events inside the cooldown still count (RateLimitEvents), they just don't
	// re-halve.
	rateLimitCooldown = 60 * time.Second

	// probeInterval is how often, with no intervening rate-limit event, the
	// dynamic cap climbs by 1 — the additive-increase half of AIMD.
	probeInterval = 5 * time.Minute
)

// EffectiveCap returns the cap this config implies right now, WITHOUT
// applying the lazy additive-probe step (only Store.EffectiveCap does that,
// since advancing the probe is a write). Adaptive off returns the operator
// cap exactly as Store.Cap() always has (including for a store written
// before these fields existed); adaptive on clamps the dynamic cap into
// [1, HardMax].
func (c Config) EffectiveCap() int {
	if !c.Adaptive {
		if c.MaxGlobalAgents <= 0 {
			return DefaultMaxGlobalAgents
		}
		return c.MaxGlobalAgents
	}
	return clamp(c.DynamicCap, 1, hardMaxOf(c))
}

// hardMaxOf returns c.HardMax, falling back to the current dynamic cap (i.e.
// no further upward room) when HardMax is unset or non-positive — a
// defensive floor so a misconfigured/hand-edited store can never clamp
// upward into an unbounded value.
func hardMaxOf(c Config) int {
	if c.HardMax > 0 {
		return c.HardMax
	}
	return c.DynamicCap
}

// clamp bounds v into [lo, hi], widening hi to lo if the range is inverted
// (defensive: never returns a value below lo).
func clamp(v, lo, hi int) int {
	if hi < lo {
		hi = lo
	}
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	default:
		return v
	}
}

// parseTime parses an RFC3339 timestamp, returning the zero Time on any
// parse failure (including an empty string) rather than an error — every
// caller here treats "no timestamp yet" as "long ago enough to not gate
// anything," which applyProbe's IsZero check special-cases separately.
func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// applyRateLimit applies one rate-limit signal to cfg in place: always counts
// it (RateLimitEvents, LastRateLimitAt) for observability, and — only when
// adaptive is on and the cooldown since the last decrease has elapsed —
// halves DynamicCap (floored at 1) and stamps LastDecreaseAt, which also
// resets the additive-probe clock (see applyProbe). Pure — no I/O — so it is
// directly table-testable; Store.ReportRateLimit wraps it under the flock.
func applyRateLimit(c *Config, now time.Time) (decreased bool) {
	c.RateLimitEvents++
	c.LastRateLimitAt = now.UTC().Format(time.RFC3339)

	if !c.Adaptive {
		return false
	}
	if last := parseTime(c.LastDecreaseAt); !last.IsZero() && now.Sub(last) < rateLimitCooldown {
		return false // inside cooldown: counted above, not re-halved
	}

	next := c.EffectiveCap() / 2
	if next < 1 {
		next = 1
	}
	c.DynamicCap = next
	c.LastDecreaseAt = c.LastRateLimitAt
	return true
}

// applyProbe advances cfg's DynamicCap by the additive-increase probe: +1 per
// full probeInterval elapsed since the later of the last decrease or the last
// probe step, up to HardMax. A decrease moving LastDecreaseAt forward is
// exactly the "no rate-limit events since" reset the design calls for — no
// separate bookkeeping needed. Returns whether cfg changed (the caller
// persists only then). Pure — no I/O — for direct table testing;
// Store.effectiveCapLocked wraps it under the flock.
func applyProbe(c *Config, now time.Time) (changed bool) {
	if !c.Adaptive {
		return false
	}

	anchor := parseTime(c.LastDecreaseAt)
	if p := parseTime(c.LastProbeAt); p.After(anchor) {
		anchor = p
	}
	if anchor.IsZero() {
		// No anchor yet (adaptive just enabled, or a hand-edited store) — seed
		// the clock rather than crediting elapsed-since-epoch growth.
		c.LastProbeAt = now.UTC().Format(time.RFC3339)
		return true
	}

	steps := int(now.Sub(anchor) / probeInterval)
	if steps <= 0 {
		return false
	}
	hardMax := hardMaxOf(*c)
	c.DynamicCap += steps
	if c.DynamicCap > hardMax {
		c.DynamicCap = hardMax
	}
	c.LastProbeAt = anchor.Add(time.Duration(steps) * probeInterval).UTC().Format(time.RFC3339)
	return true
}

// loadAndProbeLocked reads governor.json and applies (persisting, if it
// advanced) any pending additive-probe growth. Callers must already hold the
// store's flock (s.withLock) — it does not lock itself, so effectiveCapLocked
// and Acquire can share one lock acquisition with it.
func (s *Store) loadAndProbeLocked() (Config, error) {
	var c Config
	_ = fsx.ReadJSON(s.cfgPath, &c) // absent/corrupt ⇒ zero Config (adaptive off)
	if applyProbe(&c, s.Now()) {
		if err := fsx.WriteJSONAtomic(s.cfgPath, c); err != nil {
			return c, err
		}
	}
	return c, nil
}

// effectiveCapLocked is loadAndProbeLocked's result reduced to the effective
// cap; callers must already hold the store's flock.
func (s *Store) effectiveCapLocked() (int, error) {
	c, err := s.loadAndProbeLocked()
	return c.EffectiveCap(), err
}

// EffectiveCap returns the cap Acquire admits against: the static operator
// cap when the AIMD overlay is off, or the clamped dynamic cap when adaptive
// is enabled — after lazily applying (and persisting) any additive-probe
// growth pending since the last read. Fails open to the static Cap() on any
// store error, matching this package's fail-open convention.
func (s *Store) EffectiveCap() int {
	var eff int
	err := s.withLock(func() error {
		var lerr error
		eff, lerr = s.effectiveCapLocked()
		return lerr
	})
	if err != nil {
		return s.Cap()
	}
	return eff
}

// AIMDStatus returns the full persisted AIMD overlay state for observability
// (`koryph governor show`, `koryph doctor`), applying (and persisting) any
// pending additive-probe growth first — a "show" always reflects the same
// lazily-recovered capacity the next Acquire would use.
func (s *Store) AIMDStatus() (Config, error) {
	var c Config
	err := s.withLock(func() error {
		var lerr error
		c, lerr = s.loadAndProbeLocked()
		return lerr
	})
	return c, err
}

// ReportRateLimit records a rate-limit/overload signal observed from a dead
// agent's stream (koryph-2im.4). It always counts toward RateLimitEvents for
// observability; the multiplicative decrease itself only applies when
// adaptive is on and the 60s cooldown since the last decrease has elapsed, so
// a burst of near-simultaneous rate-limited deaths across engines halves the
// shared cap once, not once per engine.
func (s *Store) ReportRateLimit(now time.Time) error {
	return s.withLock(func() error {
		var c Config
		_ = fsx.ReadJSON(s.cfgPath, &c)
		applyRateLimit(&c, now)
		return fsx.WriteJSONAtomic(s.cfgPath, c)
	})
}

// SetAdaptiveCap enables the AIMD overlay: maxGlobal is the operator's
// starting/floor cap (also the DynamicCap seed), hardMax bounds upward
// probing (defaulting to 2×maxGlobal, matching `koryph governor set
// --adaptive`'s documented default). Enabling adaptive mode always starts the
// probe clock fresh from now — any prior decrease/probe history is
// intentionally discarded, mirroring SetCap's "last write wins" semantics.
func (s *Store) SetAdaptiveCap(maxGlobal, hardMax int) error {
	if maxGlobal <= 0 {
		return errors.New("govern: max_global_agents must be positive")
	}
	if hardMax <= 0 {
		hardMax = maxGlobal * 2
	}
	if hardMax < maxGlobal {
		hardMax = maxGlobal
	}
	now := s.Now().UTC().Format(time.RFC3339)
	return fsx.WriteJSONAtomic(s.cfgPath, Config{
		MaxGlobalAgents: maxGlobal,
		Adaptive:        true,
		HardMax:         hardMax,
		DynamicCap:      maxGlobal,
		LastProbeAt:     now,
	})
}
