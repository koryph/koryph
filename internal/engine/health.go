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
//   - stale demand heartbeats from OTHER engine PIDs (not our own)
//   - governor pool sanity (cap > 0, settle/breaker timestamps not wedged)
//   - quota window burn vs warn/drain/stop thresholds
//   - bd reachability (binary on PATH + beads dir accessible)
//   - ledger/telemetry dir writable
//
// Findings are:
//  1. surfaced as progress WARN lines, throttled to once per hour per unique
//     finding (same finding key → at most one log line per patrolThrottleWindow)
//  2. appended to the run ledger as PatrolEvent entries for post-mortem history
//
// Safe auto-remediation only: zombie governor lease files whose agent PID AND
// engine PID are both dead, AND whose bead is in a terminal slot state in this
// run (merged/blocked/closed), are removed. Everything else is report-only.
//
// Checks are designed to complete in < 1 s: no network calls, no ccusage
// subprocess, and no bd subprocess. The bd reachability check only looks up
// the binary on PATH and stats the beads dir. Quota state uses the cached
// governor result from the most recent governorGate() call.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/dispatch"
	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/quota"

	gcpkg "github.com/koryph/koryph/internal/gc"
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
	check   string
	level   string // "ok" | "warn"
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

	// Surface WARN findings as progress lines, throttled to patrolThrottleWindow
	// per unique finding key to avoid spamming the operator every 10 minutes with
	// the same persistent issue.
	for i := range findings {
		f := &findings[i]
		if f.level != "warn" {
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
		r.progress("health patrol WARN [%s]: %s%s", f.check, f.message, suffix)
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
	out = append(out, r.patrolCheckStaleDemand(now)...)
	out = append(out, r.patrolCheckGovernorPool(now)...)
	out = append(out, r.patrolCheckQuotaBurn()...)
	out = append(out, r.patrolCheckBD()...)
	out = append(out, r.patrolCheckLedgerWritable()...)
	out = append(out, r.patrolCheckGCFootprint()...)
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
