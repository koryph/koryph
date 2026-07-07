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

// budgetKillMarkers are the substrings (matched case-insensitively) that
// identify a death by the --max-budget-usd cap (koryph-77r.10, design
// docs/designs/2026-07-token-economy.md recovery-economics follow-up).
// Empirically pinned 2026-07 against a live `claude -p 'reply with the
// single word hi' --max-budget-usd 0.001 --output-format stream-json
// --verbose` canary run under subscription OAuth (apiKeySource "none" on
// the same run's system/init line — confirming the cap IS enforced under
// subscription auth, not just API-key billing, which is the gating fact
// this bead's whole feature depends on). The captured terminal line
// (fields reordered for readability; verbatim otherwise):
//
//	{"type":"result","subtype":"error_max_budget_usd","duration_ms":4876,
//	 "is_error":true,"num_turns":1,"stop_reason":"end_turn",
//	 "total_cost_usd":0.427796,"errors":["Reached maximum budget ($0.001)"]}
//
// Notably total_cost_usd (0.4278) is ~428x the $0.001 cap: enforcement is a
// turn-boundary check, not a mid-generation hard interrupt — the CLI lets
// the in-flight turn finish (here, one turn with heavy extended-thinking
// cache_creation) and then kills the session before the NEXT turn starts.
// "error_max_budget_usd" (the subtype) is the primary, stable marker;
// "reached maximum budget" (the human-readable errors[] text) is a
// secondary/defensive marker in case a future CLI version renames the
// subtype but keeps prose continuity — see rawLine.Errors for why that text
// reaches the classification haystack at all.
var budgetKillMarkers = []string{"error_max_budget_usd", "reached maximum budget"}

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
	// Errors is a "result" line's top-level "errors" array — the
	// human-readable string(s) a budget-kill line carries (koryph-77r.10,
	// e.g. ["Reached maximum budget ($0.001)"]) alongside its Subtype. Fed
	// into the classification haystack as a secondary/defensive signal (see
	// budgetKillMarkers' doc) so a future CLI version that keeps this prose
	// but renames the subtype does not silently stop classifying.
	Errors []string `json:"errors,omitempty"`
	// Usage is the result line's token composition (koryph-77r.1, design
	// docs/designs/2026-07-token-economy.md §3 L1) — see resultUsage's doc
	// for the confirmed real-CLI shape.
	Usage *resultUsage `json:"usage,omitempty"`
}

// resultUsage is the token composition on a stream-json "result" line's
// top-level "usage" object (koryph-77r.1). Confirmed against a live `claude
// -p ... --output-format stream-json` result line (2026-07): the SAME
// snake_case field vocabulary internal/quota/usage.go's usageTokens already
// parses off session transcript lines, so this reuses it rather than
// inventing a second one. A real result line also carries a "modelUsage"
// object (camelCase, keyed by model id, redundant with this one) that is
// deliberately not parsed here.
type resultUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
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
		if len(rl.Errors) > 0 {
			haystack += " " + strings.ToLower(strings.Join(rl.Errors, " "))
		}
		for _, marker := range rateLimitMarkers {
			if strings.Contains(haystack, marker) {
				ev.RateLimited = true
				break
			}
		}
		// BudgetKilled is computed independently of RateLimited (koryph-77r.10):
		// the two marker sets never overlap in practice, but keeping the loops
		// separate — rather than merging budgetKillMarkers into rateLimitMarkers
		// — keeps each signal's provenance obvious and lets the two evolve
		// independently (see runtime.Event.BudgetKilled's doc for why this is
		// NOT restricted to Kind==EventError the way RateLimited nominally is).
		for _, marker := range budgetKillMarkers {
			if strings.Contains(haystack, marker) {
				ev.BudgetKilled = true
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
		if rl.Usage != nil {
			ev.InputTokens = rl.Usage.InputTokens
			ev.OutputTokens = rl.Usage.OutputTokens
			ev.CacheReadTokens = rl.Usage.CacheReadInputTokens
			ev.CacheCreationTokens = rl.Usage.CacheCreationInputTokens
			ev.HasUsage = true
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

// TokenUsage is the per-attempt token composition parsed from a stream-json
// "result" line's usage block (koryph-77r.1, design
// docs/designs/2026-07-token-economy.md §3 L1): input, output, cache-read,
// and cache-creation token counts.
type TokenUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// ParseResultUsage scans r for the LAST EventResult, returning its token
// composition — the usage-block counterpart to ParseResultCost, sharing its
// exact last-wins semantics: a later result line with no usage block resets
// found to false, mirroring ParseResultCost's documented behavior for cost.
func ParseResultUsage(r io.Reader) (TokenUsage, bool) {
	es, err := (Claude{}).ParseEvents(r)
	if err != nil {
		return TokenUsage{}, false
	}
	defer es.Close()
	var usage TokenUsage
	var found bool
	for {
		ev, ok, err := es.Next()
		if err != nil || !ok {
			break
		}
		if ev.Kind == runtime.EventResult {
			if ev.HasUsage {
				usage = TokenUsage{
					InputTokens:         ev.InputTokens,
					OutputTokens:        ev.OutputTokens,
					CacheReadTokens:     ev.CacheReadTokens,
					CacheCreationTokens: ev.CacheCreationTokens,
				}
				found = true
			} else {
				usage, found = TokenUsage{}, false
			}
		}
	}
	return usage, found
}

// ParseCleanExit reports whether the LAST "result" line in r has is_error
// absent or false — that is, the agent completed its turn without a fatal
// error. Returns false when no result line is present (a process that crashed
// or was killed before writing its final JSON is NOT a clean exit) or when
// the reader is unreadable.
//
// Note: ParseCleanExit scans the raw bytes directly (bypassing classify) so
// it can read the is_error pointer independently of EventKind — classify maps
// a result+is_error:true line to EventResult (not EventError) to preserve
// ParseResultCost's documented "is_error is irrelevant to cost" behavior, so
// the normalized Event does not carry an IsError field.
func ParseCleanExit(r io.Reader) bool {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	found, clean := false, false
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rl rawLine
		if err := json.Unmarshal(line, &rl); err != nil {
			continue
		}
		if rl.Type == "result" {
			found = true
			// Clean iff is_error is absent or explicitly false.
			clean = rl.IsError == nil || !*rl.IsError
		}
	}
	return found && clean
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

// ParseBudgetKilled scans r for any event whose text matched a budget-kill
// marker (koryph-77r.10) — the reader-based equivalent of a future
// dispatch.ParseBudgetKilled(path string) wrapper, mirroring
// ParseRateLimited's "any qualifying event anywhere in the stream" semantics
// (no last-wins): a budget-kill always terminates the stream in practice, so
// in-order-vs-any distinction is moot, but matching ParseRateLimited's
// contract exactly keeps the two death-classification scans symmetric.
func ParseBudgetKilled(r io.Reader) bool {
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
		if ev.BudgetKilled {
			return true
		}
	}
	return false
}
