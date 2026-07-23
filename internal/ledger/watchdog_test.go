// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package ledger

import (
	"context"
	"log/slog"
	"os"
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

// TestAcquireReclaimGuard_SlowWaitLogsWatchdog proves koryph-lwnq's "any known
// silent wait ... (lock acquisition) logs what it is waiting on when it
// exceeds ~30s": shrink the threshold, hold the guard in one goroutine long
// enough to cross it, and assert the waiter logs exactly one "still waiting"
// line naming the guard path before it acquires.
func TestAcquireReclaimGuard_SlowWaitLogsWatchdog(t *testing.T) {
	origThreshold := lockWaitWarnThreshold
	lockWaitWarnThreshold = 50 * time.Millisecond
	t.Cleanup(func() { lockWaitWarnThreshold = origThreshold })

	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	origLog := log
	log = obs.For("ledger")
	t.Cleanup(func() {
		log = origLog
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	holder, err := acquireReclaimGuard(root)
	if err != nil {
		t.Fatalf("acquireReclaimGuard (holder): %v", err)
	}
	release := make(chan struct{})
	go func() {
		<-release
		holder.release()
	}()

	done := make(chan struct{})
	go func() {
		defer close(done)
		waiter, err := acquireReclaimGuard(root)
		if err != nil {
			t.Errorf("acquireReclaimGuard (waiter): %v", err)
			return
		}
		waiter.release()
	}()

	// Hold well past the shrunk threshold, then release so the waiter unblocks.
	time.Sleep(250 * time.Millisecond)
	close(release)
	<-done

	capH.mu.Lock()
	defer capH.mu.Unlock()
	n := 0
	for _, rec := range capH.recs {
		if strings.Contains(rec.Message, "still waiting on lock guard") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("expected exactly 1 watchdog line, got %d (recs=%v)", n, capH.recs)
	}
}
