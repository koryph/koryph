// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package doctor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/govern"
	"github.com/koryph/koryph/internal/quota"
)

// fabricate creates a minimal valid ~/.koryph skeleton in t.TempDir().
func fabricate(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	for _, sub := range []string{"registry.d", "quota", "slots", "slots/demand"} {
		if err := os.MkdirAll(filepath.Join(home, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return home
}

func opts(home string) Options {
	return Options{
		Home:     home,
		Now:      func() time.Time { return time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC) },
		Alive:    func(pid int) bool { return false }, // no real PIDs in tests
		LookPath: func(name string) (string, error) { return "/usr/bin/" + name, nil },
	}
}

func writeLease(t *testing.T, dir string, l govern.Lease) {
	t.Helper()
	name := l.Project + "__" + l.Bead + ".json"
	data, _ := json.Marshal(l)
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeDemand(t *testing.T, dir string, d govern.Demand) {
	t.Helper()
	data, _ := json.Marshal(d)
	if err := os.WriteFile(filepath.Join(dir, d.Project+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// --- circuit breaker (koryph-2im.11) ---

func TestBreakerNotConfiguredOK(t *testing.T) {
	home := fabricate(t)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelOK {
		t.Errorf("breaker level=%s, want ok when governor.json absent", f.Level)
	}
}

func TestBreakerAdaptiveOffOK(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{MaxGlobalAgents: 6})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelOK {
		t.Errorf("breaker level=%s, want ok when adaptive is off", f.Level)
	}
}

func TestBreakerClosedOK(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 4,
	})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelOK {
		t.Errorf("breaker level=%s, want ok when closed", f.Level)
	}
}

func TestBreakerOpenWarns(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
		BreakerState: "open",
	})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelWarn {
		t.Errorf("breaker level=%s, want warn when open", f.Level)
	}
}

func TestBreakerHalfOpenWarns(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
		BreakerState: "half-open", ProbeProject: "p", ProbeBead: "b1",
	})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelWarn {
		t.Errorf("breaker level=%s, want warn when half-open", f.Level)
	}
}

func TestBreakerOpenFlappingMentionsFlapping(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
		BreakerState: "open", BreakerReopenCount: 2,
	})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameBreaker)
	if f.Level != LevelWarn {
		t.Errorf("breaker level=%s, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "flapping") {
		t.Errorf("breaker message = %q, want it to call out flapping at reopen count >=2", f.Message)
	}
}

// --- per-provider pools (koryph-v8u.11, L5c) ---

// writeGovernorFile writes a multi-pool governor.json ({"pools": {...}}
// shape) directly, for tests exercising more than the single default pool.
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

// findAllChecks returns every Finding for the given check name, in report
// order — unlike findCheck (which returns only the first), used here because
// a multi-pool governor.json is expected to produce one Finding PER POOL.
func findAllChecks(r *Report, check string) []Finding {
	var out []Finding
	for _, f := range r.Findings {
		if f.Check == check {
			out = append(out, f)
		}
	}
	return out
}

// TestBreakerOnePerPool proves checkCircuitBreaker reports independently per
// pool: an open breaker in one pool must not affect another pool's (closed)
// finding, and each Finding names its own pool.
func TestBreakerOnePerPool(t *testing.T) {
	home := fabricate(t)
	writeGovernorFile(t, home, govern.File{Pools: map[string]govern.Config{
		"anthropic": {MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 4},
		"openai":    {MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1, BreakerState: "open"},
	}})
	r, _ := Run(opts(home))
	findings := findAllChecks(r, checkNameBreaker)
	if len(findings) != 2 {
		t.Fatalf("breaker findings = %d, want 2 (one per pool):\n%+v", len(findings), findings)
	}
	byPool := map[string]Finding{}
	for _, f := range findings {
		if strings.Contains(f.Message, "pool anthropic") {
			byPool["anthropic"] = f
		}
		if strings.Contains(f.Message, "pool openai") {
			byPool["openai"] = f
		}
	}
	if byPool["anthropic"].Level != LevelOK {
		t.Errorf("anthropic pool breaker = %+v, want ok (closed)", byPool["anthropic"])
	}
	if byPool["openai"].Level != LevelWarn {
		t.Errorf("openai pool breaker = %+v, want warn (open)", byPool["openai"])
	}
}

// TestAdaptiveCapPinnedOnePerPool proves checkAdaptiveCapPinned is scoped per
// pool: a long-pinned dynamic cap in one pool warns while another pool that
// has never decreased stays ok.
func TestAdaptiveCapPinnedOnePerPool(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	writeGovernorFile(t, home, govern.File{Pools: map[string]govern.Config{
		"anthropic": {MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
			LastDecreaseAt: o.now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)},
		"openai": {MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 4},
	}})
	r, _ := Run(o)
	findings := findAllChecks(r, checkNameAdaptiveCap)
	if len(findings) != 2 {
		t.Fatalf("adaptive-cap findings = %d, want 2 (one per pool):\n%+v", len(findings), findings)
	}
	var anthropicWarn, openaiOK bool
	for _, f := range findings {
		if strings.Contains(f.Message, "pool anthropic") && f.Level == LevelWarn && strings.Contains(f.Message, "pinned at 1") {
			anthropicWarn = true
		}
		if strings.Contains(f.Message, "pool openai") && f.Level == LevelOK {
			openaiOK = true
		}
	}
	if !anthropicWarn {
		t.Errorf("expected a warn Finding naming pool anthropic and 'pinned at 1':\n%+v", findings)
	}
	if !openaiOK {
		t.Errorf("expected an ok Finding naming pool openai:\n%+v", findings)
	}
}

// TestGovernorConfigOnePerPool proves checkGovernorConfig reports one Finding
// per pool, each naming its own cap.
func TestGovernorConfigOnePerPool(t *testing.T) {
	home := fabricate(t)
	writeGovernorFile(t, home, govern.File{Pools: map[string]govern.Config{
		"anthropic": {MaxGlobalAgents: 8},
		"openai":    {MaxGlobalAgents: 3},
	}})
	r, _ := Run(opts(home))
	findings := findAllChecks(r, checkNameGovernor)
	if len(findings) != 2 {
		t.Fatalf("governor findings = %d, want 2 (one per pool):\n%+v", len(findings), findings)
	}
	var sawAnthropic8, sawOpenAI3 bool
	for _, f := range findings {
		if strings.Contains(f.Message, "pool anthropic: cap=8") {
			sawAnthropic8 = true
		}
		if strings.Contains(f.Message, "pool openai: cap=3") {
			sawOpenAI3 = true
		}
	}
	if !sawAnthropic8 || !sawOpenAI3 {
		t.Errorf("expected per-pool cap findings for both pools:\n%+v", findings)
	}
}

// TestZombieLeaseDeadAgentAliveEngineNotZombie covers the koryph-p42 false-
// positive: when the agent process has legitimately exited (build done) but
// the engine is still alive managing the slot through review / rebase / gate
// / merge, the dead agent PID must NOT be flagged as a zombie. The engine PID
// is the correct liveness signal for post-build stages.
func TestZombieLeaseDeadAgentAliveEngineNotZombie(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project:   "myproject",
		Bead:      "abc-review",
		PID:       99999, // agent is dead (build finished)
		EnginePID: 88888, // engine is alive (in review/merge stage)
		Provider:  "anthropic",
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return pid == 88888 } // only engine alive
	r, _ := Run(o)
	f := findCheck(r, checkNameZombies)
	if f.Level != LevelOK {
		t.Errorf("zombie level=%s, want ok when engine is alive (post-build stage is normal): %s",
			f.Level, f.Message)
	}
}

// TestZombieLeaseDeadBothIsZombie confirms that a lease is flagged as a zombie
// only when BOTH the agent PID and the engine PID are dead (koryph-p42).
func TestZombieLeaseDeadBothIsZombie(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project:   "myproject",
		Bead:      "abc-orphan",
		PID:       99999, // agent dead
		EnginePID: 88888, // engine also dead
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return false } // both dead
	r, _ := Run(o)
	f := findCheck(r, checkNameZombies)
	if f.Level != LevelWarn {
		t.Errorf("zombie level=%s, want warn when both agent and engine are dead: %s",
			f.Level, f.Message)
	}
}

// TestZombieLeaseNamesItsPool proves a zombie-lease Finding names the pool
// the lease belongs to (Lease.Provider, koryph-v8u.11).
func TestZombieLeaseNamesItsPool(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project: "myproject", Bead: "abc-1", PID: 99999, EnginePID: 88888, Provider: "openai",
	})
	o := opts(home)
	o.Alive = func(pid int) bool { return false }
	r, _ := Run(o)
	f := findCheck(r, checkNameZombies)
	if f.Level != LevelWarn {
		t.Errorf("zombie level=%s, want warn", f.Level)
	}
	if !strings.Contains(f.Message, "pool openai") {
		t.Errorf("zombie message = %q, want it to name pool openai", f.Message)
	}
}

// --- layout ---

func TestLayoutOK(t *testing.T) {
	home := fabricate(t)
	r, err := Run(opts(home))
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameLayout)
	if f.Level != LevelOK {
		t.Errorf("layout level=%s msg=%s", f.Level, f.Message)
	}
}

func TestLayoutMissingDir(t *testing.T) {
	home := t.TempDir()
	// create home but not the subdirs
	r, err := Run(opts(home))
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameLayout)
	if f.Level != LevelError {
		t.Errorf("layout level=%s, want error", f.Level)
	}
}

func TestLayoutMissingHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "notexist")
	r, err := Run(opts(home))
	if err != nil {
		t.Fatal(err)
	}
	f := findCheck(r, checkNameLayout)
	if f.Level != LevelError {
		t.Errorf("layout level=%s, want error for missing home", f.Level)
	}
}

// --- binaries ---

func TestBinariesAllFound(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	o.LookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	r, _ := Run(o)
	for _, f := range r.Findings {
		if f.Check != checkNameBinaries {
			continue
		}
		if f.Level != LevelOK {
			t.Errorf("binary check: %s level=%s", f.Message, f.Level)
		}
	}
}

func TestBinariesMissing(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	o.LookPath = func(name string) (string, error) {
		if name == "bd" {
			return "", os.ErrNotExist
		}
		return "/usr/bin/" + name, nil
	}
	r, _ := Run(o)
	var warns int
	for _, f := range r.Findings {
		if f.Check == checkNameBinaries && f.Level == LevelWarn {
			warns++
		}
	}
	if warns != 1 {
		t.Errorf("expected 1 binary warning, got %d", warns)
	}
}

// --- registry ---

func TestRegistryOK(t *testing.T) {
	home := fabricate(t)
	// write a valid-JSON record
	_ = os.WriteFile(filepath.Join(home, "registry.d", "proj.json"),
		[]byte(`{"project_id":"proj"}`), 0o644)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameRegistry)
	if f.Level != LevelOK {
		t.Errorf("registry level=%s msg=%s", f.Level, f.Message)
	}
}

func TestRegistryCorrupt(t *testing.T) {
	home := fabricate(t)
	_ = os.WriteFile(filepath.Join(home, "registry.d", "bad.json"),
		[]byte(`NOT JSON`), 0o644)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameRegistry)
	if f.Level != LevelError {
		t.Errorf("registry level=%s, want error for corrupt file", f.Level)
	}
}

// --- governor ---

func TestGovernorAbsent(t *testing.T) {
	home := fabricate(t)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameGovernor)
	if f.Level != LevelOK {
		t.Errorf("governor level=%s, want ok when absent", f.Level)
	}
}

func TestGovernorValid(t *testing.T) {
	home := fabricate(t)
	data, _ := json.Marshal(govern.Config{MaxGlobalAgents: 6})
	_ = os.WriteFile(filepath.Join(home, "governor.json"), data, 0o644)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameGovernor)
	if f.Level != LevelOK {
		t.Errorf("governor level=%s, want ok for valid config", f.Level)
	}
}

func TestGovernorCorrupt(t *testing.T) {
	home := fabricate(t)
	_ = os.WriteFile(filepath.Join(home, "governor.json"), []byte("{{bad}}"), 0o644)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameGovernor)
	if f.Level != LevelError {
		t.Errorf("governor level=%s, want error for corrupt file", f.Level)
	}
}

// --- adaptive cap pinned (koryph-2im.4) ---

func writeGovernorConfig(t *testing.T, home string, cfg govern.Config) {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "governor.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAdaptiveCapAbsentOK(t *testing.T) {
	home := fabricate(t)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelOK {
		t.Errorf("adaptive-cap level=%s, want ok when governor.json absent", f.Level)
	}
}

func TestAdaptiveCapOffOK(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{MaxGlobalAgents: 6})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelOK {
		t.Errorf("adaptive-cap level=%s, want ok when adaptive is off", f.Level)
	}
}

func TestAdaptiveCapNoDecreaseYetOK(t *testing.T) {
	home := fabricate(t)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
	})
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelOK {
		t.Errorf("adaptive-cap level=%s, want ok when no decrease has ever been recorded", f.Level)
	}
}

func TestAdaptiveCapRecentlyPinnedOK(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
		LastDecreaseAt: o.now().Add(-5 * time.Minute).UTC().Format(time.RFC3339),
	})
	r, _ := Run(o)
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelOK {
		t.Errorf("adaptive-cap level=%s, want ok for a recent (not yet long-pinned) decrease", f.Level)
	}
}

func TestAdaptiveCapLongPinnedWarns(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 1,
		LastDecreaseAt:  o.now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
		RateLimitEvents: 12,
	})
	r, _ := Run(o)
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelWarn {
		t.Errorf("adaptive-cap level=%s, want warn for a long-pinned floor", f.Level)
	}
	if !strings.Contains(f.Message, "pinned at 1") {
		t.Errorf("adaptive-cap message = %q, want it to name the pinned value", f.Message)
	}
}

func TestAdaptiveCapNotPinnedAboveFloorOK(t *testing.T) {
	home := fabricate(t)
	o := opts(home)
	writeGovernorConfig(t, home, govern.Config{
		MaxGlobalAgents: 4, Adaptive: true, HardMax: 8, DynamicCap: 3,
		LastDecreaseAt: o.now().Add(-2 * time.Hour).UTC().Format(time.RFC3339),
	})
	r, _ := Run(o)
	f := findCheck(r, checkNameAdaptiveCap)
	if f.Level != LevelOK {
		t.Errorf("adaptive-cap level=%s, want ok when the dynamic cap has recovered above 1", f.Level)
	}
}

// --- zombie leases ---

func TestZombieLeaseDetected(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project:   "myproject",
		Bead:      "abc-1",
		PID:       99999,
		EnginePID: 88888,
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return false } // all PIDs dead
	r, _ := Run(o)
	f := findCheck(r, checkNameZombies)
	if f.Level != LevelWarn {
		t.Errorf("zombie level=%s, want warn", f.Level)
	}
}

func TestZombieLeaseFixed(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project:   "myproject",
		Bead:      "abc-1",
		PID:       99999,
		EnginePID: 88888,
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return false }
	o.Fix = true
	r, _ := Run(o)

	f := findCheck(r, checkNameZombies)
	if !f.Fixed {
		t.Errorf("zombie not fixed: level=%s msg=%s", f.Level, f.Message)
	}
	if r.FixedCount != 1 {
		t.Errorf("FixedCount=%d, want 1", r.FixedCount)
	}
	// Lease file should be gone (demand/ subdir and .lock are expected).
	entries, _ := os.ReadDir(slotsDir)
	for _, e := range entries {
		if e.IsDir() || e.Name() == ".lock" {
			continue
		}
		t.Errorf("slot file %s not removed after fix", e.Name())
	}
}

func TestAliveLeaseNotZombie(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	writeLease(t, slotsDir, govern.Lease{
		Project:   "myproject",
		Bead:      "abc-2",
		PID:       12345,
		EnginePID: 54321,
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return pid == 12345 || pid == 54321 }
	r, _ := Run(o)
	f := findCheck(r, checkNameZombies)
	if f.Level != LevelOK {
		t.Errorf("alive lease marked zombie: level=%s msg=%s", f.Level, f.Message)
	}
}

func TestZombieMultiple(t *testing.T) {
	home := fabricate(t)
	slotsDir := filepath.Join(home, "slots")
	for i, bead := range []string{"abc-1", "abc-2", "abc-3"} {
		writeLease(t, slotsDir, govern.Lease{
			Project:   "proj",
			Bead:      bead,
			PID:       10000 + i,
			EnginePID: 20000 + i,
		})
	}
	o := opts(home)
	o.Alive = func(pid int) bool { return false }
	r, _ := Run(o)

	count := 0
	for _, f := range r.Findings {
		if f.Check == checkNameZombies && f.Level == LevelWarn {
			count++
		}
	}
	if count != 3 {
		t.Errorf("expected 3 zombie findings, got %d", count)
	}
}

// --- stale demand ---

func TestStaleDemandDeadEngine(t *testing.T) {
	home := fabricate(t)
	demandDir := filepath.Join(home, "slots", "demand")
	writeDemand(t, demandDir, govern.Demand{
		Project:   "p1",
		EnginePID: 99999,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return false }
	r, _ := Run(o)
	f := findCheck(r, checkNameDemand)
	if f.Level != LevelWarn {
		t.Errorf("stale-demand level=%s, want warn for dead engine", f.Level)
	}
}

func TestStaleDemandHeartbeatExpired(t *testing.T) {
	home := fabricate(t)
	demandDir := filepath.Join(home, "slots", "demand")
	staleTime := time.Date(2026, 7, 2, 11, 0, 0, 0, time.UTC) // 1h before opts.Now
	writeDemand(t, demandDir, govern.Demand{
		Project:   "p2",
		EnginePID: 12345,
		UpdatedAt: staleTime.Format(time.RFC3339),
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return pid == 12345 } // engine alive, but stale
	r, _ := Run(o)
	f := findCheck(r, checkNameDemand)
	if f.Level != LevelWarn {
		t.Errorf("stale-demand level=%s, want warn for stale heartbeat", f.Level)
	}
}

func TestStaleDemandFixed(t *testing.T) {
	home := fabricate(t)
	demandDir := filepath.Join(home, "slots", "demand")
	writeDemand(t, demandDir, govern.Demand{
		Project:   "p3",
		EnginePID: 99999,
		UpdatedAt: time.Now().UTC().Format(time.RFC3339),
	})

	o := opts(home)
	o.Alive = func(pid int) bool { return false }
	o.Fix = true
	r, _ := Run(o)
	f := findCheck(r, checkNameDemand)
	if !f.Fixed {
		t.Errorf("stale demand not fixed: level=%s msg=%s", f.Level, f.Message)
	}
}

func TestFreshDemandNotStale(t *testing.T) {
	home := fabricate(t)
	demandDir := filepath.Join(home, "slots", "demand")
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	writeDemand(t, demandDir, govern.Demand{
		Project:   "p4",
		EnginePID: 42,
		UpdatedAt: now.Add(-1 * time.Minute).Format(time.RFC3339),
	})

	o := opts(home)
	o.Now = func() time.Time { return now }
	o.Alive = func(pid int) bool { return pid == 42 }
	r, _ := Run(o)
	f := findCheck(r, checkNameDemand)
	if f.Level != LevelOK {
		t.Errorf("fresh demand level=%s, want ok", f.Level)
	}
}

// --- quota calibration ---

func TestQuotaUncalibrated(t *testing.T) {
	home := fabricate(t)
	cfg := quota.Config{Account: "personal", WindowCeilingUSD: 0, WeeklyCeilingUSD: 0}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(home, "quota", "personal.json"), data, 0o644)

	r, _ := Run(opts(home))
	f := findCheck(r, checkNameQuota)
	if f.Level != LevelWarn {
		t.Errorf("quota level=%s, want warn for uncalibrated", f.Level)
	}
}

func TestQuotaCalibrated(t *testing.T) {
	home := fabricate(t)
	cfg := quota.Config{Account: "personal", WindowCeilingUSD: 20.0, WeeklyCeilingUSD: 80.0}
	data, _ := json.Marshal(cfg)
	_ = os.WriteFile(filepath.Join(home, "quota", "personal.json"), data, 0o644)

	r, _ := Run(opts(home))
	f := findCheck(r, checkNameQuota)
	if f.Level != LevelOK {
		t.Errorf("quota level=%s, want ok for calibrated config", f.Level)
	}
}

// --- vault providers ---

func TestVaultAbsent(t *testing.T) {
	home := fabricate(t)
	r, _ := Run(opts(home))
	f := findCheck(r, checkNameVault)
	if f.Level != LevelOK {
		t.Errorf("vault level=%s, want ok when absent", f.Level)
	}
}

func TestVaultProviderBinaryOK(t *testing.T) {
	home := fabricate(t)
	v := map[string]interface{}{
		"providers": map[string]interface{}{
			"protonpass": map[string]interface{}{
				"fetch": []string{"pass-cli", "item", "view", "{ref}"},
			},
		},
	}
	data, _ := json.Marshal(v)
	_ = os.WriteFile(filepath.Join(home, "vault.json"), data, 0o644)

	o := opts(home)
	o.LookPath = func(name string) (string, error) { return "/usr/bin/" + name, nil }
	r, _ := Run(o)
	f := findCheck(r, checkNameVault)
	if f.Level != LevelOK {
		t.Errorf("vault level=%s, want ok when binary exists", f.Level)
	}
}

func TestVaultProviderBinaryMissing(t *testing.T) {
	home := fabricate(t)
	v := map[string]interface{}{
		"providers": map[string]interface{}{
			"protonpass": map[string]interface{}{
				"fetch": []string{"pass-cli", "item", "view", "{ref}"},
			},
		},
	}
	data, _ := json.Marshal(v)
	_ = os.WriteFile(filepath.Join(home, "vault.json"), data, 0o644)

	o := opts(home)
	o.LookPath = func(name string) (string, error) { return "", os.ErrNotExist }
	r, _ := Run(o)
	f := findCheck(r, checkNameVault)
	if f.Level != LevelWarn {
		t.Errorf("vault level=%s, want warn when binary missing", f.Level)
	}
}

// --- exit codes ---

func TestExitCodeOK(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Check: "a", Level: LevelOK},
		{Check: "b", Level: LevelOK},
	}}
	if r.ExitCode() != 0 {
		t.Errorf("ExitCode=%d, want 0", r.ExitCode())
	}
}

func TestExitCodeWarn(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Check: "a", Level: LevelOK},
		{Check: "b", Level: LevelWarn},
	}}
	if r.ExitCode() != 1 {
		t.Errorf("ExitCode=%d, want 1", r.ExitCode())
	}
}

func TestExitCodeError(t *testing.T) {
	r := &Report{Findings: []Finding{
		{Check: "a", Level: LevelWarn},
		{Check: "b", Level: LevelError},
	}}
	if r.ExitCode() != 2 {
		t.Errorf("ExitCode=%d, want 2", r.ExitCode())
	}
}

// --- helpers ---

// findCheck returns the first finding for the given check name, or a warn
// "NOT FOUND" sentinel so tests that call it on a missing check fail clearly.
func findCheck(r *Report, check string) Finding {
	for _, f := range r.Findings {
		if f.Check == check {
			return f
		}
	}
	return Finding{Check: check, Level: LevelWarn, Message: "NOT FOUND in report"}
}
