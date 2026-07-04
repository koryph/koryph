// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/runtime"
)

// rateLimitMarkers are the substrings (matched case-insensitively) that
// identify a Claude API rate-limit or overload error. Verbatim port of
// internal/dispatch/cli.go's rateLimitMarkers.
var rateLimitMarkers = []string{"429", "rate_limit_error", "overloaded_error"}

// rawLine is the tolerant superset shape scanned out of a stream-json line —
// the union of internal/dispatch/cli.go's former resultLine and rlEvent
// structs, unmarshaled once per line instead of twice, so there is exactly
// one JSON shape describing "what a stream-json line might contain" instead
// of two independently-tolerant ones drifting apart.
type rawLine struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype,omitempty"`
	IsError *bool           `json:"is_error,omitempty"`
	Message json.RawMessage `json:"message,omitempty"`
	Result  string          `json:"result,omitempty"`
	// TotalCostUSD is a *float64 (rather than float64) so a "result" line
	// missing the field is distinguishable from one reporting exactly $0 —
	// see runtime.Event.HasCost's doc for why that distinction must survive.
	TotalCostUSD *float64 `json:"total_cost_usd"`
	SessionID    string   `json:"session_id,omitempty"`
	Error        *struct {
		Type    string `json:"type,omitempty"`
		Message string `json:"message,omitempty"`
	} `json:"error,omitempty"`
}

// messageText extracts rl.Message as plain text for the rate-limit haystack.
// Message is json.RawMessage (not string) because a real Claude
// "assistant"/"tool_use" event's own "message" field is a nested OBJECT
// ({"role":...,"content":[...]}) , not a string — internal/dispatch/cli.go's
// original rlEvent typed Message as a plain string, which meant those lines
// simply failed json.Unmarshal and were skip-continued (irrelevant there,
// since ParseRateLimited/ParseResultCost never needed assistant lines). This
// package's ParseEvents additionally promises an EventOpaque passthrough for
// exactly those lines (see the Runtime interface's Capabilities/EventKind
// doc), so a hard type mismatch on an unrelated field must not fail the
// whole line's classification. When Message is a JSON string (the shape
// every "error"/"result" event actually uses), this returns it verbatim,
// preserving the original haystack text byte-for-byte; anything else (an
// object, absent, or malformed) contributes no text — which is exactly what
// the original struct effectively contributed once such a line failed to
// parse: nothing.
func (rl rawLine) messageText() string {
	if len(rl.Message) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(rl.Message, &s) == nil {
		return s
	}
	return ""
}

// classify converts one raw, already-trimmed JSON line into a normalized
// Event. ok is false only when line does not parse as JSON at all — a
// malformed record is skipped (tolerant scanning, matching
// internal/dispatch/cli.go's pre-existing behavior of `continue`-ing past a
// json.Unmarshal error rather than surfacing it).
//
// Kind is chosen by the line's own "type" field — "result" wins EventResult
// regardless of is_error (ParseResultCost's documented behavior: "a result
// line with is_error==true still returns its cost ... we never filter on
// is_error"); everything else that looks error-flagged (a top-level "error"
// type, an embedded error object, or is_error true on any other type) is
// EventError; a session/init line carrying a session id is EventSession;
// anything else is EventOpaque.
//
// RateLimited is computed independently of Kind and set whenever the line is
// error-flagged AND its text matches a rateLimitMarkers substring — including
// on a Kind==EventResult line (a "result" with is_error true IS exactly that
// case; see the "result is_error true" fixtures in claude_test.go). This is a
// deliberate, narrow widening of runtime.Event's documented "RateLimited is
// valid only when Kind==EventError": both legacy functions this package
// replaces must be reconstructable from ONE classification pass over each
// line, and a "result" line can carry both a cost AND a rate-limit marker at
// once (Claude does emit is_error:true result lines). Restricting RateLimited
// to Kind==EventError would silently lose ParseRateLimited's signal on
// exactly that line shape. Consumers that care only about the documented
// contract can safely ignore RateLimited on non-EventError events; this
// package's own reducers (ParseResultCost/ParseRateLimited below) do not.
func classify(line []byte) (runtime.Event, bool) {
	var rl rawLine
	if err := json.Unmarshal(line, &rl); err != nil {
		return runtime.Event{}, false
	}

	raw := json.RawMessage(append([]byte(nil), line...))
	ev := runtime.Event{Raw: raw}

	errorish := rl.Type == "error" || rl.Error != nil || (rl.IsError != nil && *rl.IsError)
	if errorish {
		haystack := strings.ToLower(rl.Subtype + " " + rl.messageText() + " " + rl.Result)
		if rl.Error != nil {
			haystack += " " + strings.ToLower(rl.Error.Type+" "+rl.Error.Message)
		}
		for _, marker := range rateLimitMarkers {
			if strings.Contains(haystack, marker) {
				ev.RateLimited = true
				break
			}
		}
	}

	switch {
	case rl.Type == "result":
		ev.Kind = runtime.EventResult
		if rl.TotalCostUSD != nil {
			ev.CostUSD, ev.HasCost = *rl.TotalCostUSD, true
		}
	case errorish:
		ev.Kind = runtime.EventError
	case rl.SessionID != "":
		ev.Kind = runtime.EventSession
		ev.SessionID = rl.SessionID
	default:
		ev.Kind = runtime.EventOpaque
	}
	return ev, true
}

// eventStream is the runtime.EventStream ParseEvents returns: a lazy,
// line-at-a-time scan matching internal/dispatch/cli.go's former
// bufio.Scanner setup (same 64KB initial / 4MB max buffer, same "skip blank
// or non-'{'-prefixed lines" tolerance).
type eventStream struct {
	sc *bufio.Scanner
}

// Next implements runtime.EventStream.
func (es *eventStream) Next() (runtime.Event, bool, error) {
	for es.sc.Scan() {
		line := bytes.TrimSpace(es.sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		if ev, ok := classify(line); ok {
			return ev, true, nil
		}
	}
	if err := es.sc.Err(); err != nil {
		return runtime.Event{}, false, err
	}
	return runtime.Event{}, false, nil
}

// Close implements runtime.EventStream. eventStream holds no resources of
// its own (r's lifetime is the caller's), so Close is a no-op.
func (es *eventStream) Close() error { return nil }

// ParseEvents implements runtime.Runtime by wrapping the same tolerant
// line-scan internal/dispatch/cli.go's ParseResultCost/ParseRateLimited used
// to run independently (see classify's doc for how one pass now serves both).
func (c Claude) ParseEvents(r io.Reader) (runtime.EventStream, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &eventStream{sc: sc}, nil
}

// ParseResultCost scans r for the LAST EventResult, returning its CostUSD/
// HasCost — the reader-based, Runtime-generic equivalent of
// dispatch.ParseResultCost(path string), which now opens the file and
// delegates here (see internal/dispatch/cli.go). Matches ParseResultCost's
// documented last-wins semantics exactly: a later result line without a cost
// resets found to false, mirroring the original scan.
func ParseResultCost(r io.Reader) (float64, bool) {
	es, err := (Claude{}).ParseEvents(r)
	if err != nil {
		return 0, false
	}
	defer es.Close()
	var cost float64
	var found bool
	for {
		ev, ok, err := es.Next()
		if err != nil || !ok {
			break
		}
		if ev.Kind == runtime.EventResult {
			cost, found = ev.CostUSD, ev.HasCost
		}
	}
	return cost, found
}

// ParseRateLimited scans r for any event whose text matched a rate-limit/
// overload marker — the reader-based equivalent of
// dispatch.ParseRateLimited(path string), which now opens the file and
// delegates here. Matches ParseRateLimited's documented "any qualifying event
// anywhere in the stream" semantics (no last-wins).
func ParseRateLimited(r io.Reader) bool {
	es, err := (Claude{}).ParseEvents(r)
	if err != nil {
		return false
	}
	defer es.Close()
	for {
		ev, ok, err := es.Next()
		if err != nil || !ok {
			break
		}
		if ev.RateLimited {
			return true
		}
	}
	return false
}
