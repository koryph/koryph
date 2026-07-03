// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestBatchLive exercises the full BatchSubmit → BatchWait → result path
// against the live Anthropic Message Batches API.  The test is skipped
// automatically unless KORYPH_BATCH_API_KEY is explicitly set in the
// environment — it never falls back to the ambient ANTHROPIC_API_KEY.
//
// Run manually (batch processing typically takes 1-5 minutes):
//
//	KORYPH_BATCH_API_KEY=sk-ant-… \
//	  go test -v ./internal/anthro/ -run TestBatchLive -timeout 15m
func TestBatchLive(t *testing.T) {
	const keyVar = "KORYPH_BATCH_API_KEY"
	if os.Getenv(keyVar) == "" {
		t.Skipf("%s not set — skipping live batch golden test", keyVar)
	}

	client, err := NewClient(keyVar)
	if err != nil {
		t.Fatalf("NewClient(%s): %v", keyVar, err)
	}

	reqs := []MsgReq{
		{
			ID:          "live-math",
			Model:       "haiku",
			System:      "You are a concise assistant. Answer with only the requested value, no extra words.",
			User:        "What is 2 + 2? Reply with only the number.",
			MaxTokens:   16,
			CachePrefix: true,
		},
		{
			ID:          "live-geo",
			Model:       "haiku",
			System:      "You are a concise assistant. Answer with only the requested value, no extra words.",
			User:        "What is the capital of France? Reply with only the city name.",
			MaxTokens:   16,
			CachePrefix: true,
		},
	}

	estimate := EstimateUSD(reqs)
	t.Logf("pre-submit cost estimate: $%.6f", estimate)

	batchID, err := client.BatchSubmit(ctx(t), reqs, Confirm{
		Confirmed:   true,
		EstimateUSD: estimate,
		Reason:      "live golden test — KORYPH_BATCH_API_KEY is set",
	})
	if err != nil {
		t.Fatalf("BatchSubmit: %v", err)
	}
	t.Logf("submitted batch %s; polling every 15 s …", batchID)

	results, err := client.BatchWait(ctx(t), batchID, 15*time.Second)
	if err != nil {
		t.Fatalf("BatchWait: %v", err)
	}

	if len(results) != len(reqs) {
		t.Fatalf("result count = %d, want %d", len(results), len(reqs))
	}

	byID := make(map[string]BatchResult, len(results))
	for _, r := range results {
		byID[r.ID] = r
	}

	// --- live-math: model must return "4" ----------------------------------------
	if r, ok := byID["live-math"]; !ok {
		t.Error("no result for live-math")
	} else if r.Err != "" {
		t.Errorf("live-math errored: %s", r.Err)
	} else {
		t.Logf("live-math: text=%q  usage=%+v", r.Text, r.Usage)
		if !strings.Contains(r.Text, "4") {
			t.Errorf("live-math: expected '4' in response, got %q", r.Text)
		}
		if r.Usage.InputTokens == 0 || r.Usage.OutputTokens == 0 {
			t.Errorf("live-math: zero token counts in usage: %+v", r.Usage)
		}
	}

	// --- live-geo: model must return "Paris" -------------------------------------
	if r, ok := byID["live-geo"]; !ok {
		t.Error("no result for live-geo")
	} else if r.Err != "" {
		t.Errorf("live-geo errored: %s", r.Err)
	} else {
		t.Logf("live-geo:  text=%q  usage=%+v", r.Text, r.Usage)
		if !strings.Contains(r.Text, "Paris") {
			t.Errorf("live-geo: expected 'Paris' in response, got %q", r.Text)
		}
		if r.Usage.InputTokens == 0 || r.Usage.OutputTokens == 0 {
			t.Errorf("live-geo: zero token counts in usage: %+v", r.Usage)
		}
	}
}

// ctx returns a background context tied to the test deadline so the live
// poll loop respects the -timeout flag automatically.
func ctx(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		return context.Background()
	}
	c, cancel := context.WithDeadline(context.Background(), deadline)
	t.Cleanup(cancel)
	return c
}
