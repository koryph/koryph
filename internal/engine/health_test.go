// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/ledger"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/quota"
)

// --- healthInterval --------------------------------------------------------

func TestHealthInterval_Defaults(t *testing.T) {
	r := &runner{opts: Options{}, cfg: &project.Config{}}
	if got := r.healthInterval(); got != 600*time.Second {
		t.Errorf("default health interval = %v, want 600s", got)
	}
}

func TestHealthInterval_OptionOverride(t *testing.T) {
	r := &runner{opts: Options{HealthIntervalSec: 120}, cfg: &project.Config{}}
	if got := r.healthInterval(); got != 120*time.Second {
		t.Errorf("option override = %v, want 120s", got)
	}
}

func TestHealthInterval_ConfigOverride(t *testing.T) {
	r := &runner{opts: Options{}, cfg: &project.Config{HealthIntervalSeconds: 300}}
	if got := r.healthInterval(); got != 300*time.Second {
		t.Errorf("config override = %v, want 300s", got)
	}
}

func TestHealthInterval_EnvOverride(t *testing.T) {
	t.Setenv("KORYPH_HEALTH_INTERVAL_SEC", "60")
	r := &runner{opts: Options{HealthIntervalSec: 999}, cfg: &project.Config{HealthIntervalSeconds: 999}}
	if got := r.healthInterval(); got != 60*time.Second {
		t.Errorf("env override = %v, want 60s", got)
	}
}

func TestHealthInterval_OptionBeatsConfig(t *testing.T) {
	r := &runner{opts: Options{HealthIntervalSec: 200}, cfg: &project.Config{HealthIntervalSeconds: 300}}
	if got := r.healthInterval(); got != 200*time.Second {
		t.Errorf("option should beat config: got %v, want 200s", got)
	}
}

// --- patrolIfDue -----------------------------------------------------------

// patrolIfDue is exercised via lastPatrolAt: if the patrol fires, lastPatrolAt
// is advanced; if it skips, lastPatrolAt stays unchanged.

func TestPatrolIfDue_FiresInitially(t *testing.T) {
	r := patrolRunner(t)
	r.opts.HealthIntervalSec = 3600 // long interval — won't fire again later
	if !r.lastPatrolAt.IsZero() {
		t.Fatal("precondition: lastPatrolAt should be zero")
	}
	r.patrolIfDue(t.Context())
	if r.lastPatrolAt.IsZero() {
		t.Error("patrolIfDue on fresh runner: lastPatrolAt still zero after call")
	}
}

func TestPatrolIfDue_SkipsBeforeInterval(t *testing.T) {
	r := patrolRunner(t)
	r.opts.HealthIntervalSec = 3600
	before := time.Now()
	r.lastPatrolAt = before
	r.patrolIfDue(t.Context())
	if r.lastPatrolAt != before {
		t.Error("patrolIfDue within interval: should not update lastPatrolAt")
	}
}

func TestPatrolIfDue_FiresAfterInterval(t *testing.T) {
	r := patrolRunner(t)
	r.opts.HealthIntervalSec = 1
	before := time.Now().Add(-2 * time.Second)
	r.lastPatrolAt = before
	r.patrolIfDue(t.Context())
	if r.lastPatrolAt == before {
		t.Error("patrolIfDue after interval: lastPatrolAt not updated")
	}
}

// --- zombie lease detection ------------------------------------------------

func TestPatrolCheckZombieLeases_NoSlotsDir(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	r := &runner{}
	findings := r.patrolCheckZombieLeases(time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no slots dir: expected one ok finding, got %+v", findings)
	}
}

func TestPatrolCheckZombieLeases_LiveEngineLease_NotZombie(t *testing.T) {
	home, slotsDir := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)

	// Write a lease where EnginePID is THIS process (alive).
	l := govern.Lease{
		Project:   "my-project",
		Bead:      "my-bead",
		PID:       0, // agent not started yet
		EnginePID: os.Getpid(),
		Provider:  "anthropic",
	}
	writeLeaseFile(t, slotsDir, "my-bead.json", l)

	r := &runner{}
	findings := r.patrolCheckZombieLeases(time.Now())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("live engine lease incorrectly flagged as zombie: %+v", f)
		}
	}
}

func TestPatrolCheckZombieLeases_DeadEngineLease_Flagged(t *testing.T) {
	home, slotsDir := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)

	l := govern.Lease{
		Project:   "dead-project",
		Bead:      "dead-bead",
		PID:       9999999, // clearly dead
		EnginePID: 9999998, // clearly dead foreign engine
		Provider:  "anthropic",
	}
	writeLeaseFile(t, slotsDir, "dead-bead.json", l)

	r := &runner{}
	findings := r.patrolCheckZombieLeases(time.Now())
	// Auto-fix converts warn→ok+fixed when the lease can be safely removed.
	// Accept either a warn finding OR an ok+fixed finding as evidence the
	// zombie was detected and acted upon.
	detected := false
	for _, f := range findings {
		if f.check == patrolCheckZombies && (f.level == "warn" || f.fixed) {
			detected = true
		}
	}
	if !detected {
		t.Errorf("dead engine+agent lease not detected/fixed; findings = %+v", findings)
	}
}

func TestPatrolCheckZombieLeases_AutoFix_DeadForeignEngine(t *testing.T) {
	home, slotsDir := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)

	// Lease from a dead foreign engine (engine pid ≠ our pid, both dead).
	l := govern.Lease{
		Project:   "other-project",
		Bead:      "done-bead",
		PID:       9999999,
		EnginePID: 9999998,
		Provider:  "anthropic",
	}
	leasePath := filepath.Join(slotsDir, "done-bead.json")
	writeLeaseFile(t, slotsDir, "done-bead.json", l)

	// runner has no run (so beadIsTerminal returns false), but foreign engine
	// pid is dead — canFix is true because engine pid ≠ our pid.
	r := &runner{}
	findings := r.patrolCheckZombieLeases(time.Now())

	if _, err := os.Stat(leasePath); !os.IsNotExist(err) {
		t.Errorf("zombie lease file not removed after auto-fix; stat err = %v", err)
	}
	fixedCount := 0
	for _, f := range findings {
		if f.fixed {
			fixedCount++
		}
	}
	if fixedCount == 0 {
		t.Errorf("no fixed findings after auto-fix; got %+v", findings)
	}
}

func TestPatrolCheckZombieLeases_OwnLivingEngine_LeaseSkipped(t *testing.T) {
	// A lease from our own (living) engine where the agent has exited.
	// This is the normal post-build shape (koryph-p42): engine alive, agent
	// done. The patrol must NOT flag or remove this lease — it is managed
	// explicitly by the engine (review → merge → release).
	home, slotsDir := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)

	leasePath := filepath.Join(slotsDir, "building-bead.json")
	l := govern.Lease{
		Project:   "my-project",
		Bead:      "building-bead",
		PID:       9999999,     // agent process has exited
		EnginePID: os.Getpid(), // OUR engine, still alive
		Provider:  "anthropic",
	}
	writeLeaseFile(t, slotsDir, "building-bead.json", l)

	r := &runner{}
	r.patrolCheckZombieLeases(time.Now())

	// The lease file must remain: engine is alive → post-build stage → skip.
	if _, err := os.Stat(leasePath); os.IsNotExist(err) {
		t.Error("patrol incorrectly removed a lease belonging to our own living engine (post-build stage)")
	}
}

// --- stale demand detection -----------------------------------------------

func TestPatrolCheckStaleDemand_NoDemandDir(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	r := &runner{}
	findings := r.patrolCheckStaleDemand(time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no demand dir: expected ok finding, got %+v", findings)
	}
}

func TestPatrolCheckStaleDemand_OwnHeartbeat_NotFlagged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	demandDir := filepath.Join(home, "slots", "demand")
	must(t, os.MkdirAll(demandDir, 0o755))

	// Our own demand — always fresh, never stale.
	d := govern.Demand{
		Project:   "my-project",
		EnginePID: os.Getpid(),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  "anthropic",
	}
	writeDemandFile(t, demandDir, "my-project.json", d)

	r := &runner{}
	findings := r.patrolCheckStaleDemand(time.Now())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("own heartbeat should never be flagged: %+v", f)
		}
	}
}

func TestPatrolCheckStaleDemand_DeadEngine_Flagged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	demandDir := filepath.Join(home, "slots", "demand")
	must(t, os.MkdirAll(demandDir, 0o755))

	d := govern.Demand{
		Project:   "dead-project",
		EnginePID: 9999999,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  "anthropic",
	}
	writeDemandFile(t, demandDir, "dead-project.json", d)

	r := &runner{}
	findings := r.patrolCheckStaleDemand(time.Now())
	if countLevel(findings, "warn") == 0 {
		t.Errorf("dead engine demand not flagged as stale; findings = %+v", findings)
	}
}

func TestPatrolCheckStaleDemand_StaleTimestamp_Flagged(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	demandDir := filepath.Join(home, "slots", "demand")
	must(t, os.MkdirAll(demandDir, 0o755))

	// A foreign pid (not ours), with a stale timestamp (> 10m).
	d := govern.Demand{
		Project:   "stale-project",
		EnginePID: 9999999, // dead foreign engine
		UpdatedAt: time.Now().Add(-15 * time.Minute).UTC().Format(time.RFC3339),
		Provider:  "anthropic",
	}
	writeDemandFile(t, demandDir, "stale-project.json", d)

	r := &runner{}
	findings := r.patrolCheckStaleDemand(time.Now())
	if countLevel(findings, "warn") == 0 {
		t.Errorf("stale timestamp demand not flagged; findings = %+v", findings)
	}
}

// --- governor pool sanity --------------------------------------------------

func TestPatrolCheckGovernorPool_Absent(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("absent governor.json: expected ok, got %+v", findings)
	}
}

func TestPatrolCheckGovernorPool_HealthyPool(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	f := govern.File{Pools: map[string]govern.Config{
		"anthropic": {MaxGlobalAgents: 4},
	}}
	writeGovernorFile(t, home, f)

	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if countLevel(findings, "warn") > 0 {
		t.Errorf("healthy pool flagged as warn: %+v", findings)
	}
}

func TestPatrolCheckGovernorPool_BreakerOpen_Warned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	f := govern.File{Pools: map[string]govern.Config{
		"anthropic": {
			MaxGlobalAgents: 4,
			Adaptive:        true,
			DynamicCap:      4,
			BreakerState:    "open",
			BreakerOpenAt:   time.Now().UTC().Format(time.RFC3339),
		},
	}}
	writeGovernorFile(t, home, f)

	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if countLevel(findings, "warn") == 0 {
		t.Errorf("open breaker not flagged; findings = %+v", findings)
	}
}

func TestPatrolCheckGovernorPool_SettleWedged_Warned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	f := govern.File{Pools: map[string]govern.Config{
		"anthropic": {
			MaxGlobalAgents: 4,
			Adaptive:        true,
			DynamicCap:      4,
			SettleUntil:     time.Now().Add(3 * time.Hour).UTC().Format(time.RFC3339),
		},
	}}
	writeGovernorFile(t, home, f)

	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if countLevel(findings, "warn") == 0 {
		t.Errorf("wedged settle window not flagged; findings = %+v", findings)
	}
}

func TestPatrolCheckGovernorPool_AdaptiveCapPinned_Warned(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	f := govern.File{Pools: map[string]govern.Config{
		"anthropic": {
			MaxGlobalAgents: 4,
			Adaptive:        true,
			DynamicCap:      1,
			LastDecreaseAt:  time.Now().Add(-45 * time.Minute).UTC().Format(time.RFC3339),
		},
	}}
	writeGovernorFile(t, home, f)

	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if countLevel(findings, "warn") == 0 {
		t.Errorf("adaptive cap pinned at floor for >30m not flagged; findings = %+v", findings)
	}
}

func TestPatrolCheckGovernorPool_AdaptiveCapPinned_TooRecent_OK(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	f := govern.File{Pools: map[string]govern.Config{
		"anthropic": {
			MaxGlobalAgents: 4,
			Adaptive:        true,
			DynamicCap:      1,
			// Last decrease 5 minutes ago — within the 30m threshold.
			LastDecreaseAt: time.Now().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
		},
	}}
	writeGovernorFile(t, home, f)

	r := &runner{}
	findings := r.patrolCheckGovernorPool(time.Now())
	if countLevel(findings, "warn") > 0 {
		t.Errorf("recent adaptive decrease should not warn; findings = %+v", findings)
	}
}

// --- quota burn ------------------------------------------------------------

func TestPatrolCheckQuotaBurn_UncalibratedOK(t *testing.T) {
	r := &runner{quotaCfg: &quota.Config{}}
	findings := r.patrolCheckQuotaBurn()
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("uncalibrated: expected ok, got %+v", findings)
	}
}

func TestPatrolCheckQuotaBurn_NoSnapshot_OK(t *testing.T) {
	r := &runner{quotaCfg: &quota.Config{WindowCeilingUSD: 10}}
	// lastQuotaUsage.Account is empty — no snapshot yet.
	findings := r.patrolCheckQuotaBurn()
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no snapshot: expected ok, got %+v", findings)
	}
}

// freshWeekly returns a weekly window that is well below the warn threshold,
// preventing it from dominating the quota.State verdict in tests that target
// the 5h window's level specifically. quota.Window.Fraction() returns 1.0
// (fail-closed) when CeilingUSD == 0, which would force LevelStop regardless
// of the 5h window.
func freshWeekly() quota.Window {
	return quota.Window{SpentUSD: 5.0, CeilingUSD: 200.0, Source: "ccusage"} // 2.5%
}

func TestPatrolCheckQuotaBurn_WarnLevel(t *testing.T) {
	r := &runner{
		quotaCfg: &quota.Config{WindowCeilingUSD: 10, WeeklyCeilingUSD: 200},
		lastQuotaUsage: quota.Usage{
			Account:  "test",
			Window5h: quota.Window{SpentUSD: 9.1, CeilingUSD: 10.0, Source: "ccusage"}, // 91% → warn (>= 0.90, < 0.94 throttle)
			Weekly:   freshWeekly(),
		},
	}
	findings := r.patrolCheckQuotaBurn()
	if countLevel(findings, "warn") == 0 {
		t.Errorf("91%% usage at warn threshold not flagged; findings = %+v", findings)
	}
}

func TestPatrolCheckQuotaBurn_DrainLevel(t *testing.T) {
	r := &runner{
		quotaCfg: &quota.Config{WindowCeilingUSD: 10, WeeklyCeilingUSD: 200},
		lastQuotaUsage: quota.Usage{
			Account:  "test",
			Window5h: quota.Window{SpentUSD: 9.75, CeilingUSD: 10.0, Source: "ccusage"}, // 97.5% → drain (>= 0.97 graceful_stop, < 0.99 hard_stop)
			Weekly:   freshWeekly(),
		},
	}
	findings := r.patrolCheckQuotaBurn()
	if countLevel(findings, "warn") == 0 {
		t.Errorf("97.5%% usage at drain threshold not flagged; findings = %+v", findings)
	}
	hasKeyword := false
	for _, f := range findings {
		if strings.Contains(f.message, "DRAIN") {
			hasKeyword = true
		}
	}
	if !hasKeyword {
		t.Errorf("drain finding missing DRAIN keyword; findings = %+v", findings)
	}
}

func TestPatrolCheckQuotaBurn_OKLevel(t *testing.T) {
	r := &runner{
		quotaCfg: &quota.Config{WindowCeilingUSD: 10, WeeklyCeilingUSD: 200},
		lastQuotaUsage: quota.Usage{
			Account:  "test",
			Window5h: quota.Window{SpentUSD: 5.0, CeilingUSD: 10.0, Source: "ccusage"}, // 50% → ok
			Weekly:   freshWeekly(),
		},
	}
	findings := r.patrolCheckQuotaBurn()
	if countLevel(findings, "warn") > 0 {
		t.Errorf("50%% usage should not warn; findings = %+v", findings)
	}
}

// --- bd reachability -------------------------------------------------------

func TestPatrolCheckBD_MissingBeadsDir(t *testing.T) {
	r := &runner{beadsDir: "/nonexistent/beads/dir/xyz"}
	findings := r.patrolCheckBD()
	// A missing beadsDir must produce a warn regardless of bd binary status.
	if countLevel(findings, "warn") == 0 {
		t.Errorf("missing beadsDir: expected warn; findings = %+v", findings)
	}
}

func TestPatrolCheckBD_ExistingBeadsDir(t *testing.T) {
	dir := t.TempDir()
	r := &runner{beadsDir: dir}
	findings := r.patrolCheckBD()
	// Either ok (bd on PATH) or warn (bd not found). Both are valid in CI.
	// What matters is that at least one finding is returned without panic.
	if len(findings) == 0 {
		t.Error("patrolCheckBD: returned no findings")
	}
}

// --- ledger writable -------------------------------------------------------

func TestPatrolCheckLedgerWritable_Writable(t *testing.T) {
	dir := t.TempDir()
	r := &runner{store: &ledger.Store{KoryphRoot: dir}}
	findings := r.patrolCheckLedgerWritable()
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("writable dir: expected ok; findings = %+v", findings)
	}
}

func TestPatrolCheckLedgerWritable_NotWritable(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("cannot test permission denial as root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	r := &runner{store: &ledger.Store{KoryphRoot: dir}}
	findings := r.patrolCheckLedgerWritable()
	if countLevel(findings, "warn") == 0 {
		t.Errorf("non-writable dir: expected warn; findings = %+v", findings)
	}
}

// --- throttling and ledger recording ---------------------------------------

func TestRunPatrol_ThrottlesRepeatFindings(t *testing.T) {
	home, demandDir := setupDemandDir(t)
	t.Setenv("KORYPH_HOME", home)

	// Inject a stale-demand WARN from a dead engine.
	d := govern.Demand{
		Project:   "dead-project",
		EnginePID: 9999999,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  "anthropic",
	}
	writeDemandFile(t, demandDir, "dead-project.json", d)

	r := patrolRunner(t)

	r.runPatrol(t.Context())
	// patrolSeen should now have an entry.
	if len(r.patrolSeen) == 0 {
		t.Error("first patrol: patrolSeen empty — no findings were logged")
	}
	initialSeen := len(r.patrolSeen)

	// Second patrol with the same stale demand: same findings → throttled → no
	// new entries in patrolSeen (all keys already present).
	r.runPatrol(t.Context())
	if len(r.patrolSeen) != initialSeen {
		t.Errorf("second patrol: patrolSeen grew from %d to %d — throttling not working", initialSeen, len(r.patrolSeen))
	}
}

func TestRunPatrol_AppendsToLedger(t *testing.T) {
	home, demandDir := setupDemandDir(t)
	t.Setenv("KORYPH_HOME", home)

	// Stale demand → guaranteed warn finding.
	d := govern.Demand{
		Project:   "dead-project",
		EnginePID: 9999999,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
		Provider:  "anthropic",
	}
	writeDemandFile(t, demandDir, "dead-project.json", d)

	r := patrolRunner(t)
	r.runPatrol(t.Context())

	if len(r.run.PatrolEvents) == 0 {
		t.Fatal("runPatrol: no patrol events appended to run")
	}
	hasWarn := false
	for _, ev := range r.run.PatrolEvents {
		for _, f := range ev.Findings {
			if f.Level == "warn" {
				hasWarn = true
			}
		}
	}
	if !hasWarn {
		t.Errorf("patrol events have no warn findings despite stale demand; events = %+v", r.run.PatrolEvents)
	}
}

func TestRunPatrol_AllOK_NoLedgerEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)

	r := patrolRunner(t)
	r.runPatrol(t.Context())

	// A fully healthy patrol (no warnings) should NOT write a ledger entry.
	if len(r.run.PatrolEvents) != 0 {
		t.Errorf("all-ok patrol should not append to ledger; got %d events", len(r.run.PatrolEvents))
	}
}

// --- beadIsTerminal --------------------------------------------------------

func TestBeadIsTerminal(t *testing.T) {
	r := &runner{run: &ledger.Run{Slots: map[string]*ledger.Slot{
		"merged-bead":  {Status: ledger.SlotMerged},
		"blocked-bead": {Status: ledger.SlotBlocked},
		"running-bead": {Status: ledger.SlotRunning},
	}}}

	if !r.beadIsTerminal("merged-bead") {
		t.Error("merged should be terminal")
	}
	if !r.beadIsTerminal("blocked-bead") {
		t.Error("blocked should be terminal")
	}
	if r.beadIsTerminal("running-bead") {
		t.Error("running should NOT be terminal")
	}
	if r.beadIsTerminal("unknown-bead") {
		t.Error("unknown bead should NOT be terminal")
	}
}

// --- helpers ---------------------------------------------------------------

// patrolRunner builds a runner with enough state for patrol checks to work
// without crashing. KORYPH_HOME must already be set.
func patrolRunner(t *testing.T) *runner {
	t.Helper()
	ledgerDir := t.TempDir()
	return &runner{
		opts:     Options{HealthIntervalSec: 600},
		cfg:      &project.Config{},
		store:    &ledger.Store{KoryphRoot: ledgerDir},
		run:      &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{}},
		beadsDir: t.TempDir(),
	}
}

// setupSlotsDir creates and returns a fresh KORYPH_HOME with a slots dir.
func setupSlotsDir(t *testing.T) (home, slotsDir string) {
	t.Helper()
	home = t.TempDir()
	slotsDir = filepath.Join(home, "slots")
	must(t, os.MkdirAll(slotsDir, 0o755))
	return
}

// setupDemandDir creates a KORYPH_HOME with slots/demand.
func setupDemandDir(t *testing.T) (home, demandDir string) {
	t.Helper()
	home = t.TempDir()
	demandDir = filepath.Join(home, "slots", "demand")
	must(t, os.MkdirAll(demandDir, 0o755))
	return
}

func writeLeaseFile(t *testing.T, slotsDir, name string, l govern.Lease) {
	t.Helper()
	data, err := json.Marshal(l)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(slotsDir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDemandFile(t *testing.T, demandDir, name string, d govern.Demand) {
	t.Helper()
	data, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(demandDir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeGovernorFile(t *testing.T, home string, f govern.File) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "governor.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func countLevel(findings []patrolFinding, level string) int {
	n := 0
	for _, f := range findings {
		if f.level == level {
			n++
		}
	}
	return n
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
