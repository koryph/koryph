// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/plan"
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

func TestPlanHumanReportRendersActionableUnitFinding(t *testing.T) {
	var out bytes.Buffer
	printAuditReport(&out, &plan.AuditReport{
		ProjectID: "demo", EpicID: "demo-1",
		Quality: []plan.QualityFinding{{
			Severity: "error", Code: "unit-outcome-count", IssueID: "demo-1.1",
			Message:     "unit declares 2 provided outcomes; exactly one is required",
			Remediation: "split independent outcomes into separate provider beads",
		}},
	})
	for _, want := range []string{
		"QUALITY GATE — 1 error(s)",
		"[unit-outcome-count]",
		"split independent outcomes",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report missing %q:\n%s", want, out.String())
		}
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
