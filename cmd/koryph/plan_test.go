// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
)

func TestPlanStrictRequiresEpic(t *testing.T) {
	isolate(t)
	code, _, errb := runCmd("plan", "--strict")
	if code != engine.ExitUsage {
		t.Fatalf("code = %d, want %d; stderr=%s", code, engine.ExitUsage, errb)
	}
	if !strings.Contains(errb, "--strict requires --epic") {
		t.Fatalf("stderr missing strict-scope guidance: %s", errb)
	}
}

func TestPlanHelpDocumentsEpicQualityGate(t *testing.T) {
	isolate(t)
	code, out, errb := runCmd("plan", "--help")
	if code != 0 {
		t.Fatalf("code = %d; stderr=%s", code, errb)
	}
	for _, want := range []string{"--epic", "--strict", "quality gate"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing %q:\n%s", want, out)
		}
	}
}
