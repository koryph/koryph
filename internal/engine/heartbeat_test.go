// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/obs"
)

// withCapturedLog wires the package logger to a capturingHandler for the
// duration of the test (mirrors requeue_obs_test.go's pattern) and returns it.
func withCapturedLog(t *testing.T) *capturingHandler {
	t.Helper()
	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	orig := log
	log = obs.For("engine")
	t.Cleanup(func() {
		log = orig
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})
	return capH
}

// TestEmitHeartbeatReportsCountsAndLastAction proves the heartbeat line's
// content: counts from setCounts, and "last action <what> <ago>" from the
// most recent progress() call (koryph-lwnq).
func TestEmitHeartbeatReportsCountsAndLastAction(t *testing.T) {
	capH := withCapturedLog(t)

	// A console sink means progress() writes only to it, not through log.Info
	// too (D8) — otherwise this test would also capture progress()'s own
	// fallback record and conflate it with the heartbeat's.
	r := &runner{opts: Options{ProjectID: "proj", Out: &bytes.Buffer{}}}
	r.hb.setCounts(2, 5, 3)
	r.progress("dispatched koryph-abc")

	r.emitHeartbeat()

	capH.mu.Lock()
	defer capH.mu.Unlock()
	if len(capH.recs) != 1 {
		t.Fatalf("expected exactly 1 heartbeat record, got %d", len(capH.recs))
	}
	msg := capH.recs[0].Message
	for _, want := range []string{"engine alive:", "2 active", "5 ready", "wave 3", "last action", "dispatched koryph-abc", "ago"} {
		if !strings.Contains(msg, want) {
			t.Errorf("heartbeat message %q missing %q", msg, want)
		}
	}
}

// TestEmitHeartbeatNoActionYet proves the zero-value case (no progress() call
// has happened yet this run) reports something readable rather than a zero
// time or an empty dangling "ago".
func TestEmitHeartbeatNoActionYet(t *testing.T) {
	capH := withCapturedLog(t)

	r := &runner{opts: Options{ProjectID: "proj"}}
	r.emitHeartbeat()

	capH.mu.Lock()
	defer capH.mu.Unlock()
	if len(capH.recs) != 1 {
		t.Fatalf("expected exactly 1 heartbeat record, got %d", len(capH.recs))
	}
	msg := capH.recs[0].Message
	if !strings.Contains(msg, "engine alive: 0 active, 0 ready, wave 0") {
		t.Errorf("heartbeat message %q missing expected zero-state counts", msg)
	}
	if !strings.Contains(msg, "none yet this run") {
		t.Errorf("heartbeat message %q missing the no-action-yet marker", msg)
	}
}

// TestHeartbeatEmitsAtConfiguredCadence proves acceptance criterion (a): an
// interval heartbeat fires on its own schedule (koryph-lwnq), independent of
// any loop activity — the whole point is that it must keep ticking even if
// the loop goroutine is elsewhere blocked.
func TestHeartbeatEmitsAtConfiguredCadence(t *testing.T) {
	t.Setenv(envHeartbeatSec, "1")
	capH := withCapturedLog(t)

	r := &runner{opts: Options{ProjectID: "proj"}}

	ctx, cancel := context.WithTimeout(context.Background(), 2400*time.Millisecond)
	defer cancel()
	stop := r.startHeartbeat(ctx)
	defer stop()

	<-ctx.Done()
	// Give the ticker goroutine a moment to finish any in-flight log call
	// before we inspect capH under its own lock.
	time.Sleep(50 * time.Millisecond)

	capH.mu.Lock()
	defer capH.mu.Unlock()
	n := 0
	for _, rec := range capH.recs {
		if strings.HasPrefix(rec.Message, "engine alive:") {
			n++
		}
	}
	if n < 2 {
		t.Fatalf("expected at least 2 heartbeat lines in 2.4s at a 1s cadence, got %d (total recs=%d)", n, len(capH.recs))
	}
}

// TestHeartbeatStopFuncHaltsTicker proves the returned stop func actually
// stops the background goroutine rather than leaking it — calling stop()
// before the first tick must leave zero heartbeat lines behind.
func TestHeartbeatStopFuncHaltsTicker(t *testing.T) {
	t.Setenv(envHeartbeatSec, "1")
	capH := withCapturedLog(t)

	r := &runner{opts: Options{ProjectID: "proj"}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	stop := r.startHeartbeat(ctx)
	stop()

	time.Sleep(1300 * time.Millisecond)

	capH.mu.Lock()
	defer capH.mu.Unlock()
	if len(capH.recs) != 0 {
		t.Fatalf("expected no heartbeat lines after stop(), got %d: %v", len(capH.recs), capH.recs)
	}
}

// TestHeartbeatIntervalEnvOverride proves the KORYPH_HEARTBEAT_SEC override
// (tests only) and the documented default (koryph-lwnq: "default 60s").
func TestHeartbeatIntervalEnvOverride(t *testing.T) {
	if got := heartbeatInterval(); got != defaultHeartbeatSec*time.Second {
		t.Fatalf("default heartbeatInterval() = %s, want %ds", got, defaultHeartbeatSec)
	}
	t.Setenv(envHeartbeatSec, "5")
	if got := heartbeatInterval(); got != 5*time.Second {
		t.Fatalf("heartbeatInterval() with override = %s, want 5s", got)
	}
}
