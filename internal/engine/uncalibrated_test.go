// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/quota"
)

// TestGovernorGateUncalibrated covers koryph-grz: an uncalibrated governor (both
// ceilings 0) no longer passes silently. By default it warns loudly and still
// dispatches (advisory); with --require-calibration it hard-blocks.
func TestGovernorGateUncalibrated(t *testing.T) {
	t.Run("default: loud warning, still allows", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		var buf bytes.Buffer
		r.opts.Out = &buf
		r.quotaCfg = quota.DefaultConfig("personal") // uncalibrated: ceilings 0

		g := r.governorGate(context.Background())
		if !g.allowDispatch {
			t.Error("uncalibrated default must still allow dispatch (advisory), not block")
		}
		if g.uncalibratedBlock {
			t.Error("uncalibratedBlock must be false without --require-calibration")
		}
		if !strings.Contains(buf.String(), "UNCALIBRATED") {
			t.Errorf("expected a loud uncalibrated warning; got:\n%s", buf.String())
		}
	})

	t.Run("--require-calibration: blocks", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		var buf bytes.Buffer
		r.opts.Out = &buf
		r.opts.RequireCalibration = true
		r.quotaCfg = quota.DefaultConfig("personal")

		g := r.governorGate(context.Background())
		if g.allowDispatch {
			t.Error("--require-calibration + uncalibrated must block dispatch")
		}
		if !g.uncalibratedBlock {
			t.Error("uncalibratedBlock must be set so the pause reason is governor-uncalibrated, not quota-*")
		}
		if !strings.Contains(buf.String(), "refusing to dispatch") {
			t.Errorf("expected a block message; got:\n%s", buf.String())
		}
	})

	t.Run("warning fires once per run", func(t *testing.T) {
		f := newFixture(t, fixOpts{})
		r := runnerFromFixture(t, f)
		var buf bytes.Buffer
		r.opts.Out = &buf
		r.quotaCfg = quota.DefaultConfig("personal")

		r.governorGate(context.Background())
		r.governorGate(context.Background())
		if n := strings.Count(buf.String(), "UNCALIBRATED"); n != 1 {
			t.Errorf("uncalibrated warning fired %d times, want exactly 1 per run", n)
		}
	})
}
