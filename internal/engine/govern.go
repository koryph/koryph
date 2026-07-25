// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bufio"
	"context"
	"errors"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/resmon"
	"github.com/koryph/koryph/internal/strictjson"
	"github.com/koryph/koryph/internal/sysmem"
)

// The global concurrency governor caps concurrently running agents across ALL
// projects (koryph-1xk) so independent `koryph run` processes cannot
// collectively breach the Claude API rate limits. Legacy cap helpers fail open
// so a corrupt rate-limit rail cannot wedge the engine. Host-pressure admission
// is deliberately fail-safe: losing current memory/count evidence reduces
// concurrency rather than restoring unrestricted dispatch.

const (
	pressureStateName          = "pressure-state.json"
	runtimeMemoryEstimatesName = "runtime-memory-estimates.json"
)

// durablePressureState is intentionally a run-local sidecar rather than a
// ledger.Run field. Pressure state is engine control-plane state, not a slot
// result, and a sidecar lets restart recovery preserve it without coupling a
// safety-only additive checkpoint to the public ledger schema.
type durablePressureState struct {
	Schema        string               `json:"schema"`
	LastGood      *sysmem.Stat         `json:"last_good,omitempty"`
	PreviousFresh *sysmem.Stat         `json:"previous_fresh,omitempty"`
	State         govern.PressureState `json:"state"`
	ReliefHandled bool                 `json:"relief_handled,omitempty"`
	ReliefPhaseID string               `json:"relief_phase_id,omitempty"`
}

// durableRuntimeMemoryEstimates carries the last accepted calibrated value
// across runs. Fresh attempt evidence may propose a new value, but calibration
// receives this prior so small changes remain suppressed by hysteresis.
type durableRuntimeMemoryEstimates struct {
	Schema    string         `json:"schema"`
	Estimates map[string]int `json:"estimates"`
}

// poolKey is the governor pool every lease this engine constructs is admitted
// against (koryph-v8u.11, L5c: independent governor pools — see internal/govern's
// package doc). The pool is keyed on the resolved ACCOUNT (koryph-1o2.1), the
// same identity the quota ledger already uses (runner.quotaName() =
// Record.QuotaProfile ?? AccountProfile). This is the per-account concurrency
// lever: two accounts on one host — e.g. a large subscription and a smaller work
// seat — get INDEPENDENT pools, each with its own cap / fair-share / AIMD overlay
// / circuit breaker, because they have independent provider rate limits.
//
// An empty account name normalizes to govern.DefaultPool ("anthropic") inside
// every govern entry point, so a project with no resolved profile keeps today's
// single-pool behavior. Migration: an operator's existing `governor set
// --max-global` was scoped to the "anthropic" pool; a NAMED account (e.g.
// "personal"/"work") now resolves to its own pool and must have its cap set with
// `governor set --account <name>` (docs/user-guide/billing-and-quota.md).
func (r *runner) poolKey() string {
	// A runner with no resolved registry record (degenerate/test paths that
	// exercise the governor without a project) has no account, so it keeps the
	// default pool — exactly the old hardcoded constant's value, since
	// govern.NormalizeProvider("") == govern.DefaultPool.
	if r.rec == nil {
		return ""
	}
	return r.quotaName()
}

// seedCapForPool resolves the per-account seeded-default concurrency cap
// (koryph-1o2.3) for pool: r.quotaCfg.MaxThreads when pool is THIS runner's
// own pool, else 0 (no seed to offer). This is the same account→seed-cap
// mapping quota.SeedCap documents (internal/quota/governor.go) — the
// canonical contract is "account name → persisted Config.MaxThreads,
// fail-open to 0" — but reads the already-loaded r.quotaCfg in memory
// instead of calling quota.SeedCap, to avoid a redundant per-dispatch
// LoadConfig on this hot path. It is wired into r.gov.SeedCap at startup so
// govern's fallbackCap can consult it without govern importing package quota
// (layering) — govern calls this closure with an already
// govern.NormalizeProvider-normalized pool key, so the comparison normalizes
// r.poolKey() the same way before comparing (poolKey() returns "" for a
// runner with no registry record).
func (r *runner) seedCapForPool(pool string) int {
	if r.quotaCfg == nil {
		return 0
	}
	if pool != govern.NormalizeProvider(r.poolKey()) {
		return 0
	}
	return r.quotaCfg.MaxThreads
}

// refreshDemand records this project's demand for slots (it has ready work).
func (r *runner) refreshDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.RefreshDemand(r.poolKey(), r.opts.ProjectID, os.Getpid())
}

// dropDemand withdraws this project from the fair-share denominator.
func (r *runner) dropDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.DropDemand(r.poolKey(), r.opts.ProjectID)
}

// warnIfOverFairShare logs, once per run, when this project's configured wave
// width exceeds its current global fair share — the deliberate per-project
// override the operator asked for, surfaced as a fairness/rate-limit risk.
func (r *runner) warnIfOverFairShare() {
	if r.gov == nil || r.govWarned {
		return
	}
	fs, err := r.gov.FairShareFor(r.poolKey(), r.opts.ProjectID)
	if err != nil || r.width <= fs {
		return
	}
	r.govWarned = true
	r.progress("warning: project width %d exceeds its global fair share %d (cap %d across active projects) — extra slots wait for others to idle and may pressure the Claude API rate limit",
		r.width, fs, r.gov.Cap(r.poolKey()))
}

// memStat reads current system memory (total + available), preferring an
// injected probe (tests) over the real platform probe. ok=false means no usable
// reading — an unsupported platform or a probe error — on which the caller
// fails open.
func (r *runner) memStat() (sysmem.Stat, bool) {
	if r.memProbe != nil {
		return r.memProbe()
	}
	stat, err := sysmem.Available()
	if err != nil {
		return sysmem.Stat{}, false
	}
	return stat, true
}

func (r *runner) pressureCheckpointPath() string {
	if r.store == nil || r.run == nil || r.run.RunID == "" {
		return ""
	}
	return filepath.Join(r.store.RunDir(r.run.RunID), pressureStateName)
}

// loadPressureState restores the last safe kernel reading and hysteresis state
// once per runner. A malformed or unreadable sidecar is treated like no
// last-good sample: the Darwin path degrades to one active machine agent rather
// than turning corrupt safety state into unrestricted admission.
func (r *runner) loadPressureState() {
	if r.pressureLoaded {
		return
	}
	r.pressureLoaded = true
	path := r.pressureCheckpointPath()
	if path == "" {
		return
	}
	var saved durablePressureState
	if err := fsx.ReadJSON(path, &saved); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			r.progress("warning: pressure-state checkpoint unreadable; using degraded admission: %v", err)
		}
		return
	}
	if saved.Schema != "koryph.pressure-state/v1" {
		r.progress("warning: pressure-state checkpoint has unsupported schema %q; using degraded admission", saved.Schema)
		return
	}
	r.pressureLastGood = saved.LastGood
	r.pressurePreviousFresh = saved.PreviousFresh
	r.pressureState = saved.State
	r.pressureReliefHandled = saved.ReliefHandled
	r.pressureReliefPhaseID = saved.ReliefPhaseID
}

func (r *runner) savePressureState() {
	path := r.pressureCheckpointPath()
	if path == "" {
		return
	}
	saved := durablePressureState{
		Schema:        "koryph.pressure-state/v1",
		LastGood:      r.pressureLastGood,
		PreviousFresh: r.pressurePreviousFresh,
		State:         r.pressureState,
		ReliefHandled: r.pressureReliefHandled,
		ReliefPhaseID: r.pressureReliefPhaseID,
	}
	if err := fsx.WriteJSONAtomic(path, saved); err != nil {
		r.progress("warning: could not checkpoint pressure admission state: %v", err)
	}
}

func (r *runner) pressureClock() time.Time {
	if r.pressureNow != nil {
		return r.pressureNow().UTC()
	}
	return time.Now().UTC()
}

// pressureAware reports whether this reading belongs to the new host-pressure
// path. Explicit known-band test probes opt in on every platform. A real Darwin
// probe always opts in, including on failure, so failure resolves through the
// bounded last-good policy. Legacy injected/other-platform unknown-band probes
// retain the historical conservative free-page gate.
func (r *runner) pressureAware(stat sysmem.Stat) bool {
	return stat.Pressure.Known() || r.pressureLastGood != nil ||
		r.pressureState.Effective.Known() ||
		(r.memProbe == nil && goruntime.GOOS == "darwin")
}

// memoryFloorMB resolves the effective memory admission floor in MB for a host
// with totalMB physical memory (koryph-930). Resolution order:
// KORYPH_MIN_FREE_MEMORY_MB env override, else the machine-wide governor pool
// config, else unset. A setting is interpreted as: >0 an explicit absolute
// floor; <0 the gate disabled; 0/unset the auto floor sized to physical memory
// (sysmem.DefaultFloorMB). So the gate is ON by default. A non-numeric env
// value is ignored (falls through to config).
func (r *runner) memoryFloorMB(totalMB uint64) int {
	if v := os.Getenv("KORYPH_MIN_FREE_MEMORY_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return floorFromSetting(n, totalMB)
		}
	}
	configured := 0
	if r.gov != nil {
		configured = r.gov.MinFreeMemoryMB(r.poolKey())
	}
	return floorFromSetting(configured, totalMB)
}

// floorFromSetting maps a raw min-free-memory setting to an effective floor:
// positive is a literal floor, negative disables the gate (0), and 0/absent
// auto-sizes to physical memory (koryph-930).
func floorFromSetting(n int, totalMB uint64) int {
	switch {
	case n > 0:
		return n
	case n < 0:
		return 0 // explicitly disabled
	default:
		return sysmem.DefaultFloorMB(totalMB)
	}
}

// memoryAdmits reports whether there is enough free system memory to admit
// another agent (koryph-930). The gate is ON by default with a floor auto-sized
// to physical memory. It fails OPEN — returns true — when no memory reading is
// available or the floor is disabled, because the memory gate is a safety rail,
// never a correctness dependency (same posture as every other governor helper
// here). A denial defers the dispatch to a later wave, exactly like a
// concurrency-cap denial.
func (r *runner) memoryAdmits(beadID string) bool {
	stat, ok := r.memStat()
	if !ok {
		return true // no probe / unsupported platform → fail open
	}
	return r.memoryAdmitsStat(beadID, stat)
}

func (r *runner) memoryAdmitsStat(beadID string, stat sysmem.Stat) bool {
	floorMB := r.memoryFloorMB(stat.TotalMB())
	if floorMB <= 0 {
		return true // gate disabled
	}
	availMB := stat.AvailableMB()
	if availMB >= uint64(floorMB) {
		return true
	}
	r.progress("bead %s: deferring dispatch — %d MB free memory below the %d MB floor (auto-sized to physical RAM; set min_free_memory_mb / KORYPH_MIN_FREE_MEMORY_MB to override, -1 to disable)",
		beadID, availMB, floorMB)
	return false
}

// admitVerdict is acquireGlobalSlot's typed decision (koryph-4ql.3, design L3):
// the engine routes a per-bead denial differently from a machine-wide one so a
// single deferred resource-heavy bead no longer stalls the lightweight beads
// behind it.
type admitVerdict int

const (
	// admitGranted: a global slot (and every machine-resource clause) admitted
	// this bead — dispatch it.
	admitGranted admitVerdict = iota
	// admitSkip: a PER-BEAD denial (a declared resource kind at capacity, or the
	// candidate's own memory reservation tipping the floor). Skip THIS bead and
	// keep packing — the beads behind it may still fit.
	admitSkip
	// admitBreak: a MACHINE-WIDE denial (pool cap / fair share / breaker /
	// smoothing, a pure memory floor breach, or the pre-flock koryph-930 floor).
	// Nothing else fits this boundary either — break the batch and retry at the
	// next boundary, exactly as every governor denial did before koryph-4ql.3.
	admitBreak
)

// estPerAgentMB resolves the kind-less per-agent memory reservation in MB
// (koryph-3xs). Resolution order mirrors memoryFloorMB: the
// KORYPH_EST_PER_AGENT_MB env override, else the machine-wide governor pool
// config, else the package default. A setting is interpreted as: >0 an explicit
// reservation; <0 disabled (kind-less leases reserve 0, the pre-koryph-3xs
// behavior); 0/unset the conservative default (govern.DefaultEstPerAgentMB). A
// non-numeric env value is ignored (falls through to config).
func (r *runner) estPerAgentMB() int {
	if v := os.Getenv("KORYPH_EST_PER_AGENT_MB"); v != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return estFromSetting(n)
		}
	}
	configured := 0
	if r.gov != nil {
		configured = r.gov.EstPerAgentMB(r.poolKey())
	}
	return estFromSetting(configured)
}

// estFromSetting maps a raw est-per-agent setting to an effective reservation:
// positive is a literal MB reservation, negative disables it (0), and 0/absent
// applies the package default (koryph-3xs).
func estFromSetting(n int) int {
	switch {
	case n > 0:
		return n
	case n < 0:
		return 0 // explicitly disabled
	default:
		return govern.DefaultEstPerAgentMB
	}
}

// resolveMemReserveMB resolves the memory reservation for a dispatch
// (koryph-4ql.3, design L5). A bead with NO declared res:<kind> footprint
// reserves the kind-less per-agent default (estPerAgentMB, koryph-3xs) so N
// concurrent kind-less agents reserve N*est against the free-memory floor
// instead of 0. A bead WITH declared kinds sums the per-kind reservation, per
// kind, matching L2: the machine ledger's mem_mb (govern Store.Resources()) when
// set, else the project vocabulary's mem_mb (cfg.Resources), else 0. Resolved
// once at dispatch and frozen on the ledger slot (I8), so a vocabulary edit
// mid-run never re-prices a live slot.
func (r *runner) resolveMemReserveMB(kinds []string) int {
	if len(kinds) == 0 {
		return r.estPerAgentMB()
	}
	var machine map[string]govern.ResourceKind
	if r.gov != nil {
		machine = r.gov.Resources().Kinds
	}
	total := 0
	for _, k := range kinds {
		if rk, ok := machine[k]; ok && rk.MemMB > 0 {
			total += rk.MemMB
			continue
		}
		if r.cfg != nil {
			if rk, ok := r.cfg.Resources[k]; ok {
				total += rk.MemMB
			}
		}
	}
	return total
}

// resourceCapacities resolves the effective per-kind capacity map fed to
// sched.BuildWave (koryph-4ql.3, design L4) — the machine ledger's per-kind
// capacity (govern Store.Resources().Kinds). A kind absent here defaults to 1
// inside sched (the fail-safe-serial default), so this deliberately omits the
// unconfigured kinds. nil when there is no governor or no configured kinds,
// which sched reads as "every kind is capacity 1".
func (r *runner) resourceCapacities() map[string]int {
	if r.gov == nil {
		return nil
	}
	kinds := r.gov.Resources().Kinds
	if len(kinds) == 0 {
		return nil
	}
	out := make(map[string]int, len(kinds))
	for k, rk := range kinds {
		c := rk.Capacity
		if c <= 0 {
			c = govern.DefaultResourceCapacity
		}
		out[k] = c
	}
	return out
}

// liveAgentLeases returns every live governor lease across provider pools.
// Observe performs one read-only, flocked inventory scan; the process table
// itself is sampled separately and exactly once outside that lock.
func (r *runner) liveAgentLeases() ([]govern.Lease, bool) {
	if r.gov != nil {
		obs, err := r.gov.Observe()
		if err == nil {
			var leases []govern.Lease
			for _, pool := range obs.Pools {
				for _, lease := range pool.Leases {
					if lease.PID > 0 {
						leases = append(leases, lease)
					}
				}
			}
			return leases, true
		}
	}

	// This fallback is useful for a governor-less focused test and gives a
	// best-effort local RSS view after an observation error. It is deliberately
	// marked unknown: local slots cannot prove the machine-wide inventory.
	var leases []govern.Lease
	if r.run != nil {
		for _, sl := range r.run.Slots {
			if sl != nil && !ledger.Terminal(sl.Status) && sl.PID > 0 {
				leases = append(leases, govern.Lease{
					Project: r.opts.ProjectID, Bead: sl.BeadID, PID: sl.PID,
					AcquiredAt: sl.DispatchedAt,
				})
			}
		}
	}
	return leases, r.gov == nil
}

// admissionProcTable performs the one process-table sweep used both for live
// cohort RSS and for identity-checking critical-pressure relief.
func (r *runner) admissionProcTable() *resmon.ProcTable {
	probe := r.resProbe
	if probe == nil {
		probe = resmon.Snapshot
	}
	ctx, cancel := context.WithTimeout(context.Background(), resSampleTimeout)
	defer cancel()
	table, err := probe(ctx)
	if err != nil {
		return nil
	}
	return table
}

func (r *runner) currentRuntimeName() string {
	if r.rt != nil && strings.TrimSpace(r.rt.Name()) != "" {
		return r.rt.Name()
	}
	return "claude"
}

// pressureMemInput resolves exactly one kernel sample and one process snapshot
// for a direct admission, then caches the result for the rest of this control
// cadence. Poll-driven sampling calls pressureMemInputFromSnapshot with the
// poll pass's existing process snapshot instead.
func (r *runner) pressureMemInput(fresh sysmem.Stat, freshOK bool) govern.MemInput {
	leases, inventoryKnown := r.liveAgentLeases()
	table := r.admissionProcTable()
	return r.pressureMemInputFromSnapshot(
		r.pressureClock(), fresh, freshOK, table, leases, inventoryKnown,
	)
}

func (r *runner) pressureMemInputFromSnapshot(
	now time.Time,
	fresh sysmem.Stat,
	freshOK bool,
	table *resmon.ProcTable,
	leases []govern.Lease,
	inventoryKnown bool,
) govern.MemInput {
	var probeErr error
	if !freshOK {
		probeErr = errors.New("sysmem: pressure probe failed")
	}
	sample, lastGood := sysmem.ResolvePressureSample(
		now, fresh, probeErr, r.pressureLastGood, sysmem.DefaultLastGoodTTL,
	)
	r.pressureLastGood = lastGood

	// A trend is meaningful only across consecutive fresh observations. Any
	// last-good or degraded decision breaks the chain, so a later recovery does
	// not compare counters across the blind interval.
	var trend sysmem.Trend
	if sample.Origin == sysmem.SampleFresh {
		if r.pressurePreviousFresh != nil {
			trend = sample.TrendFrom(*r.pressurePreviousFresh)
		}
		current := sample.Stat
		r.pressurePreviousFresh = &current
	} else {
		r.pressurePreviousFresh = nil
	}
	freshKernelCritical := sample.Origin == sysmem.SampleFresh &&
		sample.Pressure == sysmem.PressureCritical

	policy := govern.PressurePolicy{}
	sharedStateUnreadable := false
	sharedEpisodeExists := false
	sharedClaimExists := false
	if r.gov != nil {
		_, sharedEpisodeExists, probeErr = r.gov.PressureEpisodeStatus()
		episodeErr := probeErr
		_, sharedClaimExists, probeErr = r.gov.PressureReliefStatus()
		claimErr := probeErr
		if episodeErr != nil || claimErr != nil {
			sharedStateUnreadable = true
		}
		if sharedEpisodeExists || sharedClaimExists || sharedStateUnreadable {
			// Inherit the machine circuit before advancing this runner's
			// hysteresis. Fresh-normal samples may recover it, but a fresh
			// runner can never treat an existing or unreadable shared episode
			// as an unrestricted host.
			r.pressureState.CircuitOpen = true
		}
	}
	transition := govern.AdvancePressure(r.pressureState, sample, trend, now, policy)
	r.pressureState = transition.State
	if transition.OpenCircuit {
		r.pressureReliefHandled = false
		r.pressureReliefPhaseID = ""
		r.emitSafetyTripwire(
			SafetyTripwireHostMemoryPressure,
			"",
			"persistent critical host-memory pressure opened admission circuit",
		)
	}
	// Publishing is independent of the circuit edge so a restarted runner can
	// migrate its run-local open sidecar and retry a transient write failure,
	// but only while the current kernel sample still proves critical pressure.
	// Stale hysteresis alone must not recreate an episode another runner already
	// recovered during fresh-normal observations.
	if r.pressureState.CircuitOpen && freshKernelCritical && !sharedEpisodeExists &&
		!sharedStateUnreadable && r.gov != nil && r.run != nil {
		if _, _, err := r.gov.OpenPressureEpisode(govern.PressureEpisodeRequest{
			Project: r.opts.ProjectID, RunID: r.run.RunID,
			EnginePID: os.Getpid(),
		}); err != nil {
			r.progress("pressure circuit: could not publish machine pressure episode: %v", err)
		}
	}

	// Persist a newly opened circuit before signalling anything. Ordinary
	// samples need only the final checkpoint below, avoiding two fsync-backed
	// writes per candidate. If the engine dies after SIGTERM but before that
	// final checkpoint, restart re-verifies the same PID identity before a
	// harmless repeated graceful signal.
	if transition.OpenCircuit {
		r.savePressureState()
	}

	roots := make([]int, 0, len(leases))
	for _, lease := range leases {
		roots = append(roots, lease.PID)
	}
	live := resmon.LiveCohortUsage(table, roots)
	if !inventoryKnown {
		live.RSSKnown = false
	}

	// Target election and SIGTERM require the same current kernel proof as
	// publication. transition.Effective may intentionally remain critical for
	// several fresh-normal recovery samples and is not destructive authority.
	if r.pressureState.CircuitOpen && freshKernelCritical &&
		!r.pressureReliefHandled {
		if len(roots) == 0 && inventoryKnown {
			// coordinatePressureRelief still checks for an earlier global claim
			// whose fixed target exited before its owner acknowledged it.
			r.pressureReliefHandled, r.pressureReliefPhaseID =
				r.coordinatePressureRelief(table, leases, freshKernelCritical)
			if !r.pressureReliefHandled {
				// No active cohort and no outstanding claim is already the safe
				// post-action state.
				r.pressureReliefHandled = true
			}
		} else if handled, phaseID := r.coordinatePressureRelief(
			table, leases, freshKernelCritical,
		); handled {
			r.pressureReliefHandled = true
			r.pressureReliefPhaseID = phaseID
		}
	}
	if transition.ReliefReady && (r.gov != nil || r.pressureReliefHandled) {
		recovered := r.recoverSharedPressureEpisode()
		if recovered {
			r.pressureState = govern.AcknowledgePressureRelief(r.pressureState, policy)
			if !r.pressureState.CircuitOpen {
				r.pressureReliefHandled = false
				r.pressureReliefPhaseID = ""
			}
		}
	} else if r.gov != nil && !r.pressureState.CircuitOpen &&
		transition.Effective == sysmem.PressureNormal &&
		r.pressureState.NormalSamples >= govern.DefaultRecoverySamples {
		// A fresh run may not have observed the prior circuit edge, but three
		// fresh normal samples still prove the machine episode recovered. Clear
		// a tombstone left by an exited claim owner so a future, distinct
		// pressure episode can elect one cohort.
		r.recoverSharedPressureEpisode()
	}
	r.savePressureState()

	sharedCircuitOpen := sharedStateUnreadable
	if r.gov != nil {
		_, episodeExists, episodeErr := r.gov.PressureEpisodeStatus()
		_, claimExists, claimErr := r.gov.PressureReliefStatus()
		sharedCircuitOpen = sharedCircuitOpen || episodeExists || claimExists
		if episodeErr != nil || claimErr != nil {
			// Shared pressure state is admission-authoritative. A read failure
			// therefore keeps the host circuit open rather than letting a fresh
			// runner's empty local sidecar restore unrestricted dispatch.
			sharedCircuitOpen = true
		}
	}
	floor := r.memoryFloorMB(sample.TotalMB())
	mem := govern.MemInput{
		FloorMB: floor,
		Host: &govern.HostInput{
			Sample:            sample,
			Trend:             trend,
			EffectivePressure: transition.Effective,
			CircuitOpen:       r.pressureState.CircuitOpen || sharedCircuitOpen,
			LiveRSSMB:         live.RSSMB,
			LiveRSSKnown:      live.RSSKnown,
			ObservedEstimateMB: r.runtimeMemoryEstimate(
				r.currentRuntimeName(),
			),
			Policy: policy,
		},
	}
	r.pressureSampleAt = now
	r.pressureSampleInput = mem
	r.pressureSampleValid = true
	return mem
}

func (r *runner) pressureControlInterval() time.Duration {
	if r.pressureSampleInterval > 0 {
		return r.pressureSampleInterval
	}
	return max(r.pollInterval(), resMinSampleInterval)
}

func (r *runner) cachedPressureMemInput(now time.Time) (govern.MemInput, bool) {
	if !r.pressureSampleValid || r.pressureSampleAt.IsZero() {
		return govern.MemInput{}, false
	}
	age := now.Sub(r.pressureSampleAt)
	if age < 0 || age >= r.pressureControlInterval() {
		return govern.MemInput{}, false
	}
	return r.pressureSampleInput, true
}

// pollPressureControl advances machine-pressure hysteresis on the poll cadence
// even while slot saturation means no admission is attempted. It consumes the
// process table already sampled by pollPass; it never starts a second process
// probe for the same cadence.
func (r *runner) pollPressureControl(table *resmon.ProcTable) {
	r.loadPressureState()
	now := r.pressureClock()
	if _, ok := r.cachedPressureMemInput(now); ok {
		return
	}
	fresh, freshOK := r.memStat()
	if !r.pressureAware(fresh) {
		return
	}
	leases, inventoryKnown := r.liveAgentLeases()
	r.pressureMemInputFromSnapshot(
		now, fresh, freshOK, table, leases, inventoryKnown,
	)
}

// recoverSharedPressureEpisode compare-and-deletes the exact shared episode
// after fresh-normal hysteresis. A targetless episode can recover directly; an
// episode with a relief claim requires that exact claim to be acknowledged.
// A paused owner therefore retains its immutable target until it finishes.
func (r *runner) recoverSharedPressureEpisode() bool {
	if r.gov == nil {
		return true
	}
	episode, episodeExists, err := r.gov.PressureEpisodeStatus()
	if err != nil {
		r.progress("pressure circuit: could not read recovered machine pressure episode: %v", err)
		return false
	}
	claim, claimExists, err := r.gov.PressureReliefStatus()
	if err != nil {
		r.progress("pressure circuit: could not read recovered machine relief claim: %v", err)
		return false
	}
	if episodeExists {
		expectedClaimID := ""
		if claimExists {
			if claim.AcknowledgedAt == "" {
				return false
			}
			expectedClaimID = claim.ID
		}
		recovered, err := r.gov.RecoverPressureEpisode(episode.ID, expectedClaimID)
		if err != nil {
			r.progress("pressure circuit: could not clear recovered machine pressure episode: %v", err)
			return false
		}
		return recovered
	}
	if !claimExists {
		return true
	}
	if claim.AcknowledgedAt == "" {
		return false
	}
	// Compatibility for a target-bearing claim written before independent
	// pressure-episode publication existed.
	recovered, err := r.gov.RecoverPressureRelief(claim.ID)
	if err != nil {
		r.progress("pressure circuit: could not clear recovered legacy machine relief claim: %v", err)
		return false
	}
	return recovered
}

// coordinatePressureRelief elects the newest actually recoverable cohort from
// all registered active run ledgers, then serializes the action through the
// governor's machine-global claim. An acknowledged claim remains a tombstone
// until normal recovery, so concurrent runs cannot sequentially SIGTERM local
// cohorts from the same persistent-pressure episode. freshKernelCritical is an
// explicit destructive-action proof at the function boundary; hysteretic or
// persisted critical state alone cannot elect or signal a target.
func (r *runner) coordinatePressureRelief(
	table *resmon.ProcTable,
	leases []govern.Lease,
	freshKernelCritical bool,
) (bool, string) {
	if !freshKernelCritical || table == nil || r.run == nil {
		return false, ""
	}

	var target govern.PressureReliefTarget
	if r.gov != nil {
		claim, exists, err := r.gov.PressureReliefStatus()
		if err != nil {
			r.progress("pressure circuit: could not read machine relief claim: %v", err)
			return false, ""
		}
		if exists {
			if claim.AcknowledgedAt != "" {
				return true, claim.Target.PhaseID
			}
			target = claim.Target
		}
	}
	if target.PID == 0 {
		var ok bool
		target, ok = r.newestRecoverablePressureTarget(table, leases)
		if !ok {
			return false, ""
		}
	}

	ownerProject := r.opts.ProjectID
	ownerRunID := r.run.RunID
	ownerPID := os.Getpid()
	ownerIdentity, ownerIdentityKnown := table.ProcessIdentity(ownerPID)
	if !ownerIdentityKnown || ownerIdentity == "" {
		return false, ""
	}
	if r.gov != nil {
		observedOwnerIdentity := ""
		if claim, exists, err := r.gov.PressureReliefStatus(); err != nil {
			r.progress("pressure circuit: could not re-read machine relief claim: %v", err)
			return false, ""
		} else if exists {
			observedOwnerIdentity, _ = table.ProcessIdentity(claim.OwnerEnginePID)
		}
		claim, acquired, err := r.gov.ClaimPressureRelief(govern.PressureReliefRequest{
			OwnerProject:                 ownerProject,
			OwnerRunID:                   ownerRunID,
			OwnerEnginePID:               ownerPID,
			OwnerProcessIdentity:         ownerIdentity,
			ObservedOwnerProcessIdentity: observedOwnerIdentity,
			Target:                       target,
		})
		if err != nil {
			r.progress("pressure circuit: could not claim machine relief action: %v", err)
			return false, ""
		}
		if claim.AcknowledgedAt != "" {
			return true, claim.Target.PhaseID
		}
		if !acquired {
			return false, ""
		}
		target = claim.Target // takeover always preserves the original target
	}

	// A complete snapshot can prove the fixed target exited or its PID was
	// recycled. That is a handled relief action: never signal the replacement.
	_, processPresent := table.Aggregate(target.PID)
	actualIdentity, identityKnown := table.ProcessIdentity(target.PID)
	if !processPresent || (identityKnown && actualIdentity != target.ProcessIdentity) {
		if r.acknowledgePressureRelief(
			target, ownerProject, ownerRunID, ownerPID, ownerIdentity,
		) {
			return true, target.PhaseID
		}
		return false, ""
	}
	if !identityKnown || actualIdentity != target.ProcessIdentity ||
		!r.pressureTargetRecoverable(target, table) {
		return false, ""
	}

	stop := r.pressureStop
	if stop == nil {
		stop = dispatch.StopGraceful
	}
	if err := stop(target.PID); err != nil {
		r.progress("pressure circuit: could not gracefully stop recoverable bead %s: %v",
			target.BeadID, err)
		return false, ""
	}
	if !r.acknowledgePressureRelief(
		target, ownerProject, ownerRunID, ownerPID, ownerIdentity,
	) {
		return false, ""
	}
	r.progress("pressure circuit: gracefully stopping newest recoverable bead %s (pid %d); worktree preserved",
		target.BeadID, target.PID)
	return true, target.PhaseID
}

func (r *runner) acknowledgePressureRelief(
	target govern.PressureReliefTarget,
	ownerProject, ownerRunID string,
	ownerPID int,
	ownerIdentity string,
) bool {
	if r.gov == nil {
		return true
	}
	claim, exists, err := r.gov.PressureReliefStatus()
	if err != nil || !exists {
		return false
	}
	if err := r.gov.AcknowledgePressureReliefClaim(
		claim.ID, ownerProject, ownerRunID, ownerPID, ownerIdentity,
	); err != nil {
		r.progress("pressure circuit: graceful relief completed but acknowledgement failed for bead %s: %v",
			target.BeadID, err)
		return false
	}
	return true
}

func (r *runner) newestRecoverablePressureTarget(
	table *resmon.ProcTable,
	leases []govern.Lease,
) (govern.PressureReliefTarget, bool) {
	var newest govern.PressureReliefTarget
	var newestAt time.Time
	for _, lease := range leases {
		target, ok := r.pressureTargetForLease(lease, table)
		if !ok {
			continue
		}
		at := parsePressureTime(target.DispatchedAt)
		if newest.PID == 0 || at.After(newestAt) ||
			(at.Equal(newestAt) &&
				(target.Project > newest.Project ||
					(target.Project == newest.Project && target.PhaseID > newest.PhaseID))) {
			newest, newestAt = target, at
		}
	}
	return newest, newest.PID > 0
}

func parsePressureTime(value string) time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return at
}

func (r *runner) pressureTargetForLease(
	lease govern.Lease,
	table *resmon.ProcTable,
) (govern.PressureReliefTarget, bool) {
	if lease.PID <= 0 {
		return govern.PressureReliefTarget{}, false
	}
	if lease.Project == r.opts.ProjectID && r.run != nil {
		if target, ok := r.pressureTargetFromRun(lease, r.run, table); ok {
			return target, true
		}
	}
	store := r.pressureLedgerStore(lease.Project)
	if store == nil {
		return govern.PressureReliefTarget{}, false
	}
	runIDs, err := store.ListRuns()
	if err != nil {
		return govern.PressureReliefTarget{}, false
	}
	for _, runID := range runIDs {
		if r.run != nil && lease.Project == r.opts.ProjectID && runID == r.run.RunID {
			continue
		}
		run, err := store.LoadRun(runID)
		if err != nil {
			continue
		}
		if target, ok := r.pressureTargetFromRun(lease, run, table); ok {
			return target, true
		}
	}
	return govern.PressureReliefTarget{}, false
}

func (r *runner) pressureTargetFromRun(
	lease govern.Lease,
	run *ledger.Run,
	table *resmon.ProcTable,
) (govern.PressureReliefTarget, bool) {
	for _, sl := range run.Slots {
		if sl == nil || sl.BeadID != lease.Bead || sl.PID != lease.PID {
			continue
		}
		target := govern.PressureReliefTarget{
			Project: lease.Project, RunID: run.RunID, PhaseID: sl.PhaseID,
			BeadID: sl.BeadID, PID: sl.PID, ProcessIdentity: sl.ProcessIdentity,
			DispatchedAt: sl.DispatchedAt,
		}
		if r.pressureSlotRecoverable(sl, table) {
			return target, true
		}
	}
	return govern.PressureReliefTarget{}, false
}

func (r *runner) pressureTargetRecoverable(
	target govern.PressureReliefTarget,
	table *resmon.ProcTable,
) bool {
	var run *ledger.Run
	if target.Project == r.opts.ProjectID && r.run != nil &&
		target.RunID == r.run.RunID {
		run = r.run
	} else {
		store := r.pressureLedgerStore(target.Project)
		if store == nil {
			return false
		}
		var err error
		run, err = store.LoadRun(target.RunID)
		if err != nil {
			return false
		}
	}
	sl := run.Slots[target.PhaseID]
	if sl == nil {
		for _, candidate := range run.Slots {
			if candidate != nil && candidate.BeadID == target.BeadID &&
				candidate.PID == target.PID {
				sl = candidate
				break
			}
		}
	}
	return sl != nil && sl.ProcessIdentity == target.ProcessIdentity &&
		r.pressureSlotRecoverable(sl, table)
}

func (r *runner) pressureSlotRecoverable(
	sl *ledger.Slot,
	table *resmon.ProcTable,
) bool {
	if sl == nil || ledger.Terminal(sl.Status) || sl.PID <= 0 ||
		sl.ProcessIdentity == "" || sl.VerifiedIdentity == "" ||
		sl.Worktree == "" || parsePressureTime(sl.DispatchedAt).IsZero() ||
		!table.MatchesProcess(sl.PID, sl.ProcessIdentity) {
		return false
	}
	info, err := os.Stat(sl.Worktree)
	return err == nil && info.IsDir()
}

func (r *runner) pressureLedgerStore(projectID string) *ledger.Store {
	if r.reg == nil {
		return nil
	}
	rec, err := r.reg.Get(projectID)
	if err != nil {
		return nil
	}
	return ledger.NewStore(rec.Root)
}

// runtimeMemoryEstimate calibrates once per run from recent accepted attempts.
// An attempt is eligible only when command evidence proves it did not repeat a
// broad validation command; missing/malformed evidence is conservatively
// excluded rather than teaching admission from a possibly inflated RSS peak.
func (r *runner) runtimeMemoryEstimate(runtimeName string) int {
	if !r.memoryCalibrationLoaded {
		r.loadRuntimeMemoryEstimates()
	}
	return r.memoryEstimates[runtimeName]
}

func (r *runner) loadRuntimeMemoryEstimates() {
	r.memoryCalibrationLoaded = true
	r.memoryEstimates = r.readRuntimeMemoryEstimates()
	if r.store == nil {
		return
	}
	runIDs, err := r.store.ListRuns()
	if err != nil {
		return
	}
	samples := map[string][]resmon.AttemptMemory{}
	for _, runID := range runIDs {
		run, err := r.store.LoadRun(runID)
		if err != nil {
			continue
		}
		for _, sl := range run.Slots {
			if sl == nil || sl.PeakRSSMB <= 0 || sl.ResourceSamples <= 0 ||
				!acceptedMemoryOutcome(sl.Status) {
				continue
			}
			finished, err := time.Parse(time.RFC3339, sl.FinishedAt)
			if err != nil {
				finished, err = time.Parse(time.RFC3339Nano, sl.FinishedAt)
			}
			if err != nil {
				continue
			}
			duplicate, proven := r.duplicateBroadCommand(runID, sl.PhaseID)
			runtimeName := sl.Runtime
			if runtimeName == "" {
				runtimeName = "claude"
			}
			samples[runtimeName] = append(samples[runtimeName], resmon.AttemptMemory{
				Runtime:               runtimeName,
				PeakMB:                sl.PeakRSSMB,
				Successful:            true,
				DuplicateBroadCommand: duplicate || !proven,
				FinishedAt:            finished,
			})
		}
	}
	for runtimeName, attempts := range samples {
		current := r.memoryEstimates[runtimeName]
		estimate := resmon.CalibrateRuntimeEstimate(
			runtimeName, current, attempts, resmon.EstimatePolicy{},
		)
		if estimate.EstimateMB > 0 {
			r.memoryEstimates[runtimeName] = estimate.EstimateMB
		}
	}
	r.persistRuntimeMemoryEstimates()
}

func (r *runner) runtimeMemoryEstimatesPath() string {
	if r.store == nil {
		return ""
	}
	return filepath.Join(r.store.KoryphRoot, runtimeMemoryEstimatesName)
}

func (r *runner) readRuntimeMemoryEstimates() map[string]int {
	estimates := map[string]int{}
	path := r.runtimeMemoryEstimatesPath()
	if path == "" {
		return estimates
	}
	var saved durableRuntimeMemoryEstimates
	if err := fsx.ReadJSON(path, &saved); err != nil ||
		saved.Schema != "koryph.runtime-memory-estimates/v1" {
		return estimates
	}
	for runtimeName, estimate := range saved.Estimates {
		if strings.TrimSpace(runtimeName) != "" && estimate > 0 {
			estimates[runtimeName] = estimate
		}
	}
	return estimates
}

func (r *runner) persistRuntimeMemoryEstimates() {
	path := r.runtimeMemoryEstimatesPath()
	if path == "" || len(r.memoryEstimates) == 0 {
		return
	}
	saved := durableRuntimeMemoryEstimates{
		Schema:    "koryph.runtime-memory-estimates/v1",
		Estimates: r.memoryEstimates,
	}
	if err := fsx.WriteJSONAtomic(path, saved); err != nil {
		r.progress("warning: could not persist runtime memory estimates: %v", err)
	}
}

func acceptedMemoryOutcome(status string) bool {
	switch status {
	case ledger.SlotMerged, ledger.SlotPROpened, ledger.SlotDone:
		return true
	default:
		return false
	}
}

func (r *runner) duplicateBroadCommand(runID, phaseID string) (duplicate, proven bool) {
	path := filepath.Join(r.store.KoryphRoot, runID, phaseID, ".koryph-command", "events.jsonl")
	f, err := os.Open(path)
	if err != nil {
		return false, false
	}
	defer f.Close()

	seenStart := map[string]bool{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var event resmon.CommandEvent
		if err := strictjson.Decode(scanner.Bytes(), &event); err != nil {
			return false, false
		}
		if event.Class != resmon.CommandBroad && event.Class != resmon.CommandGate {
			continue
		}
		proven = true
		if event.Event == "reuse" {
			duplicate = true
		}
		if event.Event == "start" {
			if seenStart[event.Signature] {
				duplicate = true
			}
			seenStart[event.Signature] = true
		}
	}
	if err := scanner.Err(); err != nil {
		return false, false
	}
	// An existing, valid event stream with no broad command is still evidence
	// that this attempt did not duplicate one.
	return duplicate, true
}

// acquireGlobalSlot reserves a global concurrency slot for beadID (keyed to the
// project+bead under this engine's pid; the agent pid is attached later by
// holdGlobalSlot), passing the bead's resolved resource kinds and memory
// reservation (koryph-4ql.3, L2/L5) so the flocked governor can apply the
// cross-pool capacity and reservation-aware memory clauses. Returns a typed
// verdict (admitGranted / admitSkip / admitBreak). Legacy governor errors fail
// open for compatibility; pressure-aware errors fail safe with a typed
// pressure deferral because losing the machine inventory must not restore
// unrestricted dispatch.
func (r *runner) acquireGlobalSlot(beadID string, kinds []string, memReserveMB int) admitVerdict {
	r.loadPressureState()

	// Exactly one kernel sample is resolved for this candidate. The legacy path
	// feeds that same reading to both the pre-governor floor and reservation
	// clauses; the Darwin path derives hysteresis/trend from it and never falls
	// back to a second, potentially contradictory probe.
	var mem govern.MemInput
	pressureAware := false
	if cached, ok := r.cachedPressureMemInput(r.pressureClock()); ok {
		mem = cached
		pressureAware = true
	} else {
		stat, statOK := r.memStat()
		pressureAware = r.pressureAware(stat)
		if pressureAware {
			mem = r.pressureMemInput(stat, statOK)
		} else if statOK {
			if !r.memoryAdmitsStat(beadID, stat) {
				return admitBreak
			}
			if floor := r.memoryFloorMB(stat.TotalMB()); floor > 0 {
				mem = govern.MemInput{AvailMB: stat.AvailableMB(), FloorMB: floor}
			}
		}
	}

	if r.gov == nil {
		if pressureAware {
			return r.standalonePressureVerdict(beadID, memReserveMB, mem)
		}
		return admitGranted
	}
	res, err := r.gov.AcquireEx(govern.Lease{
		Project:      r.opts.ProjectID,
		Bead:         beadID,
		EnginePID:    os.Getpid(),
		Provider:     r.poolKey(),
		Resources:    kinds,
		MemReserveMB: memReserveMB,
	}, mem)
	if err != nil {
		if pressureAware {
			r.progress("bead %s: deferring dispatch — pressure governor unavailable: %v", beadID, err)
			logDeferral(beadID, "pressure governor unavailable", "memory-pressure")
			return admitBreak
		}
		r.progress("bead %s: global governor error (allowing dispatch): %v", beadID, err)
		return admitGranted
	}
	return r.classifyAdmit(beadID, memReserveMB, res)
}

// standalonePressureVerdict preserves fail-safe host behavior in focused tests
// and degenerate runners without a governor store. Production runners use
// AcquireEx so cross-engine counts and ramp reservations remain authoritative.
func (r *runner) standalonePressureVerdict(
	beadID string,
	memReserveMB int,
	mem govern.MemInput,
) admitVerdict {
	host := mem.Host
	if host == nil {
		return admitGranted
	}
	active := 0
	if r.run != nil {
		for _, sl := range r.run.Slots {
			if sl != nil && !ledger.Terminal(sl.Status) && sl.PID > 0 {
				active++
			}
		}
	}
	pressure := host.EffectivePressure
	if !pressure.Known() {
		pressure = host.Sample.Pressure
	}
	if host.CircuitOpen || pressure == sysmem.PressureWarning ||
		pressure == sysmem.PressureCritical ||
		(host.Sample.Origin == sysmem.SampleDegraded && active >= 1) ||
		(!host.LiveRSSKnown && active >= 1) {
		return r.classifyAdmit(beadID, memReserveMB, govern.AdmitResult{
			Outcome:     govern.AdmitDeniedPressure,
			Pressure:    pressure,
			ProbeOrigin: host.Sample.Origin,
			CircuitOpen: host.CircuitOpen,
			Detail:      "standalone-pressure",
			LiveRSSMB:   host.LiveRSSMB,
		})
	}
	candidateMB := memReserveMB
	if host.ObservedEstimateMB > candidateMB {
		candidateMB = host.ObservedEstimateMB
	}
	budget := int(host.Sample.TotalMB()) - mem.FloorMB
	if budget > 0 && host.LiveRSSMB+candidateMB > budget {
		return r.classifyAdmit(beadID, memReserveMB, govern.AdmitResult{
			Outcome:           govern.AdmitDeniedReservation,
			CandidateTipped:   host.LiveRSSMB <= budget,
			Pressure:          pressure,
			ProbeOrigin:       host.Sample.Origin,
			Detail:            "standalone-reservation-budget",
			LiveRSSMB:         host.LiveRSSMB,
			CandidateMemoryMB: candidateMB,
			MemoryBudgetMB:    budget,
		})
	}
	return admitGranted
}

// classifyAdmit maps a govern.AdmitResult to an engine admitVerdict (koryph-4ql.3,
// design L3), emitting the deferral log line + structured deferral event for a
// per-bead skip so the deferrals-by-token metric picks up the kind. A cap denial
// (pool cap / fair share / breaker / smoothing) and a pure memory-floor breach
// are machine-wide → break, and their message is left to the caller's existing
// batch-break log so today's wording is unchanged. memReserveMB is the
// candidate's own reservation, echoed on a candidate-tipped memory skip.
func (r *runner) classifyAdmit(beadID string, memReserveMB int, res govern.AdmitResult) admitVerdict {
	if res.Granted {
		return admitGranted
	}
	switch res.Outcome {
	case govern.AdmitDeniedResource:
		holder := res.HolderBead
		if res.HolderProject != "" && res.HolderProject != r.opts.ProjectID {
			holder = res.HolderProject + "/" + res.HolderBead
		}
		r.progress("bead %s: deferred — resource %s at capacity (%d/%d, held by %s)",
			beadID, res.DeniedKind, res.DeniedHolders, res.DeniedCapacity, holder)
		logDeferral(beadID, "resource "+res.DeniedKind+" at capacity", "res:"+res.DeniedKind)
		return admitSkip
	case govern.AdmitDeniedMemory:
		if res.CandidateTipped {
			r.progress("bead %s: deferred — its %d MB memory reservation would breach the free-memory floor",
				beadID, memReserveMB)
			logDeferral(beadID, "memory reservation breach", "memory-reservation")
			return admitSkip
		}
		// Pure floor breach: even a zero-reserve bead fails → machine-wide break.
		return admitBreak
	case govern.AdmitDeniedPressure:
		r.progress("bead %s: deferring dispatch — host memory pressure (%s, origin=%s, detail=%s, live_rss=%d MB, circuit_open=%t)",
			beadID, res.Pressure, res.ProbeOrigin, res.Detail, res.LiveRSSMB, res.CircuitOpen)
		logDeferral(beadID, "host memory pressure: "+res.Detail, "memory-pressure")
		return admitBreak
	case govern.AdmitDeniedReservation:
		r.progress("bead %s: deferred — memory reservation budget (%d MB live + %d MB ramp + %d MB candidate > %d MB budget)",
			beadID, res.LiveRSSMB, res.ReservedMB, res.CandidateMemoryMB, res.MemoryBudgetMB)
		logDeferral(beadID, "memory reservation budget", "memory-reservation")
		if res.CandidateTipped {
			return admitSkip
		}
		return admitBreak
	case govern.AdmitDeniedCap:
		logDeferral(beadID, "global governor cap", "global-cap")
		return admitBreak
	default:
		logDeferral(beadID, "unknown governor denial", "governor-unknown")
		return admitBreak
	}
}

// holdGlobalSlot attaches the launched agent pid to the bead's lease (keyed to a
// process that outlives the engine) so the running agent is always counted —
// including a requeue/resume whose reservation was pruned. Cap admission already
// happened at acquireGlobalSlot; this is an unconditional 1:1 update. It carries
// the frozen resource kinds + memory reservation (koryph-4ql.3, L2) so Hold
// persists them on the (re)bound lease — the engine threads them from the
// persisted ledger slot, never a govern-side read of a prior lease that a prune
// gap may have removed.
func (r *runner) holdGlobalSlot(beadID string, agentPID int, model string, kinds []string, memReserveMB int) {
	if r.gov == nil {
		return
	}
	_ = r.gov.Hold(govern.Lease{
		Project:      r.opts.ProjectID,
		Bead:         beadID,
		PID:          agentPID,
		EnginePID:    os.Getpid(),
		Model:        model,
		Provider:     r.poolKey(),
		Resources:    kinds,
		MemReserveMB: memReserveMB,
	})
}

// releaseGlobalSlot frees the bead's global slot at a terminal transition.
// Idempotent — safe to call on any path that ends a slot's active life.
func (r *runner) releaseGlobalSlot(beadID string) {
	if r.run != nil && r.rec != nil {
		if err := gc.PruneTerminalSlotScratch(r.rec.Root, r.run.RunID, beadID); err != nil {
			r.progress("bead %s: terminal scratch cleanup deferred: %v", beadID, err)
		}
	}
	if r.gov == nil {
		return
	}
	_ = r.gov.Release(r.poolKey(), r.opts.ProjectID, beadID)
}

// reportRateLimit informs the machine-wide governor of a rate-limit/overload
// signal from a dead agent's stream (koryph-2im.4): every engine on the host
// shares the same AIMD backoff state for this pool, so a rate limit observed
// by any one of them halves the cap for all of them — but only within THIS
// pool (koryph-v8u.11): an Anthropic rate limit never throttles another
// provider's pool. The bead id makes burst detection count distinct slots
// and lets half-open probe reports match exactly (koryph-2im.11). Fails open
// like every other governor helper — a stuck governor must never wedge
// completion handling.
func (r *runner) reportRateLimit(beadID string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.ReportRateLimit(r.poolKey(), r.opts.ProjectID, beadID, time.Now())
}
