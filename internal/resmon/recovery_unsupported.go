// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build !linux

package resmon

import (
	"context"
	"fmt"
	"runtime"
)

// StopChildless fails closed where the host offers no stable process handle
// that can both freeze and signal an authenticated process. Numeric-PID or
// process-group signals are unsafe here because either identifier may be
// recycled after a process-table snapshot.
func StopChildless(_ context.Context, _ int, _ string, _ func() error) (bool, error) {
	return false, fmt.Errorf("%w on %s", ErrStableProcessHandleUnavailable, runtime.GOOS)
}
