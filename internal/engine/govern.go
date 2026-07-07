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

// providerAnthropic is the pool every lease this engine constructs is
// admitted against today (koryph-v8u.11, L5c: independent per-provider
// governor pools — see internal/govern's package doc). Hardcoded rather than
// resolved from the bead/project until the koryph-v8u.2 runtime adapters
// land and can supply the actual provider behind the dispatched agent; until
// then every agent this engine launches IS a claude/Anthropic session, so
// this constant is exactly today's (only) behavior, made explicit at the one
// place leases are constructed.
const providerAnthropic = govern.DefaultPool

// refreshDemand records this project's demand for slots (it has ready work).
func (r *runner) refreshDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.RefreshDemand(providerAnthropic, r.opts.ProjectID, os.Getpid())
}

// dropDemand withdraws this project from the fair-share denominator.
func (r *runner) dropDemand() {
	if r.gov == nil {
		return
	}
	_ = r.gov.DropDemand(providerAnthropic, r.opts.ProjectID)
}

// warnIfOverFairShare logs, once per run, when this project's configured wave
// width exceeds its current global fair share — the deliberate per-project
// override the operator asked for, surfaced as a fairness/rate-limit risk.
func (r *runner) warnIfOverFairShare() {
	if r.gov == nil || r.govWarned {
		return
	}
	fs, err := r.gov.FairShareFor(providerAnthropic, r.opts.ProjectID)
	if err != nil || r.width <= fs {
		return
	}
	r.govWarned = true
	r.progress("warning: project width %d exceeds its global fair share %d (cap %d across active projects) — extra slots wait for others to idle and may pressure the Claude API rate limit",
		r.width, fs, r.gov.Cap(providerAnthropic))
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
		configured = r.gov.MinFreeMemoryMB(providerAnthropic)
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

// acquireGlobalSlot reserves a global concurrency slot for beadID (keyed to the
// project+bead under this engine's pid; the agent pid is attached later by
// bindGlobalSlot). Returns true when granted or when governance is unavailable.
func (r *runner) acquireGlobalSlot(beadID string) bool {
	// Memory admission gate (koryph-930): refuse to stack another agent's
	// subprocess+worktree when the host is under memory pressure. Checked
	// BEFORE the flocked governor Acquire so the (possibly subprocess-backed)
	// memory probe never runs while holding the machine-wide lock.
	if !r.memoryAdmits(beadID) {
		return false
	}
	if r.gov == nil {
		return true
	}
	ok, err := r.gov.Acquire(govern.Lease{
		Project:   r.opts.ProjectID,
		Bead:      beadID,
		EnginePID: os.Getpid(),
		Provider:  providerAnthropic,
	})
	if err != nil {
		r.progress("bead %s: global governor error (allowing dispatch): %v", beadID, err)
		return true
	}
	return ok
}

// holdGlobalSlot attaches the launched agent pid to the bead's lease (keyed to a
// process that outlives the engine) so the running agent is always counted —
// including a requeue/resume whose reservation was pruned. Cap admission already
// happened at acquireGlobalSlot; this is an unconditional 1:1 update.
func (r *runner) holdGlobalSlot(beadID string, agentPID int, model string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.Hold(govern.Lease{
		Project:   r.opts.ProjectID,
		Bead:      beadID,
		PID:       agentPID,
		EnginePID: os.Getpid(),
		Model:     model,
		Provider:  providerAnthropic,
	})
}

// releaseGlobalSlot frees the bead's global slot at a terminal transition.
// Idempotent — safe to call on any path that ends a slot's active life.
func (r *runner) releaseGlobalSlot(beadID string) {
	if r.gov == nil {
		return
	}
	_ = r.gov.Release(providerAnthropic, r.opts.ProjectID, beadID)
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
	_ = r.gov.ReportRateLimit(providerAnthropic, r.opts.ProjectID, beadID, time.Now())
}
