// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build linux

package sysmem

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strconv"
)

// available reads /proc/meminfo. MemAvailable (present on every kernel since
// 3.14) is the kernel's own estimate of memory obtainable for a new workload
// without swapping — exactly the signal we want, so we do not have to
// approximate it from free+cached ourselves.
func available() (Stat, error) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return Stat{}, fmt.Errorf("sysmem: read /proc/meminfo: %w", err)
	}
	var totalKB, availKB uint64
	var haveTotal, haveAvail bool
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		key, valKB, ok := parseMeminfoLine(sc.Bytes())
		if !ok {
			continue
		}
		switch key {
		case "MemTotal":
			totalKB, haveTotal = valKB, true
		case "MemAvailable":
			availKB, haveAvail = valKB, true
		}
		if haveTotal && haveAvail {
			break
		}
	}
	if !haveAvail {
		return Stat{}, fmt.Errorf("sysmem: /proc/meminfo missing MemAvailable")
	}
	return Stat{TotalBytes: totalKB * 1024, AvailableBytes: availKB * 1024}, nil
}

// parseMeminfoLine parses a "MemAvailable:   12345 kB" line into its key and
// value in kB. ok is false for lines that do not match the expected shape.
func parseMeminfoLine(line []byte) (key string, valKB uint64, ok bool) {
	fields := bytes.Fields(line)
	if len(fields) < 2 {
		return "", 0, false
	}
	k := string(bytes.TrimSuffix(fields[0], []byte(":")))
	v, err := strconv.ParseUint(string(fields[1]), 10, 64)
	if err != nil {
		return "", 0, false
	}
	return k, v, true
}
