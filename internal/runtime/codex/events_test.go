// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package codex

import (
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/runtime"
)

func TestParseEventsNormalizesInclusiveCodexInput(t *testing.T) {
	stream, err := (Codex{}).ParseEvents(strings.NewReader(
		`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":3}}` + "\n",
	))
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer stream.Close()

	event, ok, err := stream.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if !ok {
		t.Fatal("Next returned no event")
	}
	if event.Kind != runtime.EventResult || !event.HasUsage {
		t.Fatalf("event kind/usage = %q/%v, want result/true", event.Kind, event.HasUsage)
	}
	if event.InputTokens != 6 || event.CacheReadTokens != 4 || event.OutputTokens != 3 {
		t.Fatalf("normalized usage = fresh:%d cached:%d output:%d, want 6/4/3",
			event.InputTokens, event.CacheReadTokens, event.OutputTokens)
	}
	if event.TokenSemantics != runtime.TokenSemanticsDisjointV1 {
		t.Fatalf("TokenSemantics = %q, want %q",
			event.TokenSemantics, runtime.TokenSemanticsDisjointV1)
	}
	if !event.HasProviderTotalInput || event.ProviderTotalInputTokens != 10 {
		t.Fatalf("provider-total audit = %d present:%v, want 10/true",
			event.ProviderTotalInputTokens, event.HasProviderTotalInput)
	}
	normalizedInput := event.InputTokens + event.CacheReadTokens + event.CacheCreationTokens
	if normalizedInput != 10 {
		t.Fatalf("normalized input classes = %d, want provider total 10", normalizedInput)
	}
}

func TestParseEventsClampsFreshInputAtZero(t *testing.T) {
	stream, err := (Codex{}).ParseEvents(strings.NewReader(
		`{"type":"turn.completed","usage":{"input_tokens":3,"cached_input_tokens":4,"output_tokens":1}}` + "\n",
	))
	if err != nil {
		t.Fatalf("ParseEvents: %v", err)
	}
	defer stream.Close()

	event, ok, err := stream.Next()
	if err != nil || !ok {
		t.Fatalf("Next = ok:%v err:%v, want event", ok, err)
	}
	if event.InputTokens != 0 {
		t.Fatalf("fresh input = %d, want max(0, 3-4) = 0", event.InputTokens)
	}
	if event.CacheReadTokens != 4 || event.ProviderTotalInputTokens != 3 {
		t.Fatalf("audit counters = cached:%d provider-total:%d, want 4/3",
			event.CacheReadTokens, event.ProviderTotalInputTokens)
	}
}
