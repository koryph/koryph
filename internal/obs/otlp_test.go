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

	h, err := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	if err != nil {
		t.Fatalf("NewOTLPHTTPHandler: %v", err)
	}
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

	base, err := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	if err != nil {
		t.Fatalf("NewOTLPHTTPHandler: %v", err)
	}
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

	base, err := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	if err != nil {
		t.Fatalf("NewOTLPHTTPHandler: %v", err)
	}
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

// TestNormaliseEndpoint verifies endpoint normalisation for the local/allowed
// cases: a bare localhost host defaults to http://, an explicit https:// for
// any host is honored, and /v1/logs is appended/deduplicated.
func TestNormaliseEndpoint(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"localhost:4318", "http://localhost:4318/v1/logs"},
		{"127.0.0.1:4318", "http://127.0.0.1:4318/v1/logs"},
		{"http://localhost:4318", "http://localhost:4318/v1/logs"},
		{"https://collector:4318", "https://collector:4318/v1/logs"},
		{"https://collector:4318/v1/logs", "https://collector:4318/v1/logs"},
		{"https://collector:4318/v1/logs/", "https://collector:4318/v1/logs"},
		// A bare (schemeless) non-localhost host defaults to https, not http
		// (koryph-5a1 #61) — the opposite of the pre-fix default.
		{"collector.internal:4318", "https://collector.internal:4318/v1/logs"},
	}
	for _, c := range cases {
		got, err := normaliseEndpoint(c.in)
		if err != nil {
			t.Errorf("normaliseEndpoint(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normaliseEndpoint(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestNormaliseEndpointRejectsCleartextForNonLocalHost is the koryph-5a1 #61
// acceptance test: an explicit http:// scheme for anything but localhost must
// be rejected, not silently honored — a collector endpoint is exactly the
// kind of config an operator copy-pastes without noticing the scheme.
func TestNormaliseEndpointRejectsCleartextForNonLocalHost(t *testing.T) {
	cases := []string{
		"http://collector:4318",
		"http://collector.internal:4318/v1/logs",
		"http://203.0.113.10:4318",
	}
	for _, in := range cases {
		if _, err := normaliseEndpoint(in); err == nil {
			t.Errorf("normaliseEndpoint(%q) = nil error, want a cleartext-rejection error", in)
		}
	}
}

// TestNewOTLPHTTPHandlerRejectsCleartext verifies the constructor itself
// refuses (rather than silently degrading) a cleartext endpoint for a
// non-localhost collector.
func TestNewOTLPHTTPHandlerRejectsCleartext(t *testing.T) {
	if _, err := NewOTLPHTTPHandler("http://collector.example.com:4318", slog.LevelInfo); err == nil {
		t.Fatal("NewOTLPHTTPHandler with cleartext http:// for a remote host: want an error, got nil")
	}
}

// TestOTLPFlushCountsDroppedOnNon2xx is the koryph-5a1 #60 acceptance test:
// before this, flush() never even inspected the response status code, so a
// collector returning e.g. 500 was indistinguishable from a clean export —
// no counter, no WARN, nothing.
func TestOTLPFlushCountsDroppedOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	h, err := NewOTLPHTTPHandler(srv.URL, slog.LevelInfo)
	if err != nil {
		t.Fatalf("NewOTLPHTTPHandler: %v", err)
	}

	before := OTLPDroppedRecords()
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "will be dropped", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := OTLPDroppedRecords()
	if after != before+1 {
		t.Errorf("OTLPDroppedRecords() = %d, want %d (before %d + the 1 record dropped on HTTP 500)", after, before+1, before)
	}
}

// TestOTLPFlushCountsDroppedOnTransportFailure verifies the counter also
// tracks records lost to a transport failure (collector unreachable), the
// other silent-drop path flush() previously had.
func TestOTLPFlushCountsDroppedOnTransportFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	unreachable := srv.URL
	srv.Close() // close immediately so the endpoint is now unreachable

	h, err := NewOTLPHTTPHandler(unreachable, slog.LevelInfo)
	if err != nil {
		t.Fatalf("NewOTLPHTTPHandler: %v", err)
	}

	before := OTLPDroppedRecords()
	if err := h.Handle(context.Background(), slog.NewRecord(time.Now(), slog.LevelInfo, "will be dropped", 0)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	after := OTLPDroppedRecords()
	if after != before+1 {
		t.Errorf("OTLPDroppedRecords() = %d, want %d (before %d + the 1 record dropped on transport failure)", after, before+1, before)
	}
}
