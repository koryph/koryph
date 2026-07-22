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

// TestZombieSlot is the koryph-k6o regression guard for the shared
// board/status zombie predicate: only a RUNNING slot whose recorded pid the
// probe reports dead is a zombie; a terminal status, an unset pid, or a live
// pid must never be flagged. Review/stuck/dispatching slots legitimately have
// a dead AGENT pid while the engine drives post-build stages, so they must NOT
// be flagged (the blocking review finding this guards).
func TestZombieSlot(t *testing.T) {
	dead := func(int) bool { return false }
	live := func(int) bool { return true }

	cases := []struct {
		name  string
		sl    *ledger.Slot
		alive func(int) bool
		want  bool
	}{
		{"running + dead pid → zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 123}, dead, true},
		{"running + live pid → not zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 123}, live, false},
		{"merged (terminal) + dead pid → not zombie", &ledger.Slot{Status: ledger.SlotMerged, PID: 123}, dead, false},
		{"review + dead agent pid → not zombie (engine drives review)", &ledger.Slot{Status: ledger.SlotReview, PID: 123}, dead, false},
		{"stuck + dead agent pid → not zombie (not the running stage)", &ledger.Slot{Status: ledger.SlotStuck, PID: 123}, dead, false},
		{"dispatching + dead agent pid → not zombie (agent not up yet)", &ledger.Slot{Status: ledger.SlotDispatching, PID: 123}, dead, false},
		{"no pid recorded → not zombie", &ledger.Slot{Status: ledger.SlotRunning, PID: 0}, dead, false},
		{"nil slot → not zombie", nil, dead, false},
	}
	for _, c := range cases {
		if got := zombieSlot(c.sl, c.alive); got != c.want {
			t.Errorf("%s: zombieSlot = %v, want %v", c.name, got, c.want)
		}
	}
}

// TestZombieCell verifies the board ZOMBIES column renders "-" when clean and
// a loud "⚠ N" otherwise, so a running:N/LIVE:0 mismatch can't be missed by
// comparing SLOTS against LIVE by eye.
func TestZombieCell(t *testing.T) {
	if got := zombieCell(0); got != "-" {
		t.Errorf("zombieCell(0) = %q, want %q", got, "-")
	}
	if got := zombieCell(2); got != "⚠ 2" {
		t.Errorf("zombieCell(2) = %q, want %q", got, "⚠ 2")
	}
}
