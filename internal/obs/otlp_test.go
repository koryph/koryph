// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestOTLPHandlerCloseIdempotent verifies that calling Close twice does not
// panic (fix for the double-close finding).
func TestOTLPHandlerCloseIdempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	h := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	if err := h.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	// Second Close must not panic.
	if err := h.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestOTLPWithAttrsPreservesContext verifies that attributes set via WithAttrs
// appear in every log record sent to the collector (fix for the major finding
// where WithAttrs returned self and dropped all scoped attributes).
func TestOTLPWithAttrsPreservesContext(t *testing.T) {
	var received []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, rl := range payload.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, rec := range sl.LogRecords {
					m := map[string]any{"msg": rec.Body.StringValue}
					for _, kv := range rec.Attributes {
						m[kv.Key] = kv.Value.StringValue
					}
					received = append(received, m)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	defer base.Close() //nolint:errcheck

	// WithAttrs must attach component and run_id to every subsequent record.
	scoped := base.WithAttrs([]slog.Attr{
		slog.String("component", "engine"),
		slog.String("run_id", "test-run-1"),
	})

	ctx := context.Background()
	_ = scoped.Handle(ctx, func() slog.Record {
		r := slog.NewRecord(time.Now(), slog.LevelInfo, "heartbeat", 0)
		return r
	}())

	// Force a flush.
	if err := base.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Give the flush goroutine a moment (Close is synchronous, but the HTTP
	// round-trip may not have returned yet).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(received) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if len(received) == 0 {
		t.Fatal("no records received by collector")
	}
	rec := received[0]
	if rec["component"] != "engine" {
		t.Errorf("component = %v, want engine (WithAttrs not propagated)", rec["component"])
	}
	if rec["run_id"] != "test-run-1" {
		t.Errorf("run_id = %v, want test-run-1 (WithAttrs not propagated)", rec["run_id"])
	}
}

// TestOTLPWithAttrsMergesLayers verifies that chained WithAttrs calls merge
// attrs correctly rather than overwriting them.
func TestOTLPWithAttrsMergesLayers(t *testing.T) {
	var received []map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload otlpPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for _, rl := range payload.ResourceLogs {
			for _, sl := range rl.ScopeLogs {
				for _, rec := range sl.LogRecords {
					m := map[string]any{}
					for _, kv := range rec.Attributes {
						m[kv.Key] = kv.Value.StringValue
					}
					received = append(received, m)
				}
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	base := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	layer1 := base.WithAttrs([]slog.Attr{slog.String("layer", "one")})
	layer2 := layer1.WithAttrs([]slog.Attr{slog.String("extra", "two")})

	ctx := context.Background()
	_ = layer2.Handle(ctx, slog.NewRecord(time.Now(), slog.LevelInfo, "chained", 0))
	if err := base.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(received) == 0 {
		time.Sleep(10 * time.Millisecond)
	}

	if len(received) == 0 {
		t.Fatal("no records received")
	}
	rec := received[0]
	if rec["layer"] != "one" {
		t.Errorf("layer = %v, want one", rec["layer"])
	}
	if rec["extra"] != "two" {
		t.Errorf("extra = %v, want two", rec["extra"])
	}
}

// TestNormaliseEndpoint verifies endpoint normalisation.
func TestNormaliseEndpoint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"localhost:4318", "http://localhost:4318/v1/logs"},
		{"http://collector:4318", "http://collector:4318/v1/logs"},
		{"https://collector:4318", "https://collector:4318/v1/logs"},
		{"http://collector:4318/v1/logs", "http://collector:4318/v1/logs"},
		{"http://collector:4318/v1/logs/", "http://collector:4318/v1/logs"},
	}
	for _, c := range cases {
		got := normaliseEndpoint(c.in)
		if got != c.want {
			t.Errorf("normaliseEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
