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
//
// koryph-2im.11 (L5b) hardens the overlay with three additional, still
// Adaptive-gated mechanisms — settle windows, a circuit breaker, and dispatch
// smoothing — because AIMD alone thrashes: agents dispatched under the OLD
// cap keep tripping rate-limit events for minutes after a decrease, a single
// burst can need more than one halving, and independent projects' refill
// loops reacting to the same cap change can dispatch simultaneously.

const (
	// probeInterval is how often, with no intervening rate-limit event, the
	// dynamic cap climbs by 1 — the additive-increase half of AIMD.
	probeInterval = 5 * time.Minute

	// DefaultSettleSeconds is how long, after ANY DynamicCap change, further
	// changes in either direction are frozen (koryph-2im.11). Subsumes the
	// pre-L5b 60s decrease cooldown.
	DefaultSettleSeconds = 120

	// DefaultBreakSeconds is the circuit breaker's base open-state duration
	// (koryph-2im.11), doubled per consecutive re-open up to maxBreakSeconds.
	DefaultBreakSeconds = 300

	// maxBreakSeconds bounds the circuit breaker's exponential re-open
	// backoff (koryph-2im.11) — an operator-visible ceiling so a flapping
	// account degrades to "checks in once an hour," never an unbounded wait.
	maxBreakSeconds = 3600

	// DefaultMinDispatchIntervalSeconds is dispatch smoothing's base
	// machine-wide minimum inter-dispatch spacing (koryph-2im.11), jittered
	// ±50% per admission.
	DefaultMinDispatchIntervalSeconds = 3

	// burstWindow / burstThreshold: >=3 DISTINCT (project,bead) rate-limit
	// events within this window is a burst — see distinctSlotsInWindow.
	burstWindow    = 30 * time.Second
	burstThreshold = 3

	// normalDecreaseFactor / burstDecreaseFactor: the multiplicative-decrease
	// divisor, 2x normally, 4x on a detected burst (koryph-2im.11).
	normalDecreaseFactor = 2
	burstDecreaseFactor  = 4

	// decreaseBurstWindow / decreaseBurstThreshold: 3 decreases within this
	// window trips the circuit breaker open (koryph-2im.11).
	decreaseBurstWindow    = 10 * time.Minute
	decreaseBurstThreshold = 3

	// maxRecentEvents bounds RecentRateLimitEvents' stored size regardless of
	// window pruning — a defensive cap so a pathological event storm cannot
	// grow governor.json unboundedly between writes.
	maxRecentEvents = 32
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

// settleSecondsOf/breakSecondsOf/minDispatchIntervalOf apply this package's
// documented defaults for the three L5b knobs when a config's stored value is
// unset/non-positive (koryph-2im.11) — the same "<=0 means default" idiom
// hardMaxOf already uses.
func settleSecondsOf(c Config) int {
	if c.SettleSeconds > 0 {
		return c.SettleSeconds
	}
	return DefaultSettleSeconds
}

func breakSecondsOf(c Config) int {
	if c.BreakSeconds > 0 {
		return c.BreakSeconds
	}
	return DefaultBreakSeconds
}

func minDispatchIntervalOf(c Config) int {
	if c.MinDispatchIntervalSeconds > 0 {
		return c.MinDispatchIntervalSeconds
	}
	return DefaultMinDispatchIntervalSeconds
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

// --- settle window (koryph-2im.11) ------------------------------------------

// freezeSettle stamps a fresh SettleUntil deadline SettleSeconds from now —
// called after ANY actual DynamicCap change (decrease or additive-increase
// step), never for a merely-counted event. The additive probe's own clock
// anchors on this deadline (see applyProbe), which is what makes "the
// quiet-clock starts at settle expiry, not at the change itself" true by
// construction rather than by a separate check.
func freezeSettle(c *Config, now time.Time) {
	c.SettleUntil = now.Add(time.Duration(settleSecondsOf(*c)) * time.Second).UTC().Format(time.RFC3339)
}

// inSettle reports whether cfg is inside its post-change freeze window.
func inSettle(c Config, now time.Time) bool {
	u := parseTime(c.SettleUntil)
	return !u.IsZero() && now.Before(u)
}

// --- burst / decrease-history bookkeeping (koryph-2im.11) -------------------

// recordRateLimitEvent appends one event to the bounded 30s-window history
// used by distinctSlotsInWindow, pruning anything outside the window (and
// then, defensively, anything beyond maxRecentEvents) on every write so
// governor.json never grows unboundedly from event storms.
func recordRateLimitEvent(c *Config, project, bead string, now time.Time) {
	c.RecentRateLimitEvents = append(c.RecentRateLimitEvents, RateLimitEvent{
		At:      now.UTC().Format(time.RFC3339),
		Project: project,
		Bead:    bead,
	})
	cutoff := now.Add(-burstWindow)
	kept := c.RecentRateLimitEvents[:0]
	for _, e := range c.RecentRateLimitEvents {
		if t := parseTime(e.At); !t.IsZero() && t.After(cutoff) {
			kept = append(kept, e)
		}
	}
	if len(kept) > maxRecentEvents {
		kept = kept[len(kept)-maxRecentEvents:]
	}
	c.RecentRateLimitEvents = kept
}

// distinctSlotsInWindow counts distinct (project,bead) identities in an
// already-pruned event history — the burst-scaled decrease's trigger.
func distinctSlotsInWindow(events []RateLimitEvent) int {
	seen := map[string]struct{}{}
	for _, e := range events {
		seen[e.Project+"|"+e.Bead] = struct{}{}
	}
	return len(seen)
}

// recordDecrease appends a decrease timestamp to the bounded 10-minute
// history used to trip the circuit breaker on 3 decreases in that window.
func recordDecrease(c *Config, now time.Time) {
	c.RecentDecreases = append(c.RecentDecreases, now.UTC().Format(time.RFC3339))
	cutoff := now.Add(-decreaseBurstWindow)
	kept := c.RecentDecreases[:0]
	for _, ts := range c.RecentDecreases {
		if t := parseTime(ts); !t.IsZero() && t.After(cutoff) {
			kept = append(kept, ts)
		}
	}
	c.RecentDecreases = kept
}

// --- circuit breaker (koryph-2im.11) ----------------------------------------

// openBreaker trips the breaker: isReopen=false is a fresh trip from closed
// (BreakerReopenCount resets to 0, base duration); isReopen=true is a
// half-open probe that failed (count increments, duration doubles, capped at
// maxBreakSeconds). Either way any outstanding probe identity is cleared —
// the breaker owns admission again, not a specific lease.
func openBreaker(c *Config, now time.Time, isReopen bool) {
	if isReopen {
		c.BreakerReopenCount++
	} else {
		c.BreakerReopenCount = 0
	}
	dur := breakSecondsOf(*c)
	for i := 0; i < c.BreakerReopenCount && dur < maxBreakSeconds; i++ {
		dur *= 2
	}
	if dur > maxBreakSeconds {
		dur = maxBreakSeconds
	}
	c.BreakerState = "open"
	c.BreakerOpenAt = now.UTC().Format(time.RFC3339)
	c.BreakerBreakSeconds = dur
	c.ProbeProject = ""
	c.ProbeBead = ""
	c.ProbeAdmittedAt = ""
}

// closeBreaker resolves a successful probe: breaker returns to closed, AIMD
// resumes from DynamicCap=1 with the probe clock anchored at now (I5 holds —
// this only affects future admission, never a running agent), and the
// consecutive-reopen counter resets (a real recovery, not a flap).
func closeBreaker(c *Config, now time.Time) {
	c.BreakerState = ""
	c.BreakerReopenCount = 0
	c.BreakerOpenAt = ""
	c.BreakerBreakSeconds = 0
	c.ProbeProject = ""
	c.ProbeBead = ""
	c.ProbeAdmittedAt = ""
	c.DynamicCap = 1
	c.LastProbeAt = now.UTC().Format(time.RFC3339)
	c.SettleUntil = ""
}

// resolveBreaker promotes an "open" breaker to "half-open" once its break
// duration has elapsed, making it eligible for exactly one probe admission.
// Returns whether cfg changed (the caller persists only then). A "half-open"
// or "" (closed) state is left untouched — only "open" ever resolves here.
func resolveBreaker(c *Config, now time.Time) bool {
	if !c.Adaptive || c.BreakerState != "open" {
		return false
	}
	opened := parseTime(c.BreakerOpenAt)
	if opened.IsZero() {
		return false
	}
	dur := c.BreakerBreakSeconds
	if dur <= 0 {
		dur = breakSecondsOf(*c)
	}
	if now.Sub(opened) < time.Duration(dur)*time.Second {
		return false
	}
	c.BreakerState = "half-open"
	return true
}

// --- dispatch smoothing (koryph-2im.11) -------------------------------------

// smoothingDenies reports whether an admission arriving at now must be
// refused for spacing: the machine-wide minimum inter-dispatch interval,
// jittered ±50% via jitter (expected in [-0.5, 0.5); see Store.jitter). No
// prior admission (LastAdmitAt unset) never denies.
func smoothingDenies(c Config, now time.Time, jitter float64) bool {
	last := parseTime(c.LastAdmitAt)
	if last.IsZero() {
		return false
	}
	base := float64(minDispatchIntervalOf(c))
	interval := base * (1 + jitter)
	if interval < 0 {
		interval = 0
	}
	required := time.Duration(interval * float64(time.Second))
	return now.Sub(last) < required
}

// --- pure AIMD step functions ------------------------------------------------

// applyRateLimit applies one rate-limit signal to cfg in place. It always
// counts it (RateLimitEvents, LastRateLimitAt) for observability. When
// adaptive is off, that is all it does. When adaptive is on it layers in
// koryph-2im.11's circuit breaker and settle window ahead of the plain AIMD
// halve:
//
//  1. A report matching (or, absent bead identity, plausibly matching — see
//     the project-only fallback below) the current half-open probe is a
//     definitive failure signal: re-open (doubled break), independent of
//     settle.
//  2. Any other report while the breaker is non-closed is counted only —
//     admission is already gated at 0 (open) or 1 (half-open, someone else's
//     probe).
//  3. Closed, with the cap already at its floor: trips the breaker open
//     (independent of settle — this is a distinct trigger from "3 decreases
//     in 10 minutes").
//  4. Closed, inside the settle window: counted only, no re-application —
//     decisions are only made against a population that reflects the
//     current cap.
//  5. Closed, settled: the ordinary AIMD halve, burst-scaled to /4 instead
//     of /2 when >=3 distinct slots reported within the last 30s, which also
//     freezes a new settle window and may itself trip the breaker (3
//     decreases within 10 minutes).
//
// Pure — no I/O — so it is directly table-testable; Store.ReportRateLimit
// wraps it under the flock. project/bead identify the reporting lease for
// burst-distinct-slot counting and probe matching; bead=="" is the degraded
// path used by the one caller that cannot thread bead identity through yet
// (see internal/engine/govern.go's reportRateLimit) — see the project-only
// fallback below.
func applyRateLimit(c *Config, project, bead string, now time.Time) (decreased, breakerOpened bool) {
	c.RateLimitEvents++
	c.LastRateLimitAt = now.UTC().Format(time.RFC3339)

	if !c.Adaptive {
		return false, false
	}

	recordRateLimitEvent(c, project, bead, now)

	if c.BreakerState == "half-open" {
		// Exact identity match is a definitive "the probe failed" signal.
		// bead=="" is the degraded caller path (project known, bead
		// plumbing not yet threaded through): if the report is from the
		// SAME project currently holding the probe, we cannot rule out that
		// it IS the probe, and assuming failure (spurious re-open, safe) is
		// strictly better than assuming success (spurious close, unsafe —
		// it would resume full admission on a still-throttled account).
		if project == c.ProbeProject && (bead == c.ProbeBead || bead == "") {
			openBreaker(c, now, true)
			return false, true
		}
		return false, false // some other in-flight lease's report; count only
	}
	if c.BreakerState == "open" {
		return false, false // admission already 0; nothing more to do
	}

	// Closed. A rate-limit while the cap is already pinned at the floor is
	// itself a trip, independent of the settle window below.
	if c.EffectiveCap() <= 1 {
		openBreaker(c, now, false)
		return false, true
	}

	if inSettle(*c, now) {
		return false, false
	}

	factor := normalDecreaseFactor
	if distinctSlotsInWindow(c.RecentRateLimitEvents) >= burstThreshold {
		factor = burstDecreaseFactor
	}
	next := c.EffectiveCap() / factor
	if next < 1 {
		next = 1
	}
	c.DynamicCap = next
	c.LastDecreaseAt = c.LastRateLimitAt
	freezeSettle(c, now)
	recordDecrease(c, now)

	if len(c.RecentDecreases) >= decreaseBurstThreshold {
		openBreaker(c, now, false)
		return true, true
	}
	return true, false
}

// applyProbe advances cfg's DynamicCap by the additive-increase probe: +1 per
// full probeInterval elapsed since the later of SettleUntil or the last probe
// step, up to HardMax. Anchoring on SettleUntil (rather than the change
// timestamp itself) is koryph-2im.11's "the quiet-clock starts at settle
// expiry, not at the change itself" — and, because a settle deadline in the
// future makes `now.Before(anchor)` true, it is also what freezes the probe
// FOR the settle window without a separate check. A non-closed breaker
// supersedes normal AIMD growth entirely (dynamicCap is irrelevant while the
// breaker gates admission itself). Returns whether cfg changed (the caller
// persists only then). Pure — no I/O — for direct table testing;
// Store.effectiveCapLocked/loadAndProbeLocked wrap it under the flock.
func applyProbe(c *Config, now time.Time) (changed bool) {
	if !c.Adaptive || c.BreakerState != "" {
		return false
	}

	anchor := parseTime(c.SettleUntil)
	if p := parseTime(c.LastProbeAt); p.After(anchor) {
		anchor = p
	}
	if anchor.IsZero() {
		// No anchor yet (adaptive just enabled, or a hand-edited store) — seed
		// the clock rather than crediting elapsed-since-epoch growth.
		c.LastProbeAt = now.UTC().Format(time.RFC3339)
		return true
	}
	if now.Before(anchor) {
		return false // still settling
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
	freezeSettle(c, now) // this step is itself a DynamicCap change (koryph-2im.11 rule 1)
	return true
}

// loadAndProbeLocked reads governor.json, extracts pool's Config, and applies
// (persisting the WHOLE file, if anything advanced) any pending
// breaker-open→half-open promotion (resolveBreaker) and additive-probe
// growth (applyProbe) for that pool only — every other pool's entry is
// carried through untouched (koryph-v8u.11). Callers must already hold the
// store's flock (s.withLock) — it does not lock itself, so
// effectiveCapLocked, AIMDStatus, PoolStatus, and Acquire can share one lock
// acquisition with it. The returned File lets a caller that needs to persist
// a FURTHER change to the same pool (e.g. Acquire granting a lease) avoid a
// redundant read — set File.Pools[pool] and write it back.
func (s *Store) loadAndProbeLocked(pool string) (Config, File, error) {
	f, err := s.readFile()
	if err != nil {
		return Config{}, File{}, err
	}
	c := f.Pools[pool] // zero Config if this pool has no entry yet
	now := s.Now()
	breakerChanged := resolveBreaker(&c, now)
	if breakerChanged {
		logBreakerHalfOpen(pool)
	}
	oldCap := c.DynamicCap
	probeChanged := applyProbe(&c, now)
	if probeChanged && oldCap > 0 && c.DynamicCap != oldCap {
		logProbeAdvanced(pool, oldCap, c.DynamicCap)
	}
	changed := breakerChanged || probeChanged
	f.Pools[pool] = c
	if changed {
		if err := fsx.WriteJSONAtomic(s.cfgPath, f); err != nil {
			return c, f, err
		}
	}
	return c, f, nil
}

// effectiveCapLocked is loadAndProbeLocked's result (for pool) reduced to the
// effective cap; callers must already hold the store's flock.
func (s *Store) effectiveCapLocked(pool string) (int, error) {
	c, _, err := s.loadAndProbeLocked(pool)
	return c.EffectiveCap(), err
}

// EffectiveCap returns the cap Acquire admits against for provider's pool
// ("" is DefaultPool): the static operator cap when that pool's AIMD overlay
// is off, or the clamped dynamic cap when adaptive is enabled — after lazily
// applying (and persisting) any additive-probe growth pending since the last
// read. Fails open to the static Cap() on any store error, matching this
// package's fail-open convention.
func (s *Store) EffectiveCap(provider string) int {
	pool := NormalizeProvider(provider)
	var eff int
	err := s.withLock(func() error {
		var lerr error
		eff, lerr = s.effectiveCapLocked(pool)
		return lerr
	})
	if err != nil {
		return s.Cap(pool)
	}
	return eff
}

// AIMDStatus returns the full persisted AIMD/L5b overlay state for provider's
// pool ("" is DefaultPool), for observability (`koryph governor show`,
// `koryph doctor`), applying (and persisting) any pending breaker promotion,
// crashed-probe timeout resolution, and additive-probe growth first — a
// "show" always reflects the same lazily-recovered state the next Acquire
// would use.
func (s *Store) AIMDStatus(provider string) (Config, error) {
	pool := NormalizeProvider(provider)
	var c Config
	err := s.withLock(func() error {
		if perr := s.prune(); perr != nil {
			return perr
		}
		var lerr error
		c, _, lerr = s.loadAndProbeLocked(pool)
		return lerr
	})
	return c, err
}

// ReportRateLimit records a rate-limit/overload signal observed from a dead
// agent's stream (koryph-2im.4) against provider's pool ("" is DefaultPool),
// identified by project/bead so koryph-2im.11's burst-distinct-slot counting
// and half-open probe matching can work (bead may be "" — see
// applyRateLimit's degraded-caller fallback). It always counts toward
// RateLimitEvents for observability; the rest (decrease, settle, breaker
// trip/re-open) is applyRateLimit's pure logic, applied here under the flock
// so every engine on the host shares one outcome FOR THAT POOL — every other
// pool's entry is untouched (koryph-v8u.11).
func (s *Store) ReportRateLimit(provider, project, bead string, now time.Time) error {
	pool := NormalizeProvider(provider)
	var decreased, breakerOpened bool
	err := s.withLock(func() error {
		f, err := s.readFile()
		if err != nil {
			return err
		}
		c := f.Pools[pool]
		decreased, breakerOpened = applyRateLimit(&c, project, bead, now)
		f.Pools[pool] = c
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
	if err == nil {
		if decreased {
			logCapDecreased(pool, project, bead, 0, 0)
		}
		if breakerOpened {
			logBreakerOpened(pool, project, bead)
		}
	}
	return err
}

// SetAdaptiveCap enables the AIMD overlay for provider's pool ("" is
// DefaultPool): maxGlobal is the operator's starting/floor cap (also the
// DynamicCap seed), hardMax bounds upward probing (defaulting to 2×maxGlobal,
// matching `koryph governor set --adaptive`'s documented default).
// settleSeconds/breakSeconds/minDispatchIntervalSeconds are koryph-2im.11's
// L5b knobs; <=0 uses this package's documented default for each
// (DefaultSettleSeconds/DefaultBreakSeconds/
// DefaultMinDispatchIntervalSeconds). Enabling adaptive mode always starts
// THIS POOL's probe clock fresh from now — any prior decrease/probe/breaker
// history for this pool is intentionally discarded, mirroring SetCap's "last
// write wins" semantics; every OTHER pool's entry is left untouched
// (koryph-v8u.11).
func (s *Store) SetAdaptiveCap(provider string, maxGlobal, hardMax, settleSeconds, breakSeconds, minDispatchIntervalSeconds int) error {
	if maxGlobal <= 0 {
		return errors.New("govern: max_global_agents must be positive")
	}
	if hardMax <= 0 {
		hardMax = maxGlobal * 2
	}
	if hardMax < maxGlobal {
		hardMax = maxGlobal
	}
	if settleSeconds <= 0 {
		settleSeconds = DefaultSettleSeconds
	}
	if breakSeconds <= 0 {
		breakSeconds = DefaultBreakSeconds
	}
	if minDispatchIntervalSeconds <= 0 {
		minDispatchIntervalSeconds = DefaultMinDispatchIntervalSeconds
	}
	pool := NormalizeProvider(provider)
	now := s.Now().UTC().Format(time.RFC3339)
	newPool := Config{
		MaxGlobalAgents:            maxGlobal,
		Adaptive:                   true,
		HardMax:                    hardMax,
		DynamicCap:                 maxGlobal,
		LastProbeAt:                now,
		SettleSeconds:              settleSeconds,
		BreakSeconds:               breakSeconds,
		MinDispatchIntervalSeconds: minDispatchIntervalSeconds,
	}
	return s.withLock(func() error {
		f, err := s.readFile()
		if err != nil {
			return err
		}
		f.Pools[pool] = newPool
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}
