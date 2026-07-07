// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package sysmem

import (
	"context"
	"fmt"
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
// process without swapping: free, inactive (reclaimable file cache),
// speculative, and purgeable.
func available() (Stat, error) {
	total, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: sysctl hw.memsize: %w", err)
	}
	pageSize, err := unix.SysctlUint64("hw.pagesize")
	if err != nil || pageSize == 0 {
		pageSize = 4096
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: vm_stat: %w", err)
	}
	pages := parseVMStat(string(out))
	availPages := pages["Pages free"] + pages["Pages inactive"] +
		pages["Pages speculative"] + pages["Pages purgeable"]
	return Stat{TotalBytes: total, AvailableBytes: availPages * pageSize}, nil
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
