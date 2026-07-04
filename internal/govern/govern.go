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

// Cap returns the machine-wide cap, defaulting when governor.json is absent or
// unset.
func (s *Store) Cap() int {
	var c Config
	if err := fsx.ReadJSON(s.cfgPath, &c); err != nil || c.MaxGlobalAgents <= 0 {
		return DefaultMaxGlobalAgents
	}
	return c.MaxGlobalAgents
}

// SetCap writes the machine-wide cap to governor.json.
func (s *Store) SetCap(n int) error {
	if n <= 0 {
		return errors.New("govern: max_global_agents must be positive")
	}
	return fsx.WriteJSONAtomic(s.cfgPath, Config{MaxGlobalAgents: n})
}

// RefreshDemand records (or refreshes) this project's demand heartbeat: it has
// ready work and wants slots. Call once per wave while work remains.
func (s *Store) RefreshDemand(project string, enginePID int) error {
	return s.withLock(func() error {
		if err := os.MkdirAll(s.demandDir, 0o755); err != nil {
			return err
		}
		return fsx.WriteJSONAtomic(s.demandPath(project), Demand{
			Project:   project,
			EnginePID: enginePID,
			UpdatedAt: s.Now().UTC().Format(time.RFC3339),
		})
	})
}

// DropDemand removes this project's demand heartbeat (frontier drained / run
// ended), releasing it from the fair-share denominator.
func (s *Store) DropDemand(project string) error {
	return s.withLock(func() error {
		err := os.Remove(s.demandPath(project))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	})
}

// Acquire attempts to take a global slot for one agent. It prunes stale state,
// computes the caller's fair share, and grants iff the cap has room AND either
// the caller is under its fair share or every other demander already holds its
// share (work-conserving top-up). Returns whether a slot was granted.
//
// When the AIMD overlay is Adaptive, koryph-2im.11's circuit breaker and
// dispatch smoothing gate admission BEFORE the cap/fair-share checks below:
// a fully-open breaker denies everything (I5 holds — this only refuses a NEW
// lease, never revokes one already granted); a half-open breaker admits
// exactly one lease as the probe (the flock serializes concurrent callers, so
// exactly one wins the race) and nothing else until it resolves; closed
// admission is further spaced by the jittered minimum dispatch interval.
func (s *Store) Acquire(l Lease) (bool, error) {
	granted := false
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		// loadAndProbeLocked (koryph-2im.4/2im.11): the static operator cap
		// when the AIMD overlay is off (byte-for-byte the prior s.Cap()
		// admission via c.EffectiveCap() below), or the probed/backed-off
		// dynamic cap — plus current settle/breaker/smoothing state — when
		// adaptive is enabled.
		c, err := s.loadAndProbeLocked()
		if err != nil {
			return err
		}
		now := s.Now()

		if c.Adaptive {
			switch c.BreakerState {
			case "open":
				return nil // deny: zero admission machine-wide while open
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
				return fsx.WriteJSONAtomic(s.cfgPath, c) // persist the probe claim
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
		leases, err := s.leases()
		if err != nil {
			return err
		}
		if len(leases) >= cap {
			return nil // globally full
		}

		demanders := s.demanders(l.Project)
		myActive := countProject(leases, l.Project)

		// Strict fair share: a project may hold up to its share of the cap.
		// Idle capacity is reclaimed not by lending (agents are never
		// preempted) but when a project drains its frontier and drops its
		// demand — that shrinks the denominator and raises everyone else's
		// share on the next acquire.
		if myActive >= fairShare(cap, demanders, l.Project, s.epoch()) {
			return nil
		}

		if err := s.grantLease(l, now); err != nil {
			return err
		}
		granted = true

		if c.Adaptive {
			c.LastAdmitAt = now.UTC().Format(time.RFC3339)
			if err := fsx.WriteJSONAtomic(s.cfgPath, c); err != nil {
				return err
			}
		}
		return nil
	})
	return granted, err
}

// grantLease writes l's lease file, stamping AcquiredAt if unset. Callers
// must already hold the store's flock and have already decided admission.
func (s *Store) grantLease(l Lease, now time.Time) error {
	if l.AcquiredAt == "" {
		l.AcquiredAt = now.UTC().Format(time.RFC3339)
	}
	if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
		return err
	}
	return fsx.WriteJSONAtomic(s.leasePath(l.Project, l.Bead), l)
}

// jitter returns Store.Jitter() when set, else a process-global math/rand
// source (koryph-2im.11's dispatch-smoothing spread).
func (s *Store) jitter() float64 {
	if s.Jitter != nil {
		return s.Jitter()
	}
	return mathrand.Float64() - 0.5
}

// Hold unconditionally writes (or updates) a lease WITHOUT a cap check. It is
// the second half of the two-phase acquire: Acquire reserves a slot under the
// engine pid before launch (cap-checked), and Hold attaches the detached agent
// pid after launch so the lease is keyed to a process that outlives the engine.
// Because it skips the cap check it also correctly re-counts a requeued or
// resumed agent whose reservation was pruned in the death→relaunch gap — a 1:1
// replacement for an already-admitted bead, so it cannot breach the cap.
func (s *Store) Hold(l Lease) error {
	return s.withLock(func() error {
		if err := os.MkdirAll(s.slotsDir, 0o755); err != nil {
			return err
		}
		if l.AcquiredAt == "" {
			l.AcquiredAt = s.Now().UTC().Format(time.RFC3339)
		}
		return fsx.WriteJSONAtomic(s.leasePath(l.Project, l.Bead), l)
	})
}

// Release frees the slot held by (project, bead). A missing lease is not an
// error (idempotent / already pruned).
//
// Circuit breaker (koryph-2im.11): releasing the half-open probe's own lease
// WITHOUT a prior rate-limit report for it (ReportRateLimit would already
// have re-opened the breaker and cleared the probe identity — see
// applyRateLimit) is the "clean" signal that closes the breaker and resumes
// AIMD from DynamicCap=1. A probe that never reaches Release at all (crashed,
// or its owning engine died) is instead resolved by pruneCrashedProbe's
// timeout fallback.
func (s *Store) Release(project, bead string) error {
	return s.withLock(func() error {
		err := os.Remove(s.leasePath(project, bead))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}

		var c Config
		if rerr := fsx.ReadJSON(s.cfgPath, &c); rerr == nil &&
			c.Adaptive && c.BreakerState == "half-open" && bead != "" &&
			project == c.ProbeProject && bead == c.ProbeBead {
			closeBreaker(&c, s.Now())
			if werr := fsx.WriteJSONAtomic(s.cfgPath, c); werr != nil {
				return werr
			}
		}
		return nil
	})
}

// Prune removes dead/stale leases and demand heartbeats.
func (s *Store) Prune() error {
	return s.withLock(func() error { return s.prune() })
}

// FairShareFor returns project p's current fair share of the cap given the live
// demand set (p is always counted, since asking implies demand). Backs the
// per-project override warning.
func (s *Store) FairShareFor(project string) (int, error) {
	var share int
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		cap, err := s.effectiveCapLocked()
		if err != nil {
			return err
		}
		share = fairShare(cap, s.demanders(project), project, s.epoch())
		return nil
	})
	return share, err
}

// Snapshot returns the cap and the current (pruned) leases and demands, for
// `koryph governor`.
func (s *Store) Snapshot() (int, []Lease, []Demand, error) {
	var (
		leases []Lease
		dem    []Demand
	)
	err := s.withLock(func() error {
		if err := s.prune(); err != nil {
			return err
		}
		var e error
		if leases, e = s.leases(); e != nil {
			return e
		}
		dem, e = s.demand()
		return e
	})
	return s.Cap(), leases, dem, err
}

// --- internals (must be called under the lock) ---------------------------

// prune drops leases whose agent pid is dead or that exceed LeaseTTL, and
// demand heartbeats whose engine pid is dead or that exceed DemandTTL.
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

// pruneCrashedProbe resolves a half-open circuit breaker (koryph-2im.11)
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
	var c Config
	if err := fsx.ReadJSON(s.cfgPath, &c); err != nil {
		return nil // absent/corrupt: checkGovernorConfig-style checks own this
	}
	if !c.Adaptive || c.BreakerState != "half-open" || c.ProbeProject == "" {
		return nil
	}
	if _, err := os.Stat(s.leasePath(c.ProbeProject, c.ProbeBead)); err == nil {
		return nil // probe lease still present — not resolved yet
	}
	timeout := s.ProbeTimeout
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	admitted := parseTime(c.ProbeAdmittedAt)
	if admitted.IsZero() || s.Now().Sub(admitted) < timeout {
		return nil // could still be mid-flight toward a normal Release/report
	}
	openBreaker(&c, s.Now(), true)
	return fsx.WriteJSONAtomic(s.cfgPath, c)
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

// demanders returns the sorted, de-duplicated set of projects with live demand,
// always including self (Acquire implies demand even if the heartbeat lagged).
func (s *Store) demanders(self string) []string {
	set := map[string]struct{}{self: {}}
	dem, _ := s.demand()
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

func (s *Store) leasePath(project, bead string) string {
	return filepath.Join(s.slotsDir, sanitize(project)+"__"+sanitize(bead)+".json")
}

func (s *Store) demandPath(project string) string {
	return filepath.Join(s.demandDir, sanitize(project)+".json")
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
