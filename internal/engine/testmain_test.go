// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"fmt"
	"os"
	"testing"
)

// TestMain isolates the entire test binary from the real ~/.koryph from the
// moment it starts — before any package-level variable is evaluated.
//
// Without this, `var log = obs.For("engine")` in obs.go (evaluated at package
// init, before the first t.Setenv in newFixture) bootstraps the obs logger
// pipeline with a file sink pointing at ~/.koryph/telemetry/. On a machine
// with live koryph loops the real slots/.lock and telemetry file are contended,
// adding multi-second latency per dispatch and causing the drain/resize timing
// tests to report "tb1 never dispatched" even though the engine logic is
// correct.
//
// Setting KORYPH_HOME before any test function runs ensures that every
// code path — including the lazy bootstrap of the obs logger and any
// govern.NewStore / quota.LoadConfig call inside Run() — uses an isolated,
// empty directory. Each individual test further narrows the isolation to its
// own per-test temp dir via newFixture's t.Setenv("KORYPH_HOME", f.home).
// When that per-test override is cleaned up after the test, KORYPH_HOME falls
// back to this binary-wide isolated home, not to the real ~/.koryph.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "engine-test-koryph-home-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "engine TestMain: mkdirtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(home)
	if err := os.Setenv("KORYPH_HOME", home); err != nil {
		fmt.Fprintf(os.Stderr, "engine TestMain: setenv KORYPH_HOME: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
