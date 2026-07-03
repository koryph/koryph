// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
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
	code, _, errs := runCmd("governor", "set", "--max-global", "0")
	if code == 0 {
		t.Errorf("governor set --max-global 0 should fail")
	}
	if !strings.Contains(errs, "positive") {
		t.Errorf("stderr = %q, want a positivity complaint", errs)
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
