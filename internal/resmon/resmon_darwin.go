// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package resmon

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
)

// Snapshot builds a ProcTable from a single `ps` sweep. macOS has no cgo-free
// unprivileged /proc equivalent, so — following sysmem_darwin's vm_stat
// convention — we shell out to ps, which is present on every install. rss is
// reported in KB and time as cumulative CPU. Disk I/O is not available through
// ps (it needs proc_pid_rusage/DTrace — the kernel-hook backend on the roadmap
// in docs/designs/2026-07-process-metrics.md), so IOAvailable is false here.
func Snapshot(ctx context.Context) (*ProcTable, error) {
	// pid, ppid, pgid, rss(KB), cumulative CPU time, and a stable process-start
	// identity — headerless via trailing '='. lstart is the strongest identity
	// ps exposes without privileged/cgo APIs; an empty/unparseable value makes
	// reattach and recovery fail closed rather than trusting PID alone.
	out, err := exec.CommandContext(ctx, "/bin/ps", "-axo", "pid=,ppid=,pgid=,rss=,time=,lstart=").Output()
	if err != nil {
		return nil, err
	}
	procs := parsePSTable(string(out))
	return newProcTable(procs, false), nil
}

// parsePSTable parses the whitespace-columned output of
// `ps -axo pid=,ppid=,pgid=,rss=,time=` into procInfo rows. Malformed lines are
// skipped. Kept separate from the exec call for unit testing.
func parsePSTable(out string) []procInfo {
	var procs []procInfo
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		ppid, err2 := strconv.Atoi(fields[1])
		pgid, err3 := strconv.Atoi(fields[2])
		rssKB, err4 := strconv.ParseInt(fields[3], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			continue
		}
		cpuSec, ok := parsePSTime(fields[4])
		if !ok {
			continue
		}
		birthID := ""
		// lstart is five fields: weekday month day time year. Preserve it as an
		// opaque platform identity so repeated snapshots compare byte-for-byte.
		if len(fields) >= 10 {
			birthID = "darwin:" + strings.Join(fields[5:10], " ")
		}
		procs = append(procs, procInfo{pid: pid, ppid: ppid, pgid: pgid, birthID: birthID, rssKB: rssKB, cpuSec: cpuSec})
	}
	return procs
}

// parsePSTime converts a ps cumulative-CPU time string to seconds. Accepted
// forms: "MM:SS.ss", "HH:MM:SS", and "DD-HH:MM:SS" (ps switches to the day form
// for very long-lived processes). ok is false on an unparseable value.
func parsePSTime(s string) (float64, bool) {
	days := 0.0
	if dash := strings.IndexByte(s, '-'); dash >= 0 {
		d, err := strconv.ParseFloat(s[:dash], 64)
		if err != nil {
			return 0, false
		}
		days = d
		s = s[dash+1:]
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	var total float64
	for _, p := range parts {
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return 0, false
		}
		total = total*60 + v
	}
	return days*86400 + total, true
}
