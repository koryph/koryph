// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package resmon

import "testing"

func TestParsePSTime(t *testing.T) {
	cases := []struct {
		in   string
		want float64
		ok   bool
	}{
		{"0:00.00", 0, true},
		{"0:05.50", 5.5, true},
		{"1:30.00", 90, true},
		{"2:03:04", 2*3600 + 3*60 + 4, true}, // HH:MM:SS
		{"1-02:00:00", 86400 + 2*3600, true}, // DD-HH:MM:SS
		{"", 0, false},
		{"garbage", 0, false},
		{"1:2:3:4", 0, false},
	}
	for _, c := range cases {
		got, ok := parsePSTime(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("parsePSTime(%q) = (%v, %v), want (%v, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParsePSTable(t *testing.T) {
	// pid ppid pgid rss(KB) time
	out := "  501   1   501   4096   0:01.50\n" +
		"  502   501   501   8192   0:10.00\n" +
		"bad line skip\n" +
		"  600   1   600   1024   1:00.00\n"
	procs := parsePSTable(out)
	if len(procs) != 3 {
		t.Fatalf("expected 3 parsed rows, got %d", len(procs))
	}
	tbl := newProcTable(procs, false)
	s, ok := tbl.Aggregate(501)
	if !ok {
		t.Fatal("expected pid 501 found")
	}
	// 501 + its child 502 (same pgid 501): rss 4096+8192, cpu 1.5+10.
	if s.RSSKB != 12288 {
		t.Errorf("RSSKB = %d, want 12288", s.RSSKB)
	}
	if s.CPUSeconds != 11.5 {
		t.Errorf("CPUSeconds = %v, want 11.5", s.CPUSeconds)
	}
	if s.PIDs != 2 {
		t.Errorf("PIDs = %d, want 2 (600 is a different group)", s.PIDs)
	}
}
