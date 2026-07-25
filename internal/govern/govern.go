// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	cryptorand "crypto/rand"
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
	"github.com/koryph/koryph/internal/sysmem"
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

	// PressureReliefClaimTTL bounds how long an unacknowledged machine-memory
	// relief claim remains exclusively owned after its engine dies. A takeover
	// preserves the original target, so recovery can finish or acknowledge that
	// one cohort without selecting and terminating another in the same pressure
	// episode.
	PressureReliefClaimTTL time.Duration

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
		slotsDir:               paths.SlotsDir(),
		demandDir:              paths.DemandDir(),
		cfgPath:                paths.GovernorConfig(),
		Now:                    time.Now,
		Alive:                  processAlive,
		RotateWindow:           time.Minute,
		DemandTTL:              10 * time.Minute,
		LeaseTTL:               24 * time.Hour,
		ProbeTimeout:           30 * time.Minute,
		PressureReliefClaimTTL: DefaultPressureReliefClaimTTL,
		Jitter:                 func() float64 { return mathrand.Float64() - 0.5 },
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

// EstPerAgentMB returns provider's RAW configured per-agent memory reservation
// for kind-less leases (koryph-3xs): >0 an explicit reservation in MB, <0 the
// reservation explicitly disabled, 0 unset (callers apply DefaultEstPerAgentMB).
// Returns 0 (unset) when governor.json or the pool entry is absent, or on any
// read error, matching MinFreeMemoryMB's fail-open posture.
func (s *Store) EstPerAgentMB(provider string) int {
	pool := NormalizeProvider(provider)
	f, err := s.readFile()
	if err != nil {
		return 0
	}
	c, ok := f.Pools[pool]
	if !ok {
		return 0
	}
	return c.EstPerAgentMB
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

// MachineCeiling returns the resolved machine-wide agent ceiling across ALL
// pools (koryph-4rk6.2): the configured max_machine_agents when >0, else
// DefaultMaxMachineAgents. Fails open to the default when governor.json is
// absent or unreadable, matching the Cap/Resources fail-open posture. A plain
// unlocked read (like Cap/MinFreeMemoryMB) — doctor and status only RENDER it.
func (s *Store) MachineCeiling() int {
	f, err := s.readFile()
	if err != nil {
		return DefaultMaxMachineAgents
	}
	return f.MachineCeiling()
}

// SetMachineCeiling writes the machine-wide agent ceiling to governor.json,
// PRESERVING every pool config and the resource ledger (the SetResource
// preserve-don't-reset precedent, NOT SetCap's wholesale reset). n<=0 is
// rejected — the ceiling is a positive count; to revert to the default, an
// operator removes the key. Relies on File.UnmarshalJSON decoding the field on
// read so an earlier set is not stripped by this whole-file rewrite.
func (s *Store) SetMachineCeiling(n int) error {
	if n <= 0 {
		return errors.New("govern: max_machine_agents must be positive")
	}
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		f.MaxMachineAgents = n
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
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
//
// Memory floor (koryph-4rk6.1): the wholesale reset would otherwise also
// silently drop the pool's min_free_memory_mb (a plain int field, zeroed by
// the fresh Config{} literal below) — the exact "cap configured, floor
// missing" shape that let "personal"/"work" run floorless in the 2026-07-21
// incident. So this preserves the pool's PRIOR explicit floor (whatever it
// was — positive, or negative/disabled) across a cap-only change, and only
// seeds DefaultMinFreeMemoryMB when the pool is being created fresh here (no
// prior entry) or its floor was never configured (raw 0). An operator's
// explicit reset-to-auto/disable made via SetMinFreeMemoryMB in a SEPARATE
// call is untouched by any OTHER pool's SetCap (only this same pool's own
// prior value is read).
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
		floor := f.Pools[pool].MinFreeMemoryMB // 0 for a brand-new pool
		if floor == 0 {
			floor = DefaultMinFreeMemoryMB
		}
		f.Pools[pool] = Config{MaxGlobalAgents: n, MinFreeMemoryMB: floor}
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
}

// BackfillMemoryFloors seeds DefaultMinFreeMemoryMB onto every EXISTING pool
// in governor.json that carries no explicit memory floor (raw
// MinFreeMemoryMB == 0 — koryph-4rk6.1): the migration for a governor.json an
// older koryph version already wrote before this field's uniform-default
// policy existed, e.g. the incident's "personal"/"work" pools (a
// max_global_agents cap but no min_free_memory_mb at all). A pool with an
// explicit floor already set (positive, or negative/disabled) is left
// completely untouched.
//
// This is a deliberately explicit, caller-invoked "on load" step — NOT wired
// into every mutating Set*/Unset* call — so a later, unrelated write (e.g.
// `governor set-resource`) never silently re-seeds a pool an operator
// separately, deliberately reset to 0 (auto) via SetMinFreeMemoryMB earlier
// in the same session. Callers (the engine run startup, `koryph doctor
// --fix`-style tooling) should invoke it once per load, not per mutation.
//
// Returns the sorted list of pool names it changed (for logging/tests) and
// performs no write at all when nothing needed backfilling. A pool that
// exists only via a live lease/demand heartbeat (no governor.json entry) is
// not touched — nothing to persist a floor onto until it gets an explicit
// Config entry (e.g. via SetCap).
func (s *Store) BackfillMemoryFloors() ([]string, error) {
	var changed []string
	err := s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		for pool, c := range f.Pools {
			if c.MinFreeMemoryMB != 0 {
				continue
			}
			c.MinFreeMemoryMB = DefaultMinFreeMemoryMB
			f.Pools[pool] = c
			changed = append(changed, pool)
		}
		if len(changed) == 0 {
			return nil
		}
		sort.Strings(changed)
		return fsx.WriteJSONAtomic(s.cfgPath, f)
	})
	return changed, err
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

// SetEstPerAgentMB writes provider's kind-less per-agent memory reservation
// (koryph-3xs) to governor.json, PRESERVING every other field of that pool's
// config (cap, floor, AIMD overlay) — the SetMinFreeMemoryMB precedent. The
// value is interpreted by readers: mb>0 an explicit reservation, mb<0 disables
// it (kind-less leases reserve 0), mb==0 resets to the package default
// (DefaultEstPerAgentMB). A pool that does not yet exist is created with only
// this field set. provider=="" is DefaultPool.
func (s *Store) SetEstPerAgentMB(provider string, mb int) error {
	pool := NormalizeProvider(provider)
	return s.withLock(func() error {
		f, err := s.readFileForWrite()
		if err != nil {
			return err
		}
		c := f.Pools[pool] // zero Config when the pool is absent
		c.EstPerAgentMB = mb
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
// Legacy clauses fail open on a lease-read error (I6 compatibility). A
// pressure-aware HostInput instead returns a typed safety deferral: missing
// count/reservation evidence must not restore unrestricted admission.
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

		// A machine-global pressure episode is admission-authoritative across
		// every runner, including a fresh process with no local PressureState.
		// Read it under the same governor flock as the lease decision. Corrupt
		// or unreadable shared state fails closed: uncertainty must not restore
		// unrestricted dispatch.
		_, episodeExists, episodeErr := s.readPressureEpisode()
		_, claimExists, claimErr := s.readPressureReliefClaim()
		if episodeErr != nil || claimErr != nil || episodeExists || claimExists {
			pressure := sysmem.PressureUnknown
			origin := sysmem.SampleDegraded
			liveRSS := 0
			if mem.Host != nil {
				pressure = effectivePressure(mem.Host)
				origin = mem.Host.Sample.Origin
				liveRSS = mem.Host.LiveRSSMB
			}
			detail := "shared-pressure-episode"
			if episodeErr != nil || claimErr != nil {
				detail = "shared-pressure-state-unreadable"
			}
			result = AdmitResult{
				Outcome:     AdmitDeniedPressure,
				Pressure:    pressure,
				ProbeOrigin: origin,
				CircuitOpen: true,
				Detail:      detail,
				LiveRSSMB:   liveRSS,
			}
			return nil
		}

		// Machine-wide ceiling across ALL pools (koryph-4rk6.2): even when this
		// pool has room under its own cap (and even for the half-open breaker
		// probe below), the sum of live leases over EVERY provider pool must not
		// exceed the machine ceiling — per-pool caps protect each provider API,
		// this protects the host. Checked here, before the adaptive/probe and
		// pool-cap sections, so it gates every admission path. The candidate's
		// own lease (same project+bead — a re-acquire or a stale reservation) is
		// excluded so it is never counted against itself. Fails OPEN on a read
		// error (I6), like the resource clauses: a denial is a deferral, not an
		// error. It is a machine-wide (not per-bead) condition, so it maps to
		// AdmitDeniedCap and the engine batch-breaks (nothing else fits either).
		machineActiveCount := 0
		machineCountKnown := false
		if all, lerr := s.leases(); lerr == nil {
			ceiling := f.MachineCeiling()
			machineActiveCount = machineActive(all, l)
			machineCountKnown = true
			if machineActiveCount >= ceiling {
				result = AdmitResult{Outcome: AdmitDeniedCap}
				logMachineCeiling(pool, l.Project, l.Bead, ceiling, machineActiveCount)
				return nil
			}
		}

		// Kernel pressure is machine-wide and therefore precedes provider
		// breaker/fair-share and per-bead resource checks. A warning, critical
		// band, rising swap/compressor trend, open pressure circuit, or
		// unbounded degraded probe denies admission without claiming a
		// half-open provider probe.
		if denial := checkPressure(mem.Host, machineActiveCount, machineCountKnown); denial != nil {
			result = *denial
			return nil
		}

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
// no subprocess (I7). Legacy input retains I6 fail-open behavior; pressure-
// aware input fails safe with a typed denial when the lease ledger is
// unreadable. Callers must already hold the store's flock and have pruned.
//
// The candidate's own lease (same project+bead — a re-acquire or a stale
// reservation) is excluded from both the capacity count and the ramping
// reservation sum so it is never charged against itself.
func (s *Store) checkResourcesLocked(rc *ResourcesConfig, cand Lease, mem MemInput, now time.Time) *AdmitResult {
	memActive := mem.Host != nil || (mem.AvailMB > 0 && mem.FloorMB > 0)
	if len(cand.Resources) == 0 && !memActive {
		return nil // nothing declared and no reading → no machine clause applies
	}
	all, err := s.leases() // every pool: machine resources are cross-pool
	if err != nil {
		if mem.Host != nil {
			return &AdmitResult{
				Outcome:     AdmitDeniedPressure,
				Pressure:    effectivePressure(mem.Host),
				ProbeOrigin: mem.Host.Sample.Origin,
				CircuitOpen: mem.Host.CircuitOpen,
				Detail:      "reservation-ledger-unavailable",
				LiveRSSMB:   mem.Host.LiveRSSMB,
			}
		}
		return nil // legacy compatibility: no pressure-aware safety state
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

	// Clause 2a — pressure-aware reservation budget. Under NORMAL Darwin
	// pressure, low free/speculative/purgeable page counts are not by
	// themselves a denial. Instead, project from observed live cohort RSS and
	// outstanding ramp reservations, charging the larger of the candidate's
	// declared reservation and its calibrated runtime estimate:
	//
	//   liveRSS + rampReservations + candidateEstimate <= total - floor
	//
	// Warning/critical/degraded handling already ran in checkPressure.
	if host := mem.Host; host != nil {
		reserved := 0
		for _, l := range others {
			if l.MemReserveMB > 0 && leaseRamping(l, rc, now) {
				reserved += l.MemReserveMB
			}
		}
		candidateMB := cand.MemReserveMB
		if host.ObservedEstimateMB > candidateMB {
			candidateMB = host.ObservedEstimateMB
		}
		totalMB := int(host.Sample.TotalMB())
		floorMB := mem.FloorMB
		if floorMB < 0 {
			floorMB = 0
		}
		if totalMB > 0 {
			budget := totalMB - floorMB
			if budget < 0 {
				budget = 0
			}
			base := host.LiveRSSMB + reserved
			if base+candidateMB > budget {
				return &AdmitResult{
					Outcome:           AdmitDeniedReservation,
					CandidateTipped:   base <= budget,
					Pressure:          effectivePressure(host),
					ProbeOrigin:       host.Sample.Origin,
					CircuitOpen:       host.CircuitOpen,
					Detail:            "reservation-budget",
					LiveRSSMB:         host.LiveRSSMB,
					ReservedMB:        reserved,
					CandidateMemoryMB: candidateMB,
					MemoryBudgetMB:    budget,
				}
			}
		}
		return nil
	}

	// Clause 2b — legacy reservation-aware conservative page floor (L5), only
	// with a real reading and no HostInput:
	//   availMB − Σ(ramping leases' MemReserveMB) − candidate MemReserveMB ≥ floorMB
	// Signed arithmetic avoids uint underflow when reservations exceed avail.
	if mem.AvailMB > 0 && mem.FloorMB > 0 {
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

// checkPressure applies the machine-wide pressure clauses. machineCountKnown
// is false only when the lease inventory could not be read. That uncertainty
// fails closed only for degraded pressure samples; fresh/last-good known bands
// continue to follow their explicit kernel state.
func checkPressure(host *HostInput, active int, machineCountKnown bool) *AdmitResult {
	if host == nil {
		return nil
	}
	pressure := effectivePressure(host)
	denied := func(p sysmem.PressureBand, circuit bool, detail string) *AdmitResult {
		return &AdmitResult{
			Outcome:     AdmitDeniedPressure,
			Pressure:    p,
			ProbeOrigin: host.Sample.Origin,
			CircuitOpen: circuit,
			Detail:      detail,
			LiveRSSMB:   host.LiveRSSMB,
		}
	}
	if !machineCountKnown {
		return denied(sysmem.PressureUnknown, host.CircuitOpen, "machine-count-unavailable")
	}
	if host.CircuitOpen {
		return denied(pressure, true, "pressure-circuit-open")
	}
	if host.Sample.Origin == sysmem.SampleDegraded || !pressure.Known() {
		if active >= 1 {
			return denied(sysmem.PressureUnknown, false, "pressure-probe-degraded")
		}
		// With no usable probe, allow exactly the first active agent. Count,
		// declared-resource, and provider constraints still apply below.
		return nil
	}
	if !host.LiveRSSKnown && active >= 1 {
		return denied(pressure, false, "live-rss-unavailable")
	}
	if pressure == sysmem.PressureCritical || pressure == sysmem.PressureWarning {
		return denied(pressure, false, "kernel-pressure-"+pressure.String())
	}
	if trendWarns(host.Trend, host.Policy) {
		return denied(sysmem.PressureWarning, false, "swap-compressor-rising")
	}
	return nil
}

func effectivePressure(host *HostInput) sysmem.PressureBand {
	if host == nil {
		return sysmem.PressureUnknown
	}
	if host.EffectivePressure.Known() {
		return host.EffectivePressure
	}
	return host.Sample.Pressure
}

func trendWarns(trend sysmem.Trend, policy PressurePolicy) bool {
	policy = policy.normalized()
	const mib = int64(1024 * 1024)
	return trend.SwapBytes >= policy.SwapRiseMB*mib ||
		trend.CompressedBytes >= policy.CompressorRiseMB*mib
}

// AdvancePressure is the pure pressure hysteresis/calibration primitive the
// engine calls after resolving a sysmem sample. It stops admission immediately
// on warning/critical or a meaningful rising trend; opens the durable circuit
// only after critical pressure is both repeated and sustained; and requires
// several normal samples before reporting relief. A degraded probe never
// mutates a known state because sysmem's bounded last-good resolver owns that
// transition.
func AdvancePressure(
	previous PressureState,
	sample sysmem.PressureSample,
	trend sysmem.Trend,
	now time.Time,
	policy PressurePolicy,
) PressureTransition {
	policy = policy.normalized()
	now = now.UTC()
	state := previous
	if sample.Origin == sysmem.SampleDegraded || !sample.Pressure.Known() {
		return PressureTransition{
			State: state, Effective: sysmem.PressureUnknown,
		}
	}
	if sample.Origin == sysmem.SampleLastGood {
		effective := state.Effective
		if !effective.Known() {
			effective = sample.Pressure
		}
		return PressureTransition{State: state, Effective: effective}
	}

	observed := sample.Pressure
	trendWarning := trendWarns(trend, policy)
	if observed == sysmem.PressureNormal && trendWarning {
		observed = sysmem.PressureWarning
	}

	transition := PressureTransition{State: state, Effective: observed, TrendWarning: trendWarning}
	switch observed {
	case sysmem.PressureCritical:
		if state.CriticalSince == "" {
			state.CriticalSince = now.Format(time.RFC3339Nano)
			state.CriticalSamples = 0
		}
		if state.CriticalSamples < policy.CriticalSamples {
			state.CriticalSamples++
		}
		state.NormalSamples = 0
		state.Effective = sysmem.PressureCritical
		since := parseTime(state.CriticalSince)
		persistent := state.CriticalSamples >= policy.CriticalSamples &&
			!since.IsZero() && now.Sub(since) >= policy.CriticalFor
		if persistent && !state.CircuitOpen {
			state.CircuitOpen = true
			transition.OpenCircuit = true
		}
	case sysmem.PressureWarning:
		state.Effective = sysmem.PressureWarning
		state.NormalSamples = 0
		state.CriticalSince = ""
		state.CriticalSamples = 0
	case sysmem.PressureNormal:
		state.CriticalSince = ""
		state.CriticalSamples = 0
		if state.NormalSamples < policy.RecoverySamples {
			state.NormalSamples++
		}
		if !state.Effective.Known() || state.Effective == sysmem.PressureNormal ||
			state.NormalSamples >= policy.RecoverySamples {
			state.Effective = sysmem.PressureNormal
		}
		if state.CircuitOpen && state.NormalSamples >= policy.RecoverySamples {
			transition.ReliefReady = true
		}
	}
	transition.State = state
	transition.Effective = state.Effective
	return transition
}

// AcknowledgePressureRelief closes an open pressure circuit only after the
// engine has completed its identity-checked graceful relief action. It is a
// no-op until AdvancePressure has observed the configured normal hysteresis.
func AcknowledgePressureRelief(state PressureState, policy PressurePolicy) PressureState {
	policy = policy.normalized()
	if state.CircuitOpen && state.Effective == sysmem.PressureNormal &&
		state.NormalSamples >= policy.RecoverySamples {
		state.CircuitOpen = false
		state.CriticalSince = ""
		state.CriticalSamples = 0
	}
	return state
}

const pressureReliefClaimSchema = "koryph.pressure-relief-claim/v1"
const pressureEpisodeSchema = "koryph.pressure-episode/v1"

func (s *Store) pressureReliefClaimPath() string {
	return filepath.Join(s.slotsDir, "control", "pressure-relief.json")
}

func (s *Store) pressureEpisodePath() string {
	return filepath.Join(s.slotsDir, "control", "pressure-episode.json")
}

func validatePressureReliefRequest(req PressureReliefRequest) error {
	target := req.Target
	if req.OwnerProject == "" || req.OwnerRunID == "" || req.OwnerEnginePID <= 0 ||
		req.OwnerProcessIdentity == "" {
		return errors.New("govern: pressure relief owner identity is incomplete")
	}
	if target.Project == "" || target.RunID == "" || target.PhaseID == "" ||
		target.BeadID == "" || target.PID <= 0 || target.ProcessIdentity == "" ||
		parseTime(target.DispatchedAt).IsZero() {
		return errors.New("govern: pressure relief target identity is incomplete")
	}
	return nil
}

func (s *Store) writePressureReliefClaim(claim PressureReliefClaim) error {
	path := s.pressureReliefClaimPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteJSONAtomicPerm(path, claim, 0o600)
}

func (s *Store) readPressureReliefClaim() (PressureReliefClaim, bool, error) {
	var claim PressureReliefClaim
	err := fsx.ReadJSON(s.pressureReliefClaimPath(), &claim)
	if errors.Is(err, os.ErrNotExist) {
		return PressureReliefClaim{}, false, nil
	}
	if err != nil {
		return PressureReliefClaim{}, false, err
	}
	if claim.Schema != pressureReliefClaimSchema || claim.ID == "" {
		return PressureReliefClaim{}, false, errors.New("govern: invalid pressure relief claim")
	}
	return claim, true, nil
}

func (s *Store) writePressureEpisode(episode PressureEpisode) error {
	path := s.pressureEpisodePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsx.WriteJSONAtomicPerm(path, episode, 0o600)
}

func (s *Store) readPressureEpisode() (PressureEpisode, bool, error) {
	var episode PressureEpisode
	err := fsx.ReadJSON(s.pressureEpisodePath(), &episode)
	if errors.Is(err, os.ErrNotExist) {
		return PressureEpisode{}, false, nil
	}
	if err != nil {
		return PressureEpisode{}, false, err
	}
	if episode.Schema != pressureEpisodeSchema || episode.ID == "" ||
		parseTime(episode.OpenedAt).IsZero() {
		return PressureEpisode{}, false, errors.New("govern: invalid pressure episode")
	}
	return episode, true, nil
}

// OpenPressureEpisode publishes the admission circuit independently of target
// election. The first caller fixes the episode ID; concurrent and later
// callers observe the same episode until fresh-normal hysteresis CAS-recovers
// it.
func (s *Store) OpenPressureEpisode(
	req PressureEpisodeRequest,
) (PressureEpisode, bool, error) {
	if req.Project == "" || req.RunID == "" || req.EnginePID <= 0 {
		return PressureEpisode{}, false, errors.New("govern: pressure episode source is incomplete")
	}
	var result PressureEpisode
	opened := false
	err := s.withLock(func() error {
		existing, exists, err := s.readPressureEpisode()
		if err != nil {
			return err
		}
		if exists {
			result = existing
			return nil
		}
		var nonce [16]byte
		if _, err := cryptorand.Read(nonce[:]); err != nil {
			return fmt.Errorf("govern: generate pressure episode id: %w", err)
		}
		now := s.Now().UTC()
		result = PressureEpisode{
			Schema:          pressureEpisodeSchema,
			ID:              fmt.Sprintf("%d-%d-%x", now.UnixNano(), req.EnginePID, nonce),
			OpenedByProject: req.Project,
			OpenedByRunID:   req.RunID,
			OpenedByPID:     req.EnginePID,
			OpenedAt:        now.Format(time.RFC3339Nano),
		}
		if err := s.writePressureEpisode(result); err != nil {
			return err
		}
		opened = true
		return nil
	})
	return result, opened, err
}

// PressureEpisodeStatus returns the durable machine-global admission circuit.
func (s *Store) PressureEpisodeStatus() (PressureEpisode, bool, error) {
	var episode PressureEpisode
	var exists bool
	err := s.withLock(func() error {
		var err error
		episode, exists, err = s.readPressureEpisode()
		return err
	})
	return episode, exists, err
}

// ClaimPressureRelief serializes the one graceful cohort action allowed during
// a persistent machine-pressure episode. The first caller fixes Target. Other
// runs observe but cannot act; after an unacknowledged owner's death and a
// bounded timeout, a new owner may finish only that same target. An
// acknowledged claim continues blocking new targets until
// RecoverPressureRelief clears the episode after fresh normal hysteresis.
func (s *Store) ClaimPressureRelief(
	req PressureReliefRequest,
) (PressureReliefClaim, bool, error) {
	if err := validatePressureReliefRequest(req); err != nil {
		return PressureReliefClaim{}, false, err
	}
	var result PressureReliefClaim
	acquired := false
	err := s.withLock(func() error {
		now := s.Now().UTC()
		existing, ok, err := s.readPressureReliefClaim()
		if err != nil {
			return err
		}
		if ok {
			result = existing
			if existing.AcknowledgedAt != "" {
				return nil
			}
			if existing.OwnerProject == req.OwnerProject &&
				existing.OwnerRunID == req.OwnerRunID &&
				existing.OwnerEnginePID == req.OwnerEnginePID &&
				existing.OwnerProcessIdentity == req.OwnerProcessIdentity {
				acquired = true
				return nil
			}
			ttl := s.PressureReliefClaimTTL
			if ttl <= 0 {
				ttl = DefaultPressureReliefClaimTTL
			}
			claimedAt := parseTime(existing.ClaimedAt)
			ownerPIDAlive := s.Alive != nil && s.Alive(existing.OwnerEnginePID)
			ownerIdentityKnown := req.ObservedOwnerProcessIdentity != ""
			ownerIdentityBound := existing.OwnerProcessIdentity != ""
			ownerIdentityMatches := ownerIdentityBound && ownerIdentityKnown &&
				req.ObservedOwnerProcessIdentity == existing.OwnerProcessIdentity
			// A claim written by an older binary has no process identity. Treat
			// its live PID as the owner and require the ordinary dead-owner TTL;
			// an absent legacy field must not masquerade as proof of PID reuse.
			ownerReused := ownerIdentityBound && ownerIdentityKnown && !ownerIdentityMatches
			ownerAlive := ownerPIDAlive && !ownerReused
			if ownerAlive || claimedAt.IsZero() ||
				(!ownerReused && now.Sub(claimedAt) < ttl) {
				return nil
			}
			// Take over ownership but preserve the original immutable target.
			existing.OwnerProject = req.OwnerProject
			existing.OwnerRunID = req.OwnerRunID
			existing.OwnerEnginePID = req.OwnerEnginePID
			existing.OwnerProcessIdentity = req.OwnerProcessIdentity
			existing.ClaimedAt = now.Format(time.RFC3339Nano)
			if err := s.writePressureReliefClaim(existing); err != nil {
				return err
			}
			result = existing
			acquired = true
			return nil
		}

		var nonce [16]byte
		if _, err := cryptorand.Read(nonce[:]); err != nil {
			return fmt.Errorf("govern: generate pressure relief claim id: %w", err)
		}
		result = PressureReliefClaim{
			Schema:               pressureReliefClaimSchema,
			ID:                   fmt.Sprintf("%d-%d-%x", now.UnixNano(), req.OwnerEnginePID, nonce),
			OwnerProject:         req.OwnerProject,
			OwnerRunID:           req.OwnerRunID,
			OwnerEnginePID:       req.OwnerEnginePID,
			OwnerProcessIdentity: req.OwnerProcessIdentity,
			Target:               req.Target,
			ClaimedAt:            now.Format(time.RFC3339Nano),
		}
		if err := s.writePressureReliefClaim(result); err != nil {
			return err
		}
		acquired = true
		return nil
	})
	return result, acquired, err
}

// AcknowledgePressureReliefClaim records that the claim owner either sent the
// identity-checked SIGTERM successfully or proved the originally claimed
// process has exited. The claim remains as the episode tombstone.
func (s *Store) AcknowledgePressureReliefClaim(
	claimID, ownerProject, ownerRunID string,
	ownerEnginePID int,
	ownerProcessIdentity string,
) error {
	return s.withLock(func() error {
		claim, ok, err := s.readPressureReliefClaim()
		if err != nil {
			return err
		}
		if !ok {
			return errors.New("govern: pressure relief claim is missing")
		}
		if claim.ID != claimID || claim.OwnerProject != ownerProject ||
			claim.OwnerRunID != ownerRunID ||
			claim.OwnerEnginePID != ownerEnginePID ||
			claim.OwnerProcessIdentity != ownerProcessIdentity {
			return errors.New("govern: pressure relief claim ownership changed")
		}
		if claim.AcknowledgedAt != "" {
			return nil
		}
		claim.AcknowledgedAt = s.Now().UTC().Format(time.RFC3339Nano)
		return s.writePressureReliefClaim(claim)
	})
}

// RecoverPressureRelief compare-and-deletes exactly expectedClaimID, and only
// after that claim is acknowledged. It never removes an unacknowledged claim
// or a newer episode installed after the caller's observation.
func (s *Store) RecoverPressureRelief(expectedClaimID string) (bool, error) {
	recovered := false
	err := s.withLock(func() error {
		claim, exists, err := s.readPressureReliefClaim()
		if err != nil {
			return err
		}
		if !exists {
			recovered = true
			return nil
		}
		if expectedClaimID == "" || claim.ID != expectedClaimID ||
			claim.AcknowledgedAt == "" {
			return nil
		}
		if err := os.Remove(s.pressureReliefClaimPath()); err != nil {
			return err
		}
		recovered = true
		return nil
	})
	return recovered, err
}

// RecoverPressureEpisode compare-and-deletes the exact episode observed after
// fresh-normal hysteresis. If a relief claim exists, the same locked operation
// requires the exact observed claim to be acknowledged before removing either
// record. A claim appearing after the caller's observation therefore blocks
// recovery rather than being orphaned or accidentally erased.
func (s *Store) RecoverPressureEpisode(
	expectedEpisodeID, expectedClaimID string,
) (bool, error) {
	recovered := false
	err := s.withLock(func() error {
		episode, episodeExists, err := s.readPressureEpisode()
		if err != nil {
			return err
		}
		if !episodeExists {
			recovered = true
			return nil
		}
		if expectedEpisodeID == "" || episode.ID != expectedEpisodeID {
			return nil
		}
		claim, claimExists, err := s.readPressureReliefClaim()
		if err != nil {
			return err
		}
		if claimExists {
			if expectedClaimID == "" || claim.ID != expectedClaimID ||
				claim.AcknowledgedAt == "" {
				return nil
			}
			if err := os.Remove(s.pressureReliefClaimPath()); err != nil {
				return err
			}
		} else if expectedClaimID != "" {
			return nil
		}
		if err := os.Remove(s.pressureEpisodePath()); err != nil {
			return err
		}
		recovered = true
		return nil
	})
	return recovered, err
}

// PressureReliefStatus returns the optional target-bearing relief claim without
// mutating it. PressureEpisodeStatus is the independent admission authority.
func (s *Store) PressureReliefStatus() (PressureReliefClaim, bool, error) {
	var claim PressureReliefClaim
	var ok bool
	err := s.withLock(func() error {
		var err error
		claim, ok, err = s.readPressureReliefClaim()
		return err
	})
	return claim, ok, err
}

// machineActive counts live leases across ALL pools for the machine-wide
// ceiling (koryph-4rk6.2), EXCLUDING the candidate's own lease (same
// project+bead — a re-acquire or a stale reservation) so a bead is never
// counted against itself. Pure; callers hold the flock and have pruned.
func machineActive(all []Lease, cand Lease) int {
	n := 0
	for _, l := range all {
		if l.Project == cand.Project && l.Bead == cand.Bead {
			continue
		}
		n++
	}
	return n
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
