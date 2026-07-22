// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
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

// TestProgressDoesNotDoubleEmitToSlog proves D8: with a console sink set,
// progress writes the human line only there — it does NOT also mirror the
// same string into the structured slog stream, which doubled every line once
// stdout and stderr were captured into one run log. The dedicated engine.*
// records remain the structured channel.
func TestProgressDoesNotDoubleEmitToSlog(t *testing.T) {
	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	orig := log
	log = obs.For("engine")
	t.Cleanup(func() {
		log = orig
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	var out bytes.Buffer
	r := &runner{opts: Options{Out: &out}}
	r.progress("hello %d", 7)

	if !strings.Contains(out.String(), "hello 7") {
		t.Errorf("console sink missing the progress line: %q", out.String())
	}
	capH.mu.Lock()
	defer capH.mu.Unlock()
	for _, rec := range capH.recs {
		if strings.Contains(rec.Message, "hello 7") {
			t.Fatalf("progress mirrored the human line into slog (D8 duplication): %q", rec.Message)
		}
	}
}

// TestProgressFallsBackToSlogWhenHeadless proves the headless path still emits:
// with no console sink, progress records the line via the structured logger so
// it is not silently dropped.
func TestProgressFallsBackToSlogWhenHeadless(t *testing.T) {
	capH := &capturingHandler{}
	obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, capH)
	orig := log
	log = obs.For("engine")
	t.Cleanup(func() {
		log = orig
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil))
	})

	r := &runner{opts: Options{Out: nil}}
	r.progress("headless %d", 9)

	capH.mu.Lock()
	defer capH.mu.Unlock()
	found := false
	for _, rec := range capH.recs {
		if strings.Contains(rec.Message, "headless 9") {
			found = true
		}
	}
	if !found {
		t.Error("headless progress was dropped; expected a slog fallback record")
	}
}

// TestProgressRedactsConsoleSink guards the fix for the audit finding that
// RedactRecord never scans the slog Message field while engine progress
// logging formats raw errors (wrapping git/gh/gate stderr) into Message —
// but that guard only covered the log.Info fallback (TestProgressFallsBack...
// above): the console sink (opts.Out) path bypassed the slog handler, and
// therefore RedactRecord, entirely. A secret-shaped string reaching
// r.progress must not appear verbatim on either sink.
func TestProgressRedactsConsoleSink(t *testing.T) {
	var out bytes.Buffer
	r := &runner{opts: Options{Out: &out}}
	secret := "ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789" // split: dodge gitleaks on a fake token
	r.progress("gate failed: %s", secret)

	if strings.Contains(out.String(), secret) {
		t.Errorf("console sink leaked an unredacted secret: %q", out.String())
	}
	if !strings.Contains(out.String(), obs.Redacted) {
		t.Errorf("console sink missing the redaction marker: %q", out.String())
	}
}

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
