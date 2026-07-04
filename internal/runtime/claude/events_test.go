// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// The fixtures below are copied verbatim from
// internal/dispatch/cli_test.go's TestParseResultCost/TestParseRateLimited so
// this package's reader-based ParseResultCost/ParseRateLimited are proven
// against the SAME inputs the pre-koryph-v8u.2 path-based functions were
// tested against — the golden data for the parsing half of the extraction.

func TestParseResultCostLastLineWins(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"result","total_cost_usd":0.5,"is_error":false}`,
		`not json at all`,
		`{"type":"assistant","message":{}}`,
		`{"type":"result","total_cost_usd":2.5,"is_error":true}`,
	}, "\n") + "\n"

	cost, ok := ParseResultCost(strings.NewReader(lines))
	if !ok || cost != 2.5 {
		t.Errorf("ParseResultCost = %v, %v; want 2.5, true", cost, ok)
	}
}

func TestParseResultCostNoResultLine(t *testing.T) {
	cost, ok := ParseResultCost(strings.NewReader(`{"type":"system"}` + "\n"))
	if ok || cost != 0 {
		t.Errorf("ParseResultCost = %v, %v; want 0, false", cost, ok)
	}
}

func TestParseRateLimitedFixtures(t *testing.T) {
	positive := map[string]string{
		"top-level error event, rate_limit_error":               `{"type":"error","error":{"type":"rate_limit_error","message":"Number of request tokens has exceeded your rate limit."}}`,
		"top-level error event, 429 in message":                 `{"type":"error","message":"HTTP 429 Too Many Requests"}`,
		"result is_error true, embedded error object":           `{"type":"result","is_error":true,"subtype":"error_during_execution","result":"API Error: 429 {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}"}`,
		"result is_error true, overloaded_error in result text": `{"type":"result","is_error":true,"subtype":"error","result":"overloaded_error: the service is temporarily overloaded"}`,
	}
	for name, line := range positive {
		t.Run("positive/"+name, func(t *testing.T) {
			body := strings.Join([]string{`{"type":"system","subtype":"init"}`, line}, "\n") + "\n"
			if !ParseRateLimited(strings.NewReader(body)) {
				t.Errorf("ParseRateLimited(%q) = false, want true", line)
			}
		})
	}

	negative := map[string][]string{
		"ordinary max-turns error": {
			`{"type":"result","is_error":true,"subtype":"error_max_turns","result":"Max turns reached"}`,
		},
		"clean success result": {
			`{"type":"result","total_cost_usd":1.23,"is_error":false}`,
		},
		"429 mentioned in ordinary (non-error) assistant text": {
			`{"type":"assistant","message":{"content":[{"type":"text","text":"the API returned 429 once but retried fine"}]}}`,
			`{"type":"result","total_cost_usd":0.10,"is_error":false}`,
		},
		"garbage lines and no result": {
			`not json at all`,
			`{"type":"system"}`,
		},
	}
	for name, lines := range negative {
		t.Run("negative/"+name, func(t *testing.T) {
			body := strings.Join(lines, "\n") + "\n"
			if ParseRateLimited(strings.NewReader(body)) {
				t.Errorf("ParseRateLimited() = true for %v, want false", lines)
			}
		})
	}
}

// TestResultLineWithRateLimitMarkerReportsBoth proves a single "result"
// line that is both cost-bearing-shaped (type result) and rate-limit-flagged
// (is_error true + marker) is classified as EventResult (so ParseResultCost
// can still see it) while still tripping RateLimited (so ParseRateLimited
// still sees it) — see classify's doc for why this is a deliberate widening
// of the "RateLimited valid only on EventError" convention.
func TestResultLineWithRateLimitMarkerReportsBoth(t *testing.T) {
	line := `{"type":"result","is_error":true,"result":"API Error: 429 rate_limit_error"}`
	ev, ok := classify([]byte(line))
	if !ok {
		t.Fatal("classify: not ok")
	}
	if ev.Kind != runtime.EventResult {
		t.Errorf("Kind = %v, want EventResult", ev.Kind)
	}
	if !ev.RateLimited {
		t.Error("RateLimited = false, want true")
	}
	if ev.HasCost {
		t.Error("HasCost = true for a line with no total_cost_usd field")
	}
}

func TestClassifySkipsMalformedJSON(t *testing.T) {
	if _, ok := classify([]byte("not json")); ok {
		t.Error("classify(malformed) ok = true, want false")
	}
}

func TestParseEventsOpaquePassthrough(t *testing.T) {
	// A real assistant event's "message" is an OBJECT, not a string — this
	// proves classify tolerates that shape (see rawLine.messageText's doc)
	// instead of failing the whole line's unmarshal.
	es, err := (Claude{}).ParseEvents(strings.NewReader(`{"type":"assistant","message":{"role":"assistant","content":[]}}` + "\n"))
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer es.Close()
	ev, ok, err := es.Next()
	if err != nil || !ok {
		t.Fatalf("Next() = %v, %v, %v", ev, ok, err)
	}
	if ev.Kind != runtime.EventOpaque {
		t.Errorf("Kind = %v, want EventOpaque", ev.Kind)
	}
	if _, ok, err := es.Next(); ok || err != nil {
		t.Errorf("second Next() = ok=%v err=%v, want ok=false err=nil", ok, err)
	}
}
