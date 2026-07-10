// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime

import "encoding/json"

// EventKind coarsely classifies a normalized Event so engine logic (cost
// extraction, rate-limit classification, session-id capture) can switch on a
// small closed set instead of re-parsing each runtime's native event shapes.
// It mirrors exactly the three signals internal/dispatch/cli.go extracts
// from a Claude stream-json transcript today (ParseResultCost,
// ParseRateLimited, session id), plus one passthrough bucket for everything
// else — see the package doc's "Normalized event envelope" section.
type EventKind string

const (
	// EventResult is a terminal turn result. HasCost/CostUSD carry the
	// normalized equivalent of a Claude stream-json "result" line's
	// total_cost_usd (dispatch.ParseResultCost) — the LAST such event in a
	// stream is authoritative, matching ParseResultCost's documented
	// last-wins semantics.
	EventResult EventKind = "result"
	// EventError is an error-flagged event (a top-level error event, a
	// result with is_error true, or an embedded error object in the source
	// format). RateLimited carries the normalized equivalent of
	// dispatch.ParseRateLimited: whether this specific error event's text
	// matched a rate-limit/overload marker (429, rate_limit_error,
	// overloaded_error, or a runtime-specific equivalent). Unlike
	// EventResult, ANY EventError with RateLimited==true anywhere in the
	// stream marks the whole run rate-limited — there is no "last one wins"
	// here, mirroring ParseRateLimited's documented behavior.
	EventError EventKind = "error"
	// EventSession carries the runtime's session/conversation identifier
	// (e.g. a Claude "system"/"init" event, or whatever a given runtime
	// surfaces its resumable session id through). SessionID is populated.
	EventSession EventKind = "session"
	// EventOpaque is everything ParseEvents does not need to interpret
	// today: assistant text, tool_use/tool_result events, and any other
	// native event shape. Only Raw is populated — callers that want more
	// than the engine currently consumes read Raw themselves. This bucket
	// is deliberately the default/fallback so a Runtime implementation is
	// never forced to understand its own full event vocabulary on day one.
	EventOpaque EventKind = "opaque"
)

// Event is the normalized envelope every Runtime.ParseEvents implementation
// produces from its native stream format. It is intentionally minimal —
// covering only the signals the engine actually consumes today, per the
// package doc — rather than a full transcript/message model; Raw retains the
// original line so a future consumer can add fields without another
// interface change. Event is a plain value type (JSON-tagged so it can be
// logged, persisted, or round-tripped in tests) with no behavior of its own.
type Event struct {
	// Kind selects which of the fields below are meaningful; see EventKind.
	Kind EventKind `json:"kind"`

	// CostUSD is valid only when Kind==EventResult && HasCost. HasCost
	// exists (rather than a *float64 or a cost==0 sentinel) because
	// dispatch.ParseResultCost explicitly documents that a result line
	// missing total_cost_usd is a distinct case from a result costing
	// exactly $0 — both must round-trip through this envelope without
	// collapsing into each other.
	CostUSD float64 `json:"cost_usd,omitempty"`
	HasCost bool    `json:"has_cost,omitempty"`

	// InputTokens/OutputTokens/CacheReadTokens/CacheCreationTokens are valid
	// only when Kind==EventResult && HasUsage — the per-attempt token
	// composition off a Claude stream-json "result" line's usage block
	// (koryph-77r.1, design docs/designs/2026-07-token-economy.md §3 L1).
	// HasUsage exists for the same reason HasCost does: a result line with no
	// usage block must round-trip as "unknown", not silently collapse into an
	// all-zero reading indistinguishable from a genuinely token-free turn.
	InputTokens         int64 `json:"input_tokens,omitempty"`
	OutputTokens        int64 `json:"output_tokens,omitempty"`
	CacheReadTokens     int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens int64 `json:"cache_creation_tokens,omitempty"`
	HasUsage            bool  `json:"has_usage,omitempty"`

	// ModelUsage maps a model id to the output tokens it produced in this
	// result, parsed off a Claude result line's "modelUsage" object
	// (koryph-qf6.2). Valid only when Kind==EventResult; nil when the line
	// carries none. This is the ground truth for which model ACTUALLY served
	// the session: every dispatch runs with a hardcoded --fallback-model, so
	// the requested tier can silently degrade mid-session and the slot's
	// recorded Model alone would mis-attribute the outcome. Multiple keys are
	// normal (subagent calls bill under their own model); consumers wanting a
	// single attribution reduce to the dominant (max-output) entry.
	ModelUsage map[string]int64 `json:"model_usage,omitempty"`

	// RateLimited is valid only when Kind==EventError; see EventError.
	RateLimited bool `json:"rate_limited,omitempty"`

	// BudgetKilled reports whether this event's text matched a budget-kill
	// marker (koryph-77r.10, design docs/designs/2026-07-token-economy.md
	// recovery-economics follow-up): the agent was terminated by the
	// --max-budget-usd cap rather than crashing, being rate-limited, or
	// finishing its turn. Empirically pinned (2026-07) against a live
	// `claude -p ... --max-budget-usd ...` run: the CLI lets the in-flight
	// turn complete (often well over the cap) and then emits a normal
	// "result" line with subtype "error_max_budget_usd" and is_error true —
	// so, like RateLimited, BudgetKilled can be true on a Kind==EventResult
	// line (that line's CostUSD/HasCost remain meaningful: "is_error is
	// irrelevant to cost" applies here too). Unlike RateLimited it is not
	// restricted to Kind==EventError at all, since a budget-kill always
	// surfaces as a "result" line in practice.
	BudgetKilled bool `json:"budget_killed,omitempty"`

	// SessionID is valid when Kind==EventSession, and MAY also be populated
	// on other kinds for runtimes that stamp a session id on every event —
	// consumers should only rely on it being present when Kind==EventSession.
	SessionID string `json:"session_id,omitempty"`

	// Raw is the original source line/record for this event, when the
	// Runtime implementation has one to offer (always true for a
	// line-delimited JSON stream). It is the passthrough surface for
	// EventOpaque and a debugging aid for every other kind.
	Raw json.RawMessage `json:"raw,omitempty"`
}

// EventStream is a pull iterator over the normalized Events produced by a
// Runtime.ParseEvents call. It reads lazily from the underlying io.Reader —
// a single Next call consumes at most the input needed to produce (or fail
// to produce) one Event — so a caller can tail a live, still-growing
// stream.jsonl the same way the engine reads a running agent's output today.
//
// Contract: Next returns (event, true, nil) for each successive event; when
// the underlying reader is exhausted, Next returns (Event{}, false, nil) —
// io.EOF is not itself an error at this layer. A malformed record is an
// implementation choice (skip and continue, matching dispatch/cli.go's
// tolerant line-scanning, or surface as an error) that each Runtime
// implementation documents; the interface only requires that a non-nil
// error from Next always pairs with ok==false, and that Next is not called
// again after it has returned a non-nil error (undefined behavior).
// Close releases any resources the stream holds (e.g. an internal buffered
// reader); it does not close the io.Reader passed to ParseEvents, since that
// reader's lifetime is the caller's to manage. Close is idempotent.
type EventStream interface {
	Next() (Event, bool, error)
	Close() error
}
