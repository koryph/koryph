// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

// OTLPHTTPHandler exports log records to an OTLP/HTTP JSON endpoint
// (default port 4318, path /v1/logs).  Records are batched in memory and
// flushed periodically (every 5 s) or when the batch reaches 100 records.
// Close must be called to flush the final batch and stop the flush goroutine;
// calling Close more than once is safe (subsequent calls are no-ops).
//
// When otel_endpoint does not include a scheme it is assumed to be HTTP.
// Use an https:// scheme for non-localhost collectors; plain http:// sends
// telemetry in cleartext.
// The path /v1/logs is always appended unless the endpoint already ends with
// /v1/logs.
//
// Record format is OTLP/JSON Logs as defined by the OpenTelemetry spec.
type OTLPHTTPHandler struct {
	endpoint string
	client   *http.Client
	min      slog.Level

	mu    sync.Mutex
	batch []otlpLogRecord

	flushC chan struct{}
	done   chan struct{}
	once   sync.Once // guards close(done)
}

// otlpLogRecord is the OTLP LogRecord wire type (JSON subset used here).
type otlpLogRecord struct {
	TimeUnixNano   string       `json:"timeUnixNano"`
	SeverityNumber int          `json:"severityNumber"`
	SeverityText   string       `json:"severityText"`
	Body           otlpAnyValue `json:"body"`
	Attributes     []otlpKV     `json:"attributes,omitempty"`
}

type otlpAnyValue struct {
	StringValue string `json:"stringValue"`
}

type otlpKV struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpPayload is the root OTLP/HTTP Logs JSON envelope.
type otlpPayload struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpScope struct {
	Name string `json:"name"`
}

// severityNumber maps slog.Level to OTLP severity numbers (roughly aligned
// with OpenTelemetry spec Table 7).
func severityNumber(level slog.Level) int {
	switch {
	case level >= slog.LevelError:
		return 17 // SEVERITY_NUMBER_ERROR
	case level >= slog.LevelWarn:
		return 13 // SEVERITY_NUMBER_WARN
	case level >= slog.LevelInfo:
		return 9 // SEVERITY_NUMBER_INFO
	case level >= slog.LevelDebug:
		return 5 // SEVERITY_NUMBER_DEBUG
	default:
		return 1 // SEVERITY_NUMBER_TRACE
	}
}

// normaliseEndpoint ensures the endpoint has an HTTP scheme and ends with
// /v1/logs.  A bare host (no scheme) is treated as http://; use an explicit
// https:// scheme for any non-localhost collector to avoid cleartext export.
func normaliseEndpoint(ep string) string {
	if !strings.HasPrefix(ep, "http://") && !strings.HasPrefix(ep, "https://") {
		ep = "http://" + ep
	}
	// Strip trailing slash for consistent appending.
	ep = strings.TrimRight(ep, "/")
	if !strings.HasSuffix(ep, "/v1/logs") {
		ep = ep + "/v1/logs"
	}
	return ep
}

// NewOTLPHTTPHandler creates and starts an OTLPHTTPHandler.
// endpoint is the OTLP/HTTP base URL (e.g. "localhost:4318" or
// "http://collector.internal:4318").
// Close must be called when the handler is no longer needed.
func NewOTLPHTTPHandler(endpoint string, min slog.Level) *OTLPHTTPHandler {
	h := &OTLPHTTPHandler{
		endpoint: normaliseEndpoint(endpoint),
		client:   &http.Client{Timeout: 10 * time.Second},
		min:      min,
		batch:    make([]otlpLogRecord, 0, 100),
		flushC:   make(chan struct{}, 1),
		done:     make(chan struct{}),
	}
	go h.flushLoop()
	return h
}

func (h *OTLPHTTPHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.min
}

func (h *OTLPHTTPHandler) Handle(_ context.Context, r slog.Record) error {
	rec := otlpLogRecord{
		TimeUnixNano:   fmt.Sprintf("%d", r.Time.UnixNano()),
		SeverityNumber: severityNumber(r.Level),
		SeverityText:   LevelString(r.Level),
		Body:           otlpAnyValue{StringValue: r.Message},
	}
	r.Attrs(func(a slog.Attr) bool {
		rec.Attributes = append(rec.Attributes, otlpKV{
			Key:   a.Key,
			Value: otlpAnyValue{StringValue: fmt.Sprintf("%v", a.Value.Any())},
		})
		return true
	})

	h.mu.Lock()
	h.batch = append(h.batch, rec)
	full := len(h.batch) >= 100
	h.mu.Unlock()

	if full {
		select {
		case h.flushC <- struct{}{}:
		default:
		}
	}
	return nil
}

// WithAttrs returns a new handler that prepends attrs to every log record.
// The returned handler shares the same batch queue and background goroutine
// as h, so Close must still be called on the original OTLPHTTPHandler.
func (h *OTLPHTTPHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cp := make([]slog.Attr, len(attrs))
	copy(cp, attrs)
	return &otlpWithAttrs{OTLPHTTPHandler: h, attrs: cp}
}

func (h *OTLPHTTPHandler) WithGroup(_ string) slog.Handler { return h }

// Close flushes any pending batch and stops the background goroutine.
// Safe to call more than once; subsequent calls are no-ops.
func (h *OTLPHTTPHandler) Close() error {
	h.once.Do(func() { close(h.done) })
	h.flush()
	return nil
}

// otlpWithAttrs is returned by WithAttrs.  It wraps OTLPHTTPHandler and
// prepends the scoped attrs to every Handle call, preserving correlation
// context (component, run_id, etc.) in every exported record.
type otlpWithAttrs struct {
	*OTLPHTTPHandler
	attrs []slog.Attr
}

func (w *otlpWithAttrs) Enabled(ctx context.Context, level slog.Level) bool {
	return w.OTLPHTTPHandler.Enabled(ctx, level)
}

// Handle prepends the scoped attrs then delegates to the shared handler.
func (w *otlpWithAttrs) Handle(ctx context.Context, r slog.Record) error {
	nr := slog.NewRecord(r.Time, r.Level, r.Message, r.PC)
	nr.AddAttrs(w.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		nr.AddAttrs(a)
		return true
	})
	return w.OTLPHTTPHandler.Handle(ctx, nr)
}

func (w *otlpWithAttrs) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, len(w.attrs)+len(attrs))
	copy(merged, w.attrs)
	copy(merged[len(w.attrs):], attrs)
	return &otlpWithAttrs{OTLPHTTPHandler: w.OTLPHTTPHandler, attrs: merged}
}

func (w *otlpWithAttrs) WithGroup(_ string) slog.Handler { return w }

func (h *OTLPHTTPHandler) flushLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-h.done:
			return
		case <-h.flushC:
			h.flush()
		case <-ticker.C:
			h.flush()
		}
	}
}

func (h *OTLPHTTPHandler) flush() {
	h.mu.Lock()
	if len(h.batch) == 0 {
		h.mu.Unlock()
		return
	}
	records := h.batch
	h.batch = make([]otlpLogRecord, 0, 100)
	h.mu.Unlock()

	payload := otlpPayload{
		ResourceLogs: []otlpResourceLogs{{
			Resource: otlpResource{
				Attributes: []otlpKV{{
					Key:   "service.name",
					Value: otlpAnyValue{StringValue: "koryph"},
				}},
			},
			ScopeLogs: []otlpScopeLogs{{
				Scope:      otlpScope{Name: "koryph"},
				LogRecords: records,
			}},
		}},
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return // drop if marshalling fails
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.endpoint, bytes.NewReader(data))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return // best-effort; log loss is acceptable
	}
	_ = resp.Body.Close()
}
