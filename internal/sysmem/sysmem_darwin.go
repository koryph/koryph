// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package sysmem

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// available reads total memory via sysctl (hw.memsize) and the reclaimable page
// classes via vm_stat. macOS has no cgo-free sysctl for free/inactive page
// counts — host_statistics64 is a Mach call x/sys/unix does not wrap — so we
// shell out to /usr/bin/vm_stat, which is present on every macOS install. The
// "available" estimate sums the page classes the kernel can hand to a new
// process WITHOUT swapping (see availablePages for why inactive is excluded).
func available() (Stat, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: sysctl hw.memsize: %w", err)
	}
	rawPressure, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: sysctl memory pressure: %w", err)
	}
	pressure, err := parseDarwinPressure(rawPressure)
	if err != nil {
		return Stat{}, err
	}
	pageSize, err := unix.SysctlUint64("hw.pagesize")
	if err != nil || pageSize == 0 {
		pageSize = 4096
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	swapOut, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", "vm.swapusage").Output()
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: sysctl vm.swapusage: %w", err)
	}
	swapUsed, err := parseSwapUsed(string(swapOut))
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: parse vm.swapusage: %w", err)
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: vm_stat: %w", err)
	}
	pages := parseVMStat(string(out))
	return Stat{
		TotalBytes:      total,
		AvailableBytes:  availablePages(pages) * pageSize,
		Pressure:        pressure,
		SwapUsedBytes:   swapUsed,
		CompressedBytes: pages["Pages occupied by compressor"] * pageSize,
		SampledAt:       time.Now().UTC(),
	}, nil
}

// parseDarwinPressure maps the memorystatus kernel bit values shared with
// DISPATCH_MEMORYPRESSURE_* into the portable admission bands. Unknown values
// are a probe failure, not "normal": an OS change must degrade admission
// safely until koryph understands it.
func parseDarwinPressure(raw uint32) (PressureBand, error) {
	switch raw {
	case 1:
		return PressureNormal, nil
	case 2:
		return PressureWarning, nil
	case 4:
		return PressureCritical, nil
	default:
		return PressureUnknown, fmt.Errorf("sysmem: unknown Darwin pressure level %d", raw)
	}
}

// parseSwapUsed extracts the "used = 12.34M" value from vm.swapusage.
func parseSwapUsed(raw string) (uint64, error) {
	fields := strings.Fields(raw)
	for i := 0; i+2 < len(fields); i++ {
		if fields[i] != "used" || fields[i+1] != "=" {
			continue
		}
		return parseByteQuantity(fields[i+2])
	}
	return 0, errors.New("missing used field")
}

func parseByteQuantity(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if len(raw) < 2 {
		return 0, fmt.Errorf("invalid byte quantity %q", raw)
	}
	unit := raw[len(raw)-1]
	var multiplier float64
	switch unit {
	case 'K', 'k':
		multiplier = 1024
	case 'M', 'm':
		multiplier = 1024 * 1024
	case 'G', 'g':
		multiplier = 1024 * 1024 * 1024
	case 'T', 't':
		multiplier = 1024 * 1024 * 1024 * 1024
	default:
		return 0, fmt.Errorf("unknown byte unit %q", string(unit))
	}
	n, err := strconv.ParseFloat(raw[:len(raw)-1], 64)
	if err != nil || n < 0 || math.IsNaN(n) || math.IsInf(n, 0) {
		return 0, fmt.Errorf("invalid byte quantity %q", raw)
	}
	bytes := n * multiplier
	if bytes > math.MaxUint64 {
		return 0, fmt.Errorf("byte quantity overflows uint64: %q", raw)
	}
	return uint64(bytes), nil
}

// availablePages sums the vm_stat page classes the kernel can promptly hand to a
// new process without swapping: free, speculative (read-ahead cache), and
// purgeable (volatile) pages.
//
// It deliberately EXCLUDES "Pages inactive" (koryph-3xs). On macOS the inactive
// list is NOT a proxy for reclaimable file cache the way it is on Linux: it
// holds dirty and compressor-backed pages that the kernel must write out (to
// swap or the compressor) before it can reuse them, so counting them as "free
// for the taking" over-reports headroom. During the 2026-07 OOM incident the
// old sum (which added inactive) admitted agents into a host that was already
// swapping. Excluding inactive is the conservative, cgo-free fix the bead calls
// for — it can only UNDER-report availability, which fails the admission gate
// safe (defer a dispatch), never OOM the machine.
func availablePages(pages map[string]uint64) uint64 {
	return pages["Pages free"] + pages["Pages speculative"] + pages["Pages purgeable"]
}

// parseVMStat maps each "Pages free:   6859." line to its page count. Values
// carry a trailing period, which is stripped. Unrecognized/malformed lines are
// skipped, so a missing class simply contributes zero (a conservative
// underestimate of availability).
func parseVMStat(out string) map[string]uint64 {
	pages := make(map[string]uint64)
	for _, line := range strings.Split(out, "\n") {
		key, rawVal, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		v := strings.TrimSpace(rawVal)
		v = strings.TrimSuffix(v, ".")
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			continue
		}
		pages[strings.TrimSpace(key)] = n
	}
	return pages
}
