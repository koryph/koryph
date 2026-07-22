// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/sysmem"
)

// The global concurrency governor caps concurrently running agents across ALL
// projects (koryph-1xk) so independent `koryph run` processes cannot
// collectively breach the Claude API rate limits. Every helper fails OPEN — a
// governor error never blocks dispatch — because a stuck governor must not wedge
// the engine; the cap is a safety rail, not a correctness dependency.

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
// own pool, else 0 (no seed to offer). It is wired into r.gov.SeedCap at
// startup so govern's fallbackCap can consult it without govern importing
// package quota (layering) — govern calls this closure with an already
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

// resolveMemReserveMB sums the per-kind memory reservation for the resolved
// resource kinds (koryph-4ql.3, design L5). Resolution order per kind, matching
// L2: the machine ledger's mem_mb (govern Store.Resources()) when set, else the
// project vocabulary's mem_mb (cfg.Resources), else 0. Resolved once at dispatch
// and frozen on the ledger slot (I8), so a vocabulary edit mid-run never
// re-prices a live slot. A bead with no declared kinds reserves 0 — today's
// behavior (I9).
func (r *runner) resolveMemReserveMB(kinds []string) int {
	if len(kinds) == 0 {
		return 0
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

// acquireGlobalSlot reserves a global concurrency slot for beadID (keyed to the
// project+bead under this engine's pid; the agent pid is attached later by
// holdGlobalSlot), passing the bead's resolved resource kinds and memory
// reservation (koryph-4ql.3, L2/L5) so the flocked governor can apply the
// cross-pool capacity and reservation-aware memory clauses. Returns a typed
// verdict (admitGranted / admitSkip / admitBreak); every error path fails OPEN
// (admitGranted) because the governor is a safety rail, not a correctness
// dependency (I6).
func (r *runner) acquireGlobalSlot(beadID string, kinds []string, memReserveMB int) admitVerdict {
	// Memory admission gate (koryph-930): refuse to stack another agent's
	// subprocess+worktree when the host is already under memory pressure.
	// Checked BEFORE the flocked governor Acquire so the (possibly
	// subprocess-backed) memory probe never runs while holding the machine-wide
	// lock (I7). A candidate-agnostic floor breach is machine-wide → break.
	if !r.memoryAdmits(beadID) {
		return admitBreak
	}
	if r.gov == nil {
		return admitGranted
	}
	// Reservation-aware memory reading (L5), resolved OUTSIDE the flock (I7) and
	// handed to AcquireEx, which subtracts every engine's ramping reservations
	// under the lock. No usable reading (or a disabled floor) → MemInput{}, which
	// skips the memory clause and keeps only the capacity clause (fail open, I6).
	var mem govern.MemInput
	if stat, ok := r.memStat(); ok {
		if floor := r.memoryFloorMB(stat.TotalMB()); floor > 0 {
			mem = govern.MemInput{AvailMB: stat.AvailableMB(), FloorMB: floor}
		}
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
		r.progress("bead %s: global governor error (allowing dispatch): %v", beadID, err)
		return admitGranted
	}
	return r.classifyAdmit(beadID, memReserveMB, res)
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
	default: // AdmitDeniedCap and any unknown outcome: machine-wide → break.
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
