// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !linux && !darwin

package resmon

import (
	"context"
	"fmt"
	"runtime"
)

// Snapshot is unavailable on platforms other than Linux and macOS. koryph runs
// its agents on those two; a resource sampler for another OS would add its own
// build-tagged backend (see docs/designs/2026-07-process-metrics.md).
func Snapshot(_ context.Context) (*ProcTable, error) {
	return nil, fmt.Errorf("resmon: process sampling unsupported on %s", runtime.GOOS)
}
