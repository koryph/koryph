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

func TestGovernorUnknownSubcommand(t *testing.T) {
	isolate(t)
	code, _, errs := runCmd("governor", "wat")
	if code == 0 || !strings.Contains(errs, "unknown governor subcommand") {
		t.Errorf("code=%d stderr=%q, want usage error", code, errs)
	}
}
