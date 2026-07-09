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

	"github.com/koryph/koryph/internal/beads"
	"github.com/koryph/koryph/internal/epicreview"
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

// --- suspected leaked resources (koryph-4ql.8) ------------------------------

func TestPatrolCheckLeakedResources_NoSlots(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{}}}
	findings := r.patrolCheckLeakedResources(time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no slots: expected one ok finding, got %+v", findings)
	}
}

func TestPatrolCheckLeakedResources_NilRun(t *testing.T) {
	r := &runner{}
	findings := r.patrolCheckLeakedResources(time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("nil run: expected one ok finding, got %+v", findings)
	}
}

// TestPatrolCheckLeakedResources_DeadAgentWithResources_Flagged is this
// bead's core fixture: a non-terminal slot (running) whose agent pid is
// dead, carrying declared Slot.Resources, must be flagged WARN naming both
// the bead and its declared kinds — and the finding must never mutate
// anything (report-only, unlike the zombie-lease auto-fix above).
func TestPatrolCheckLeakedResources_DeadAgentWithResources_Flagged(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{
		"my-bead": {
			PhaseID:   "my-bead",
			Status:    ledger.SlotRunning,
			PID:       9999999, // clearly dead
			Resources: []string{"kind-cluster", "docker"},
		},
	}}}
	findings := r.patrolCheckLeakedResources(time.Now())

	var warn *patrolFinding
	for i := range findings {
		if findings[i].level == "warn" {
			warn = &findings[i]
		}
	}
	if warn == nil {
		t.Fatalf("expected a warn finding for a dead-agent slot with declared resources, got %+v", findings)
	}
	if warn.check != patrolCheckLeakedResources {
		t.Errorf("check = %q, want %q", warn.check, patrolCheckLeakedResources)
	}
	for _, want := range []string{"my-bead", "kind-cluster", "docker", "agent died", "tear down manually"} {
		if !strings.Contains(warn.message, want) {
			t.Errorf("message = %q, want it to contain %q", warn.message, want)
		}
	}
}

// TestPatrolCheckLeakedResources_TerminalFailed_Flagged covers the design's
// "or terminal-failed" clause: a slot already marked failed still carries
// its resource declaration and must be flagged regardless of its stored PID.
func TestPatrolCheckLeakedResources_TerminalFailed_Flagged(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{
		"failed-bead": {
			PhaseID:   "failed-bead",
			Status:    ledger.SlotFailed,
			Resources: []string{"kind-cluster"},
		},
	}}}
	findings := r.patrolCheckLeakedResources(time.Now())
	found := false
	for _, f := range findings {
		if f.level == "warn" && strings.Contains(f.message, "failed-bead") {
			found = true
		}
	}
	if !found {
		t.Errorf("terminal-failed slot with resources not flagged: %+v", findings)
	}
}

// TestPatrolCheckLeakedResources_LivePID_NotFlagged proves a live agent
// holding declared resources is never flagged — only a DEAD agent is
// evidence of a possible leak.
func TestPatrolCheckLeakedResources_LivePID_NotFlagged(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{
		"alive-bead": {
			PhaseID:   "alive-bead",
			Status:    ledger.SlotRunning,
			PID:       os.Getpid(), // alive
			Resources: []string{"kind-cluster"},
		},
	}}}
	findings := r.patrolCheckLeakedResources(time.Now())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("live agent incorrectly flagged as a leaked-resource suspect: %+v", f)
		}
	}
}

// TestPatrolCheckLeakedResources_NoResources_NotFlagged proves a dead-agent
// slot with NO declared resources is out of scope for this check (the
// zombie-lease check above is the right signal for a plain dead agent).
func TestPatrolCheckLeakedResources_NoResources_NotFlagged(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{
		"plain-bead": {
			PhaseID: "plain-bead",
			Status:  ledger.SlotRunning,
			PID:     9999999, // dead
			// no Resources
		},
	}}}
	findings := r.patrolCheckLeakedResources(time.Now())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("resource-free dead slot incorrectly flagged: %+v", f)
		}
	}
}

// TestPatrolCheckLeakedResources_MergedWithResources_NotFlagged proves a
// slot that reached a clean terminal state OTHER than "failed" (e.g.
// merged) is not flagged even with a dead PID and declared resources — the
// design's "or terminal-failed" clause is deliberately narrower than
// ledger.Terminal.
func TestPatrolCheckLeakedResources_MergedWithResources_NotFlagged(t *testing.T) {
	r := &runner{run: &ledger.Run{RunID: "test-run", Slots: map[string]*ledger.Slot{
		"merged-bead": {
			PhaseID:   "merged-bead",
			Status:    ledger.SlotMerged,
			PID:       9999999, // dead — expected once merged
			Resources: []string{"kind-cluster"},
		},
	}}}
	findings := r.patrolCheckLeakedResources(time.Now())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("merged slot incorrectly flagged: %+v", f)
		}
	}
}

// --- resource-probe diffing (koryph-4ql.8) ----------------------------------

func TestPatrolCheckResourceProbes_NoGovernorFile(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	r := &runner{}
	findings := r.patrolCheckResourceProbes(t.Context())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no governor.json: expected one ok finding, got %+v", findings)
	}
}

func TestPatrolCheckResourceProbes_NoProbeConfigured(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"kind-cluster": {Capacity: 1}}, // no Probe
	}})
	r := &runner{}
	findings := r.patrolCheckResourceProbes(t.Context())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("no probe configured: expected one ok finding, got %+v", findings)
	}
}

// TestPatrolCheckResourceProbes_OrphanFlagged exercises the full path via a
// real `sh -c` echo probe (production RunProbeShell), diffed against an
// EMPTY slots dir (no live lease anywhere) — the instance must be flagged.
func TestPatrolCheckResourceProbes_OrphanFlagged(t *testing.T) {
	home, _ := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"kind-cluster": {Probe: "echo kind-cluster-koryph-orphan"}},
	}})
	r := &runner{}
	findings := r.patrolCheckResourceProbes(t.Context())
	found := false
	for _, f := range findings {
		if f.level == "warn" && strings.Contains(f.message, "kind-cluster-koryph-orphan") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a warn finding naming the orphaned instance, got %+v", findings)
	}
}

// TestPatrolCheckResourceProbes_LiveLeaseNotFlagged proves an instance whose
// bead-id suffix has a live lease of the same kind is NOT flagged.
func TestPatrolCheckResourceProbes_LiveLeaseNotFlagged(t *testing.T) {
	home, slotsDir := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"kind-cluster": {Probe: "echo kind-cluster-koryph-live"}},
	}})
	writeLeaseFile(t, slotsDir, "koryph-live.json", govern.Lease{
		Project: "my-project", Bead: "koryph-live",
		PID: os.Getpid(), EnginePID: os.Getpid(),
		Resources: []string{"kind-cluster"},
	})
	r := &runner{}
	findings := r.patrolCheckResourceProbes(t.Context())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("instance with a live lease incorrectly flagged: %+v", f)
		}
	}
}

// TestPatrolCheckResourceProbes_ProbeFailureSoftSkip proves a failing probe
// command degrades to an OK "skipped" note, never a warn finding (I6
// fail-soft).
func TestPatrolCheckResourceProbes_ProbeFailureSoftSkip(t *testing.T) {
	home, _ := setupSlotsDir(t)
	t.Setenv("KORYPH_HOME", home)
	writeGovernorFile(t, home, govern.File{Resources: &govern.ResourcesConfig{
		Kinds: map[string]govern.ResourceKind{"kind-cluster": {Probe: "exit 1"}},
	}})
	r := &runner{}
	findings := r.patrolCheckResourceProbes(t.Context())
	for _, f := range findings {
		if f.level == "warn" {
			t.Errorf("a failing probe must never produce a warn finding: %+v", findings)
		}
	}
	skipped := false
	for _, f := range findings {
		if f.level == "ok" && strings.Contains(f.message, "skipped") {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("expected an ok 'skipped' note for a failing probe, got %+v", findings)
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

// stubBDOnPath puts a no-op `bd` executable on PATH so patrol's bd-reachable
// check passes on machines without bd installed (CI runners) — the all-ok
// assertion below must not depend on the host environment.
func stubBDOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, "bd")
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunPatrol_AllOK_NoLedgerEntry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KORYPH_HOME", home)
	stubBDOnPath(t)

	r := patrolRunner(t)
	r.runPatrol(t.Context())

	// A fully healthy patrol (no warnings) should NOT write a ledger entry.
	if len(r.run.PatrolEvents) != 0 {
		t.Errorf("all-ok patrol should not append to ledger; got %d events", len(r.run.PatrolEvents))
	}
}

// --- completed-but-unvalidated epics (koryph-bbe) ---------------------------

// TestPatrolCheckUnvalidatedEpics_ReQueuesAndValidationStarts is the AC's
// fixture-locked test: a stranded completed epic (all children closed, no
// validation label at all — the crash/cold-start gap epicvalidate.go's doc
// comment calls out) is re-queued by the patrol, and validation starts on
// the very next tick exactly as it would for an ordinary freshly-closed
// child.
func TestPatrolCheckUnvalidatedEpics_ReQueuesAndValidationStarts(t *testing.T) {
	fake := closedEpicFixture() // ep1: open epic, unlabeled; c1: its one closed child
	r, calls := epicRunner(t, fake, epicreview.Verdict{Met: true})

	findings := r.patrolCheckUnvalidatedEpics(t.Context(), time.Now())

	if !r.epicPending["ep1"] {
		t.Fatal("stranded completed epic was not re-queued into the pending set")
	}
	if got := countLevel(findings, "info"); got != 1 {
		t.Fatalf("info findings = %d, want 1; findings = %+v", got, findings)
	}
	if !strings.Contains(findings[0].message, "ep1") {
		t.Errorf("finding message = %q, want it to name ep1", findings[0].message)
	}

	// The pending set drains on the next tick, same as the ordinary
	// bead-close trigger.
	r.maybeStartEpicValidation(t.Context(), true)
	if r.epicInFlight != "ep1" {
		t.Fatalf("epicInFlight = %q, want ep1 (validation must start on the next tick)", r.epicInFlight)
	}
	drainVerdict(t, r)
	if got := calls.Load(); got != 1 {
		t.Errorf("validator calls = %d, want 1", got)
	}
}

func TestPatrolCheckUnvalidatedEpics_ValidationPassedRequeues(t *testing.T) {
	fake := closedEpicFixture()
	iss := fake.issues["ep1"]
	iss.Labels = []string{epicreview.LabelPassed}
	fake.issues["ep1"] = iss
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	findings := r.patrolCheckUnvalidatedEpics(t.Context(), time.Now())

	if !r.epicPending["ep1"] {
		t.Fatal("validation:passed epic with all children closed must be re-queued (close-after-docs path)")
	}
	found := false
	for _, f := range findings {
		if f.level == "info" && strings.Contains(f.message, "close-after-docs") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a close-after-docs finding; got %+v", findings)
	}
}

func TestPatrolCheckUnvalidatedEpics_SkipsParkedDegradedNoValidate(t *testing.T) {
	for _, label := range []string{epicreview.LabelParked, epicreview.LabelDegraded, epicreview.LabelNoValidate} {
		fake := closedEpicFixture()
		iss := fake.issues["ep1"]
		iss.Labels = []string{label}
		fake.issues["ep1"] = iss
		r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

		r.patrolCheckUnvalidatedEpics(t.Context(), time.Now())
		if r.epicPending["ep1"] {
			t.Errorf("label %s: epic must not be re-queued", label)
		}
	}
}

func TestPatrolCheckUnvalidatedEpics_OpenChildSkipped(t *testing.T) {
	fake := closedEpicFixture()
	fake.children["ep1"] = append(fake.children["ep1"], beads.Issue{ID: "c2", Status: "open", ParentID: "ep1"})
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	r.patrolCheckUnvalidatedEpics(t.Context(), time.Now())
	if r.epicPending["ep1"] {
		t.Error("epic with an open child must not be re-queued")
	}
}

func TestPatrolCheckUnvalidatedEpics_NoListerAdapter_OK(t *testing.T) {
	r := &runner{adapter: &fakeSource{}}
	findings := r.patrolCheckUnvalidatedEpics(t.Context(), time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("findings = %+v, want a single ok finding for an adapter without List", findings)
	}
}

// TestPatrolCheckUnvalidatedEpics_CadenceThrottlesRescans verifies the
// coarser-cadence gate documented on epicListCadence: the bd-equivalent
// List/ListChildren calls happen once per scan, are skipped (findings
// replayed from cache) inside the cadence window, and resume once the window
// elapses.
func TestPatrolCheckUnvalidatedEpics_CadenceThrottlesRescans(t *testing.T) {
	fake := closedEpicFixture()
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	now := time.Now()
	r.patrolCheckUnvalidatedEpics(t.Context(), now)
	if fake.listCalls != 1 {
		t.Fatalf("listCalls after first scan = %d, want 1", fake.listCalls)
	}

	findings := r.patrolCheckUnvalidatedEpics(t.Context(), now.Add(time.Minute))
	if fake.listCalls != 1 {
		t.Errorf("listCalls after throttled scan = %d, want still 1 (cadence not elapsed)", fake.listCalls)
	}
	if countLevel(findings, "info") != 1 {
		t.Errorf("throttled scan should replay the cached info finding; got %+v", findings)
	}

	r.patrolCheckUnvalidatedEpics(t.Context(), now.Add(epicListCadence+time.Minute))
	if fake.listCalls != 2 {
		t.Errorf("listCalls after cadence elapsed = %d, want 2", fake.listCalls)
	}
}

// --- parked/degraded epic validations (koryph-wo0.7) ------------------------

// TestPatrolCheckEpicValidations_ParkedWarns is the AC's fixture-locked test:
// a validation:parked epic produces a WARN finding carrying the round and
// reason parsed from its note via the shared epicreview codec.
func TestPatrolCheckEpicValidations_ParkedWarns(t *testing.T) {
	fake := closedEpicFixture()
	iss := fake.issues["ep1"]
	iss.Labels = []string{epicreview.LabelParked}
	iss.Notes = epicreview.FormatParkedNote(3, 5, "koryph epic validate ep1 --project proj")
	fake.issues["ep1"] = iss
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	findings := r.patrolCheckEpicValidations(t.Context(), time.Now())

	if got := countLevel(findings, "warn"); got != 1 {
		t.Fatalf("warn findings = %d, want 1; findings = %+v", got, findings)
	}
	msg := findings[0].message
	if !strings.Contains(msg, "ep1") || !strings.Contains(msg, "round=3") || !strings.Contains(msg, "max_rounds=5") {
		t.Errorf("finding message = %q, want it to name ep1/round=3/max_rounds=5", msg)
	}
}

// TestPatrolCheckEpicValidations_DegradedWarns is the AC's fixture-locked
// test: a validation:degraded epic produces a WARN finding carrying the
// round and reason parsed from its note via the shared epicreview codec.
func TestPatrolCheckEpicValidations_DegradedWarns(t *testing.T) {
	fake := closedEpicFixture()
	iss := fake.issues["ep1"]
	iss.Labels = []string{epicreview.LabelDegraded}
	iss.Notes = epicreview.FormatDegradedNote(2, "validator timeout")
	fake.issues["ep1"] = iss
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	findings := r.patrolCheckEpicValidations(t.Context(), time.Now())

	if got := countLevel(findings, "warn"); got != 1 {
		t.Fatalf("warn findings = %d, want 1; findings = %+v", got, findings)
	}
	msg := findings[0].message
	if !strings.Contains(msg, "ep1") || !strings.Contains(msg, "round=2") || !strings.Contains(msg, `reason="validator timeout"`) {
		t.Errorf("finding message = %q, want it to name ep1/round=2/reason=validator timeout", msg)
	}
}

// TestPatrolCheckEpicValidations_UnlabeledProducesNone is the AC's negative
// fixture: an epic with neither validation:parked nor validation:degraded
// produces no WARN finding.
func TestPatrolCheckEpicValidations_UnlabeledProducesNone(t *testing.T) {
	fake := closedEpicFixture() // ep1: open epic, unlabeled
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	findings := r.patrolCheckEpicValidations(t.Context(), time.Now())

	if got := countLevel(findings, "warn"); got != 0 {
		t.Errorf("warn findings = %d, want 0; findings = %+v", got, findings)
	}
	if got := countLevel(findings, "ok"); got != 1 {
		t.Errorf("ok findings = %d, want 1; findings = %+v", got, findings)
	}
}

// TestPatrolCheckEpicValidations_ClosedEpicProducesNone is the AC's other
// negative fixture: a closed epic carrying validation:parked must not be
// reported — the state is moot once the epic itself is closed. The listing
// is primed directly (bypassing the fake's own closed-issue filtering,
// which mirrors bd's real List() contract) so this exercises the check's own
// status guard rather than relying on the fake.
func TestPatrolCheckEpicValidations_ClosedEpicProducesNone(t *testing.T) {
	r := &runner{
		adapter: &epicFakeStore{},
		epicPatrolIssues: []beads.Issue{
			{
				ID: "ep-closed", Title: "closed epic", IssueType: "epic", Status: "closed",
				Labels: []string{epicreview.LabelParked},
				Notes:  epicreview.FormatParkedNote(1, 5, "koryph epic validate ep-closed"),
			},
		},
		epicPatrolAt: time.Now(),
	}

	findings := r.patrolCheckEpicValidations(t.Context(), time.Now())

	if got := countLevel(findings, "warn"); got != 0 {
		t.Errorf("warn findings = %d, want 0; findings = %+v", got, findings)
	}
}

func TestPatrolCheckEpicValidations_NoListerAdapter_OK(t *testing.T) {
	r := &runner{adapter: &fakeSource{}}
	findings := r.patrolCheckEpicValidations(t.Context(), time.Now())
	if len(findings) != 1 || findings[0].level != "ok" {
		t.Errorf("findings = %+v, want a single ok finding for an adapter without List", findings)
	}
}

// TestPatrolCheckEpicValidations_SharesCacheWithUnvalidatedEpics verifies the
// bd-call-cadence decision recorded on patrolCheckEpicValidations: it reuses
// patrolCheckUnvalidatedEpics's cached listing rather than issuing a second
// bd call within the same patrol tick.
func TestPatrolCheckEpicValidations_SharesCacheWithUnvalidatedEpics(t *testing.T) {
	fake := closedEpicFixture()
	iss := fake.issues["ep1"]
	iss.Labels = []string{epicreview.LabelDegraded}
	iss.Notes = epicreview.FormatDegradedNote(1, "boom")
	fake.issues["ep1"] = iss
	r, _ := epicRunner(t, fake, epicreview.Verdict{Met: true})

	now := time.Now()
	r.patrolCheckUnvalidatedEpics(t.Context(), now)
	if fake.listCalls != 1 {
		t.Fatalf("listCalls after unvalidated-epics scan = %d, want 1", fake.listCalls)
	}

	findings := r.patrolCheckEpicValidations(t.Context(), now)
	if fake.listCalls != 1 {
		t.Errorf("listCalls after epic-validations scan = %d, want still 1 (shared cache, no second bd call)", fake.listCalls)
	}
	if countLevel(findings, "warn") != 1 {
		t.Errorf("expected 1 warn finding for the degraded epic; got %+v", findings)
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
