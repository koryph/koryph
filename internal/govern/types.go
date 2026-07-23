// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package govern is the machine-global concurrency governor: a cross-process
// cap on the number of agents running at once across ALL projects, so
// independent `koryph run` invocations cannot collectively breach the Claude
// API concurrency / rate limits.
//
// It coordinates through files under ~/.koryph (paths.SlotsDir) guarded by a
// short flock — no daemon:
//
//   - governor.json          per-provider pools {"pools": {<provider>: {...}}}
//   - slots/<lease>.json      one lease per running agent (keyed to the AGENT pid)
//   - slots/demand/<proj>.json per-project demand heartbeat (fair-share input)
//
// Slots are allocated fair-share across the projects that currently have ready
// work: each demander gets floor(cap/n) slots, the cap%n remainder rotates over
// time (so no project starves when projects outnumber slots), and idle capacity
// is handed out work-conservingly when every other demander already holds its
// share. See docs/developer-guide/global-governor.md.
//
// # Per-provider pools (koryph-v8u.11, design doc L5c)
//
// The service providers behind agent runtimes (Anthropic/claude, OpenAI/codex,
// Google/gemini, xAI/grok build, …) enforce INDEPENDENT rate limits, so a
// single machine-wide pool would be wrong in both directions: an Anthropic 429
// must not throttle codex agents, and codex load must not consume claude
// admission slots. EVERY piece of governor state — operator cap, leases,
// demand, fair share, the AIMD overlay, settle window, circuit breaker,
// dispatch-smoothing clock — is scoped to a pool, keyed by an opaque provider
// string (see DefaultPool, NormalizeProvider, and Config for what one pool
// holds). There is deliberately NO cross-provider shared cap: total machine
// concurrency is the sum of pool caps, since each provider's API is the
// resource being protected (local CPU/RAM pressure is the operator's
// --max-global choice, made per pool).
package govern

import (
	"encoding/json"
	"time"
)

// DefaultMaxGlobalAgents is the cap used when a pool has no configured
// max_global_agents (governor.json absent, or the pool has no entry). Raised
// to 8 to let a single self-hosting project run a wider wave; being monitored
// for Claude API rate limiting — drop to 6 if beads start getting throttled. A
// governor.json still overrides this per machine, per pool.
const DefaultMaxGlobalAgents = 8

// DefaultMaxMachineAgents is the machine-wide ceiling across ALL pools
// combined (koryph-4rk6.2, docs/designs/2026-07-resource-governor.md) used
// when governor.json sets no max_machine_agents. Per-pool caps
// (DefaultMaxGlobalAgents) protect each provider's API; this ceiling protects
// the local machine, since three pools at their individual caps (personal 16
// + anthropic 8 + work 1 = 25) can still jointly sink one laptop. Set to 8 per
// operator direction; a governor.json max_machine_agents overrides it per
// machine. The value is tied to the docs by a drift test (scripts/).
const DefaultMaxMachineAgents = 8

// DefaultMinFreeMemoryMB is the memory admission floor SEEDED onto a pool
// that carries no explicit min_free_memory_mb (koryph-4rk6.1): matches the
// "anthropic" pool's shipped explicit floor at the time of the 2026-07-21 OOM
// incident, where "personal"/"work" had a max_global_agents cap but NO memory
// floor at all — an uneven default that let 11+ agents dispatch before the
// implicit per-host auto floor (sysmem.DefaultFloorMB, still the engine's
// admission-time fallback for a raw 0/unset setting — see
// internal/engine/govern.go's floorFromSetting) caught up. This constant
// exists to make every pool's floor an explicit, uniform, operator-visible
// number in governor.json rather than relying solely on that implicit
// fallback: Store.SetCap seeds it onto a newly-created pool, and
// Store.BackfillMemoryFloors writes it onto any EXISTING pool an older
// koryph version already persisted without one. Neither touches a pool that
// already carries an explicit setting (positive, or negative/disabled) —
// only a raw 0 (never configured, or reset to auto) is backfilled.
const DefaultMinFreeMemoryMB = 2048

// DefaultEstPerAgentMB is the memory a single dispatched agent is assumed to
// consume when the bead declares no res:<kind> footprint (koryph-3xs). Every
// agent is a claude subprocess plus a git worktree; with no declared kind the
// reservation-aware memory clause otherwise priced a kind-less lease at 0, so N
// concurrent kind-less admissions could collectively blow past the free-memory
// floor while each individually cleared it. Applying this default per-agent
// reservation to kind-less leases makes admitting K of them reserve K*est
// against the floor, exactly as res:<kind> beads already reserve their per-kind
// mem_mb. ~1.5 GB is a conservative estimate for a claude agent + worktree;
// tune per pool via `governor set --est-per-agent-mb` or the
// KORYPH_EST_PER_AGENT_MB env override (0 = this default; negative = disabled).
const DefaultEstPerAgentMB = 1536

// DefaultPool is the pool key used when a lease, demand heartbeat, or store
// entry point carries no explicit provider — i.e. today's single implicit
// pool, and the migration target for a legacy (pre-koryph-v8u.11)
// governor.json. The key is deliberately an opaque string (not an enum) so it
// can later refine to "provider:account" (rate limits are really per-account
// within a provider) without another schema change.
const DefaultPool = "anthropic"

// NormalizeProvider maps "" to DefaultPool so every store entry point and
// on-disk record uses a non-empty pool key — the API-boundary normalization
// koryph-v8u.11 requires ("stored state never has an empty key"). Every other
// value passes through unchanged (an opaque pool key).
func NormalizeProvider(provider string) string {
	if provider == "" {
		return DefaultPool
	}
	return provider
}

// Config is one pool's full governor state: the operator cap plus the AIMD
// overlay. A governor.json predating the AIMD overlay (koryph-2im.4,
// docs/designs/2026-07-scheduler-throughput.md L5) unmarshals those fields to
// their zero values, i.e. Adaptive=false, which reproduces the original
// static-cap behavior byte-for-byte — see Config.EffectiveCap.
//
// The settle-window / circuit-breaker / dispatch-smoothing fields
// (koryph-2im.11, docs/designs/2026-07-scheduler-throughput.md L5b) are
// likewise additive and, like the rest of the AIMD overlay, only take effect
// when Adaptive is on — see internal/govern/aimd.go.
//
// Per koryph-v8u.11 (L5c), a Config is now ALWAYS one entry in File.Pools
// (keyed by provider) rather than the whole of governor.json — see File and
// Store's pool-scoped entry points. The type itself is unchanged from
// koryph-2im.11 so every pure AIMD/settle/breaker function below (and its
// existing unit tests) keeps operating on a plain Config value.
type Config struct {
	MaxGlobalAgents int `json:"max_global_agents"`

	// MinFreeMemoryMB is a machine-wide memory admission floor (koryph-930):
	// the scheduler refuses to admit a new agent while the host's available
	// memory is below the floor, deferring the dispatch to a later wave. It
	// guards against OOM when many agents (each a claude subprocess + a git
	// worktree) run concurrently — adaptive concurrency can climb well past
	// MaxGlobalAgents. The gate is ON by default, sized to physical memory.
	// This raw setting is interpreted by readers: >0 an explicit floor in MB;
	// <0 the gate explicitly disabled; 0/unset the auto floor (a fraction of
	// physical RAM — see sysmem.DefaultFloorMB). Lives here, next to
	// MaxGlobalAgents, because free RAM is a machine property shared by every
	// koryph run on the host, exactly like the concurrency cap.
	MinFreeMemoryMB int `json:"min_free_memory_mb,omitempty"`

	// EstPerAgentMB is the per-agent memory reservation applied to a dispatched
	// bead that declares NO res:<kind> footprint (koryph-3xs): the machine-wide
	// memory admission gate (MinFreeMemoryMB) subtracts it, like every declared
	// kind's mem_mb, so N concurrent kind-less agents reserve N*est against the
	// floor instead of 0. Interpreted by readers: >0 an explicit reservation in
	// MB; <0 disabled (kind-less leases reserve 0, the pre-koryph-3xs behavior);
	// 0/unset the package default (DefaultEstPerAgentMB). Lives here beside
	// MinFreeMemoryMB because it is a property of the shared machine memory
	// budget, not of any one project. res:<kind> beads are unaffected — they keep
	// reserving their declared per-kind mem_mb.
	EstPerAgentMB int `json:"est_per_agent_mb,omitempty"`

	// Adaptive enables the AIMD overlay: the effective cap floats between 1
	// and HardMax (probing up on quiet, halving on rate-limit) instead of
	// pinning to MaxGlobalAgents.
	Adaptive bool `json:"adaptive,omitempty"`
	// HardMax bounds upward probing while Adaptive is on; ignored otherwise.
	HardMax int `json:"hard_max,omitempty"`
	// DynamicCap is the current floating cap. Seeded to MaxGlobalAgents when
	// adaptive is enabled; then adjusted by ReportRateLimit (halve) and the
	// lazy additive probe (see Store.EffectiveCap).
	DynamicCap int `json:"dynamic_cap,omitempty"`
	// LastDecreaseAt is the RFC3339 timestamp of the most recent multiplicative
	// decrease. Observability only as of koryph-2im.11 — the additive probe's
	// elapsed-time clock now anchors on SettleUntil (see applyProbe), since
	// the settle window subsumes the old decrease-cooldown's role.
	LastDecreaseAt string `json:"last_decrease_at,omitempty"`
	// LastRateLimitAt is the RFC3339 timestamp of the most recent rate-limit
	// report, applied or merely counted while settling/open.
	LastRateLimitAt string `json:"last_rate_limit_at,omitempty"`
	// LastProbeAt is internal bookkeeping for the additive-increase probe: the
	// RFC3339 timestamp the probe last advanced from. Persisted (not just
	// in-memory) so probing survives an engine restart.
	LastProbeAt string `json:"last_probe_at,omitempty"`
	// RateLimitEvents counts every ReportRateLimit call — applied or
	// suppressed by settle/breaker state — for operator observability
	// (`governor show`, `koryph doctor`).
	RateLimitEvents int `json:"rate_limit_events,omitempty"`

	// --- settle window / burst detection (koryph-2im.11) -------------------

	// SettleSeconds is how long, after ANY DynamicCap change (decrease or
	// additive increase), further changes in either direction are frozen.
	// <=0 uses DefaultSettleSeconds. Subsumes the old 60s decrease cooldown.
	SettleSeconds int `json:"settle_seconds,omitempty"`
	// SettleUntil is the RFC3339 deadline of the current freeze; "" or a past
	// time means not settling. Also anchors the additive probe's clock (the
	// quiet-clock starts at settle expiry, not at the change itself).
	SettleUntil string `json:"settle_until,omitempty"`
	// RecentRateLimitEvents is a small bounded (pruned-on-write) history of
	// rate-limit events within the last 30s, used only to count DISTINCT
	// (project, bead) slots for the burst-scaled decrease.
	RecentRateLimitEvents []RateLimitEvent `json:"recent_rate_limit_events,omitempty"`
	// RecentDecreases is a small bounded (pruned-on-write) history of
	// multiplicative-decrease timestamps within the last 10 minutes, used
	// only to trip the circuit breaker on 3 decreases in that window.
	RecentDecreases []string `json:"recent_decreases,omitempty"`

	// --- circuit breaker (koryph-2im.11) ------------------------------------

	// BreakSeconds is the base open-state duration; doubled per consecutive
	// re-open (BreakerReopenCount), capped at maxBreakSeconds. <=0 uses
	// DefaultBreakSeconds.
	BreakSeconds int `json:"break_seconds,omitempty"`
	// BreakerState is "" (closed), "open", or "half-open".
	BreakerState string `json:"breaker_state,omitempty"`
	// BreakerOpenAt is the RFC3339 timestamp the breaker most recently opened.
	BreakerOpenAt string `json:"breaker_open_at,omitempty"`
	// BreakerBreakSeconds is the concrete (already-doubled, already-capped)
	// duration of the CURRENT open period, computed once at open time so a
	// later BreakSeconds config edit cannot retroactively change it.
	BreakerBreakSeconds int `json:"breaker_break_seconds,omitempty"`
	// BreakerReopenCount is the number of consecutive times the breaker has
	// re-opened after a failed half-open probe; resets to 0 on a clean close.
	BreakerReopenCount int `json:"breaker_reopen_count,omitempty"`
	// ProbeProject/ProbeBead identify the single lease admitted while
	// half-open (the probe dispatch); both empty means no probe outstanding.
	ProbeProject string `json:"probe_project,omitempty"`
	ProbeBead    string `json:"probe_bead,omitempty"`
	// ProbeAdmittedAt is the RFC3339 timestamp the probe was admitted, used by
	// the crashed-probe timeout fallback (a probe that dies without a release
	// or a rate-limit report cannot wedge the breaker half-open forever).
	ProbeAdmittedAt string `json:"probe_admitted_at,omitempty"`

	// --- dispatch smoothing (koryph-2im.11) ---------------------------------

	// MinDispatchIntervalSeconds is the machine-wide minimum spacing between
	// admitted dispatches, jittered ±50%. <=0 uses
	// DefaultMinDispatchIntervalSeconds. Gated on Adaptive, like the rest of
	// this section, for zero behavior change on non-adaptive setups.
	MinDispatchIntervalSeconds int `json:"min_dispatch_interval_seconds,omitempty"`
	// LastAdmitAt is the RFC3339 timestamp of the most recent admitted
	// dispatch (any project IN THIS POOL) — the spacing clock's anchor.
	LastAdmitAt string `json:"last_admit_at,omitempty"`
}

// RateLimitEvent is one bounded entry in Config.RecentRateLimitEvents: enough
// identity (project+bead) to count DISTINCT in-flight slots reporting a
// rate-limit within the burst window.
type RateLimitEvent struct {
	At      string `json:"at"`
	Project string `json:"project,omitempty"`
	Bead    string `json:"bead,omitempty"`
}

// File is the on-disk shape of governor.json: independent per-provider
// governor pools (koryph-v8u.11, L5c). See Config for what one pool holds.
//
// UnmarshalJSON transparently migrates a legacy (pre-koryph-v8u.11)
// governor.json — a flat document with no top-level "pools" key, i.e. every
// shape from koryph-1xk/2im.4/2im.11 — into Pools[DefaultPool], preserving
// every field. This is what makes "an existing single-pool governor.json
// loads transparently as the anthropic pool" true for every reader (Store AND
// internal/doctor, which parses governor.json directly): the migration lives
// once, here, rather than being re-implemented at each call site.
type File struct {
	Pools map[string]Config `json:"pools"`

	// MaxMachineAgents is the machine-wide ceiling across ALL pools combined
	// (koryph-4rk6.2): the sum of live leases over every provider pool may
	// never exceed it, regardless of each pool's own max_global_agents. It
	// lives at the File level (like Resources) because it is a machine
	// property, not a provider one — per-pool caps protect each API, this
	// protects the host. <=0 (absent/unset) resolves to DefaultMaxMachineAgents
	// via machineCeiling. Additive/omitempty: a governor.json without the key
	// round-trips unchanged and the legacy flat-document migration leaves it 0.
	// Like Resources, File.UnmarshalJSON MUST decode it explicitly (see below)
	// or every readFile would silently drop it.
	MaxMachineAgents int `json:"max_machine_agents,omitempty"`

	// Resources is the machine's top-level external-resource ledger
	// (koryph-4ql.1, docs/designs/2026-07-resource-governor.md L2),
	// deliberately OUTSIDE the per-provider pools because RAM/clusters/daemons
	// are machine properties shared across every provider. nil means "no
	// resources configured" — which is NOT resource-free behavior: every kind
	// a bead declares still binds at the fail-safe default capacity 1 (see
	// ResourcesConfig). Additive/omitempty: a governor.json with no "resources"
	// key round-trips unchanged, and the legacy flat-document migration leaves
	// this nil. There is NO custom MarshalJSON, so default marshaling emits
	// this field — but UnmarshalJSON is custom and MUST decode it explicitly
	// (see below), or every readFile would silently drop it.
	Resources *ResourcesConfig `json:"resources,omitempty"`
}

// UnmarshalJSON implements the legacy-shape migration described on File.
func (f *File) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if raw, ok := probe["pools"]; ok {
		var pools map[string]Config
		if err := json.Unmarshal(raw, &pools); err != nil {
			return err
		}
		f.Pools = pools
		// koryph-4rk6.2: the machine-wide ceiling rides the pools-shaped
		// document alongside the resource ledger; this custom decoder copies
		// only keys it names, so decode "max_machine_agents" explicitly or it
		// is dropped on every readFile and stripped by the next whole-file
		// rewrite. Absent → 0 (machineCeiling resolves the default).
		if mraw, ok := probe["max_machine_agents"]; ok {
			if err := json.Unmarshal(mraw, &f.MaxMachineAgents); err != nil {
				return err
			}
		}
		// koryph-4ql.1 (L2): the machine resource ledger rides the
		// pools-shaped document. This custom decoder only copies keys it names,
		// so a struct field alone would be silently dropped on every readFile
		// and then stripped from disk by the next setter's whole-file rewrite —
		// decode "resources" explicitly here. Absent on this path → nil
		// (no capacity configured; declared kinds still serialize at default 1).
		if rraw, ok := probe["resources"]; ok {
			var rc ResourcesConfig
			if err := json.Unmarshal(rraw, &rc); err != nil {
				return err
			}
			f.Resources = &rc
		}
		return nil
	}
	// No "pools" key: this is a pre-koryph-v8u.11 document. The whole thing
	// IS one flat Config — decode it as such and wrap it as the sole
	// anthropic pool so every existing field (cap, AIMD overlay, settle,
	// breaker, smoothing) round-trips unchanged. A legacy flat document
	// predates the resource ledger entirely, so f.Resources genuinely stays
	// nil here (koryph-4ql.1) — the first setter rewrite reshapes it into the
	// pools envelope, still resource-free.
	var legacy Config
	if err := json.Unmarshal(data, &legacy); err != nil {
		return err
	}
	f.Pools = map[string]Config{DefaultPool: legacy}
	return nil
}

// MachineCeiling resolves the machine-wide agent ceiling across ALL pools
// (koryph-4rk6.2): the configured MaxMachineAgents when >0, else
// DefaultMaxMachineAgents. The default ALWAYS binds — including for a File
// with no key at all — so a machine that never configured a ceiling still
// caps total concurrency at the safe default rather than at the unbounded sum
// of its pool caps. Safe on a zero-value receiver. Exported so readers that
// parse governor.json directly (internal/doctor) resolve it identically to the
// admission path rather than re-implementing the default fallback.
func (f File) MachineCeiling() int {
	if f.MaxMachineAgents > 0 {
		return f.MaxMachineAgents
	}
	return DefaultMaxMachineAgents
}

// PoolStatus is one pool's full observable state, for `koryph governor show`
// and `koryph doctor` (koryph-v8u.11): the live leases and demand heartbeats
// plus the AIMD/settle/breaker/smoothing overlay. AIMD.EffectiveCap() is the
// cap Acquire actually admits against for this pool.
type PoolStatus struct {
	Pool   string
	Leases []Lease
	Demand []Demand
	AIMD   Config
}

// Lease records one running agent holding a global slot in one provider pool
// (koryph-v8u.11). It is keyed to the detached AGENT pid so the lease
// survives an engine restart/resume and frees only when the real agent
// process dies.
type Lease struct {
	Project    string `json:"project"`
	Bead       string `json:"bead"`
	PID        int    `json:"pid"`        // agent process id
	EnginePID  int    `json:"engine_pid"` // owning koryph run pid
	Model      string `json:"model,omitempty"`
	AcquiredAt string `json:"acquired_at"` // RFC3339
	// Provider identifies the pool this lease counts against (koryph-v8u.11).
	// "" means DefaultPool (anthropic) — every Store entry point normalizes
	// before it reads/writes, so a freshly written lease always carries the
	// resolved, non-empty pool key; a lease file written before this field
	// existed decodes with Provider=="" and is treated as anthropic, which is
	// exactly what it always was.
	Provider string `json:"provider,omitempty"`

	// Resources are the machine resource kinds this agent declared it will
	// provision/consume (koryph-4ql.1, docs/designs/2026-07-resource-governor.md
	// L2): the RESOLVED kind tokens (not the res:* labels — the engine resolves
	// them at dispatch and freezes them here per I8). Empty = "agent + worktree
	// only", the common lightweight case, which is byte-for-byte today's
	// behavior (I9). Counted cross-pool against per-kind capacity in Acquire.
	Resources []string `json:"resources,omitempty"`

	// MemReserveMB is this agent's declared memory reservation (koryph-4ql.1,
	// L5): Σ mem_mb over its declared kinds, resolved by the engine at dispatch
	// (machine ledger override → project vocabulary). Subtracted from availMB
	// while the lease is ramping (now-AcquiredAt < ramp_seconds) so admission
	// sees declared future demand, not just the current reading. 0 = undeclared
	// (degrades to the koryph-930 floor, today's behavior). Both fields are
	// omitempty/additive: an old lease file decodes as resource-free.
	MemReserveMB int `json:"mem_reserve_mb,omitempty"`
}

// Demand is a project's "I have ready work and want slots" heartbeat in one
// provider pool (koryph-v8u.11), refreshed each wave and pruned when stale or
// its engine dies. The set of live demands within a pool is that pool's
// fair-share denominator.
type Demand struct {
	Project   string `json:"project"`
	EnginePID int    `json:"engine_pid"`
	UpdatedAt string `json:"updated_at"` // RFC3339
	// Provider identifies the pool this demand heartbeat counts in
	// (koryph-v8u.11); see Lease.Provider for the "" == DefaultPool contract.
	Provider string `json:"provider,omitempty"`
}

// --- machine resource ledger (koryph-4ql.1, L2/L5) -------------------------

const (
	// DefaultResourceCapacity is the capacity a DECLARED but unconfigured
	// resource kind binds at (L2): the fail-safe-serial default. It ALWAYS
	// binds — even for a nil ResourcesConfig / a governor.json with no
	// resources section at all — so two beads declaring the same unconfigured
	// kind can never co-dispatch (the whole point of §1.2). Distinct unknown
	// kinds do NOT collide with each other (unlike footprints' domain:unknown).
	DefaultResourceCapacity = 1

	// DefaultRampSeconds is the ramp window (L5) applied when neither the kind
	// nor the top-level resources config sets one. A lease's declared
	// MemReserveMB is reserved for this long after AcquiredAt, then assumed
	// materialized (retired to avoid double-counting the real availMB reading).
	// Sized to worst-case time-to-provision, per §7's late-provisioning risk.
	DefaultRampSeconds = 600
)

// ResourcesConfig is governor.json's top-level machine resource ledger
// (koryph-4ql.1, L2): the shared external-resource kinds this host can run.
// See File.Resources for why it lives outside the per-provider pools and for
// the nil-means-"default-1-still-binds" contract. Growth is bounded (kinds × 4
// scalar fields, no per-event history — the aimd.go maxRecentEvents
// convention, §7).
type ResourcesConfig struct {
	// RampSeconds is the global default ramp window in seconds (L5); a per-kind
	// ResourceKind.RampSeconds overrides it. <=0 falls back to
	// DefaultRampSeconds.
	RampSeconds int `json:"ramp_seconds,omitempty"`
	// Kinds maps an opaque resource kind token ([a-z0-9-]+, matched exactly)
	// to its machine capacity/cost. A kind declared on a bead but ABSENT here
	// still resolves to {Capacity:1, MemMB:0} (see capacityOf) — configured
	// only to raise the capacity above 1 or to attach a reservation/probe.
	Kinds map[string]ResourceKind `json:"kinds,omitempty"`
}

// ResourceKind is one external resource kind's machine configuration
// (koryph-4ql.1, L2). Every field is additive/omitempty; an absent kind
// resolves to {Capacity:1, MemMB:0, ramp default, no probe}.
type ResourceKind struct {
	// Capacity is the max number of live leases that may hold this kind at
	// once ACROSS ALL POOLS (machine resources are cross-pool). <=0 resolves
	// to DefaultResourceCapacity (1). Capacity 1 is the exclusive case;
	// capacity N is "up to N threads may share this kind".
	Capacity int `json:"capacity,omitempty"`
	// MemMB is the per-holder memory reservation charged during the ramp
	// window (L5). 0 = uncalibrated (no reservation) — an unconfigured machine
	// gets capacity serialization but no reservations (R9 calibrates this).
	MemMB int `json:"mem_mb,omitempty"`
	// RampSeconds overrides ResourcesConfig.RampSeconds for this kind (L5);
	// <=0 falls back to the global default.
	RampSeconds int `json:"ramp_seconds,omitempty"`
	// Probe is an operator-authored shell command that lists live instance
	// names for leak detection (L7); consumed only by the patrol/doctor
	// (R8), NEVER on the admission path (I7). Stored here so
	// `governor set-resource --probe` round-trips; admission ignores it.
	Probe string `json:"probe,omitempty"`
}

// capacityOf returns the effective capacity for kind (koryph-4ql.1, L2): the
// machine Kinds[kind].Capacity when >0, else DefaultResourceCapacity. The
// default ALWAYS binds — including for a nil rc — which is what serializes two
// holders of a declared-but-unconfigured kind. Safe on a nil receiver.
func (rc *ResourcesConfig) capacityOf(kind string) int {
	if rc != nil {
		if k, ok := rc.Kinds[kind]; ok && k.Capacity > 0 {
			return k.Capacity
		}
	}
	return DefaultResourceCapacity
}

// memMBOf returns kind's configured per-holder reservation (0 when unconfigured
// or nil rc). Safe on a nil receiver.
func (rc *ResourcesConfig) memMBOf(kind string) int {
	if rc != nil {
		if k, ok := rc.Kinds[kind]; ok {
			return k.MemMB
		}
	}
	return 0
}

// probeOf returns kind's configured leak-probe command ("" when unset). Safe
// on a nil receiver.
func (rc *ResourcesConfig) probeOf(kind string) string {
	if rc != nil {
		if k, ok := rc.Kinds[kind]; ok {
			return k.Probe
		}
	}
	return ""
}

// globalRampSeconds returns the machine default ramp window (L5): the
// top-level RampSeconds when >0, else DefaultRampSeconds. Safe on a nil
// receiver.
func (rc *ResourcesConfig) globalRampSeconds() int {
	if rc != nil && rc.RampSeconds > 0 {
		return rc.RampSeconds
	}
	return DefaultRampSeconds
}

// rampSecondsOf returns kind's effective ramp window (L5): the per-kind
// override when >0, else the global default. Safe on a nil receiver.
func (rc *ResourcesConfig) rampSecondsOf(kind string) int {
	if rc != nil {
		if k, ok := rc.Kinds[kind]; ok && k.RampSeconds > 0 {
			return k.RampSeconds
		}
	}
	return rc.globalRampSeconds()
}

// leaseRamping reports whether l is still inside its ramp window at now (L5):
// its declared MemReserveMB is reserved (subtracted from availMB), not yet
// assumed materialized in the real reading. The window is the MAX ramp_seconds
// across the kinds l holds — a lease holding several kinds ramps until the
// slowest is assumed up — falling back to the global default when l holds no
// configured kind. An unparseable AcquiredAt is treated as ramping (the
// over-reserving, safe direction, per §7's double-count note). Pure.
func leaseRamping(l Lease, rc *ResourcesConfig, now time.Time) bool {
	acq := parseTime(l.AcquiredAt)
	if acq.IsZero() {
		return true // unknown age → assume ramping (safe: over-reserve)
	}
	ramp := rc.globalRampSeconds()
	for _, kind := range l.Resources {
		if r := rc.rampSecondsOf(kind); r > ramp {
			ramp = r
		}
	}
	return now.Sub(acq) < time.Duration(ramp)*time.Second
}

// MemInput carries an optional current-memory reading into AcquireEx for the
// reservation-aware memory clause (koryph-4ql.1, L5). A zero value (AvailMB==0
// or FloorMB<=0) SKIPS the memory clause entirely, so a caller with no reading
// (or a disabled floor) passes MemInput{} and keeps only the capacity clause.
// The engine resolves the reading and floor OUTSIDE the flock (I7) and hands
// the pair in; Acquire admits under the flock where it can see every engine's
// reservations.
type MemInput struct {
	AvailMB uint64 // current host available memory, MB (0 = no reading)
	FloorMB int    // effective admission floor, MB (<=0 = clause off)
}

// AdmitOutcome classifies an AcquireEx verdict (koryph-4ql.1, L3) so the engine
// (R3) can route skip-vs-break: a pool-wide denial breaks the batch as today,
// while a per-bead resource/memory denial skips just that bead and keeps
// packing.
type AdmitOutcome int

const (
	// AdmitGranted: a slot was taken.
	AdmitGranted AdmitOutcome = iota
	// AdmitDeniedCap: a pool-wide condition — pool cap, fair share, the circuit
	// breaker (open, or half-open with a probe already outstanding), or
	// dispatch smoothing. The engine batch-breaks (nothing else fits either).
	AdmitDeniedCap
	// AdmitDeniedResource: a declared kind is at capacity across all pools
	// (L2). Per-bead — the engine skips this bead. DeniedKind/DeniedCapacity/
	// DeniedHolders/HolderProject/HolderBead describe it for the deferral line.
	AdmitDeniedResource
	// AdmitDeniedMemory: the reservation-aware memory floor (L5) refused the
	// candidate. CandidateTipped splits a per-bead skip (the candidate's own
	// MemReserveMB tipped the inequality — it would have passed at 0) from a
	// pure floor breach (batch-break: even a 0-reserve bead fails).
	AdmitDeniedMemory
)

// AdmitResult is AcquireEx's typed verdict (koryph-4ql.1, L3). Granted is the
// boolean the legacy Acquire returns; Outcome plus the descriptive fields drive
// the engine's typed deferrals. Only the fields relevant to Outcome are set.
type AdmitResult struct {
	Granted bool
	Outcome AdmitOutcome

	// Populated only for AdmitDeniedResource:
	DeniedKind     string // the kind at capacity
	DeniedCapacity int    // that kind's resolved capacity (the "/1" in "1/1")
	DeniedHolders  int    // live holders counted (the "1" in "1/1")
	HolderProject  string // a representative current holder (deterministic)
	HolderBead     string

	// Populated only for AdmitDeniedMemory:
	CandidateTipped bool // the candidate's own MemReserveMB tipped the floor
}

// ResourceStatus is one kind's live observable state for `koryph governor show`
// and the cockpit (koryph-4ql.1, L7), computed from the machine ledger + live
// leases (the PoolStatus precedent) so the CLI/IDE bead only RENDERS it. All
// accounting lives here.
type ResourceStatus struct {
	Kind        string
	Capacity    int    // resolved (default 1 when unconfigured)
	MemMB       int    // configured per-holder reservation (0 = uncalibrated)
	RampSeconds int    // resolved ramp window
	Probe       string // configured leak-probe command ("" = none)
	Holders     []ResourceHolder
	// ReservedMB is Σ MemReserveMB of holders still ramping (the live
	// reservation still subtracted from availMB); MaterializedMB is the rest
	// (holders past their ramp, assumed showing in the real reading). A holder
	// spanning multiple kinds has its full MemReserveMB attributed to EACH
	// kind here (rare; observability only, not the admission accounting).
	ReservedMB     int
	MaterializedMB int
}

// ResourceHolder identifies one live lease holding a kind, with its ramp
// state, for ResourceStatus (koryph-4ql.1, L7).
type ResourceHolder struct {
	Project      string
	Bead         string
	MemReserveMB int
	Ramping      bool
}
