// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package timeoutcfg

import (
	"testing"
	"time"
)

func TestBeadTimeout(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		want   int
		ok     bool
	}{
		{"none", []string{"model:opus", "area:sched"}, 0, false},
		{"nil", nil, 0, false},
		{"bare", []string{"timeout:900"}, 900, true},
		{"first wins", []string{"timeout:900", "timeout:1800"}, 900, true},
		{"above builtin ok", []string{"timeout:3600"}, 3600, true},
		{"empty value skipped", []string{"timeout:", "timeout:120"}, 120, true},
		{"non-numeric skipped", []string{"timeout:soon", "timeout:120"}, 120, true},
		{"zero skipped", []string{"timeout:0", "timeout:120"}, 120, true},
		{"negative skipped", []string{"timeout:-5", "timeout:120"}, 120, true},
		{"scoped form skipped", []string{"timeout:review:120"}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BeadTimeout(tc.labels)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("BeadTimeout(%v) = (%d,%v), want (%d,%v)", tc.labels, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestResolvePrecedence(t *testing.T) {
	cases := []struct {
		name                        string
		bead, project, system, want int
	}{
		{"all unset -> builtin", 0, 0, 0, BuiltinDefaultSec},
		{"system only", 0, 0, 300, 300},
		{"project over system", 0, 200, 300, 200},
		{"bead over all", 100, 200, 300, 100},
		{"bead over builtin", 100, 0, 0, 100},
		{"negative treated as unset", -1, -1, 500, 500},
		{"override exceeds builtin (no policy cap)", 5000, 0, 0, 5000},
		{"absurd override clamped to overflow guard", 1 << 40, 0, 0, MaxSaneSec},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Resolve(tc.bead, tc.project, tc.system); got != tc.want {
				t.Fatalf("Resolve(%d,%d,%d) = %d, want %d", tc.bead, tc.project, tc.system, got, tc.want)
			}
		})
	}
}

func TestResolveAlwaysPositive(t *testing.T) {
	if got := Resolve(0, 0, 0); got <= 0 {
		t.Fatalf("Resolve(0,0,0) = %d, want > 0", got)
	}
}

func TestClamp(t *testing.T) {
	cases := []struct{ in, want int }{
		{0, 0},
		{1200, 1200},
		{MaxSaneSec, MaxSaneSec},
		{MaxSaneSec + 1, MaxSaneSec},
		{1 << 40, MaxSaneSec},
	}
	for _, tc := range cases {
		if got := Clamp(tc.in); got != tc.want {
			t.Errorf("Clamp(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestClampPreventsDurationOverflow is the regression guard for the review
// finding: the clamped value, converted to a time.Duration nanosecond count,
// must stay positive (a value near ~9.2e9 s would wrap int64 to negative).
func TestClampPreventsDurationOverflow(t *testing.T) {
	d := time.Duration(Clamp(1<<40)) * time.Second
	if d <= 0 {
		t.Fatalf("clamped duration = %v, want > 0 (overflow guard failed)", d)
	}
}
