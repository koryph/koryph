// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"encoding/json"
	"errors"
	"fmt"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/procx"
)

// corruptBackupSuffix names the sibling file readFile/readFileForWrite copy a
// present-but-unparseable governor.json to before falling open (koryph audit
// finding #27) — see backupCorrupt.
const corruptBackupSuffix = ".corrupt-backup"

// Store coordinates global concurrency through files under paths.SlotsDir,
// guarded by a flock. Now/Alive are injectable for tests.
type Store struct {
	slotsDir  string
	demandDir string
	cfgPath   string

	// Now supplies the clock (rotation epoch, timestamps, TTL). Defaults to
	// time.Now.
	Now func() time.Time
	// Alive reports whether a pid is a live process. Defaults to a signal-0 probe.
	Alive func(pid int) bool

	// RotateWindow is the fair-share remainder rotation period; DemandTTL and
	// LeaseTTL are the staleness backstops (PID liveness is the primary signal).
	RotateWindow time.Duration
	DemandTTL    time.Duration
	LeaseTTL     time.Duration

	// ProbeTimeout bounds the circuit breaker's half-open probe (koryph-2im.11):
	// a probe lease that is gone (crashed, or the engine that dispatched it
	// died) without ever going through Release's clean-close path or
	// ReportRateLimit's re-open path is presumed failed once this much time has
	// elapsed since it was admitted — see pruneCrashedProbe. Defaults to
	// LeaseTTL-scale generosity (long enough that a legitimately slow probe
	// agent is never mistaken for a crash).
	ProbeTimeout time.Duration

	// Jitter returns a value in [-0.5, 0.5) for dispatch smoothing's ±50%
	// spread (koryph-2im.11); overridable for deterministic tests. Defaults to
	// a process-global math/rand source — jitter need only be unpredictable
	// enough to avoid a thundering herd, not cryptographically random.
	Jitter func() float64

	// SeedCap optionally supplies a per-pool PERSISTED DEFAULT cap (koryph-1o2.3):
	// when pool has no explicit operator cap (Cap/EffectiveCap's
	// MaxGlobalAgents<=0 branch), fallbackCap consults SeedCap(pool) — a
	// positive return wins over the package default — before falling through
	// to the "anthropic" pool's own cap for migration continuity. This is how
	// the engine's per-account quota.Config.MaxThreads seed reaches admission
	// WITHOUT govern importing package quota (layering): the engine wires a
	// closure here that reads its own already-loaded quota config. nil (the
	// zero value — every NewStore() and every hand-built Store{} in existing
	// tests) preserves today's behavior exactly: no seed, straight to the
	// anthropic-pool/package-default fallback.
	SeedCap func(pool string) int
}

// NewStore returns a Store rooted at the current KORYPH_HOME.
func NewStore() *Store {
	return &Store{
		slotsDir:     paths.SlotsDir(),
		demandDir:    paths.DemandDir(),
		cfgPath:      paths.GovernorConfig(),
		Now:          time.Now,
		Alive:        processAlive,
		RotateWindow: time.Minute,
		DemandTTL:    10 * time.Minute,
		LeaseTTL:     24 * time.Hour,
		ProbeTimeout: 30 * time.Minute,
		Jitter:       func() float64 { return mathrand.Float64() - 0.5 },
	}
}

// Cap returns provider's pool cap, defaulting when governor.json (or the
// pool's entry) is absent or unset. provider=="" is DefaultPool
// (koryph-v8u.11). An unset explicit cap falls through the koryph-1o2.3
// precedence chain — see fallbackCap — before reaching the package default.
func (s *Store) Cap(provider string) int {
	pool := NormalizeProvider(provider)
	f, err := s.readFile()
	if err != nil {
		return DefaultMaxGlobalAgents
	}
	c := f.Pools[pool] // zero Config (MaxGlobalAgents 0) if this pool has no entry
	if c.MaxGlobalAgents > 0 {
		return c.MaxGlobalAgents
	}
	return s.fallbackCap(pool)
}

// fallbackCap resolves the cap for pool when it carries no explicit operator
// setting (koryph-1o2.3 precedence levels 2-4, level 1 — the explicit cap —
// having already been checked by the caller): the engine-supplied per-account
// seed (SeedCap; e.g. quota.Config.MaxThreads, wired by the engine so govern
// itself never imports quota) when set and positive; else, for a NAMED
// account pool, the "anthropic" default pool's own cap, for migration
// continuity (an operator's pre-per-account-pools `governor set --max-global`
// still governs a newly-onboarded named account that has configured neither
// an explicit cap nor a quota seed); else the package default. Resolving the
// anthropic pool itself skips the continuity hop — Cap(DefaultPool) would
// just re-enter this same branch — terminating in the package default
// directly.
func (s *Store) fallbackCap(pool string) int {
	if s.SeedCap != nil {
		if seed := s.SeedCap(pool); seed > 0 {
			return seed
		}
	}
	if pool != DefaultPool {
		return s.Cap(DefaultPool)
	}
	return DefaultMaxGlobalAgents
}

// effectiveCapFor mirrors Config.EffectiveCap for pool's config c, except its
// non-adaptive "no explicit operator cap" branch consults fallbackCap
// (koryph-1o2.3) instead of jumping straight to the package default, so an
// account's seeded MaxThreads (or the anthropic pool's continuity cap) wins
// over DefaultMaxGlobalAgents at admission. Adaptive pools are untouched:
// SetAdaptiveCap always seeds a positive MaxGlobalAgents/DynamicCap, so
// there is never an "unset adaptive pool" for the seed to apply to.
func (s *Store) effectiveCapFor(pool string, c Config) int {
	if !c.Adaptive && c.MaxGlobalAgents <= 0 {
		return s.fallbackCap(pool)
	}
	return c.EffectiveCap()
}

// MinFreeMemoryMB returns provider's RAW configured memory admission floor
// setting (koryph-930): >0 an explicit floor in MB, <0 the gate explicitly
// disabled, 0 unset (callers auto-size the floor to physical memory — the gate
// is ON by default). Returns 0 (auto) when governor.json or the pool entry is
// absent, or on any read error, matching the governor's fail-open posture.
func (s *Store) MinFreeMemoryMB(provider string) int {
	pool := NormalizeProvider(provider)
	f, err := s.readFile()
	if err != nil {
		return 0
	}
	c, ok := f.Pools[pool]
	if !ok {
		return 0
	}
	return c.MinFreeMemoryMB
}

// Resources returns the machine's top-level resource ledger (koryph-4ql.1,
// docs/designs/2026-07-resource-governor.md L2): the configured per-kind
// capacities/costs shared across every provider pool. Fails open to the zero
// ResourcesConfig{} (no kinds — every declared kind still binds at the default
// capacity 1, reservations off) when governor.json is absent, unreadable, or
// has no resources section, matching the MinFreeMemoryMB fail-open precedent.
// The engine (R3) reads it to resolve effective capacities and per-bead memory
// reservations at dispatch; unlike Acquire's own accounting, this is a plain
// read, so it is deliberately unlocked (like Cap/MinFreeMemoryMB).
func (s *Store) Resources() ResourcesConfig {
	f, err := s.readFile()
	if err != nil || f.Resources == nil {
		return ResourcesConfig{}
	}
	return *f.Resources
}

// SetResource writes (or replaces) kind's machine capacity/cost in
// governor.json's top-level resources ledger (koryph-4ql.1, L2), PRESERVING
// every pool config and every OTHER kind — the SetMinFreeMemoryMB
// preserve-don't-reset precedent, NOT SetCap's wholesale reset. The resources
// section (and its kinds map) is created on first use. Backs `koryph governor
// set-resource <kind> --capacity N [--mem-mb M] [--ramp-seconds S] [--probe
// CMD]` (R5). Relies on File.UnmarshalJSON decoding the section on read, so an
// earlier `set`/`set-resource` is not stripped by this whole-file rewrite.
func (s *Store) SetResource(kind string, spec ResourceKind) error {
	if kind == "" {
		return errors.New("govern: resource kind must be non-empty")
	}
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		if f.Resources == nil {
			f.Resources = &ResourcesConfig{}
		}
		if f.Resources.Kinds == nil {
			f.Resources.Kinds = map[string]ResourceKind{}
		}
		f.Resources.Kinds[kind] = spec
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}

// UnsetResource removes kind from the resources ledger (koryph-4ql.1),
// preserving every pool config and every other kind. A missing kind (or an
// absent section) is not an error — idempotent, like DropDemand. After removal
// the kind reverts to the fail-safe default (capacity 1, no reservation), so
// beads declaring it still serialize. Backs `governor set-resource <kind>
// --unset` (R5).
func (s *Store) UnsetResource(kind string) error {
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		if f.Resources == nil || f.Resources.Kinds == nil {
			return nil
		}
		delete(f.Resources.Kinds, kind)
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}

// SetCap writes provider's pool cap to governor.json, resetting that pool's
// AIMD/settle/breaker/smoothing state wholesale (exactly today's single-pool
// SetCap semantics — a plain `set` disables any previously-enabled overlay)
// while leaving every OTHER pool untouched (koryph-v8u.11).
func (s *Store) SetCap(provider string, n int) error {
	if n <= 0 {
		return errors.New("govern: max_global_agents must be positive")
	}
	pool := NormalizeProvider(provider)
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		f.Pools[pool] = Config{MaxGlobalAgents: n}
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}

// SetMinFreeMemoryMB writes provider's memory admission floor (koryph-930) to
// governor.json, PRESERVING every other field of that pool's config (cap, AIMD
// overlay, breaker/settle state) — unlike SetCap, which resets the pool
// wholesale. The value is interpreted by readers: mb>0 an explicit floor, mb<0
// disables the gate, mb==0 resets to the auto floor (sized to physical memory,
// the default). A pool that does not yet exist is created with only the floor
// set (its cap defaults via Cap()). provider=="" is DefaultPool.
func (s *Store) SetMinFreeMemoryMB(provider string, mb int) error {
	pool := NormalizeProvider(provider)
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		c := f.Pools[pool] // zero Config when the pool is absent
		c.MinFreeMemoryMB = mb
		f.Pools[pool] = c
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}

// RefreshDemand records (or refreshes) this project's demand heartbeat within
// provider's pool: it has ready work and wants slots. Call once per wave
// while work remains. provider=="" is DefaultPool.
func (s *Store) RefreshDemand(provider, project string, enginePID int) error {
	pool := NormalizeProvider(provider)
	return s.withLock(func() error {
		if err := os.MkdirAll(s.demandDir, 0o755); err != nil {
			return err
		}
		return fsx.WriteJSONAtomic(s.demandPath(pool, project), Demand{
			Project:   project,
			EnginePID: enginePID,
			UpdatedAt: s.Now().UTC().Format(time.RFC3339),
			Provider:  pool,
		})
	})
}

// DropDemand removes this project's demand heartbeat from provider's pool
// (frontier drained / run ended), releasing it from that pool's fair-share
// denominator. provider=="" is DefaultPool.
func (s *Store) DropDemand(provider, project string) error {
	pool := NormalizeProvider(provider)
	return s.withLock(func() error {
		err := os.Remove(s.demandPath(pool, project))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Acquire attempts to take a slot for one agent, in the pool named by
// l.Provider ("" normalizes to DefaultPool — koryph-v8u.11). It prunes stale
// state, computes the caller's fair share WITHIN THAT POOL, and grants iff
// that pool's cap has room AND either the caller is under its fair share or
// every other demander in the pool already holds its share (work-conserving
// top-up). Returns whether a slot was granted.
//
// When the pool's AIMD overlay is Adaptive, koryph-2im.11's circuit breaker
// and dispatch smoothing gate admission BEFORE the cap/fair-share checks
// below: a fully-open breaker denies everything IN THIS POOL (I5 holds — this
// only refuses a NEW lease, never revokes one already granted, and never
// touches any other pool); a half-open breaker admits exactly one lease as
// the probe (the flock serializes concurrent callers, so exactly one wins the
// race) and nothing else in this pool until it resolves; closed admission is
// further spaced by the jittered minimum dispatch interval.
//
// Acquire is the compatibility shape (koryph-4ql.1): it routes through
// AcquireEx with an empty MemInput, so a resource-free lease with no memory
// reading behaves byte-for-byte as before. The engine keeps calling it until
// it adopts AcquireEx in R3.
func (s *Store) Acquire(l Lease) (bool, error) {
	res, err := s.AcquireEx(l, MemInput{})
	return res.Granted, err
}

// AcquireEx is Acquire with the two machine-resource admission clauses
// (koryph-4ql.1, L2/L5) layered onto the pool cap / fair share / breaker /
// smoothing checks, and a typed verdict so the engine (R3) can route
// skip-vs-break (see AdmitOutcome). mem carries an optional current-memory
// reading (MemInput{} skips the memory clause); both new clauses are pure
// lease-file arithmetic under the existing flock, so they satisfy I7 (no
// subprocess probes under the lock).
//
// The clauses ALSO gate the half-open circuit-breaker probe grant, which
// returns early before the cap/fair-share section: a resource-declared probe
// must still pass capacity, and a resource-denied probe candidate leaves the
// probe slot open for the next caller (it never claims the probe). Every
// clause fails open on error (I6): a denial is a normal deferral with a
// reason, never an error.
func (s *Store) AcquireEx(l Lease, mem MemInput) (AdmitResult, error) {
	pool := NormalizeProvider(l.Provider)
	l.Provider = pool // stored state never has an empty key (koryph-v8u.11)
	var result AdmitResult
	var grantedCap, grantedActive int
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		// loadAndProbeLocked (koryph-2im.4/2im.11/koryph-v8u.11): the static
		// operator cap when this pool's AIMD overlay is off (byte-for-byte
		// the prior s.Cap() admission via c.EffectiveCap() below), or the
		// probed/backed-off dynamic cap — plus current settle/breaker/
		// smoothing state — when adaptive is enabled for this pool. f carries
		// the decoded top-level resource ledger (f.Resources) too.
		c, f, err := s.loadAndProbeLocked(pool)
		if err != nil {
			return err
		}
		now := s.Now()

		if c.Adaptive {
			switch c.BreakerState {
			case "open":
				result = AdmitResult{Outcome: AdmitDeniedCap}
				return nil // deny: zero admission in this pool while open
			case "half-open":
				if c.ProbeProject != "" || c.ProbeBead != "" {
					result = AdmitResult{Outcome: AdmitDeniedCap}
					return nil // a probe is already outstanding; deny everyone else
				}
				// A resource-declared probe must clear the machine clauses
				// (L2/L5) before claiming the probe; a resource/memory-denied
				// candidate returns WITHOUT taking the probe, leaving the slot
				// open for the next caller.
				if denial := s.checkResourcesLocked(f.Resources, l, mem, now); denial != nil {
					result = *denial
					return nil
				}
				c.ProbeProject = l.Project
				c.ProbeBead = l.Bead
				c.ProbeAdmittedAt = now.UTC().Format(time.RFC3339)
				c.LastAdmitAt = c.ProbeAdmittedAt
				if err := s.grantLease(l, now); err != nil {
					return err
				}
				result = AdmitResult{Granted: true, Outcome: AdmitGranted}
				grantedCap = s.effectiveCapFor(pool, c)
				f.Pools[pool] = c
				return fsx.WriteJSONAtomic(s.cfgPath, f) // persist the probe claim
			}

			// Dispatch smoothing (koryph-2im.11): closed-state admission only —
			// the probe above is a single, deliberate dispatch, not part of a
			// burst a spacing rule needs to defend against. A denial here must
			// NOT touch LastAdmitAt (smoothingDenies reads it fresh on the
			// engine's next refill-tick retry).
			if smoothingDenies(c, now, s.jitter()) {
				result = AdmitResult{Outcome: AdmitDeniedCap}
				return nil
			}
		}

		cap := s.effectiveCapFor(pool, c)
		leases, err := s.leasesForPool(pool)
		if err != nil {
			return err
		}
		if len(leases) >= cap {
			result = AdmitResult{Outcome: AdmitDeniedCap}
			return nil // pool full
		}

		demanders := s.demanders(pool, l.Project)
		myActive := countProject(leases, l.Project)

		// Strict fair share WITHIN THIS POOL: a project may hold up to its
		// share of the pool's cap. Idle capacity is reclaimed not by lending
		// (agents are never preempted) but when a project drains its
		// frontier and drops its demand — that shrinks the denominator and
		// raises everyone else's share on the next acquire.
		if myActive >= fairShare(cap, demanders, l.Project, s.epoch()) {
			result = AdmitResult{Outcome: AdmitDeniedCap}
			return nil
		}

		// Machine resource clauses (L2 capacity + L5 reservation-aware memory):
		// a SECOND, additive admission dimension checked only once the pool has
		// room for this lease (I1 — resources never relax a footprint/cap
		// conflict, only add one). Cross-pool, pure arithmetic.
		if denial := s.checkResourcesLocked(f.Resources, l, mem, now); denial != nil {
			result = *denial
			return nil
		}

		if err := s.grantLease(l, now); err != nil {
			return err
		}
		result = AdmitResult{Granted: true, Outcome: AdmitGranted}
		grantedCap = s.effectiveCapFor(pool, c)
		grantedActive = len(leases) + 1 // +1: the lease just written

		if c.Adaptive {
			c.LastAdmitAt = now.UTC().Format(time.RFC3339)
			f.Pools[pool] = c
			if err := fsx.WriteJSONAtomic(s.cfgPath, f); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		if result.Granted {
			logGranted(pool, l.Project, l.Bead, grantedCap, grantedActive)
		} else {
			// For denied: read cap outside the lock for the log (best-effort).
			logDenied(pool, l.Project, l.Bead, s.Cap(pool), 0)
		}
	}
	return result, err
}

// checkResourcesLocked runs the two machine-scoped admission clauses for
// candidate cand under the flock (koryph-4ql.1, L2/L5): capacity, then
// reservation-aware memory. rc is the decoded machine ledger (may be nil).
// Returns a denial AdmitResult (naming the kind/holder, or the memory verdict)
// when a clause refuses, or nil when both pass. Pure lease-file arithmetic —
// no subprocess (I7) — and every error path fails OPEN (returns nil, i.e.
// admit) per I6. Callers must already hold the store's flock and have pruned.
//
// The candidate's own lease (same project+bead — a re-acquire or a stale
// reservation) is excluded from both the capacity count and the ramping
// reservation sum so it is never charged against itself.
func (s *Store) checkResourcesLocked(rc *ResourcesConfig, cand Lease, mem MemInput, now time.Time) *AdmitResult {
	memActive := mem.AvailMB > 0 && mem.FloorMB > 0
	if len(cand.Resources) == 0 && !memActive {
		return nil // nothing declared and no reading → no machine clause applies
	}
	all, err := s.leases() // every pool: machine resources are cross-pool
	if err != nil {
		return nil // fail open (I6)
	}
	others := make([]Lease, 0, len(all))
	for _, l := range all {
		if l.Project == cand.Project && l.Bead == cand.Bead {
			continue // never count the candidate against itself
		}
		others = append(others, l)
	}

	// Clause 1 — capacity (L2). For each declared kind, count cross-pool
	// holders; admit iff holders+1 <= capacity(kind). The default capacity 1
	// always binds (even with no resources section), so two holders of an
	// unconfigured kind cannot co-dispatch.
	for _, kind := range cand.Resources {
		capK := rc.capacityOf(kind)
		holders := 0
		var holderProj, holderBead string
		for _, l := range others {
			if containsStr(l.Resources, kind) {
				holders++
				if holderBead == "" { // first (leases() is sorted) → deterministic
					holderProj, holderBead = l.Project, l.Bead
				}
			}
		}
		if holders+1 > capK {
			return &AdmitResult{
				Outcome:        AdmitDeniedResource,
				DeniedKind:     kind,
				DeniedCapacity: capK,
				DeniedHolders:  holders,
				HolderProject:  holderProj,
				HolderBead:     holderBead,
			}
		}
	}

	// Clause 2 — reservation-aware memory (L5), only with a real reading:
	//   availMB − Σ(ramping leases' MemReserveMB) − candidate MemReserveMB ≥ floorMB
	// Signed arithmetic avoids uint underflow when reservations exceed avail.
	if memActive {
		reserved := 0
		for _, l := range others {
			if l.MemReserveMB > 0 && leaseRamping(l, rc, now) {
				reserved += l.MemReserveMB
			}
		}
		availLessReserved := int64(mem.AvailMB) - int64(reserved)
		withCand := availLessReserved - int64(cand.MemReserveMB)
		if withCand < int64(mem.FloorMB) {
			return &AdmitResult{
				Outcome: AdmitDeniedMemory,
				// Would it have passed at MemReserveMB=0? Then the candidate's
				// own reservation tipped it (per-bead skip); otherwise a pure
				// floor breach (batch-break) — even a 0-reserve bead fails.
				CandidateTipped: availLessReserved >= int64(mem.FloorMB),
			}
		}
	}
	return nil
}

// grantLease writes l's lease file, stamping AcquiredAt if unset. l.Provider
// must already be normalized (non-empty). Callers must already hold the
// store's flock and have already decided admission.
func (s *Store) grantLease(l Lease, now time.Time) error {
	if l.AcquiredAt == "" {
		l.AcquiredAt = now.UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
		return err
	}
	return fsx.WriteJSONAtomic(s.leasePath(l.Provider, l.Project, l.Bead), l)
}

// jitter returns Store.Jitter() when set, else a process-global math/rand
// source (koryph-2im.11's dispatch-smoothing spread).
func (s *Store) jitter() float64 {
	if s.Jitter != nil {
		return s.Jitter()
	}
	return mathrand.Float64() - 0.5
}

// Hold unconditionally writes (or updates) a lease WITHOUT a cap check, in
// the pool named by l.Provider ("" normalizes to DefaultPool). It is the
// second half of the two-phase acquire: Acquire reserves a slot under the
// engine pid before launch (cap-checked), and Hold attaches the detached agent
// pid after launch so the lease is keyed to a process that outlives the engine.
// Because it skips the cap check it also correctly re-counts a requeued or
// resumed agent whose reservation was pruned in the death→relaunch gap — a 1:1
// replacement for an already-admitted bead, so it cannot breach the cap.
//
// Resource ledger (koryph-4ql.1, L2): Hold persists the caller-supplied lease
// verbatim, so l.Resources / l.MemReserveMB are written to the lease file and
// counted by every subsequent Acquire's capacity/reservation clauses — the
// engine threads them in from the persisted ledger slot (never a govern-side
// read of the prior lease, which after a prune gap does not exist). Hold does
// NOT re-check the machine clauses (the no-recheck 1:1 contract stands; §7
// documents the bounded requeue-window capacity breach this accepts). It
// stamps AcquiredAt when unset — and the engine always leaves it unset — so
// the ramp clock (L5) restarts per (re)bind, the over-reserving, safe
// direction.
func (s *Store) Hold(l Lease) error {
	l.Provider = NormalizeProvider(l.Provider)
	return s.withLock(func() error {
		if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
			return err
		}
		if l.AcquiredAt == "" {
			l.AcquiredAt = s.Now().UTC().Format(time.RFC3339)
		}
		return fsx.WriteJSONAtomic(s.leasePath(l.Provider, l.Project, l.Bead), l)
	})
}

// Release frees the slot held by (project, bead) in provider's pool ("" is
// DefaultPool). A missing lease is not an error (idempotent / already
// pruned).
//
// Circuit breaker (koryph-2im.11): releasing the half-open probe's own lease
// WITHOUT a prior rate-limit report for it (ReportRateLimit would already
// have re-opened the breaker and cleared the probe identity — see
// applyRateLimit) is the "clean" signal that closes the breaker and resumes
// AIMD from DynamicCap=1. A probe that never reaches Release at all (crashed,
// or its owning engine died) is instead resolved by pruneCrashedProbe's
// timeout fallback.
func (s *Store) Release(provider, project, bead string) error {
	pool := NormalizeProvider(provider)
	breakerClosed := false
	err := s.withLock(func() error {
		err := os.Remove(s.leasePath(pool, project, bead))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		f, ferr := s.readFile()
		if ferr == nil {
			if c, ok := f.Pools[pool]; ok && c.Adaptive && c.BreakerState == "half-open" &&
				bead != "" && project == c.ProbeProject && bead == c.ProbeBead {
				closeBreaker(&c, s.Now())
				f.Pools[pool] = c
				breakerClosed = true
				if werr := fsx.WriteJSONAtomic(s.cfgPath, f); werr != nil {
					return werr
				}
			}
		}
		return nil
	})
	if err == nil && breakerClosed {
		logBreakerClosed(pool)
	}
	return err
}

// Prune removes dead/stale leases and demand heartbeats across ALL pools.
func (s *Store) Prune() error {
	return s.withLock(func() error { return s.prune() })
}

// FairShareFor returns project p's current fair share of provider's pool cap
// given that pool's live demand set (p is always counted, since asking
// implies demand). Backs the per-project override warning. provider=="" is
// DefaultPool.
func (s *Store) FairShareFor(provider, project string) (int, error) {
	pool := NormalizeProvider(provider)
	var share int
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		cap, err := s.effectiveCapLocked(pool)
		if err != nil {
			return err
		}
		share = fairShare(cap, s.demanders(pool, project), project, s.epoch())
		return nil
	})
	return share, err
}

// Snapshot returns provider's pool's operator cap and its current (pruned)
// leases and demands, for `koryph governor`/tests. provider=="" is
// DefaultPool. Use Pools + PoolStatus to enumerate every pool at once.
func (s *Store) Snapshot(provider string) (int, []Lease, []Demand, error) {
	pool := NormalizeProvider(provider)
	ps, err := s.PoolStatus(pool)
	if err != nil {
		return 0, nil, nil, err
	}
	return s.Cap(pool), ps.Leases, ps.Demand, nil
}

// PoolStatus returns provider's pool's full snapshot (pruning stale state
// first): its leases, demand heartbeats, and AIMD/settle/breaker/smoothing
// overlay. provider=="" is DefaultPool.
func (s *Store) PoolStatus(provider string) (PoolStatus, error) {
	pool := NormalizeProvider(provider)
	var ps PoolStatus
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		leases, err := s.leasesForPool(pool)
		if err != nil {
			return err
		}
		dem, err := s.demandForPool(pool)
		if err != nil {
			return err
		}
		c, _, err := s.loadAndProbeLocked(pool)
		if err != nil {
			return err
		}
		ps = PoolStatus{Pool: pool, Leases: leases, Demand: dem, AIMD: c}
		return nil
	})
	return ps, err
}

// ResourcesStatus returns the live per-kind resource ledger state across ALL
// pools (koryph-4ql.1, L7), for `koryph governor show` / the cockpit: every
// CONFIGURED kind plus every kind any live lease HOLDS (a lease may hold an
// unconfigured kind, which binds at default capacity 1), each with its
// resolved capacity/cost/ramp/probe, its live holders and their ramp state,
// and the reserved-vs-materialized memory split. Prunes stale state first (the
// PoolStatus precedent). Sorted by kind. The CLI/IDE bead only renders this;
// all accounting lives here. Machine resources are cross-pool, so this has no
// provider parameter.
func (s *Store) ResourcesStatus() ([]ResourceStatus, error) {
	var out []ResourceStatus
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		f, err := s.readFile()
		if err != nil {
			return err
		}
		all, err := s.leases()
		if err != nil {
			return err
		}
		out = assembleResourceStatuses(f.Resources, all, s.Now())
		return nil
	})
	return out, err
}

// assembleResourceStatuses builds the per-kind resource ledger view from an
// already-read config and lease set: every CONFIGURED kind plus every kind any
// live lease HOLDS, with resolved capacity/cost/ramp/probe, live holders, and
// the reserved-vs-materialized memory split. Pure — shared by ResourcesStatus
// (pruning path) and Observe (read-only path).
func assembleResourceStatuses(rc *ResourcesConfig, all []Lease, now time.Time) []ResourceStatus {
	// Union of configured kinds and held kinds → a stable sorted set.
	kinds := map[string]struct{}{}
	if rc != nil {
		for k := range rc.Kinds {
			kinds[k] = struct{}{}
		}
	}
	for _, l := range all {
		for _, k := range l.Resources {
			kinds[k] = struct{}{}
		}
	}
	names := make([]string, 0, len(kinds))
	for k := range kinds {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]ResourceStatus, 0, len(names))
	for _, kind := range names {
		st := ResourceStatus{
			Kind:        kind,
			Capacity:    rc.capacityOf(kind),
			MemMB:       rc.memMBOf(kind),
			RampSeconds: rc.rampSecondsOf(kind),
			Probe:       rc.probeOf(kind),
		}
		for _, l := range all {
			if !containsStr(l.Resources, kind) {
				continue
			}
			ramping := leaseRamping(l, rc, now)
			st.Holders = append(st.Holders, ResourceHolder{
				Project:      l.Project,
				Bead:         l.Bead,
				MemReserveMB: l.MemReserveMB,
				Ramping:      ramping,
			})
			if ramping {
				st.ReservedMB += l.MemReserveMB
			} else {
				st.MaterializedMB += l.MemReserveMB
			}
		}
		out = append(out, st)
	}
	return out
}

// Observation is a consistent snapshot of the whole governor — every pool's
// status plus the machine resource ledger — assembled in ONE lock acquisition
// and ONE scan of the lease/demand directories.
type Observation struct {
	Pools     map[string]PoolStatus
	Resources []ResourceStatus
}

// Observe assembles an Observation WITHOUT mutating any governor state. Unlike
// Pools/PoolStatus/ResourcesStatus — which prune stale lease files, resolve
// crashed probes, and persist pending AIMD probe growth on every call — this
// path never writes: stale leases and demand (dead PID / expired TTL) are
// filtered from the returned counts in memory, and pending breaker promotion +
// probe growth are applied to the returned Config copies only, so the observed
// DynamicCap matches what the engine would compute next without the observer
// advancing the probe clock or deleting files. This is the path for monitors
// (the TUI cockpit, `koryph governor show --watch`) polling at high frequency:
// a monitor must observe the control loop, not participate in it. The engine's
// own next mutating call does the real pruning.
func (s *Store) Observe() (Observation, error) {
	obs := Observation{Pools: map[string]PoolStatus{}}
	err := s.withLock(func() error {
		now := s.Now()

		leaseMap, err := s.leaseFiles()
		if err != nil {
			return err
		}
		leases := make([]Lease, 0, len(leaseMap))
		for _, l := range leaseMap {
			// Mirror prune's staleness rules, filtering instead of deleting.
			alivePID := l.PID
			if alivePID <= 0 {
				alivePID = l.EnginePID
			}
			if !s.Alive(alivePID) || s.expired(l.AcquiredAt, s.LeaseTTL) {
				continue
			}
			leases = append(leases, l)
		}
		sort.Slice(leases, func(i, j int) bool { return leases[i].AcquiredAt < leases[j].AcquiredAt })

		demMap, err := s.demandFiles()
		if err != nil {
			return err
		}
		demand := make([]Demand, 0, len(demMap))
		for _, d := range demMap {
			if !s.Alive(d.EnginePID) || s.expired(d.UpdatedAt, s.DemandTTL) {
				continue
			}
			demand = append(demand, d)
		}
		sort.Slice(demand, func(i, j int) bool { return demand[i].Project < demand[j].Project })

		f, err := s.readFile()
		if err != nil {
			return err
		}

		poolSet := map[string]struct{}{DefaultPool: {}}
		for p := range f.Pools {
			poolSet[p] = struct{}{}
		}
		for _, l := range leases {
			poolSet[NormalizeProvider(l.Provider)] = struct{}{}
		}
		for _, d := range demand {
			poolSet[NormalizeProvider(d.Provider)] = struct{}{}
		}

		for pool := range poolSet {
			c := f.Pools[pool]
			// In-memory only: both helpers are pure; nothing is persisted.
			resolveBreaker(&c, now)
			applyProbe(&c, now)
			ps := PoolStatus{Pool: pool, AIMD: c}
			for _, l := range leases {
				if NormalizeProvider(l.Provider) == pool {
					ps.Leases = append(ps.Leases, l)
				}
			}
			for _, d := range demand {
				if NormalizeProvider(d.Provider) == pool {
					ps.Demand = append(ps.Demand, d)
				}
			}
			obs.Pools[pool] = ps
		}

		obs.Resources = assembleResourceStatuses(f.Resources, leases, now)
		return nil
	})
	return obs, err
}

// Pools returns the sorted set of every pool with any live state: an
// explicit governor.json entry, a lease, or a demand heartbeat. DefaultPool
// is always included so `governor show`/`doctor` never report zero pools on
// a freshly initialized ~/.koryph (koryph-v8u.11).
func (s *Store) Pools() ([]string, error) {
	set := map[string]struct{}{DefaultPool: {}}
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		f, err := s.readFile()
		if err != nil {
			return err
		}
		for p := range f.Pools {
			set[p] = struct{}{}
		}
		leases, err := s.leases()
		if err != nil {
			return err
		}
		for _, l := range leases {
			set[NormalizeProvider(l.Provider)] = struct{}{}
		}
		dem, err := s.demand()
		if err != nil {
			return err
		}
		for _, d := range dem {
			set[NormalizeProvider(d.Provider)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// --- internals (must be called under the lock) ---------------------------

// readFile reads governor.json into a File (transparently migrating a legacy
// single-pool document — see File.UnmarshalJSON). Absent fails open to an
// empty pool map, matching this package's existing fail-open convention (a
// stuck/missing governor.json must never block dispatch: admission reads —
// Cap, MinFreeMemoryMB, Resources, loadAndProbeLocked, etc. — all go through
// this path deliberately).
//
// A PRESENT-but-unparseable file (disk corruption, a hand-edit gone wrong, an
// old non-atomic write torn by a crash) is a DIFFERENT failure mode from
// "absent" and used to be handled identically — silently as empty — which is
// exactly what let a corrupt file be lost forever: readFile's caller in
// SetCap/SetMinFreeMemoryMB/SetResource/UnsetResource takes that empty File
// and unconditionally rewrites governor.json wholesale, permanently erasing
// every OTHER pool's operator cap and the machine resource ledger, and
// quietly RELAXING (or tightening) every cap back to the package default in
// the meantime (koryph audit finding #27). readFile still fails open here
// (so admission itself is never the thing that blocks on this), but it now
// backs the corrupt file up first — see backupCorrupt — so the original
// bytes always survive even if a subsequent write clobbers governor.json.
// Write paths use readFileForWrite instead, which turns this same corruption
// into a hard error rather than silently proceeding to overwrite it.
func (s *Store) readFile() (File, error) {
	f, corrupt := s.readFileRaw()
	if corrupt {
		s.backupCorrupt()
	}
	if f.Pools == nil {
		f.Pools = map[string]Config{}
	}
	return f, nil
}

// readFileForWrite is readFile's fail-CLOSED counterpart for the
// Set*/Unset* mutators (koryph audit finding #27): every one of them reads
// governor.json, mutates the in-memory File, and then unconditionally
// rewrites the WHOLE file — so silently swallowing a corrupt file as empty
// (readFile's admission-path behavior) would make an ordinary `governor set`
// permanently wipe every other pool's config and the resource ledger with no
// warning. A present-but-corrupt file is backed up (see backupCorrupt, same
// as readFile) and then returned as a hard error, forcing the operator to
// notice and resolve it before any write proceeds. An absent file still
// fails open to an empty File — first-run / freshly-initialized ~/.koryph is
// not corruption.
func (s *Store) readFileForWrite() (File, error) {
	f, corrupt := s.readFileRaw()
	if corrupt {
		s.backupCorrupt()
		return File{}, fmt.Errorf(
			"govern: %s exists but failed to parse; a copy was saved to %s — repair or remove it before writing",
			s.cfgPath, s.cfgPath+corruptBackupSuffix)
	}
	if f.Pools == nil {
		f.Pools = map[string]Config{}
	}
	return f, nil
}

// readFileRaw reads governor.json, additionally reporting whether the file
// EXISTS but failed to parse (as opposed to simply being absent) — the
// distinction readFile/readFileForWrite need to choose fail-open vs
// fail-closed handling. Absent (os.ErrNotExist) is never "corrupt".
func (s *Store) readFileRaw() (f File, corrupt bool) {
	err := fsx.ReadJSON(s.cfgPath, &f)
	if err == nil {
		return f, false
	}
	if errors.Is(err, os.ErrNotExist) {
		return File{Pools: map[string]Config{}}, false
	}
	return File{Pools: map[string]Config{}}, true
}

// backupCorrupt best-effort copies the current (unparseable) governor.json to
// a sibling ".corrupt-backup" file so the original bytes are recoverable
// after a fail-open read or a refused write. Idempotent and non-overwriting:
// once a backup exists it is left alone, so the FIRST corruption observed —
// the one most likely to still resemble the operator's real config, before
// any further writes land — is the one preserved, not clobbered by repeated
// detections of the same (or a newly, differently corrupt) file across many
// calls.
func (s *Store) backupCorrupt() {
	backup := s.cfgPath + corruptBackupSuffix
	if fsx.Exists(backup) {
		return
	}
	data, err := os.ReadFile(s.cfgPath)
	if err != nil {
		return
	}
	_ = fsx.WriteAtomic(backup, data, 0o644)
}

// prune drops leases whose agent pid is dead or that exceed LeaseTTL, and
// demand heartbeats whose engine pid is dead or that exceed DemandTTL, across
// ALL pools (pid liveness/TTL staleness are pool-agnostic facts).
func (s *Store) prune() error {
	leases, err := s.leaseFiles()
	if err != nil {
		return err
	}
	for name, l := range leases {
		// Before Bind the agent pid is 0 (reserved); fall back to the owning
		// engine pid so a fresh reservation is not pruned before launch.
		alivePID := l.PID
		if alivePID <= 0 {
			alivePID = l.EnginePID
		}
		if !s.Alive(alivePID) || s.expired(l.AcquiredAt, s.LeaseTTL) {
			_ = os.Remove(filepath.Join(s.slotsDir, name))
		}
	}
	dem, err := s.demandFiles()
	if err != nil {
		return err
	}
	for name, d := range dem {
		if !s.Alive(d.EnginePID) || s.expired(d.UpdatedAt, s.DemandTTL) {
			_ = os.Remove(filepath.Join(s.demandDir, name))
		}
	}
	return s.pruneCrashedProbe()
}

// pruneCrashedProbe resolves a half-open circuit breaker (koryph-2im.11), IN
// EVERY POOL that has one (koryph-v8u.11 — the breaker is now per-pool state)
// whose probe lease is gone — the agent pid died (pruned above, or never
// launched), or its owning engine crashed before ever calling Release/
// ReportRateLimit for it — without EITHER of the two definitive signals
// (Release's clean-close, ReportRateLimit's re-open) ever arriving. Neither
// signal can be inferred from "the lease file is gone" alone (a legitimate
// clean Release also removes it), so this waits out ProbeTimeout before
// deciding, and then conservatively RE-OPENS (doubled break) rather than
// closing — assuming failure is the safe direction; a spurious re-open only
// costs another wait, a spurious close could resume full admission on a
// still-throttled account. This is the "cannot wedge the breaker half-open
// forever" fallback the L5b design calls for.
func (s *Store) pruneCrashedProbe() error {
	f, err := s.readFile()
	if err != nil {
		return nil // absent/corrupt: checkGovernorConfig-style checks own this
	}
	changed := false
	for pool, c := range f.Pools {
		if !c.Adaptive || c.BreakerState != "half-open" || c.ProbeProject == "" {
			continue
		}
		if _, err := os.Stat(s.leasePath(pool, c.ProbeProject, c.ProbeBead)); err == nil {
			continue // probe lease still present — not resolved yet
		}
		timeout := s.ProbeTimeout
		if timeout <= 0 {
			timeout = 30 * time.Minute
		}
		admitted := parseTime(c.ProbeAdmittedAt)
		if admitted.IsZero() || s.Now().Sub(admitted) < timeout {
			continue // could still be mid-flight toward a normal Release/report
		}
		openBreaker(&c, s.Now(), true)
		f.Pools[pool] = c
		changed = true
		logCrashedProbeReopened(pool)
	}
	if !changed {
		return nil
	}
	return fsx.WriteJSONAtomic(s.cfgPath, f)
}

// expired reports whether ts (RFC3339) is older than ttl. An unparseable
// timestamp is treated as NOT expired (fall back to the pid-liveness check).
func (s *Store) expired(ts string, ttl time.Duration) bool {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return false
	}
	return s.Now().Sub(t) > ttl
}

// demanders returns the sorted, de-duplicated set of projects with live
// demand WITHIN pool, always including self (Acquire implies demand even if
// the heartbeat lagged).
func (s *Store) demanders(pool, self string) []string {
	set := map[string]struct{}{self: {}}
	dem, _ := s.demandForPool(pool)
	for _, d := range dem {
		set[d.Project] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for p := range set {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// epoch is the current rotation bucket for the fair-share remainder.
func (s *Store) epoch() int {
	w := s.RotateWindow
	if w <= 0 {
		w = time.Minute
	}
	return int(s.Now().Unix() / int64(w.Seconds()))
}

// leases returns every lease across ALL pools.
func (s *Store) leases() ([]Lease, error) {
	m, err := s.leaseFiles()
	if err != nil {
		return nil, err
	}
	out := make([]Lease, 0, len(m))
	for _, l := range m {
		out = append(out, l)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AcquiredAt < out[j].AcquiredAt })
	return out, nil
}

// leasesForPool returns only pool's leases (koryph-v8u.11).
func (s *Store) leasesForPool(pool string) ([]Lease, error) {
	all, err := s.leases()
	if err != nil {
		return nil, err
	}
	out := make([]Lease, 0, len(all))
	for _, l := range all {
		if NormalizeProvider(l.Provider) == pool {
			out = append(out, l)
		}
	}
	return out, nil
}

// demand returns every demand heartbeat across ALL pools.
func (s *Store) demand() ([]Demand, error) {
	m, err := s.demandFiles()
	if err != nil {
		return nil, err
	}
	out := make([]Demand, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Project < out[j].Project })
	return out, nil
}

// demandForPool returns only pool's demand heartbeats (koryph-v8u.11).
func (s *Store) demandForPool(pool string) ([]Demand, error) {
	all, err := s.demand()
	if err != nil {
		return nil, err
	}
	out := make([]Demand, 0, len(all))
	for _, d := range all {
		if NormalizeProvider(d.Provider) == pool {
			out = append(out, d)
		}
	}
	return out, nil
}

// leaseFiles maps lease filename -> Lease for every *.json directly in slotsDir.
func (s *Store) leaseFiles() (map[string]Lease, error) {
	entries, err := os.ReadDir(s.slotsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]Lease{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var l Lease
		if readJSON(filepath.Join(s.slotsDir, e.Name()), &l) && l.Project != "" {
			out[e.Name()] = l
		}
	}
	return out, nil
}

func (s *Store) demandFiles() (map[string]Demand, error) {
	entries, err := os.ReadDir(s.demandDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	out := map[string]Demand{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var d Demand
		if readJSON(filepath.Join(s.demandDir, e.Name()), &d) && d.Project != "" {
			out[e.Name()] = d
		}
	}
	return out, nil
}

// leasePath derives the lease filename for (provider, project, bead). pool
// must already be normalized (non-empty). The DefaultPool case deliberately
// keeps the pre-koryph-v8u.11 filename (no provider segment) so a lease
// written by an engine mid-upgrade is found by both old and new code paths;
// every other pool gets a namespaced filename to avoid collisions.
func (s *Store) leasePath(pool, project, bead string) string {
	if pool == DefaultPool {
		return filepath.Join(s.slotsDir, sanitize(project)+"__"+sanitize(bead)+".json")
	}
	return filepath.Join(s.slotsDir, sanitize(pool)+"__"+sanitize(project)+"__"+sanitize(bead)+".json")
}

// demandPath derives the demand-heartbeat filename for (provider, project);
// see leasePath for the DefaultPool back-compat naming rationale.
func (s *Store) demandPath(pool, project string) string {
	if pool == DefaultPool {
		return filepath.Join(s.demandDir, sanitize(project)+".json")
	}
	return filepath.Join(s.demandDir, sanitize(pool)+"__"+sanitize(project)+".json")
}

// withLock runs fn while holding an exclusive flock on slots/.lock. The flock is
// released by the OS if this process dies, so a crash cannot wedge the governor.
func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.slotsDir, ".lock"), os.O_CREATE|os.O_RDWR, 0o644)
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

// --- pure fair-share helpers ---------------------------------------------

// fairShare returns how many slots project p may hold this round. cap is spread
// over the sorted demanders: floor(cap/n) each, with the cap%n remainder handed
// to a rotating window of demanders (so zero-share turns, when n > cap, rotate
// and nobody starves).
func fairShare(cap int, demanders []string, p string, epoch int) int {
	n := len(demanders)
	if n == 0 {
		return cap
	}
	idx := indexOf(demanders, p)
	if idx < 0 {
		return 0
	}
	base := cap / n
	rem := cap % n
	if rem == 0 {
		return base
	}
	// The rem extra slots go to demanders whose rotated position is < rem.
	if ((idx + epoch) % n) < rem {
		return base + 1
	}
	return base
}

func countProject(leases []Lease, project string) int {
	n := 0
	for _, l := range leases {
		if l.Project == project {
			n++
		}
	}
	return n
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}

// containsStr reports whether v is in ss (a lease's resolved resource kinds
// are a small slice, so a linear scan is fine). koryph-4ql.1.
func containsStr(ss []string, v string) bool {
	for _, x := range ss {
		if x == v {
			return true
		}
	}
	return false
}

// --- small utilities ------------------------------------------------------

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// processAlive reports whether pid is a live process (signal-0 probe).
func processAlive(pid int) bool { return procx.Alive(pid) }

// sanitize keeps a filename to a safe charset; anything else becomes '-'.
func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
