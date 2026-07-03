// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package anthro wraps the official anthropic-sdk-go for the two EXPLICIT
// per-token surfaces: single non-interactive calls (review second opinions,
// migration analysis, summarization) and Message Batches (bulk planning /
// scoring / backfill at the 50% batch discount).
//
// Spend guardrails (non-negotiable):
//   - The client is constructed ONLY from an explicitly configured key:
//     resolve the env var NAMED by the caller (e.g. KORYPH_BATCH_API_KEY),
//     never ambient ANTHROPIC_API_KEY.
//   - Every entry point takes a Confirm value the CLI populates after
//     showing a cost estimate; absent confirmation → refuse.
//   - Nothing in internal/engine may import this package (the loop can
//     never spend per-token implicitly). Enforced by a test in this package
//     that greps the engine's imports.
//
// Model ids (API needs full ids, not CLI tiers):
//
//	opus → claude-opus-4-8, sonnet → claude-sonnet-5, haiku →
//	claude-haiku-4-5-20251001, fable → claude-fable-5 (explicit policy only).
//
// Prompt-cache policy: batch requests set a cache_control breakpoint
// ("ephemeral", 1h) on the shared stable prefix so the 0.1x read multiplier
// stacks with the batch discount.
//
// Implementation contract (client.go, batch.go):
//   - NewClient(keyEnvVar string) (*Client, error) — fail if unset/empty.
//   - Client.Message(ctx, MsgReq) (text string, usage Usage, err error).
//   - Client.BatchSubmit(ctx, []MsgReq, Confirm) (batchID string, err error).
//   - Client.BatchWait(ctx, batchID, poll time.Duration) ([]BatchResult, error).
//   - EstimateUSD([]MsgReq) float64 — rough pre-submit estimate for Confirm.
package anthro

// TierToAPIModel maps CLI tiers to API model ids.
var TierToAPIModel = map[string]string{
	"haiku":  "claude-haiku-4-5-20251001",
	"sonnet": "claude-sonnet-5",
	"opus":   "claude-opus-4-8",
	"fable":  "claude-fable-5",
}

// MsgReq is one message request.
type MsgReq struct {
	ID          string // caller correlation id (bead id etc.)
	Model       string // CLI tier; mapped via TierToAPIModel
	System      string // stable prefix (cache breakpoint applied here)
	User        string // volatile content
	MaxTokens   int
	CachePrefix bool // apply 1h cache_control to System
}

// Usage reports token consumption for spend logging.
type Usage struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CacheRead    int64   `json:"cache_read_tokens"`
	CacheWrite   int64   `json:"cache_write_tokens"`
	EstimateUSD  float64 `json:"estimate_usd"`
}

// BatchResult is one completed batch entry.
type BatchResult struct {
	ID    string `json:"id"`
	Text  string `json:"text"`
	Err   string `json:"error,omitempty"`
	Usage Usage  `json:"usage"`
}

// Confirm is explicit spend consent, populated by the CLI after showing the
// estimate. Zero value == not confirmed.
type Confirm struct {
	Confirmed   bool
	EstimateUSD float64
	Reason      string // e.g. "user passed --yes after estimate"
}
