// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package sysmem reports coarse system memory availability with no external
// dependencies and no cgo. It exists so the scheduler can refuse to admit
// another agent when the host is under memory pressure (koryph-930): each
// dispatched agent is a separate claude subprocess plus a git worktree, and a
// wide wave (adaptive concurrency can climb well past the static cap) can
// exhaust RAM and OOM the machine.
//
// AvailableBytes is a deliberately conservative "how much could a new process
// use right now" estimate, not an exact figure — on Linux it is
// /proc/meminfo's MemAvailable; on macOS it is the reclaimable page classes
// (free + inactive + speculative + purgeable) reported by vm_stat. Callers use
// it as a soft admission floor, never as a hard accounting number.
package sysmem

import "errors"

// ErrUnsupported is returned by Available on platforms without a memory probe.
// Callers MUST fail open (allow dispatch) on this error: the gate is a safety
// rail, not a correctness dependency.
var ErrUnsupported = errors.New("sysmem: unsupported platform")

// Stat is a point-in-time system memory reading.
type Stat struct {
	TotalBytes     uint64
	AvailableBytes uint64
}

// AvailableMB returns AvailableBytes rounded down to whole megabytes — the unit
// the governor's min_free_memory_mb floor is expressed in.
func (s Stat) AvailableMB() uint64 { return s.AvailableBytes / (1024 * 1024) }

// TotalMB returns TotalBytes rounded down to whole megabytes.
func (s Stat) TotalMB() uint64 { return s.TotalBytes / (1024 * 1024) }

// Available reads current system memory. It returns ErrUnsupported on a
// platform with no probe; every other error means the probe was attempted but
// failed (e.g. vm_stat missing) and the caller should likewise fail open.
func Available() (Stat, error) { return available() }
