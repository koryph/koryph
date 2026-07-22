// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package procx holds small OS-process primitives shared across the recovery,
// governor, and health paths — factored out so the signal-0 liveness probe has
// exactly one implementation instead of the four byte-identical copies that had
// accreted in ledger, govern, doctor, and dispatch.
package procx

import (
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Alive reports whether pid is a live process, probed with signal 0 (POSIX
// kill(pid, 0) semantics): a nil error means the process exists and is
// signalable by us; EPERM means it exists but is owned by another user (still
// alive); ESRCH — or any other error — means it is gone. A non-positive pid is
// never alive.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}

// StartTime returns pid's approximate process start time, for callers that
// need to cross-check a liveness signal against a recorded dispatch time
// (koryph-1es) — kill(0) alone cannot tell "the process we dispatched is
// still running" from "an unrelated later process happens to reuse the same
// pid." It shells out to `ps -o etime=`, which both BSD (macOS) and GNU
// (Linux) ps implement, so this works cross-platform without cgo or
// /proc-only parsing. ok is false when ps is unavailable, the pid is not
// found, or the elapsed-time field cannot be parsed — callers must treat that
// as "unknown," not "suspicious." Precision is coarse: `ps etime` truncates to
// whole seconds and, for long-elapsed processes, to coarser units, so the
// returned time can drift by a similar margin — fine for a "started long
// after dispatch" cross-check, not for exact timing.
func StartTime(pid int) (time.Time, bool) {
	if pid <= 0 {
		return time.Time{}, false
	}
	out, err := exec.Command("ps", "-o", "etime=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return time.Time{}, false
	}
	elapsed, ok := parseEtime(strings.TrimSpace(string(out)))
	if !ok {
		return time.Time{}, false
	}
	return time.Now().Add(-elapsed), true
}

// parseEtime parses a `ps -o etime=` value, format [[DD-]HH:]MM:SS (the
// common form shared by BSD and GNU ps).
func parseEtime(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var days int
	if i := strings.IndexByte(s, '-'); i >= 0 {
		d, err := strconv.Atoi(s[:i])
		if err != nil {
			return 0, false
		}
		days = d
		s = s[i+1:]
	}
	parts := strings.Split(s, ":")
	var h, m, sec int
	var err error
	switch len(parts) {
	case 3:
		if h, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false
		}
		if m, err = strconv.Atoi(parts[1]); err != nil {
			return 0, false
		}
		if sec, err = strconv.Atoi(parts[2]); err != nil {
			return 0, false
		}
	case 2:
		if m, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false
		}
		if sec, err = strconv.Atoi(parts[1]); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	total := time.Duration(days)*24*time.Hour + time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
	return total, true
}
