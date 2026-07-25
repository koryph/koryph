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
// /proc/meminfo's MemAvailable; on macOS it is the PROMPTLY reclaimable page
// classes (free + speculative + purgeable) reported by vm_stat — inactive pages
// are excluded because on macOS they are not promptly reclaimable (koryph-3xs).
// Callers use it as a soft admission floor, never as a hard accounting number.
package sysmem

import (
	"errors"
	"math"
	"time"
)

// ErrUnsupported is returned by Available on platforms without a memory probe.
// Pressure-aware callers feed it to ResolvePressureSample, which uses a bounded
// last-good sample and then degrades admission to one active agent.
var ErrUnsupported = errors.New("sysmem: unsupported platform")

// Stat is a point-in-time system memory reading.
type Stat struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`

	// Pressure is the kernel's admission-oriented memory pressure band. Darwin
	// supplies all three bands from kern.memorystatus_vm_pressure_level.
	// Platforms without that signal leave it PressureUnknown.
	Pressure PressureBand `json:"pressure"`

	// SwapUsedBytes and CompressedBytes are point-in-time counters used with a
	// prior Stat to detect a host that is actively trading memory for swap or
	// compressor work even before it enters a worse pressure band.
	SwapUsedBytes   uint64 `json:"swap_used_bytes"`
	CompressedBytes uint64 `json:"compressed_bytes"`

	// SampledAt binds trend and last-good decisions to a bounded observation
	// time. A zero value is accepted from legacy probes and stamped by
	// ResolvePressureSample before the sample becomes trusted.
	SampledAt time.Time `json:"sampled_at"`
}

// AvailableMB returns AvailableBytes rounded down to whole megabytes — the unit
// the governor's min_free_memory_mb floor is expressed in.
func (s Stat) AvailableMB() uint64 { return s.AvailableBytes / (1024 * 1024) }

// TotalMB returns TotalBytes rounded down to whole megabytes.
func (s Stat) TotalMB() uint64 { return s.TotalBytes / (1024 * 1024) }

// PressureBand is the kernel memory-pressure state used for admission.
type PressureBand uint8

const (
	PressureUnknown PressureBand = iota
	PressureNormal
	PressureWarning
	PressureCritical
)

func (p PressureBand) String() string {
	switch p {
	case PressureNormal:
		return "normal"
	case PressureWarning:
		return "warning"
	case PressureCritical:
		return "critical"
	default:
		return "unknown"
	}
}

// Known reports whether the host supplied an admission-safe pressure band.
func (p PressureBand) Known() bool {
	return p == PressureNormal || p == PressureWarning || p == PressureCritical
}

// Trend is the signed change between two pressure samples. Positive values
// mean swap/compressor use grew; negative values mean it fell.
type Trend struct {
	SwapBytes       int64
	CompressedBytes int64
}

// TrendFrom compares s to older. A zero timestamp, a non-forward sample, or a
// missing counter produces a zero delta rather than inventing a trend.
func (s Stat) TrendFrom(older Stat) Trend {
	if s.SampledAt.IsZero() || older.SampledAt.IsZero() || !s.SampledAt.After(older.SampledAt) {
		return Trend{}
	}
	return Trend{
		SwapBytes:       signedDelta(s.SwapUsedBytes, older.SwapUsedBytes),
		CompressedBytes: signedDelta(s.CompressedBytes, older.CompressedBytes),
	}
}

func signedDelta(current, previous uint64) int64 {
	if current >= previous {
		delta := current - previous
		if delta > math.MaxInt64 {
			return math.MaxInt64
		}
		return int64(delta)
	}
	delta := previous - current
	if delta > math.MaxInt64 {
		return math.MinInt64
	}
	return -int64(delta)
}

// SampleOrigin explains whether admission is using a fresh kernel sample, a
// bounded last-good sample, or the fail-safe degraded posture.
type SampleOrigin string

const (
	SampleFresh    SampleOrigin = "fresh"
	SampleLastGood SampleOrigin = "last-good"
	SampleDegraded SampleOrigin = "degraded"
)

// DefaultLastGoodTTL is deliberately short: it bridges a transient sysctl or
// vm_stat failure without turning an old healthy reading into permission to
// keep dispatching through sustained probe failure.
const DefaultLastGoodTTL = 30 * time.Second

// PressureSample is the resolved input the governor consumes.
type PressureSample struct {
	Stat
	Origin     SampleOrigin `json:"origin"`
	ProbeError string       `json:"probe_error,omitempty"`
}

// ResolvePressureSample applies the fail-safe pressure-probe policy. A fresh
// known-band sample replaces lastGood. On failure, a known last-good sample is
// reused only inside ttl. Once it expires (or never existed), the result is
// degraded with no memory counters; the governor then admits at most one
// machine agent instead of failing open without limit.
//
// The returned *Stat is the last-good value callers should persist. It is a
// copy, so callers cannot accidentally mutate the supplied sample.
func ResolvePressureSample(
	now time.Time,
	fresh Stat,
	probeErr error,
	lastGood *Stat,
	ttl time.Duration,
) (PressureSample, *Stat) {
	now = now.UTC()
	if ttl <= 0 {
		ttl = DefaultLastGoodTTL
	}
	if probeErr == nil && fresh.Pressure.Known() {
		if fresh.SampledAt.IsZero() {
			fresh.SampledAt = now
		}
		fresh.SampledAt = fresh.SampledAt.UTC()
		saved := fresh
		return PressureSample{Stat: fresh, Origin: SampleFresh}, &saved
	}

	errText := ""
	if probeErr != nil {
		errText = probeErr.Error()
	} else {
		errText = "sysmem: pressure band unavailable"
	}
	if lastGood != nil && lastGood.Pressure.Known() && !lastGood.SampledAt.IsZero() {
		age := now.Sub(lastGood.SampledAt)
		if age >= 0 && age <= ttl {
			saved := *lastGood
			return PressureSample{
				Stat: saved, Origin: SampleLastGood, ProbeError: errText,
			}, &saved
		}
	}
	return PressureSample{
		Stat:       Stat{Pressure: PressureUnknown, SampledAt: now},
		Origin:     SampleDegraded,
		ProbeError: errText,
	}, lastGood
}

// Available reads current system memory. It returns ErrUnsupported on a
// platform with no probe; every other error means the probe was attempted but
// failed (e.g. vm_stat missing). Pressure-aware callers resolve either error
// through ResolvePressureSample rather than admitting without a bound.
func Available() (Stat, error) { return available() }

// Auto-floor sizing band (koryph-930): the default memory admission floor is a
// fraction of physical RAM, clamped so it is protective on small hosts without
// over-reserving on very large ones.
const (
	autoFloorFraction = 8    // 1/8 of physical memory ≈ 12.5%
	minAutoFloorMB    = 1024 // never reserve less than 1 GB
	maxAutoFloorMB    = 8192 // never reserve more than 8 GB, however large the host
)

// DefaultFloorMB is the memory admission floor to use when an operator has not
// configured an explicit one: a fraction of physical memory (sized to the host),
// clamped to [minAutoFloorMB, maxAutoFloorMB]. totalMB is the host's physical
// memory in megabytes (Stat.TotalMB). Returns 0 only when totalMB is 0 (no
// reading), which callers treat as "gate disabled / fail open".
func DefaultFloorMB(totalMB uint64) int {
	if totalMB == 0 {
		return 0
	}
	mb := int(totalMB / autoFloorFraction)
	if mb < minAutoFloorMB {
		mb = minAutoFloorMB
	}
	if mb > maxAutoFloorMB {
		mb = maxAutoFloorMB
	}
	return mb
}
