// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package runtime_test

import (
	"encoding/json"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

// TestEventJSONRoundTrip confirms the normalized envelope encodes/decodes
// every field, including the HasCost/CostUSD pair that distinguishes "no
// cost reported" from "cost reported as exactly zero" (koryph-v8u.1; mirrors
// the distinction internal/dispatch.ParseResultCost's (float64, bool) return
// already makes).
func TestEventJSONRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		ev   runtime.Event
	}{
		{
			name: "result with cost",
			ev: runtime.Event{
				Kind:    runtime.EventResult,
				CostUSD: 0.1234,
				HasCost: true,
				Raw:     json.RawMessage(`{"type":"result","total_cost_usd":0.1234}`),
			},
		},
		{
			name: "result with zero cost, distinct from no cost",
			ev: runtime.Event{
				Kind:    runtime.EventResult,
				CostUSD: 0,
				HasCost: true,
				Raw:     json.RawMessage(`{"type":"result","total_cost_usd":0}`),
			},
		},
		{
			name: "result with no cost reported at all",
			ev: runtime.Event{
				Kind:    runtime.EventResult,
				HasCost: false,
				Raw:     json.RawMessage(`{"type":"result"}`),
			},
		},
		{
			name: "rate-limited error",
			ev: runtime.Event{
				Kind:        runtime.EventError,
				RateLimited: true,
				Raw:         json.RawMessage(`{"type":"error","error":{"type":"rate_limit_error"}}`),
			},
		},
		{
			name: "non-rate-limited error",
			ev: runtime.Event{
				Kind:        runtime.EventError,
				RateLimited: false,
				Raw:         json.RawMessage(`{"type":"error","error":{"type":"invalid_request"}}`),
			},
		},
		{
			name: "session",
			ev: runtime.Event{
				Kind:      runtime.EventSession,
				SessionID: "sess-123",
				Raw:       json.RawMessage(`{"type":"session","session_id":"sess-123"}`),
			},
		},
		{
			name: "opaque passthrough",
			ev: runtime.Event{
				Kind: runtime.EventOpaque,
				Raw:  json.RawMessage(`{"type":"assistant","text":"hello"}`),
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.ev)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var got runtime.Event
			if err := json.Unmarshal(data, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			if got.Kind != tc.ev.Kind {
				t.Errorf("Kind: got %q, want %q", got.Kind, tc.ev.Kind)
			}
			if got.CostUSD != tc.ev.CostUSD {
				t.Errorf("CostUSD: got %v, want %v", got.CostUSD, tc.ev.CostUSD)
			}
			if got.HasCost != tc.ev.HasCost {
				t.Errorf("HasCost: got %v, want %v", got.HasCost, tc.ev.HasCost)
			}
			if got.RateLimited != tc.ev.RateLimited {
				t.Errorf("RateLimited: got %v, want %v", got.RateLimited, tc.ev.RateLimited)
			}
			if got.SessionID != tc.ev.SessionID {
				t.Errorf("SessionID: got %q, want %q", got.SessionID, tc.ev.SessionID)
			}
			if string(got.Raw) != string(tc.ev.Raw) {
				t.Errorf("Raw: got %s, want %s", got.Raw, tc.ev.Raw)
			}
		})
	}
}
