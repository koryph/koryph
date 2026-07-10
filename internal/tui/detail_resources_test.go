// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/cockpit"
)

// TestDetailResourceSection verifies the Detail panel renders per-bead clock
// times and process-cohort resource usage (koryph process-metrics), with both
// date and time on the timestamps.
func TestDetailResourceSection(t *testing.T) {
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 40)
	start := time.Date(2026, 7, 9, 22, 15, 0, 0, time.Local)
	fin := time.Date(2026, 7, 10, 1, 42, 30, 0, time.Local)
	m.SetDetail(cockpit.BeadDetailSnapshot{
		BeadID: "koryph-4ql.8", Title: "resource governor", Status: "merged", IssueType: "task",
		StartedAt: start, FinishedAt: fin, ComputedAt: fin,
		PeakRSSMB: 1840, AvgRSSMB: 1210, CPUSeconds: 5400, CPUUtilPct: 43.7,
		IOReadMB: 320.5, IOWriteMB: 88.2, ResourceSamples: 74,
	})
	out := stripANSI(m.View())

	for _, want := range []string{
		"Resources",
		"Jul 09", // started date+time (issue #4: date present)
		"Jul 10", // finished date+time
		"avg 1210 MB", "peak 1840 MB",
		"% util",
		"320.5 MB read",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Detail resources view missing %q\n---\n%s", want, out)
		}
	}
}

// TestDetailNoResourceSectionWhenEmpty verifies the Resources section is absent
// for a bead with no timing or samples (no empty scaffold).
func TestDetailNoResourceSectionWhenEmpty(t *testing.T) {
	m := newDetailModel(DefaultTheme())
	m.Resize(100, 40)
	m.SetDetail(cockpit.BeadDetailSnapshot{BeadID: "x-1", Title: "no metrics", Status: "open", IssueType: "task"})
	if strings.Contains(stripANSI(m.View()), "Resources") {
		t.Error("Resources section rendered for a bead with no metrics")
	}
}
