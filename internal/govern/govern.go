// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package govern

import (
	"encoding/json"
	"errors"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

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
// (koryph-v8u.11).
func (s *Store) Cap(provider string) int {
	pool := NormalizeProvider(provider)
	f, err := s.readFile()
	if err != nil {
		return DefaultMaxGlobalAgents
	}
	c, ok := f.Pools[pool]
	if !ok || c.MaxGlobalAgents <= 0 {
		return DefaultMaxGlobalAgents
	}
	return c.MaxGlobalAgents
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
		f, err := s.readFile()
		if err != nil {
			return err
		}
		f.Pools[pool] = Config{MaxGlobalAgents: n}
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
func (s *Store) Acquire(l Lease) (bool, error) {
	pool := NormalizeProvider(l.Provider)
	l.Provider = pool // stored state never has an empty key (koryph-v8u.11)
	granted := false
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		// loadAndProbeLocked (koryph-2im.4/2im.11/koryph-v8u.11): the static
		// operator cap when this pool's AIMD overlay is off (byte-for-byte
		// the prior s.Cap() admission via c.EffectiveCap() below), or the
		// probed/backed-off dynamic cap — plus current settle/breaker/
		// smoothing state — when adaptive is enabled for this pool.
		c, f, err := s.loadAndProbeLocked(pool)
		if err != nil {
			return err
		}
		now := s.Now()

		if c.Adaptive {
			switch c.BreakerState {
			case "open":
				return nil // deny: zero admission in this pool while open
			case "half-open":
				if c.ProbeProject != "" || c.ProbeBead != "" {
					return nil // a probe is already outstanding; deny everyone else
				}
				c.ProbeProject = l.Project
				c.ProbeBead = l.Bead
				c.ProbeAdmittedAt = now.UTC().Format(time.RFC3339)
				c.LastAdmitAt = c.ProbeAdmittedAt
				if err := s.grantLease(l, now); err != nil {
					return err
				}
				granted = true
				f.Pools[pool] = c
				return fsx.WriteJSONAtomic(s.cfgPath, f) // persist the probe claim
			}

			// Dispatch smoothing (koryph-2im.11): closed-state admission only —
			// the probe above is a single, deliberate dispatch, not part of a
			// burst a spacing rule needs to defend against. A denial here must
			// NOT touch LastAdmitAt (smoothingDenies reads it fresh on the
			// engine's next refill-tick retry).
			if smoothingDenies(c, now, s.jitter()) {
				return nil
			}
		}

		cap := c.EffectiveCap()
		leases, err := s.leasesForPool(pool)
		if err != nil {
			return err
		}
		if len(leases) >= cap {
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
			return nil
		}

		if err := s.grantLease(l, now); err != nil {
			return err
		}
		granted = true

		if c.Adaptive {
			c.LastAdmitAt = now.UTC().Format(time.RFC3339)
			f.Pools[pool] = c
			if err := fsx.WriteJSONAtomic(s.cfgPath, f); err != nil {
				return err
			}
		}
		return nil
	})
	return granted, err
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
	return s.withLock(func() error {
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
				if werr := fsx.WriteJSONAtomic(s.cfgPath, f); werr != nil {
					return werr
				}
			}
		}
		return nil
	})
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
// single-pool document — see File.UnmarshalJSON). Absent/corrupt fails open
// to an empty pool map, matching this package's existing fail-open
// convention (a stuck/missing governor.json must never block dispatch).
func (s *Store) readFile() (File, error) {
	var f File
	if err := fsx.ReadJSON(s.cfgPath, &f); err != nil {
		return File{Pools: map[string]Config{}}, nil
	}
	if f.Pools == nil {
		f.Pools = map[string]Config{}
	}
	return f, nil
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

// --- small utilities ------------------------------------------------------

func readJSON(path string, v any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, v) == nil
}

// processAlive reports whether pid is a live process (signal-0 probe): ESRCH →
// false, EPERM → true (exists, not signalable).
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

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
