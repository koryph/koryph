// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package procx

import (
	"os"
	"testing"
)

func TestAlive(t *testing.T) {
	if !Alive(os.Getpid()) {
		t.Error("Alive(self) = false, want true")
	}
	if Alive(0) || Alive(-1) {
		t.Error("Alive(non-positive) = true, want false")
	}
	// A PID far above any pid_max is a dead process (ESRCH).
	if dead := 2000000000; Alive(dead) {
		t.Skipf("chosen dead pid %d is unexpectedly alive; skipping", dead)
	}
}
