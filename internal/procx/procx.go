// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package procx holds small OS-process primitives shared across the recovery,
// governor, and health paths — factored out so the signal-0 liveness probe has
// exactly one implementation instead of the four byte-identical copies that had
// accreted in ledger, govern, doctor, and dispatch.
package procx

import (
	"errors"
	"syscall"
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
