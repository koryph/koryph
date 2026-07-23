// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package execx

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/koryph/koryph/internal/obs"
)

// capturingHandler collects emitted slog records for assertion (mirrors
// internal/engine/requeue_obs_test.go's helper of the same name).
type capturingHandler struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (h *capturingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *capturingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.recs = append(h.recs, r.Clone())
	h.mu.Unlock()
	return nil
}
func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *capturingHandler) WithGroup(string) slog.Handler      { return h }

// TestRun_SlowCommandLogsWatchdog proves koryph-lwnq's "any known silent wait
// ... (subprocess call) logs what it is waiting on when it exceeds ~30s":
// shrink the threshold so the test doesn't actually wait 30s, run a command
// that outlives it, and assert exactly one "still waiting" line names the
// command.
func TestRun_SlowCommandLogsWatchdog(t *testing.T) {
	origThreshold := slowWarnThreshold
	slowWarnThreshold = 50 * time.Millisecond
	t.Cleanup(func() { slowWarnThreshold = origThreshold })

	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	origLog := log
	log = obs.For("execx")
	t.Cleanup(func() {
		log = origLog
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	_, err := Run(context.Background(), Cmd{Name: "sleep", Args: []string{"0.3"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	capH.mu.Lock()
	defer capH.mu.Unlock()
	n := 0
	for _, rec := range capH.recs {
		if strings.Contains(rec.Message, "still waiting on") && strings.Contains(rec.Message, "sleep") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 watchdog line for the slow command, got %d (recs=%v)", n, capH.recs)
	}
}

// TestRun_FastCommandNeverLogsWatchdog proves the common (fast) case never
// logs anything — the watchdog is a one-shot timer for genuinely slow
// commands only, not a per-call cost.
func TestRun_FastCommandNeverLogsWatchdog(t *testing.T) {
	origThreshold := slowWarnThreshold
	slowWarnThreshold = 2 * time.Second
	t.Cleanup(func() { slowWarnThreshold = origThreshold })

	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	origLog := log
	log = obs.For("execx")
	t.Cleanup(func() {
		log = origLog
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	_, err := Run(context.Background(), Cmd{Name: "true"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	capH.mu.Lock()
	defer capH.mu.Unlock()
	if len(capH.recs) != 0 {
		t.Fatalf("expected no watchdog lines for a fast command, got %d: %v", len(capH.recs), capH.recs)
	}
}
