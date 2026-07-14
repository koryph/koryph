// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

// health.go — periodic in-loop health patrol (koryph-gus).
//
// Every healthInterval (default 10 m, config + --health-interval) the engine
// runs a curated subset of the checks that `koryph doctor` performs:
//
//   - zombie governor leases (stage-aware: skip post-build stages where the
//     engine PID is still alive — same logic as koryph-p42)
//   - suspected leaked external resources: a CURRENT-run slot whose agent
//     died while it held declared Slot.Resources, level warn — report-only,
//     NEVER auto-torn-down (koryph-4ql.8, design
//     docs/designs/2026-07-resource-governor.md L7)
//   - per-kind resource-probe diffing: an operator-configured probe command's
//     output diffed against the `<kind>-<bead-id>` naming convention and the
//     governor's live leases, level warn per suspected leaked instance
//     (koryph-4ql.8, L7 "per-kind probe (opt-in)"), sharing the diff
//     primitive with `koryph doctor` (internal/doctor.DiffResourceProbe)
//   - stale demand heartbeats from OTHER engine PIDs (not our own)
//   - governor pool sanity (cap > 0, settle/breaker timestamps not wedged)
//   - quota window burn vs warn/drain/stop thresholds
//   - bd reachability (binary on PATH + beads dir accessible)
//   - ledger/telemetry dir writable
//   - completed-but-unvalidated epics: OPEN epics whose children are all
//     closed but that never entered (or fell out of) the runner's in-memory
//     epic-validation pending set are re-queued, level info — self-
//     remediating, not an operator action (koryph-bbe)
//   - parked/degraded epic validations: OPEN epics carrying
//     validation:parked or validation:degraded, level warn — an operator
//     decision is required, mirroring internal/doctor's checkEpicValidations
//     (koryph-wo0.7)
//
// Findings are:
//  1. surfaced as progress WARN/INFO lines, throttled to once per hour per
//     unique finding (same finding key → at most one log line per
//     patrolThrottleWindow)
//  2. appended to the run ledger as PatrolEvent entries for post-mortem history
//
// Safe auto-remediation only: zombie governor lease files whose agent PID AND
// engine PID are both dead, AND whose bead is in a terminal slot state in this
// run (merged/blocked/closed), are removed. Everything else is report-only
// (the completed-but-unvalidated-epics check re-queues into the existing
// pending set rather than mutating beads directly — the ordinary validation
// flow in epicvalidate.go does the actual work on a later tick).
//
// Checks are designed to complete in < 1 s: no network calls, no ccusage
// subprocess, and (with one exception) no bd subprocess. The bd reachability
// check only looks up the binary on PATH and stats the beads dir. Quota state
// uses the cached governor result from the most recent governorGate() call.
// The completed-but-unvalidated-epics check is the exception: listing every
// epic and its children needs one bd call, so the listing is cached and
// refreshed at most once per epicListCadence (patrolThrottleWindow, 1 h)
// rather than once per healthInterval tick (default 10 m) — see
// epicListCadence's doc comment below.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/doctor"
	"github.com/koryph/koryph/internal/epicreview"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/quota"

	gcpkg "github.com/koryph/koryph/internal/gc"
	"github.com/koryph/koryph/internal/worktree"
)

// defaultHealthIntervalSec is the health patrol cadence when no override is set.
const defaultHealthIntervalSec = 600 // 10 minutes

// patrolThrottleWindow suppresses repeated identical findings so the patrol
// never floods the operator with the same message every 10 minutes.
const patrolThrottleWindow = time.Hour

// patrolSettleWedgeThreshold is how far in the future a SettleUntil timestamp
// must be before we flag it as wedged (a legitimate settle window is ≤ the
// DefaultSettleSeconds of ~5 minutes; 2 h means the clock or config is broken).
const patrolSettleWedgeThreshold = 2 * time.Hour

// patrolBreakerWedgeAge is how long a circuit breaker may sit open before we
// flag it as wedged rather than simply "open" (a freshly opened breaker is
// expected; one open for > 8 h without ever transitioning to half-open is stuck).
const patrolBreakerWedgeAge = 8 * time.Hour

// patrolFinding is an internal (engine-package-private) finding from one check.
type patrolFinding struct {
	check string
	// level is "ok" (healthy), "warn" (needs an operator's attention), or
	// "info" (a self-remediating action was taken — reported for visibility,
	// not because anything is wrong; used by patrolCheckUnvalidatedEpics).
	level   string
	message string
	fixed   bool
}

// healthInterval resolves the effective health patrol cadence, highest
// precedence first:
//  1. KORYPH_HEALTH_INTERVAL_SEC env — operator/test override.
//  2. Options.HealthIntervalSec, when the caller set it (> 0).
//  3. The project config's health_interval_seconds (> 0).
//  4. defaultHealthIntervalSec (600 s / 10 m).
func (r *runner) healthInterval() time.Duration {
	if v, ok := envInt("KORYPH_HEALTH_INTERVAL_SEC"); ok && v > 0 {
		return time.Duration(v) * time.Second
	}
	if r.opts.HealthIntervalSec > 0 {
		return time.Duration(r.opts.HealthIntervalSec) * time.Second
	}
	if r.cfg != nil && r.cfg.HealthIntervalSeconds > 0 {
		return time.Duration(r.cfg.HealthIntervalSeconds) * time.Second
	}
	return time.Duration(defaultHealthIntervalSec) * time.Second
}

// patrolIfDue fires the health patrol when at least healthInterval has elapsed
// since the last run. Cheap to call on every loop tick — the interval check is
// a single time.Since comparison.
func (r *runner) patrolIfDue(ctx context.Context) {
	if r.lastPatrolAt.IsZero() || time.Since(r.lastPatrolAt) >= r.healthInterval() {
		r.runPatrol(ctx)
		r.lastPatrolAt = time.Now()
	}
}

// runPatrol executes all curated patrol checks, surfaces WARN findings as
// throttled progress lines, and appends the full set to the run ledger.
func (r *runner) runPatrol(ctx context.Context) {
	now := time.Now()
	findings := r.collectPatrolFindings(ctx, now)

	// Surface WARN and INFO findings as progress lines, throttled to
	// patrolThrottleWindow per unique finding key to avoid spamming the
	// operator every 10 minutes with the same persistent issue. OK findings
	// never print — they carry nothing for an operator to see.
	for i := range findings {
		f := &findings[i]
		if f.level != "warn" && f.level != "info" {
			continue
		}
		key := f.check + "\x00" + f.message
		if last, seen := r.patrolSeen[key]; seen && now.Sub(last) < patrolThrottleWindow {
			continue
		}
		if r.patrolSeen == nil {
			r.patrolSeen = make(map[string]time.Time)
		}
		r.patrolSeen[key] = now
		suffix := ""
		if f.fixed {
			suffix = " [auto-fixed]"
		}
		r.progress("health patrol %s [%s]: %s%s", strings.ToUpper(f.level), f.check, f.message, suffix)
	}

	// Append to the run ledger for post-mortem visibility. Only persist when
	// there are findings to record (skip all-OK patrols to keep the ledger trim).
	if r.run == nil {
		return
	}
	warnOrFixed := false
	for _, f := range findings {
		if f.level != "ok" || f.fixed {
			warnOrFixed = true
			break
		}
	}
	if !warnOrFixed {
		return
	}
	pf := make([]ledger.PatrolFinding, len(findings))
	for i, f := range findings {
		pf[i] = ledger.PatrolFinding{
			Check:   f.check,
			Level:   f.level,
			Message: f.message,
			Fixed:   f.fixed,
		}
	}
	r.run.PatrolEvents = append(r.run.PatrolEvents, ledger.PatrolEvent{
		At:       now.UTC().Format(time.RFC3339),
		Findings: pf,
	})
	_ = r.store.SaveRun(r.run)
}

// collectPatrolFindings runs each curated check and returns the merged results.
// Each check is cheap: no network calls, no subprocesses except the bd binary
// lookup, all results from in-memory state or local file I/O.
func (r *runner) collectPatrolFindings(ctx context.Context, now time.Time) []patrolFinding {
	var out []patrolFinding
	out = append(out, r.patrolCheckZombieLeases(now)...)
	out = append(out, r.patrolCheckLeakedResources(now)...)
	out = append(out, r.patrolCheckResourceProbes(ctx)...)
	out = append(out, r.patrolCheckStaleDemand(now)...)
	out = append(out, r.patrolCheckGovernorPool(now)...)
	out = append(out, r.patrolCheckQuotaBurn()...)
	out = append(out, r.patrolCheckBD()...)
	out = append(out, r.patrolCheckLedgerWritable()...)
	out = append(out, r.patrolCheckGCFootprint()...)
	out = append(out, r.patrolCheckStaleWorktrees(ctx)...)
	out = append(out, r.patrolCheckUnvalidatedEpics(ctx, now)...)
	out = append(out, r.patrolCheckEpicValidations(ctx, now)...)
	return out
}

// --- check: zombie governor leases ----------------------------------------

const patrolCheckZombies = "zombie-leases"

// patrolCheckZombieLeases mirrors the logic in internal/doctor's checkZombieLeases
// (koryph-p42 stage-awareness) with safe auto-remediation: a lease file is
// removed only when BOTH pids are dead AND the bead is in a terminal slot
// state in this engine's run (or comes from a foreign engine run entirely).
func (r *runner) patrolCheckZombieLeases(now time.Time) []patrolFinding {
	slotsDir := paths.SlotsDir()
	entries, err := os.ReadDir(slotsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []patrolFinding{{check: patrolCheckZombies, level: "ok", message: "no slots dir"}}
		}
		return []patrolFinding{{check: patrolCheckZombies, level: "warn",
			message: fmt.Sprintf("read slots dir: %v", err)}}
	}

	myPID := os.Getpid()
	var findings []patrolFinding
	clean := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		path := filepath.Join(slotsDir, e.Name())
		var l govern.Lease
		data, rerr := os.ReadFile(path)
		if rerr != nil || json.Unmarshal(data, &l) != nil || l.Project == "" {
			continue
		}

		// Determine the probe PID: if the agent has not started yet (PID 0),
		// use the engine PID as the liveness signal (mirrors govern.Store.prune).
		probePID := l.PID
		if probePID <= 0 {
			probePID = l.EnginePID
		}

		agentDead := probePID <= 0 || !dispatch.Alive(probePID)
		engineDead := l.EnginePID <= 0 || !dispatch.Alive(l.EnginePID)

		// koryph-p42: a live engine holding this lease means the slot is in a
		// post-build stage (review/rebase/gate/merge) — the normal shape once the
		// agent process exits. Never flag as zombie while the engine is still up.
		if !engineDead {
			clean++
			continue
		}
		if !agentDead {
			clean++
			continue
		}

		pool := govern.NormalizeProvider(l.Provider)
		f := patrolFinding{
			check:   patrolCheckZombies,
			level:   "warn",
			message: fmt.Sprintf("zombie lease: pool %s: %s/%s (agent pid %d, engine pid %d both dead)", pool, l.Project, l.Bead, l.PID, l.EnginePID),
		}

		// Safe auto-remediation: remove the lease file when it belongs to this
		// engine's run AND the bead is already in a terminal slot state OR when
		// the lease belongs to a foreign engine (engine pid dead ≠ our pid) —
		// in both cases no live process can be waiting on this lease file.
		canFix := engineDead && (l.EnginePID != myPID || r.beadIsTerminal(l.Bead))
		if canFix {
			if rerr := os.Remove(path); rerr == nil {
				f.level = "ok"
				f.message = fmt.Sprintf("zombie lease removed: pool %s: %s/%s", pool, l.Project, l.Bead)
				f.fixed = true
			}
		}
		findings = append(findings, f)
	}

	if len(findings) == 0 {
		return []patrolFinding{{check: patrolCheckZombies, level: "ok",
			message: fmt.Sprintf("%d active lease(s), none zombie", clean)}}
	}
	return findings
}

// beadIsTerminal reports whether the named bead has a terminal slot in this run.
func (r *runner) beadIsTerminal(beadID string) bool {
	if r.run == nil {
		return false
	}
	sl, ok := r.run.Slots[beadID]
	return ok && sl != nil && ledger.Terminal(sl.Status)
}

// --- check: suspected leaked external resources ----------------------------

// patrolCheckLeakedResources is the check name for the suspected-leaked-
// resources finding (koryph-4ql.8, design L7): "a slot whose agent died ...
// with non-empty Slot.Resources and no clean-teardown marker in its
// SUMMARY/status is a suspected leak — the cluster outlives the pid."
const patrolCheckLeakedResources = "leaked-resources"

// patrolCheckLeakedResources flags a CURRENT-run slot whose agent died — a
// non-terminal status with a dead PID, or a terminal-failed status — while
// it held declared external resources (ledger.Slot.Resources non-empty).
// Report-only: unlike patrolCheckZombieLeases above, this NEVER signals a
// process, deletes a lease file, or runs a teardown — the design is explicit
// that leak remediation is always a manual operator action (design §7
// "Leaked instances: ... auto-teardown is deliberately deferred behind an
// explicit opt-in").
//
// Pragmatic scope note (koryph-4ql.8 deviation from the design's literal
// wording): the design says "no clean-teardown marker in its SUMMARY/
// status". That presumes parsing SUMMARY.md's CONTENT for such a marker, but
// no such marker convention exists anywhere in this codebase today — the one
// place this package touches SUMMARY.md (poll.go's finishCandidate) only
// checks its mere EXISTENCE, as a "did the agent produce a result" signal
// for the review/merge path, never its content. Per this bead's AC ("if
// SUMMARY.md parsing is not an established pattern in health.go, treat ANY
// dead-agent slot with declared resources as a suspected leak"), this check
// flags every qualifying dead-agent slot unconditionally, with no
// teardown-marker exemption. This is deliberately over-inclusive: a false
// positive is cheap (report-only — the operator verifies before tearing
// anything down), while a false negative would silently strand a
// provisioned cluster.
func (r *runner) patrolCheckLeakedResources(now time.Time) []patrolFinding {
	if r.run == nil || len(r.run.Slots) == 0 {
		return []patrolFinding{{check: patrolCheckLeakedResources, level: "ok", message: "no slots in this run"}}
	}

	ids := make([]string, 0, len(r.run.Slots))
	for id := range r.run.Slots {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var findings []patrolFinding
	clean := 0
	for _, id := range ids {
		sl := r.run.Slots[id]
		if sl == nil || len(sl.Resources) == 0 {
			continue
		}

		agentDied := sl.Status == ledger.SlotFailed ||
			(!ledger.Terminal(sl.Status) && sl.PID > 0 && !dispatch.Alive(sl.PID))
		if !agentDied {
			clean++
			continue
		}

		findings = append(findings, patrolFinding{
			check: patrolCheckLeakedResources,
			level: "warn",
			message: fmt.Sprintf(
				"suspected leaked resources (%s) from bead %s — agent died; verify and tear down manually",
				strings.Join(sl.Resources, ", "), id),
		})
	}

	if len(findings) == 0 {
		return []patrolFinding{{check: patrolCheckLeakedResources, level: "ok",
			message: fmt.Sprintf("%d resource-declaring slot(s), none suspected leaked", clean)}}
	}
	return findings
}

// --- check: per-kind resource-probe diffing (leaked instances) -------------

// patrolCheckResourceProbe is the check name for probe-diffing findings —
// deliberately matching internal/doctor's checkNameResourceProbe string
// (patrolCheckEpicVal/checkNameEpicValidations precedent) so both surfaces
// report under the same check label.
const patrolCheckResourceProbe = "resource-probe"

// patrolCheckResourceProbes diffs each configured resource kind's probe
// output against the governor's live leases (koryph-4ql.8, design L7
// "per-kind probe (opt-in)"), sharing the diff primitive with `koryph
// doctor` (internal/doctor.DiffResourceProbe/LiveResourceHolders/
// LoadResourcesConfig) so both surfaces flag the same leak identically.
// Kinds without a configured probe are skipped silently (opt-in per the
// design). A probe command failure is fail-soft (I6): one OK "skipped" note,
// never a leak finding — an unconfigured or flaky probe binary must not
// manufacture false leaks. Report-only, like patrolCheckLeakedResources
// above: never signals, deletes, or tears anything down.
func (r *runner) patrolCheckResourceProbes(ctx context.Context) []patrolFinding {
	rc := doctor.LoadResourcesConfig(paths.GovernorConfig())
	if rc == nil || len(rc.Kinds) == 0 {
		return []patrolFinding{{check: patrolCheckResourceProbe, level: "ok", message: "no resource kinds configured"}}
	}

	kinds := make([]string, 0, len(rc.Kinds))
	for k, spec := range rc.Kinds {
		if spec.Probe != "" {
			kinds = append(kinds, k)
		}
	}
	if len(kinds) == 0 {
		return []patrolFinding{{check: patrolCheckResourceProbe, level: "ok", message: "no per-kind probes configured"}}
	}
	sort.Strings(kinds)

	holders, err := doctor.LiveResourceHolders(paths.SlotsDir(), dispatch.Alive)
	if err != nil {
		return []patrolFinding{{check: patrolCheckResourceProbe, level: "warn",
			message: fmt.Sprintf("read slots dir: %v", err)}}
	}

	var out []patrolFinding
	for _, kind := range kinds {
		leaks, perr := doctor.DiffResourceProbe(ctx, kind, rc.Kinds[kind].Probe, holders[kind], nil)
		if perr != nil {
			out = append(out, patrolFinding{check: patrolCheckResourceProbe, level: "ok",
				message: fmt.Sprintf("kind %s: probe failed, skipped: %v", kind, perr)})
			continue
		}
		if len(leaks) == 0 {
			out = append(out, patrolFinding{check: patrolCheckResourceProbe, level: "ok",
				message: fmt.Sprintf("kind %s: probe ok, no orphaned instances", kind)})
			continue
		}
		for _, lk := range leaks {
			out = append(out, patrolFinding{check: patrolCheckResourceProbe, level: "warn", message: lk.Message()})
		}
	}
	return out
}

// --- check: stale demand from other engines --------------------------------

const patrolCheckDemand = "stale-demand"

// patrolCheckStaleDemand flags demand heartbeats from OTHER engine PIDs that
// are dead or whose timestamp is past the demandTTL. Our own demand heartbeat
// is always fresh (refreshed every poll tick), so we skip it explicitly.
func (r *runner) patrolCheckStaleDemand(now time.Time) []patrolFinding {
	const demandTTL = 10 * time.Minute

	demandDir := paths.DemandDir()
	entries, err := os.ReadDir(demandDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []patrolFinding{{check: patrolCheckDemand, level: "ok", message: "no demand heartbeats"}}
		}
		return []patrolFinding{{check: patrolCheckDemand, level: "warn",
			message: fmt.Sprintf("read demand dir: %v", err)}}
	}

	myPID := os.Getpid()
	var findings []patrolFinding
	fresh := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(demandDir, e.Name()))
		var d govern.Demand
		if rerr != nil || json.Unmarshal(data, &d) != nil || d.Project == "" {
			continue
		}

		// Skip our own demand entry — it is always fresh by construction.
		if d.EnginePID == myPID {
			fresh++
			continue
		}

		engineDead := d.EnginePID > 0 && !dispatch.Alive(d.EnginePID)
		heartbeatStale := false
		if t, perr := time.Parse(time.RFC3339, d.UpdatedAt); perr == nil {
			heartbeatStale = now.Sub(t) > demandTTL
		}

		if !engineDead && !heartbeatStale {
			fresh++
			continue
		}

		reason := "engine dead"
		if heartbeatStale && !engineDead {
			reason = fmt.Sprintf("heartbeat stale >%v", demandTTL)
		}
		pool := govern.NormalizeProvider(d.Provider)
		findings = append(findings, patrolFinding{
			check:   patrolCheckDemand,
			level:   "warn",
			message: fmt.Sprintf("stale demand from other engine: pool %s: %s engine_pid=%d (%s)", pool, d.Project, d.EnginePID, reason),
		})
	}

	if len(findings) == 0 {
		return []patrolFinding{{check: patrolCheckDemand, level: "ok",
			message: fmt.Sprintf("%d demand heartbeat(s) from other engines, none stale", fresh)}}
	}
	return findings
}

// --- check: governor pool sanity -------------------------------------------

const patrolCheckGovPool = "governor-pool"

// patrolCheckGovernorPool validates the governor.json pool configuration:
// cap > 0, adaptive cap not pinned at floor, settle window not wedged into the
// far future, circuit breaker not stuck open.
func (r *runner) patrolCheckGovernorPool(now time.Time) []patrolFinding {
	cfgPath := paths.GovernorConfig()
	data, err := os.ReadFile(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		return []patrolFinding{{check: patrolCheckGovPool, level: "ok",
			message: fmt.Sprintf("governor.json absent (default cap %d in use)", govern.DefaultMaxGlobalAgents)}}
	}
	if err != nil {
		return []patrolFinding{{check: patrolCheckGovPool, level: "warn",
			message: fmt.Sprintf("read governor.json: %v", err)}}
	}

	var f govern.File
	if jerr := json.Unmarshal(data, &f); jerr != nil {
		return []patrolFinding{{check: patrolCheckGovPool, level: "warn",
			message: fmt.Sprintf("parse governor.json: %v", jerr)}}
	}

	if len(f.Pools) == 0 {
		return []patrolFinding{{check: patrolCheckGovPool, level: "ok",
			message: "governor.json: no pools configured"}}
	}

	var out []patrolFinding
	for pool, cfg := range f.Pools {
		out = append(out, patrolCheckOnePool(pool, cfg, now)...)
	}
	if len(out) == 0 {
		return []patrolFinding{{check: patrolCheckGovPool, level: "ok", message: "all pools healthy"}}
	}
	return out
}

// patrolCheckOnePool validates a single governor pool's Config.
func patrolCheckOnePool(pool string, cfg govern.Config, now time.Time) []patrolFinding {
	var out []patrolFinding

	// Cap sanity: MaxGlobalAgents <= 0 means the default is used, which is fine
	// but worth noting; we warn only if Adaptive is on with DynamicCap <= 0.
	if cfg.MaxGlobalAgents <= 0 {
		out = append(out, patrolFinding{
			check:   patrolCheckGovPool,
			level:   "ok",
			message: fmt.Sprintf("pool %s: cap=0 (default %d in use)", pool, govern.DefaultMaxGlobalAgents),
		})
	}

	if cfg.Adaptive {
		// Adaptive cap pinned at floor.
		if cfg.DynamicCap <= 1 && cfg.LastDecreaseAt != "" {
			if last, perr := time.Parse(time.RFC3339, cfg.LastDecreaseAt); perr == nil {
				if now.Sub(last) > 30*time.Minute {
					out = append(out, patrolFinding{
						check:   patrolCheckGovPool,
						level:   "warn",
						message: fmt.Sprintf("pool %s: adaptive dynamic cap pinned at %d for >30m (possible persistent rate-limit or hard-max too low)", pool, cfg.DynamicCap),
					})
				}
			}
		}

		// Settle window wedged: SettleUntil far in the future.
		if cfg.SettleUntil != "" {
			if until, perr := time.Parse(time.RFC3339, cfg.SettleUntil); perr == nil {
				if until.Sub(now) > patrolSettleWedgeThreshold {
					out = append(out, patrolFinding{
						check:   patrolCheckGovPool,
						level:   "warn",
						message: fmt.Sprintf("pool %s: settle window wedged — SettleUntil %s is >%v from now", pool, cfg.SettleUntil, patrolSettleWedgeThreshold),
					})
				}
			}
		}

		// Circuit breaker stuck open.
		if cfg.BreakerState == "open" || cfg.BreakerState == "half-open" {
			msg := fmt.Sprintf("pool %s: circuit breaker %s (reopen count %d) — admission is 0 in this pool while it holds", pool, cfg.BreakerState, cfg.BreakerReopenCount)
			if cfg.BreakerOpenAt != "" {
				if openedAt, perr := time.Parse(time.RFC3339, cfg.BreakerOpenAt); perr == nil && now.Sub(openedAt) > patrolBreakerWedgeAge {
					msg = fmt.Sprintf("pool %s: circuit breaker %s WEDGED — open for >%v since %s (reopen count %d)", pool, cfg.BreakerState, patrolBreakerWedgeAge, cfg.BreakerOpenAt, cfg.BreakerReopenCount)
				}
			}
			out = append(out, patrolFinding{check: patrolCheckGovPool, level: "warn", message: msg})
		}
	}

	return out
}

// --- check: quota window burn ----------------------------------------------

const patrolCheckQuota = "quota-burn"

// patrolCheckQuotaBurn flags quota usage at warn/drain/stop using the cached
// governor state from the most recent governorGate call. Avoids re-running
// ccusage or a transcript scan (those happen in the governance loop already).
func (r *runner) patrolCheckQuotaBurn() []patrolFinding {
	// If the account is uncalibrated, quota gating is advisory and there's
	// nothing useful to flag.
	if r.quotaCfg == nil || (r.quotaCfg.WindowCeilingUSD <= 0 && r.quotaCfg.WeeklyCeilingUSD <= 0) {
		return []patrolFinding{{check: patrolCheckQuota, level: "ok", message: "quota not calibrated (advisory mode)"}}
	}
	u := r.lastQuotaUsage
	if u.Account == "" {
		// No snapshot yet (early in the run before the first governorGate).
		return []patrolFinding{{check: patrolCheckQuota, level: "ok", message: "no quota snapshot yet"}}
	}
	level, _ := quota.State(u, r.quotaCfg)
	switch level {
	case quota.LevelStop:
		return []patrolFinding{{check: patrolCheckQuota, level: "warn",
			message: fmt.Sprintf("quota HARD-STOP: window %.0f%% of ceiling — agents interrupted, run parked", u.Window5h.Fraction()*100)}}
	case quota.LevelDrain:
		return []patrolFinding{{check: patrolCheckQuota, level: "warn",
			message: fmt.Sprintf("quota DRAIN: window %.0f%% of ceiling — no new dispatch", u.Window5h.Fraction()*100)}}
	case quota.LevelThrottle:
		return []patrolFinding{{check: patrolCheckQuota, level: "warn",
			message: fmt.Sprintf("quota THROTTLE: window %.0f%% of ceiling — slot scaling active", u.Window5h.Fraction()*100)}}
	case quota.LevelWarn:
		return []patrolFinding{{check: patrolCheckQuota, level: "warn",
			message: fmt.Sprintf("quota WARN: window %.0f%% of ceiling", u.Window5h.Fraction()*100)}}
	default:
		return []patrolFinding{{check: patrolCheckQuota, level: "ok",
			message: fmt.Sprintf("window %.0f%% of ceiling (level ok)", u.Window5h.Fraction()*100)}}
	}
}

// --- check: bd reachability ------------------------------------------------

const patrolCheckBD = "bd-reachable"

// patrolCheckBD verifies that `bd` is on PATH and the project's .beads
// directory is accessible. These are the two necessary conditions for the
// engine to continue dispatching new work.
func (r *runner) patrolCheckBD() []patrolFinding {
	if _, err := exec.LookPath("bd"); err != nil {
		return []patrolFinding{{check: patrolCheckBD, level: "warn", message: "bd: not found on PATH"}}
	}
	if r.beadsDir != "" {
		if _, err := os.Stat(r.beadsDir); err != nil {
			return []patrolFinding{{check: patrolCheckBD, level: "warn",
				message: fmt.Sprintf("beads dir inaccessible: %s: %v", r.beadsDir, err)}}
		}
	}
	return []patrolFinding{{check: patrolCheckBD, level: "ok", message: "bd on PATH, beads dir accessible"}}
}

// --- check: ledger dir writable --------------------------------------------

const patrolCheckLedger = "ledger-writable"

// patrolCheckLedgerWritable verifies that the engine can still write to the
// run ledger directory. A full disk or a permissions change would silently
// drop checkpoints and manifests without this check.
func (r *runner) patrolCheckLedgerWritable() []patrolFinding {
	if r.store == nil {
		return []patrolFinding{{check: patrolCheckLedger, level: "ok", message: "no ledger store (dry-run)"}}
	}
	dir := r.store.KoryphRoot
	f, err := os.CreateTemp(dir, ".patrol-probe-*")
	if err != nil {
		return []patrolFinding{{check: patrolCheckLedger, level: "warn",
			message: fmt.Sprintf("ledger dir not writable: %s: %v", dir, err)}}
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return []patrolFinding{{check: patrolCheckLedger, level: "ok", message: "ledger dir writable"}}
}

// --- check: gc footprint ---------------------------------------------------

const patrolCheckGCFootprint = "gc-footprint"

// patrolCheckGCFootprint performs a dry-run gc scan and warns when pending-gc
// exceeds the configured threshold (gc.Config.FootprintWarnGB, default 1 GiB).
// If gc_auto is true in the config, it also runs a live gc pass.
func (r *runner) patrolCheckGCFootprint() []patrolFinding {
	repoRoot := ""
	if r.rec != nil {
		repoRoot = r.rec.Root
	}

	cfg, err := gcpkg.LoadConfig(repoRoot)
	if err != nil {
		return []patrolFinding{{check: patrolCheckGCFootprint, level: "warn",
			message: fmt.Sprintf("gc-footprint: cannot load retention config: %v", err)}}
	}

	dryOpts := gcpkg.Options{RepoRoot: repoRoot, DryRun: true, Config: &cfg}
	res, err := gcpkg.Run(dryOpts)
	if err != nil {
		return []patrolFinding{{check: patrolCheckGCFootprint, level: "warn",
			message: fmt.Sprintf("gc-footprint: scan failed: %v", err)}}
	}

	totalMB := res.TotalReclaimedMB()
	totalGB := totalMB / 1024
	warnGB := cfg.FootprintWarnGB

	if totalGB < warnGB {
		return []patrolFinding{{check: patrolCheckGCFootprint, level: "ok",
			message: fmt.Sprintf("gc-footprint: reclaimable=%.1f MB (threshold %.1f GB)", totalMB, warnGB)}}
	}

	// Threshold exceeded.
	f := patrolFinding{
		check:   patrolCheckGCFootprint,
		level:   "warn",
		message: fmt.Sprintf("gc-footprint: %.2f GB reclaimable exceeds threshold (%.1f GB) — run `koryph gc [--project ID]`", totalGB, warnGB),
	}

	// Auto-gc: run a live pass if enabled (opt-in only).
	if cfg.GCAuto {
		liveOpts := gcpkg.Options{RepoRoot: repoRoot, DryRun: false, Config: &cfg}
		liveRes, gerr := gcpkg.Run(liveOpts)
		if gerr != nil {
			f.message += fmt.Sprintf(" [auto-gc failed: %v]", gerr)
		} else {
			reclaimed := liveRes.TotalReclaimedMB()
			f.message = fmt.Sprintf("gc-footprint: auto-gc reclaimed %.1f MB", reclaimed)
			f.level = "ok"
			f.fixed = true
		}
	}

	return []patrolFinding{f}
}

// --- check: stale/orphaned agent worktrees ---------------------------------

const patrolCheckStaleWt = "stale-worktrees"

// patrolCheckStaleWorktrees flags a linked agent worktree that is DIRTY yet
// whose HEAD is already merged into the default branch. That combination is the
// orphan signature (2026-07-10 audit): an active dispatch worktree has an
// UNmerged tip (its work has not landed), so a merged-tip + dirty worktree is
// landed work with a stray uncommitted tail that worktree.Remove's dirty-refusal
// left behind — exactly the state that silently persisted for six days. Report
// only: cleanup is a `git worktree remove --force` an operator must run, never
// auto-destroyed (uncommitted work may be salvageable). Cheap: `git worktree
// list` plus one merge-base check per dirty worktree.
//
// Worktrees owned by a non-terminal slot of the current run are excluded first
// (koryph-050): a live agent that has not yet made its first commit shares the
// orphan signature exactly (dirty tree + HEAD still at the merged main tip), so
// flagging it would recommend destroying in-progress work.
func (r *runner) patrolCheckStaleWorktrees(ctx context.Context) []patrolFinding {
	repoRoot := ""
	if r.rec != nil {
		repoRoot = r.rec.Root
	}
	if repoRoot == "" {
		return []patrolFinding{{check: patrolCheckStaleWt, level: "ok", message: "stale-worktrees: no repo root"}}
	}
	def := ""
	if r.rec != nil {
		def = r.rec.DefaultBranch
	}
	if def == "" {
		def = "main"
	}

	infos, err := worktree.List(ctx, repoRoot)
	if err != nil {
		return []patrolFinding{{check: patrolCheckStaleWt, level: "warn",
			message: fmt.Sprintf("stale-worktrees: cannot list worktrees: %v", err)}}
	}

	// A worktree owned by a non-terminal slot of THIS run is a live agent, not
	// an orphan (koryph-050). A long-running agent that has not yet made its
	// first commit still has HEAD at the merged main tip AND a dirty tree —
	// exactly the orphan signature below — so without this guard the patrol
	// recommends `git worktree remove --force` against in-progress work, and
	// the more (and longer) work the agent has done pre-first-commit, the wider
	// that window. Match on both the cleaned path and the basename (the message
	// reports the basename) so a /tmp↔/private/tmp symlink or a trailing slash
	// cannot defeat the skip.
	livePaths := make(map[string]bool)
	liveBases := make(map[string]bool)
	for _, sl := range r.run.Slots {
		if sl == nil || ledger.Terminal(sl.Status) || sl.Worktree == "" {
			continue
		}
		livePaths[filepath.Clean(sl.Worktree)] = true
		liveBases[filepath.Base(sl.Worktree)] = true
	}

	var orphans []string
	for _, wt := range infos {
		// Skip the main checkout and any clean worktree; only dirty linked
		// worktrees are candidates.
		if wt.Path == repoRoot || !wt.Dirty || wt.Head == "" {
			continue
		}
		// Skip a live agent's worktree (koryph-050): a running slot's tree is
		// dirty by definition and, pre-first-commit, its HEAD is the merged
		// main tip — the orphan signature is indistinguishable from healthy
		// in-progress work, so ownership by a non-terminal slot is authoritative.
		if livePaths[filepath.Clean(wt.Path)] || liveBases[filepath.Base(wt.Path)] {
			continue
		}
		// Merged tip? `git merge-base --is-ancestor <head> <def>` exits 0.
		res, merr := exec.CommandContext(ctx, "git", "-C", repoRoot,
			"merge-base", "--is-ancestor", wt.Head, def).CombinedOutput()
		_ = res
		if merr == nil {
			orphans = append(orphans, filepath.Base(wt.Path))
		}
	}

	if len(orphans) == 0 {
		return []patrolFinding{{check: patrolCheckStaleWt, level: "ok",
			message: "stale-worktrees: none"}}
	}
	sort.Strings(orphans)
	return []patrolFinding{{check: patrolCheckStaleWt, level: "warn",
		message: fmt.Sprintf("stale-worktrees: %d dirty worktree(s) with an already-merged HEAD (orphaned by a dirty-tree cleanup skip) — inspect and `git worktree remove --force`: %s",
			len(orphans), strings.Join(orphans, ", "))}}
}

// --- check: completed-but-unvalidated epics --------------------------------

const patrolCheckEpics = "unvalidated-epics"

// epicListCadence gates the bd subprocess calls this check (and its sibling,
// koryph-wo0.7's parked/degraded WARN check) need — bd has no verb cheaper
// than a full project listing for "every open epic", and a per-epic
// ListChildren call is needed on top of that to see closed children (bd's
// plain list excludes closed issues by default; ListChildren does not) — to a
// coarser cadence than healthInterval: once per patrolThrottleWindow (1 h)
// rather than once per patrol tick (default every 10 m). The raw open-issue
// listing (r.epicPatrolIssues) is cached for wo0.7 to reuse without a second
// bd call; this check additionally caches its own derived findings
// (r.epicPatrolFindings) so the per-epic ListChildren calls are also paid
// once per refresh, not once per patrol tick.
const epicListCadence = patrolThrottleWindow

// epicLister is the optional WorkSource capability for listing every OPEN
// issue in the project (beads.Adapter.List excludes closed issues by
// design — see its doc comment). It is not part of WorkSource itself — most
// loop operations never need a full listing — so it is probed with a type
// assertion the same way epicreview.BeadStore is probed for
// AddLabel/AppendNotes elsewhere in this package: missing the verb degrades
// to a report-only OK finding rather than a panic or a build-time
// requirement on every WorkSource/test fake.
type epicLister interface {
	List(ctx context.Context) ([]beads.Issue, error)
}

// patrolEpicIssues returns the cached open-issue listing, refreshing via one
// bd subprocess call at most once per epicListCadence, and reports whether
// this call was the one that refreshed it. See epicListCadence's doc comment
// for why this is one of two exceptions to this file's "no bd subprocess"
// design note. On a fetch error the previous cache (possibly nil, on a cold
// start) is returned alongside the error so callers can still act on stale
// data if they choose; epicPatrolAt is left untouched so the next patrol
// tick retries rather than waiting out the full cadence.
func (r *runner) patrolEpicIssues(ctx context.Context, lister epicLister, now time.Time) (issues []beads.Issue, refreshed bool, err error) {
	if !r.epicPatrolAt.IsZero() && now.Sub(r.epicPatrolAt) < epicListCadence {
		return r.epicPatrolIssues, false, nil
	}
	issues, err = lister.List(ctx)
	if err != nil {
		return r.epicPatrolIssues, false, err
	}
	r.epicPatrolIssues = issues
	r.epicPatrolAt = now
	return issues, true, nil
}

// patrolCheckUnvalidatedEpics re-queues any OPEN epic whose children are all
// closed but that never entered (or fell out of) the runner's in-memory
// epic-validation pending set (internal/engine/epicvalidate.go's doc comment
// calls this exact gap out: "a crash between a bead close and its validation
// loses the candidate, and epics completed while no loop was running are
// never noticed by the edge-triggered hook"). Two shapes qualify:
//
//   - an epic with none of no-validate/validation:passed/parked/degraded:
//     validation was simply never triggered.
//   - an epic already carrying validation:passed: the close-after-docs path
//     (maybeStartEpicValidation closes it without re-validating once the docs
//     bead lands) stalled the same way — the docs bead closed, but nothing
//     was running to observe the edge and finish the close.
//
// validation:parked and validation:degraded epics are deliberately excluded:
// those are operator-decision states, not self-remediating — surfacing them
// live is koryph-wo0.7's WARN check, layered on top of the same open-issue
// listing this check populates (r.epicPatrolIssues).
//
// Findings are level "info", not "warn": the re-queue is the remediation —
// maybeStartEpicValidation drains the pending set on a later tick exactly as
// it does for a freshly-closed child — so there is nothing for an operator to
// act on. An epic already mid-validation (r.epicInFlight) is left alone so
// this check can never trigger a redundant second validator run for the same
// epic.
//
// The scan (and its re-queue side effects) runs only when patrolEpicIssues
// actually refreshes — see epicListCadence's doc comment — reusing the
// findings from the last real scan on the ticks in between.
func (r *runner) patrolCheckUnvalidatedEpics(ctx context.Context, now time.Time) []patrolFinding {
	lister, ok := r.adapter.(epicLister)
	if !ok {
		return []patrolFinding{{check: patrolCheckEpics, level: "ok",
			message: "adapter has no epic-listing verb"}}
	}
	issues, refreshed, err := r.patrolEpicIssues(ctx, lister, now)
	if err != nil {
		return []patrolFinding{{check: patrolCheckEpics, level: "warn",
			message: fmt.Sprintf("list epics: %v", err)}}
	}
	if !refreshed {
		return r.epicPatrolFindings
	}

	var out []patrolFinding
	for _, epic := range issues {
		if epic.IssueType != "epic" || epic.Status == "closed" || epic.Status == "done" {
			continue
		}
		if epic.HasLabel(epicreview.LabelNoValidate) || epic.HasLabel(epicreview.LabelParked) ||
			epic.HasLabel(epicreview.LabelDegraded) {
			continue
		}
		if epic.ID == r.epicInFlight {
			continue // already validating — never double-queue
		}
		children, cerr := r.adapter.ListChildren(ctx, epic.ID)
		if cerr != nil {
			out = append(out, patrolFinding{check: patrolCheckEpics, level: "warn",
				message: fmt.Sprintf("epic %s: list children: %v", epic.ID, cerr)})
			continue
		}
		if len(children) == 0 || anyOpenChild(children) {
			continue // not complete yet; the ordinary close-edge hook will catch it
		}

		if r.epicPending == nil {
			r.epicPending = map[string]bool{}
		}
		r.epicPending[epic.ID] = true

		reason := "validation was never triggered"
		if epic.HasLabel(epicreview.LabelPassed) {
			reason = "validation:passed but the epic never closed (close-after-docs path stalled)"
		}
		out = append(out, patrolFinding{
			check: patrolCheckEpics,
			level: "info",
			message: fmt.Sprintf("epic %s: all %d child(ren) closed, %s — re-queued for validation",
				epic.ID, len(children), reason),
		})
	}

	if len(out) == 0 {
		out = []patrolFinding{{check: patrolCheckEpics, level: "ok",
			message: "no stranded completed epics"}}
	}
	r.epicPatrolFindings = out
	return out
}

// --- check: parked/degraded epic validations (koryph-wo0.7) ----------------

// patrolCheckEpicVal is the check name for parked/degraded epic-validation
// WARN findings — deliberately matching internal/doctor's
// checkNameEpicValidations string so both surfaces report under the same
// check label.
const patrolCheckEpicVal = "epic-validations"

// patrolCheckEpicValidations surfaces every OPEN epic carrying
// validation:parked or validation:degraded as a WARN finding, with the round
// number and reason parsed from the epic's notes via the shared epicreview
// codec (koryph-qta.7). This closes the black-box gap design §4/§4b calls
// out: a parked epic (round-cap exceeded — an "operator decides" state) or a
// degraded epic (validator infra failure) must be visible from the live loop,
// not only from a separately-run `koryph doctor`. Mirrors internal/doctor's
// checkEpicValidations (koryph-wo0.6) exactly, including its "last matching
// note line wins" scan order.
//
// bd-call cadence (decision recorded per this bead's AC): this check does
// NOT issue its own bd call. It reuses r.patrolEpicIssues's cached open-issue
// listing — the same cache patrolCheckUnvalidatedEpics populates (koryph-bbe)
// — so the one bd subprocess call per epicListCadence window (1 h) is shared
// between both epic-awareness checks rather than doubled. Deriving WARN
// findings from that cached listing is pure in-memory regex/label work with
// no per-epic bd call (no ListChildren, unlike patrolCheckUnvalidatedEpics),
// so unlike that check this one recomputes on every patrol tick rather than
// needing its own second-level findings cache — the outer patrolSeen
// throttle (runPatrol, once per patrolThrottleWindow per unique finding) is
// what keeps the operator from seeing the same WARN spammed every tick.
func (r *runner) patrolCheckEpicValidations(ctx context.Context, now time.Time) []patrolFinding {
	lister, ok := r.adapter.(epicLister)
	if !ok {
		return []patrolFinding{{check: patrolCheckEpicVal, level: "ok",
			message: "adapter has no epic-listing verb"}}
	}
	issues, _, err := r.patrolEpicIssues(ctx, lister, now)
	if err != nil {
		return []patrolFinding{{check: patrolCheckEpicVal, level: "warn",
			message: fmt.Sprintf("list epics: %v", err)}}
	}

	var out []patrolFinding
	for _, epic := range issues {
		if epic.IssueType != "epic" || epic.Status == "closed" || epic.Status == "done" {
			continue
		}
		switch {
		case epic.HasLabel(epicreview.LabelParked):
			round, reason := parkedNoteForPatrol(epic.Notes)
			out = append(out, patrolFinding{
				check: patrolCheckEpicVal,
				level: "warn",
				message: fmt.Sprintf(
					"parked epic: %s %q round=%d reason=%q — run `koryph epic validate %s` to recover",
					epic.ID, epic.Title, round, reason, epic.ID),
			})
		case epic.HasLabel(epicreview.LabelDegraded):
			round, reason := degradedNoteForPatrol(epic.Notes)
			out = append(out, patrolFinding{
				check: patrolCheckEpicVal,
				level: "warn",
				message: fmt.Sprintf(
					"degraded epic: %s %q round=%d reason=%q — validator infra failure; run `koryph epic validate %s` to retry",
					epic.ID, epic.Title, round, reason, epic.ID),
			})
		}
	}

	if len(out) == 0 {
		return []patrolFinding{{check: patrolCheckEpicVal, level: "ok",
			message: "no parked or degraded epic validations"}}
	}
	return out
}

// degradedNoteForPatrol scans an epic's notes field (most-recent line first)
// and returns the round and reason from the last matching
// "validation:degraded (round N): reason" line, via the shared epicreview
// codec. Returns (0, "") when no matching line is found. Mirrors
// internal/doctor's parseDegradedNote exactly.
func degradedNoteForPatrol(notes string) (round int, reason string) {
	lines := strings.Split(notes, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, r, ok := epicreview.ParseDegradedNote(strings.TrimSpace(lines[i])); ok {
			return n, r
		}
	}
	return 0, ""
}

// parkedNoteForPatrol scans an epic's notes field (most-recent line first)
// and returns the round and a short description from the last matching
// parked note line (canonical colon form or legacy space form), via the
// shared epicreview codec. Returns (0, "") when no matching line is found.
// Mirrors internal/doctor's parseParkedNote exactly.
func parkedNoteForPatrol(notes string) (round int, reason string) {
	lines := strings.Split(notes, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if n, maxRounds, ok := epicreview.ParseParkedNote(strings.TrimSpace(lines[i])); ok {
			return n, fmt.Sprintf("exceeded max_rounds=%d", maxRounds)
		}
	}
	return 0, ""
}
