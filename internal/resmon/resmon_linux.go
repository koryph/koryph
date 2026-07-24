// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build linux

package resmon

import (
	"context"
	"os"
	"strconv"
	"strings"
)

// userHZ is the kernel's clock-tick rate (sysconf(_SC_CLK_TCK)), used to convert
// the utime/stime jiffies in /proc/<pid>/stat to seconds. It has been 100 on
// every mainstream Linux configuration since the 2.6 series; reading it exactly
// would require cgo (sysconf), which this package avoids by convention. If a
// kernel is ever built with a different CONFIG_HZ, only the CPU-seconds scale
// shifts — memory and I/O are unaffected — and the eBPF backend (design doc)
// reads scheduling time directly, sidestepping USER_HZ entirely.
const userHZ = 100.0

// pageKB is the memory page size in kilobytes, resolved once at init.
var pageKB = int64(os.Getpagesize()) / 1024

// Snapshot builds a ProcTable from /proc. It reads every numeric /proc/<pid>
// entry's stat (ppid, CPU jiffies), statm (resident pages), and io (disk
// read/write bytes). Processes that vanish mid-scan are skipped. I/O is marked
// available when at least one process exposed a readable /proc/<pid>/io.
func Snapshot(ctx context.Context) (*ProcTable, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	procs := make([]procInfo, 0, len(entries))
	ioAvailable := false
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue // not a pid dir
		}
		statBytes, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // process exited between ReadDir and now
		}
		ppid, pgid, cpuSec, birthID, ok := parseStatCPUWithBirth(string(statBytes))
		if !ok {
			continue
		}
		pi := procInfo{pid: pid, ppid: ppid, pgid: pgid, birthID: birthID, cpuSec: cpuSec}

		if statmBytes, err := os.ReadFile("/proc/" + e.Name() + "/statm"); err == nil {
			pi.rssKB = parseStatmRSS(string(statmBytes), pageKB)
		}
		if ioBytes, err := os.ReadFile("/proc/" + e.Name() + "/io"); err == nil {
			if r, w, ok := parseProcIO(string(ioBytes)); ok {
				pi.ioReadKB, pi.ioWriteKB = r, w
				ioAvailable = true
			}
		}
		procs = append(procs, pi)
	}
	return newProcTable(procs, ioAvailable), nil
}

// parseStatCPU extracts the parent PID, process-group id, and cumulative CPU
// seconds from a /proc/<pid>/stat line. The comm field (field 2) is wrapped in
// parentheses and may itself contain spaces and parentheses, so fields are
// counted from AFTER the final ')' — the standard robust parse. Field layout
// after comm: state(3) ppid(4) pgrp(5) … utime(14) stime(15) cutime(16) cstime(17).
//
// CPU seconds is (utime+stime) — this process's own time — PLUS (cutime+cstime),
// the time of its WAITED-FOR (reaped) children and their descendants. Without
// cutime/cstime the sampler would lose the CPU of every short-lived subprocess
// that exits between poll ticks — which is most of an agent's work (the model
// CLI reaps a long sequence of git/ripgrep/bash tools). Because cutime accrues
// only from reaped children (never in the live process set) and each child is
// reaped by exactly one parent, summing (utime+stime+cutime+cstime) across the
// live cohort counts every reaped descendant exactly once, with no double-count.
func parseStatCPU(content string) (ppid, pgid int, cpuSec float64, ok bool) {
	ppid, pgid, cpuSec, _, ok = parseStatCPUWithBirth(content)
	return ppid, pgid, cpuSec, ok
}

// parseStatCPUWithBirth additionally returns the Linux /proc stat starttime
// (field 22) as a boot-relative clock-tick identity. A PID cannot be reused
// while retaining that start time, so this distinguishes the dispatched agent
// from a later unrelated process that inherited its numeric PID.
func parseStatCPUWithBirth(content string) (ppid, pgid int, cpuSec float64, birthID string, ok bool) {
	rparen := strings.LastIndexByte(content, ')')
	if rparen < 0 || rparen+2 >= len(content) {
		return 0, 0, 0, "", false
	}
	rest := strings.Fields(content[rparen+2:])
	// rest[1]=ppid(f4) rest[2]=pgrp(f5) rest[11..14]=utime,stime,cutime,cstime(f14..17)
	if len(rest) < 15 {
		return 0, 0, 0, "", false
	}
	ppid, err := strconv.Atoi(rest[1])
	if err != nil {
		return 0, 0, 0, "", false
	}
	pgid, err = strconv.Atoi(rest[2])
	if err != nil {
		return 0, 0, 0, "", false
	}
	utime, err1 := strconv.ParseInt(rest[11], 10, 64)
	stime, err2 := strconv.ParseInt(rest[12], 10, 64)
	cutime, err3 := strconv.ParseInt(rest[13], 10, 64)
	cstime, err4 := strconv.ParseInt(rest[14], 10, 64)
	if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
		return 0, 0, 0, "", false
	}
	// starttime is field 22, offset 19 after the comm field. Old or malformed
	// rows still supply resource metrics, but cannot authenticate a PID.
	if len(rest) < 20 {
		return ppid, pgid, float64(utime+stime+cutime+cstime) / userHZ, "", true
	}
	start, err := strconv.ParseUint(rest[19], 10, 64)
	if err != nil {
		return ppid, pgid, float64(utime+stime+cutime+cstime) / userHZ, "", true
	}
	return ppid, pgid, float64(utime+stime+cutime+cstime) / userHZ, "linux:" + strconv.FormatUint(start, 10), true
}

// parseStatmRSS returns the resident-set size in KB from a /proc/<pid>/statm
// line ("size resident shared text lib data dt"): field 2 is resident pages.
func parseStatmRSS(content string, pageSizeKB int64) int64 {
	f := strings.Fields(content)
	if len(f) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return 0
	}
	return pages * pageSizeKB
}

// parseProcIO extracts read_bytes/write_bytes (actual bytes fetched from / sent
// to the storage layer) from a /proc/<pid>/io block and returns them in KB.
// ok is false when neither counter was present.
func parseProcIO(content string) (readKB, writeKB int64, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		key, val, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSpace(key) {
		case "read_bytes":
			readKB = n / 1024
			ok = true
		case "write_bytes":
			writeKB = n / 1024
			ok = true
		}
	}
	return readKB, writeKB, ok
}
