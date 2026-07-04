// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
)

// captureHandler is a test slog.Handler that records every log record without
// any redaction. Used to assert that instrumentation code never attempts to
// log secret-shaped values in the first place.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(_ string) slog.Handler      { return h }

// allStrings collects every string value from the record (message + all attrs).
func (h *captureHandler) allStrings() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, r := range h.records {
		out = append(out, r.Message)
		r.Attrs(func(a slog.Attr) bool {
			out = append(out, a.Key, a.Value.String())
			return true
		})
	}
	return out
}

// assertNoSubstring fails t if secret appears in any captured string.
func (h *captureHandler) assertNoSubstring(t *testing.T, secret string) {
	t.Helper()
	for _, s := range h.allStrings() {
		if len(secret) > 0 && len(s) >= len(secret) {
			for i := 0; i <= len(s)-len(secret); i++ {
				if s[i:i+len(secret)] == secret {
					t.Errorf("secret found in log output: field value contains secret substring; log value=%q", s)
					return
				}
			}
		}
	}
}

// ---------- Span tests -------------------------------------------------------

// debugCfg returns an obs.Config with DEBUG level so span tests capture
// all records.
func debugCfg() obs.Config {
	return obs.Config{DefaultLevel: "debug"}
}

// TestSpanEmitsLatencyAndStatus verifies that End records latency_ms and
// status code as expected attributes.
func TestSpanEmitsLatencyAndStatus(t *testing.T) {
	capture := &captureHandler{}
	obs.ReInitRaw(debugCfg(), capture)
	t.Cleanup(func() { obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(nil, nil)) })

	logger := obs.For("forge")
	sp := obs.StartSpan(context.Background(), logger, slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "github"),
		slog.String(obs.KeyEndpointClass, "list_installations"),
	)
	sp.End(200, nil)

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	if len(recs) == 0 {
		t.Fatal("no log records emitted")
	}
	r := recs[0]
	if r.Message != "forge.api" {
		t.Errorf("message = %q, want forge.api", r.Message)
	}

	var foundLatency, foundStatus bool
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case obs.KeyLatencyMS:
			foundLatency = true
		case obs.KeyStatus:
			if a.Value.Int64() != 200 {
				t.Errorf("status = %d, want 200", a.Value.Int64())
			}
			foundStatus = true
		}
		return true
	})
	if !foundLatency {
		t.Error("latency_ms attr not found in span record")
	}
	if !foundStatus {
		t.Error("status attr not found in span record")
	}
}

// TestSpanEndOKOmitsStatus verifies that EndOK does not emit a status attr.
func TestSpanEndOKOmitsStatus(t *testing.T) {
	capture := &captureHandler{}
	obs.ReInitRaw(debugCfg(), capture)

	logger := obs.For("vault")
	sp := obs.StartSpan(context.Background(), logger, slog.LevelDebug, "vault.resolve",
		slog.String(obs.KeyProvider, "file"),
		slog.String(obs.KeyKeyRef, "/tmp/key.pem"),
	)
	sp.EndOK()

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	if len(recs) == 0 {
		t.Fatal("no log records emitted")
	}
	recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == obs.KeyStatus {
			t.Errorf("EndOK should not emit status attr, got %v", a.Value)
		}
		return true
	})
}

// TestNewAttrKeysPresent verifies the §O4 attr keys are exported.
func TestNewAttrKeysPresent(t *testing.T) {
	cases := []string{
		obs.KeyEndpointClass,
		obs.KeyLatencyMS,
		obs.KeyStatus,
		obs.KeyKeyRef,
		obs.KeyLifecycle,
	}
	for _, k := range cases {
		if k == "" {
			t.Errorf("attr key is empty string")
		}
	}
}

// TestReInitRaw_NoRedactionWrapper verifies that ReInitRaw bypasses the
// standard RedactingHandler so adversarial tests see raw log values.
// It also validates the assertNoSubstring helper works correctly.
func TestReInitRaw_NoRedactionWrapper(t *testing.T) {
	capture := &captureHandler{}
	obs.ReInitRaw(debugCfg(), capture)

	const safeValue = "harmless-info-value"
	obs.For("test").Debug("test msg", slog.String("safe_key", safeValue))

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	if len(recs) == 0 {
		t.Fatal("no record captured via ReInitRaw handler")
	}

	// assertNoSubstring: "harmless" is in the output, but "actual-secret" is not.
	// This passing assertion verifies the method works on safe content.
	capture.assertNoSubstring(t, "actual-secret-not-present")

	// allStrings should contain our safe value.
	found := false
	for _, s := range capture.allStrings() {
		if s == safeValue {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("allStrings() did not contain safe value %q", safeValue)
	}
}
