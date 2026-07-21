// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/ledger"
)

func frontierRun() *ledger.Run {
	return &ledger.Run{
		RunID: "r1", Wave: 3,
		Frontier: &ledger.FrontierSnapshot{
			At: "2026-07-21T10:00:00Z", Wave: 3,
			Entries: []ledger.FrontierEntry{
				{BeadID: "d1", Title: "do thing", Verdict: "dispatched"},
				{BeadID: "x1", Title: "blocked work", Verdict: "deferred", Reason: "footprint conflict with d1 (in-flight)"},
				{BeadID: "s1", Title: "an epic", Verdict: "skipped", Reason: "container bead"},
			},
		},
	}
}

// TestPrintFrontier proves the D7/D9 rendering: every candidate's verdict and
// full reason is shown, with per-verdict counts.
func TestPrintFrontier(t *testing.T) {
	var out, errb bytes.Buffer
	if code := printFrontier(&out, &errb, "proj", frontierRun(), false); code != 0 {
		t.Fatalf("exit %d (stderr=%s)", code, errb.String())
	}
	s := out.String()
	for _, want := range []string{
		"1 dispatched", "1 deferred", "1 skipped",
		"d1", "dispatched",
		"footprint conflict with d1 (in-flight)", // full reason, not truncated
		"container bead",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("frontier output missing %q:\n%s", want, s)
		}
	}
}

func TestPrintFrontier_Empty(t *testing.T) {
	var out, errb bytes.Buffer
	printFrontier(&out, &errb, "proj", &ledger.Run{RunID: "r2"}, false)
	if !strings.Contains(out.String(), "no frontier recorded") {
		t.Errorf("empty frontier: want a graceful message, got %q", out.String())
	}
}

func TestPrintFrontier_JSON(t *testing.T) {
	var out, errb bytes.Buffer
	printFrontier(&out, &errb, "proj", frontierRun(), true)
	s := out.String()
	if !strings.Contains(s, `"verdict"`) || !strings.Contains(s, "deferred") {
		t.Errorf("json frontier missing verdict field:\n%s", s)
	}
}
