// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/koryph/koryph/internal/pricing"
)

// price is USD per million tokens for one tier.
type price = pricing.Rate

// priceTable is the per-token price table (USD / MTok), keyed by tier, derived
// from the canonical base rates in internal/pricing (koryph-fiv finding #5) —
// before consolidation this was a hand-maintained copy that had drifted from
// the real list price (it priced Opus at $5/$25 rather than $15/$75). Cache
// multipliers per the API pricing model: reads 0.1x input, 1h-TTL writes 2x
// input (this client requests 1-hour ephemeral cache).
var priceTable = buildPriceTable()

// buildPriceTable indexes pricing.Claude's canonical base rates by tier name
// for anthro's exact-key lookups (EstimateUSD / estimateUsageUSD).
func buildPriceTable() map[string]price {
	m := make(map[string]price, len(pricing.Claude))
	for _, t := range pricing.Claude {
		m[t.Name] = t.Rate
	}
	return m
}

const (
	cacheReadMultiplier  = pricing.CacheReadMultiplier
	cacheWriteMultiplier = pricing.CacheWrite1HourMultiplier // 1h ephemeral TTL writes
)

// messageBackend is the single-message slice of the SDK, factored out so
// tests never touch the network.
type messageBackend interface {
	create(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

// batchBackend is the Message Batches slice of the SDK.
type batchBackend interface {
	submit(ctx context.Context, params anthropic.MessageBatchNewParams) (string, error)
	status(ctx context.Context, batchID string) (string, error)
	results(ctx context.Context, batchID string) ([]anthropic.MessageBatchIndividualResponse, error)
}

// Client wraps the official anthropic-sdk-go for explicit per-token spend.
type Client struct {
	messages messageBackend
	batches  batchBackend
}

// sdkBackend adapts the real SDK client to the backend interfaces.
type sdkBackend struct {
	api anthropic.Client
}

func (s sdkBackend) create(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	return s.api.Messages.New(ctx, params)
}

func (s sdkBackend) submit(ctx context.Context, params anthropic.MessageBatchNewParams) (string, error) {
	batch, err := s.api.Messages.Batches.New(ctx, params)
	if err != nil {
		return "", err
	}
	return batch.ID, nil
}

func (s sdkBackend) status(ctx context.Context, batchID string) (string, error) {
	batch, err := s.api.Messages.Batches.Get(ctx, batchID)
	if err != nil {
		return "", err
	}
	return string(batch.ProcessingStatus), nil
}

func (s sdkBackend) results(ctx context.Context, batchID string) ([]anthropic.MessageBatchIndividualResponse, error) {
	stream := s.api.Messages.Batches.ResultsStreaming(ctx, batchID)
	defer stream.Close()
	var out []anthropic.MessageBatchIndividualResponse
	for stream.Next() {
		out = append(out, stream.Current())
	}
	if err := stream.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// NewClient constructs a Client from an EXPLICITLY named key env var.
// Guardrails (fail closed):
//   - keyEnvVar must be non-empty,
//   - keyEnvVar must not be ANTHROPIC_API_KEY (never read the ambient key
//     implicitly — use a purpose-named var like KORYPH_BATCH_API_KEY),
//   - the variable's value must be non-empty.
func NewClient(keyEnvVar string) (*Client, error) {
	if keyEnvVar == "" {
		return nil, fmt.Errorf("anthro: explicit key env var required")
	}
	if keyEnvVar == "ANTHROPIC_API_KEY" {
		return nil, fmt.Errorf("anthro: refusing ambient ANTHROPIC_API_KEY — name a purpose-specific var (e.g. KORYPH_BATCH_API_KEY)")
	}
	key := os.Getenv(keyEnvVar)
	if key == "" {
		return nil, fmt.Errorf("anthro: env var %s is unset or empty", keyEnvVar)
	}
	// WithoutEnvironmentDefaults skips the SDK's ambient credential autoload
	// (client.go DefaultClientOptions: ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN)
	// so an ambient ANTHROPIC_AUTH_TOKEN cannot ride along as an
	// Authorization: Bearer header beside our explicit x-api-key — the
	// explicitly-named key is the ONLY credential on the request (koryph-i3b
	// review finding; same fix as ProbeLiveness in liveness.go).
	backend := sdkBackend{api: anthropic.NewClient(option.WithoutEnvironmentDefaults(), option.WithAPIKey(key))}
	return &Client{messages: backend, batches: backend}, nil
}

// Message sends one non-interactive request and returns the concatenated
// text output plus usage (with a local USD estimate).
func (c *Client) Message(ctx context.Context, req MsgReq) (string, Usage, error) {
	params, err := buildMessageParams(req)
	if err != nil {
		return "", Usage{}, err
	}
	msg, err := c.messages.create(ctx, params)
	if err != nil {
		return "", Usage{}, fmt.Errorf("anthro: message %s: %w", req.ID, err)
	}
	usage := usageFromSDK(req.Model, msg.Usage)
	return extractText(msg.Content), usage, nil
}

// buildMessageParams maps a MsgReq onto SDK params: tier → API model id
// (unknown tier is an error), system as a text block with an ephemeral 1h
// cache_control breakpoint when CachePrefix, user message, MaxTokens
// defaulting to 4096.
func buildMessageParams(req MsgReq) (anthropic.MessageNewParams, error) {
	model, ok := TierToAPIModel[req.Model]
	if !ok {
		return anthropic.MessageNewParams{}, fmt.Errorf("anthro: unknown model tier %q (known: %s)", req.Model, strings.Join(knownTiers(), ", "))
	}
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 4096
	}
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(model),
		MaxTokens: int64(maxTokens),
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(req.User)),
		},
	}
	if req.System != "" {
		block := anthropic.TextBlockParam{Text: req.System}
		if req.CachePrefix {
			block.CacheControl = anthropic.CacheControlEphemeralParam{
				TTL: anthropic.CacheControlEphemeralTTLTTL1h,
			}
		}
		params.System = []anthropic.TextBlockParam{block}
	}
	return params, nil
}

// extractText concatenates the text blocks of a response.
func extractText(content []anthropic.ContentBlockUnion) string {
	var sb strings.Builder
	for _, block := range content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

// usageFromSDK maps SDK usage onto ours and attaches the USD estimate.
func usageFromSDK(tier string, u anthropic.Usage) Usage {
	out := Usage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		CacheRead:    u.CacheReadInputTokens,
		CacheWrite:   u.CacheCreationInputTokens,
	}
	out.EstimateUSD = estimateUsageUSD(tier, out)
	return out
}

// estimateUsageUSD prices observed usage with the local table.
func estimateUsageUSD(tier string, u Usage) float64 {
	p, ok := priceTable[tier]
	if !ok {
		return 0
	}
	const mtok = 1e6
	return float64(u.InputTokens)*p.InPerMTok/mtok +
		float64(u.OutputTokens)*p.OutPerMTok/mtok +
		float64(u.CacheRead)*p.InPerMTok*cacheReadMultiplier/mtok +
		float64(u.CacheWrite)*p.InPerMTok*cacheWriteMultiplier/mtok
}

// EstimateUSD is the rough pre-submit estimate shown to the user before a
// Confirm: chars/4 input-token heuristic plus MaxTokens/2 assumed output,
// at the tier prices. Unknown tiers contribute zero.
func EstimateUSD(reqs []MsgReq) float64 {
	const mtok = 1e6
	var total float64
	for _, r := range reqs {
		p, ok := priceTable[r.Model]
		if !ok {
			continue
		}
		maxTokens := r.MaxTokens
		if maxTokens <= 0 {
			maxTokens = 4096
		}
		inTokens := float64(len(r.System)+len(r.User)) / 4
		outTokens := float64(maxTokens) / 2
		total += inTokens*p.InPerMTok/mtok + outTokens*p.OutPerMTok/mtok
	}
	return total
}

// knownTiers lists the mappable tiers for error messages (sorted-ish,
// small fixed set).
func knownTiers() []string {
	return []string{"haiku", "sonnet", "opus", "fable"}
}
