// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"fmt"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
)

// BatchSubmit submits reqs as one Message Batch (custom_id = req.ID) and
// returns the batch id. Spend guardrail: absent explicit confirmation the
// submit is refused — nothing is sent.
func (c *Client) BatchSubmit(ctx context.Context, reqs []MsgReq, confirm Confirm) (string, error) {
	if !confirm.Confirmed {
		return "", fmt.Errorf("anthro: batch spend not confirmed (estimate $%.2f) — refusing submit", EstimateUSD(reqs))
	}
	if len(reqs) == 0 {
		return "", fmt.Errorf("anthro: batch submit: no requests")
	}
	requests := make([]anthropic.MessageBatchNewParamsRequest, 0, len(reqs))
	for _, r := range reqs {
		if r.ID == "" {
			return "", fmt.Errorf("anthro: batch submit: request with empty ID")
		}
		params, err := buildMessageParams(r)
		if err != nil {
			return "", fmt.Errorf("anthro: batch submit %s: %w", r.ID, err)
		}
		requests = append(requests, anthropic.MessageBatchNewParamsRequest{
			CustomID: r.ID,
			Params: anthropic.MessageBatchNewParamsRequestParams{
				Model:     params.Model,
				MaxTokens: params.MaxTokens,
				Messages:  params.Messages,
				System:    params.System,
			},
		})
	}
	batchID, err := c.batches.submit(ctx, anthropic.MessageBatchNewParams{Requests: requests})
	if err != nil {
		return "", fmt.Errorf("anthro: batch submit: %w", err)
	}
	return batchID, nil
}

// BatchWait polls the batch every poll interval until processing ends,
// then streams and maps the results (custom_id → BatchResult). Succeeded
// entries carry text + usage; errored/canceled/expired entries carry Err.
func (c *Client) BatchWait(ctx context.Context, batchID string, poll time.Duration) ([]BatchResult, error) {
	if poll <= 0 {
		poll = 30 * time.Second
	}
	for {
		status, err := c.batches.status(ctx, batchID)
		if err != nil {
			return nil, fmt.Errorf("anthro: batch %s status: %w", batchID, err)
		}
		if status == string(anthropic.MessageBatchProcessingStatusEnded) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}

	raw, err := c.batches.results(ctx, batchID)
	if err != nil {
		return nil, fmt.Errorf("anthro: batch %s results: %w", batchID, err)
	}
	tierByModel := tierFromAPIModel()
	results := make([]BatchResult, 0, len(raw))
	for _, entry := range raw {
		res := BatchResult{ID: entry.CustomID}
		switch entry.Result.Type {
		case "succeeded":
			msg := entry.Result.Message
			res.Text = extractText(msg.Content)
			res.Usage = usageFromSDK(tierByModel[string(msg.Model)], msg.Usage)
		case "errored":
			res.Err = fmt.Sprintf("%s: %s", entry.Result.Error.Error.Type, entry.Result.Error.Error.Message)
		default: // canceled | expired | anything future
			res.Err = entry.Result.Type
		}
		results = append(results, res)
	}
	return results, nil
}

// tierFromAPIModel inverts TierToAPIModel for pricing batch results.
func tierFromAPIModel() map[string]string {
	inv := make(map[string]string, len(TierToAPIModel))
	for tier, model := range TierToAPIModel {
		inv[model] = tier
	}
	return inv
}
