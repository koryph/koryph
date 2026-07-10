// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build linux

package resmon

import "testing"

func TestParseStatCPU(t *testing.T) {
	// Field layout: pid (comm) state ppid pgrp ... utime(14) stime(15) cutime(16) cstime(17)
	// comm deliberately contains spaces and a ')' to exercise the LastIndexByte
	// parse. ppid=1000, pgrp=1200, utime=150, stime=50, cutime=300, cstime=100
	// → (150+50+300+100)/100 = 6.0s (reaped children's CPU is folded in).
	line := "1234 (weird )name) S 1000 1200 1200 0 -1 0 0 0 0 0 150 50 300 100 20 0 1 0 100 0 0"
	ppid, pgid, cpu, ok := parseStatCPU(line)
	if !ok {
		t.Fatal("parseStatCPU returned ok=false")
	}
	if ppid != 1000 {
		t.Errorf("ppid = %d, want 1000", ppid)
	}
	if pgid != 1200 {
		t.Errorf("pgid = %d, want 1200", pgid)
	}
	if cpu != 6.0 {
		t.Errorf("cpuSec = %v, want 6.0 (utime+stime+cutime+cstime)", cpu)
	}
}

func TestParseStatCPU_Malformed(t *testing.T) {
	if _, _, _, ok := parseStatCPU("no paren here"); ok {
		t.Error("expected ok=false for malformed stat")
	}
	if _, _, _, ok := parseStatCPU("1 (x) S 2"); ok {
		t.Error("expected ok=false for too-few fields")
	}
}

func TestParseStatmRSS(t *testing.T) {
	// "size resident shared ...": resident=2560 pages. With a 4 KB page → 10240 KB.
	if got := parseStatmRSS("5000 2560 100 1 0 200 0", 4); got != 10240 {
		t.Errorf("parseStatmRSS = %d, want 10240", got)
	}
	if got := parseStatmRSS("", 4); got != 0 {
		t.Errorf("empty statm = %d, want 0", got)
	}
}

func TestParseProcIO(t *testing.T) {
	io := "rchar: 1000\nwchar: 2000\nread_bytes: 1048576\nwrite_bytes: 524288\ncancelled_write_bytes: 0\n"
	r, w, ok := parseProcIO(io)
	if !ok {
		t.Fatal("parseProcIO ok=false")
	}
	if r != 1024 { // 1048576 / 1024
		t.Errorf("readKB = %d, want 1024", r)
	}
	if w != 512 { // 524288 / 1024
		t.Errorf("writeKB = %d, want 512", w)
	}
	if _, _, ok := parseProcIO("rchar: 1\nwchar: 2\n"); ok {
		t.Error("expected ok=false when no read_bytes/write_bytes present")
	}
}
