// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
)

// capturingHandler collects emitted slog records for assertion. It routes as
// the obs root handler via obs.ReInitRaw; the registry layers above it pass
// records through unchanged (see internal/obs/span_test.go's captureHandler).
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

// TestLogRequeueEventCarriesRunAndProject pins koryph-x5d: engine.slot.
// requeue_event must carry run_id + project so per-project cost rollups include
// requeued-attempt spend. Before this, requeue cost records omitted both keys
// (unlike engine.slot.merged), hiding ~20-30% of true run cost from project-
// filtered accounting.
func TestLogRequeueEventCarriesRunAndProject(t *testing.T) {
	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	orig := log
	log = obs.For("engine")
	t.Cleanup(func() {
		log = orig
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	logRequeueEvent("run-xyz", "proj-abc", "bead-1", "merge conflict", 2, 3.5)

	capH.mu.Lock()
	defer capH.mu.Unlock()
	var rec *slog.Record
	for i := range capH.recs {
		if capH.recs[i].Message == "engine.slot.requeue_event" {
			rec = &capH.recs[i]
			break
		}
	}
	if rec == nil {
		t.Fatal("no engine.slot.requeue_event record emitted")
	}

	got := map[string]string{}
	rec.Attrs(func(a slog.Attr) bool {
		got[a.Key] = a.Value.String()
		return true
	})
	if got[obs.KeyRunID] != "run-xyz" {
		t.Errorf("run_id = %q, want %q", got[obs.KeyRunID], "run-xyz")
	}
	if got[obs.KeyProject] != "proj-abc" {
		t.Errorf("project = %q, want %q", got[obs.KeyProject], "proj-abc")
	}
	if got[obs.KeyBeadID] != "bead-1" {
		t.Errorf("bead_id = %q, want %q", got[obs.KeyBeadID], "bead-1")
	}
}
