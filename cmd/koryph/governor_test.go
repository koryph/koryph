// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/govern"
)

func TestGovernorShowDefaultAndSet(t *testing.T) {
	isolate(t)

	// No config yet → default cap, no leases.
	code, out, _ := runCmd("governor")
	if code != 0 {
		t.Fatalf("governor: code %d", code)
	}
	wantCap := fmt.Sprintf("cap: %d", govern.DefaultMaxGlobalAgents)
	if !strings.Contains(out, wantCap) || !strings.Contains(out, "no active leases") {
		t.Errorf("default show unexpected (want %q):\n%s", wantCap, out)
	}

	// Set a new cap and confirm it round-trips.
	code, out, _ = runCmd("governor", "set", "--max-global", "9")
	if code != 0 || !strings.Contains(out, "set to 9") {
		t.Fatalf("set: code %d out %q", code, out)
	}
	if _, out, _ = runCmd("governor", "show"); !strings.Contains(out, "cap: 9") {
		t.Errorf("cap not updated:\n%s", out)
	}
}

func TestGovernorSetRejectsNonPositive(t *testing.T) {
	isolate(t)
	// A non-positive cap with no memory floor requested is a no-op — reject it.
	code, _, errs := runCmd("governor", "set", "--max-global", "0")
	if code == 0 {
		t.Errorf("governor set --max-global 0 (no floor) should fail")
	}
	if !strings.Contains(errs, "max-global") {
		t.Errorf("stderr = %q, want it to name the required --max-global", errs)
	}
}

// TestGovernorSetMemoryFloor proves koryph-930: --min-free-memory-mb can be set
// alone (no --max-global), persists to governor.json, and reads back — including
// the negative (disable) and 0 (reset-to-auto) sentinels.
func TestGovernorSetMemoryFloor(t *testing.T) {
	isolate(t)

	// Explicit floor.
	code, out, errs := runCmd("governor", "set", "--min-free-memory-mb", "4096")
	if code != 0 {
		t.Fatalf("floor-only set failed: code %d stderr %q", code, errs)
	}
	if !strings.Contains(out, "4096 MB") {
		t.Errorf("stdout = %q, want the floor confirmation", out)
	}
	if got := govern.NewStore().MinFreeMemoryMB(""); got != 4096 {
		t.Errorf("persisted floor = %d, want 4096", got)
	}

	// Negative disables the gate (persists the -1 sentinel).
	if code, out, _ := runCmd("governor", "set", "--min-free-memory-mb", "-1"); code != 0 || !strings.Contains(out, "disabled") {
		t.Fatalf("disabling the gate: code %d out %q", code, out)
	}
	if got := govern.NewStore().MinFreeMemoryMB(""); got != -1 {
		t.Errorf("floor after disable = %d, want -1", got)
	}

	// 0 resets to auto (sized to physical memory); the raw setting is 0.
	if code, out, _ := runCmd("governor", "set", "--min-free-memory-mb", "0"); code != 0 || !strings.Contains(out, "auto") {
		t.Fatalf("resetting to auto: code %d out %q", code, out)
	}
	if got := govern.NewStore().MinFreeMemoryMB(""); got != 0 {
		t.Errorf("floor after reset = %d, want 0 (auto)", got)
	}
}

// TestGovernorShowMemoryFloorAuto confirms `governor show` reports the auto
// floor (default, no config) sized to physical memory.
func TestGovernorShowMemoryFloorAuto(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("governor", "show")
	if code != 0 {
		t.Fatalf("governor show: code %d", code)
	}
	if !strings.Contains(out, "memory floor: auto") {
		t.Errorf("governor show should report the auto memory floor by default:\n%s", out)
	}
}

// TestGovernorSetAdaptiveShowsOverlayFields proves koryph-2im.4: --adaptive
// seeds the AIMD overlay and `governor show` surfaces its fields (adaptive
// on/off, dynamic cap, hard max, last decrease, rate-limit event count).
func TestGovernorSetAdaptiveShowsOverlayFields(t *testing.T) {
	isolate(t)

	code, out, _ := runCmd("governor", "set", "--max-global", "4", "--adaptive")
	if code != 0 || !strings.Contains(out, "adaptive: dynamic cap 4, hard max 8") {
		t.Fatalf("set --adaptive: code %d out %q", code, out)
	}

	_, out, _ = runCmd("governor", "show")
	for _, want := range []string{
		"adaptive: on",
		"dynamic cap 4",
		"hard max 8",
		"rate-limit events 0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("governor show missing %q:\n%s", want, out)
		}
	}
}

// TestGovernorSetAdaptiveExplicitHardMax proves --hard-max overrides the
// 2x default.
func TestGovernorSetAdaptiveExplicitHardMax(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("governor", "set", "--max-global", "3", "--adaptive", "--hard-max", "20")
	if code != 0 || !strings.Contains(out, "hard max 20") {
		t.Fatalf("set --adaptive --hard-max 20: code %d out %q", code, out)
	}
	_, out, _ = runCmd("governor", "show")
	if !strings.Contains(out, "hard max 20") {
		t.Errorf("show missing overridden hard max:\n%s", out)
	}
}

// TestGovernorSetWithoutAdaptiveDisablesOverlay proves a plain `set` (no
// --adaptive) clears a previously-enabled overlay — today's semantics.
func TestGovernorSetWithoutAdaptiveDisablesOverlay(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("governor", "set", "--max-global", "4", "--adaptive"); code != 0 {
		t.Fatal("enabling adaptive failed")
	}
	if code, _, _ := runCmd("governor", "set", "--max-global", "6"); code != 0 {
		t.Fatal("disabling adaptive (plain set) failed")
	}
	_, out, _ := runCmd("governor", "show")
	if !strings.Contains(out, "adaptive: off") {
		t.Errorf("overlay not disabled by a plain set:\n%s", out)
	}
	if !strings.Contains(out, "cap: 6") {
		t.Errorf("cap not updated by the disabling set:\n%s", out)
	}
}

func TestGovernorUnknownSubcommand(t *testing.T) {
	isolate(t)
	code, _, errs := runCmd("governor", "wat")
	if code == 0 || !strings.Contains(errs, "unknown governor subcommand") {
		t.Errorf("code=%d stderr=%q, want usage error", code, errs)
	}
}

// TestGovernorSetL5bFlagsPersistAndShow proves koryph-2im.11: --settle-sec/
// --break-sec/--min-dispatch-interval persist under --adaptive and surface in
// `governor show` (closed breaker, no active settle, the configured
// smoothing interval).
func TestGovernorSetL5bFlagsPersistAndShow(t *testing.T) {
	isolate(t)
	code, out, _ := runCmd("governor", "set", "--max-global", "4", "--adaptive",
		"--settle-sec", "30", "--break-sec", "60", "--min-dispatch-interval", "1")
	if code != 0 {
		t.Fatalf("set: code %d out %q", code, out)
	}

	_, out, _ = runCmd("governor", "show")
	for _, want := range []string{
		"adaptive: on",
		"settle: not active",
		"breaker: closed",
		"smoothing: min dispatch interval 1s",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("governor show missing %q:\n%s", want, out)
		}
	}
}

// TestGovernorSetL5bFlagsDefaultWhenOmitted proves the three L5b flags
// default (this package's documented constants) rather than persisting 0
// when the operator omits them.
func TestGovernorSetL5bFlagsDefaultWhenOmitted(t *testing.T) {
	isolate(t)
	if code, out, _ := runCmd("governor", "set", "--max-global", "4", "--adaptive"); code != 0 {
		t.Fatalf("set --adaptive: code %d out %q", code, out)
	}
	_, out, _ := runCmd("governor", "show")
	wantInterval := fmt.Sprintf("smoothing: min dispatch interval %ds", govern.DefaultMinDispatchIntervalSeconds)
	if !strings.Contains(out, wantInterval) {
		t.Errorf("governor show missing default smoothing interval %q:\n%s", wantInterval, out)
	}
	if !strings.Contains(out, "breaker: closed") {
		t.Errorf("governor show missing closed breaker:\n%s", out)
	}
}

// --- per-provider pools (koryph-v8u.11, L5c) --------------------------------

// TestGovernorSetProviderRoundTripsIndependentPools proves --provider set/
// show: a pool other than the default (anthropic) round-trips its own cap,
// `governor show` lists BOTH pools, and setting one pool never touches the
// other's cap.
func TestGovernorSetProviderRoundTripsIndependentPools(t *testing.T) {
	isolate(t)

	// Default (--provider omitted) is exactly the anthropic pool.
	if code, out, _ := runCmd("governor", "set", "--max-global", "5"); code != 0 || !strings.Contains(out, "set to 5") {
		t.Fatalf("set (default pool): code %d out %q", code, out)
	}
	// A second, named pool.
	if code, out, _ := runCmd("governor", "set", "--max-global", "3", "--provider", "openai"); code != 0 || !strings.Contains(out, "set to 3") {
		t.Fatalf("set --provider openai: code %d out %q", code, out)
	}

	_, out, _ := runCmd("governor", "show")
	if !strings.Contains(out, "pool anthropic:") || !strings.Contains(out, "pool openai:") {
		t.Errorf("governor show did not list both pools:\n%s", out)
	}
	// Each pool's own cap must appear, and NEITHER cap value may have leaked
	// into the other pool's block.
	anthropicBlock := out[strings.Index(out, "pool anthropic:"):]
	openaiBlock := out[strings.Index(out, "pool openai:"):]
	if !strings.Contains(anthropicBlock, "cap: 5") {
		t.Errorf("anthropic pool block missing cap 5:\n%s", anthropicBlock)
	}
	if !strings.Contains(openaiBlock, "cap: 3") {
		t.Errorf("openai pool block missing cap 3:\n%s", openaiBlock)
	}

	// Re-setting the anthropic pool must not disturb openai's cap.
	if code, _, _ := runCmd("governor", "set", "--max-global", "7"); code != 0 {
		t.Fatal("re-set of default pool failed")
	}
	_, out, _ = runCmd("governor", "show")
	openaiBlock = out[strings.Index(out, "pool openai:"):]
	if !strings.Contains(openaiBlock, "cap: 3") {
		t.Errorf("openai pool cap changed by an unrelated anthropic-pool set:\n%s", openaiBlock)
	}
}

// TestGovernorProviderOmittedDefaultsToAnthropic proves --provider omitted on
// `set` is fully back-compat with the pre-koryph-v8u.11 single-pool CLI: it
// writes the anthropic pool, and `governor show` (still no --provider flag —
// show always lists every pool) contains exactly one pool block for a
// freshly configured machine.
func TestGovernorProviderOmittedDefaultsToAnthropic(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("governor", "set", "--max-global", "4"); code != 0 {
		t.Fatal("set failed")
	}
	_, out, _ := runCmd("governor", "show")
	if strings.Count(out, "pool ") != 1 || !strings.Contains(out, "pool anthropic:") {
		t.Errorf("expected exactly one (anthropic) pool block:\n%s", out)
	}
}

// TestGovernorShowJSON proves `governor show --json` emits a JSON array with
// the correct pool name, cap, in_use, free, and empty lease/demand slices on
// a fresh machine.
func TestGovernorShowJSON(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("governor", "set", "--max-global", "5"); code != 0 {
		t.Fatal("set failed")
	}

	code, out, errb := runCmd("governor", "show", "--json")
	if code != 0 {
		t.Fatalf("governor show --json: code %d stderr=%s", code, errb)
	}

	var snaps []governorPoolJSON
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("governor show --json not valid JSON: %v\n%s", err, out)
	}
	if len(snaps) != 1 {
		t.Fatalf("expected 1 pool, got %d", len(snaps))
	}
	s := snaps[0]
	if s.Pool != govern.DefaultPool {
		t.Errorf("pool = %q, want %q", s.Pool, govern.DefaultPool)
	}
	if s.Cap != 5 {
		t.Errorf("cap = %d, want 5", s.Cap)
	}
	if s.InUse != 0 || s.Free != 5 {
		t.Errorf("in_use=%d free=%d, want 0/5", s.InUse, s.Free)
	}
	if s.Leases == nil || s.Demand == nil {
		t.Errorf("leases/demand should be non-nil empty slices, got leases=%v demand=%v", s.Leases, s.Demand)
	}
}

// TestGovernorShowJSONAdaptive proves `governor show --json` surfaces the AIMD
// overlay fields (adaptive, dynamic_cap, hard_max) when --adaptive was set.
func TestGovernorShowJSONAdaptive(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("governor", "set", "--max-global", "3", "--adaptive", "--hard-max", "12"); code != 0 {
		t.Fatal("set --adaptive failed")
	}

	code, out, errb := runCmd("governor", "show", "--json")
	if code != 0 {
		t.Fatalf("governor show --json: code %d stderr=%s", code, errb)
	}

	var snaps []governorPoolJSON
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("governor show --json not valid JSON: %v\n%s", err, out)
	}
	if len(snaps) == 0 {
		t.Fatal("no pool snapshots returned")
	}
	s := snaps[0]
	if !s.AIMD.Adaptive {
		t.Errorf("AIMD.Adaptive = false, want true")
	}
	if s.AIMD.HardMax != 12 {
		t.Errorf("AIMD.HardMax = %d, want 12", s.AIMD.HardMax)
	}
}

// TestGovernorShowJSONMultiPool proves `governor show --json` returns one entry
// per pool when multiple pools are configured.
func TestGovernorShowJSONMultiPool(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("governor", "set", "--max-global", "4"); code != 0 {
		t.Fatal("set anthropic failed")
	}
	if code, _, _ := runCmd("governor", "set", "--max-global", "2", "--provider", "openai"); code != 0 {
		t.Fatal("set openai failed")
	}

	code, out, errb := runCmd("governor", "show", "--json")
	if code != 0 {
		t.Fatalf("governor show --json: code %d stderr=%s", code, errb)
	}

	var snaps []governorPoolJSON
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(snaps) != 2 {
		t.Fatalf("expected 2 pools, got %d", len(snaps))
	}
	caps := map[string]int{}
	for _, s := range snaps {
		caps[s.Pool] = s.Cap
	}
	if caps[govern.DefaultPool] != 4 {
		t.Errorf("anthropic cap = %d, want 4", caps[govern.DefaultPool])
	}
	if caps["openai"] != 2 {
		t.Errorf("openai cap = %d, want 2", caps["openai"])
	}
}

// TestGovernorShowJSONNoSubVerb proves `governor show --json` with the
// explicit "show" sub-verb emits valid JSON when no pools are explicitly set
// (default pool is always included).
func TestGovernorShowJSONNoSubVerb(t *testing.T) {
	isolate(t)
	// No explicit `set` — default pool is always present.
	code, out, errb := runCmd("governor", "show", "--json")
	if code != 0 {
		t.Fatalf("governor show --json: code %d stderr=%s", code, errb)
	}
	var snaps []governorPoolJSON
	if err := json.Unmarshal([]byte(out), &snaps); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(snaps) == 0 {
		t.Error("expected at least one pool snapshot")
	}
}
