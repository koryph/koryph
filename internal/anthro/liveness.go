// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// oauthBetaHeader is the anthropic-beta value a CLAUDE_CODE_OAUTH_TOKEN
// (`claude setup-token`) credential requires on every request, liveness
// probes included (design docs/designs/2026-07-api-key-auth.md §5) — without
// it the API rejects an otherwise-valid oauth-token credential.
const oauthBetaHeader = "oauth-2025-04-20"

// ProbeLiveness validates a resolved credential against Anthropic with the
// cheapest available call — GET /v1/models, which is free (no token spend)
// and requires no request body (koryph-i3b, design §5 point 3). A
// successful list confirms the credential is live; an error (typically a
// 401) means it is invalid, expired, or revoked. The credential is never
// logged (it never appears in an error message or span attribute).
//
// useBearer selects the auth header scheme the API expects for each
// long-lived-credential auth mode (design §5):
//   - false (AuthModeAPIKey): x-api-key: <credential>.
//   - true (AuthModeOAuthToken): Authorization: Bearer <credential>, plus
//     anthropic-beta: oauth-2025-04-20 — a setup-token credential is
//     rejected as an x-api-key and needs the beta header to be accepted at
//     all.
func ProbeLiveness(ctx context.Context, credential string, useBearer bool) error {
	return probeLiveness(ctx, credential, useBearer)
}

// probeLiveness is ProbeLiveness's implementation, taking extra SDK request
// options so tests can redirect the client at a local httptest server
// (option.WithBaseURL) instead of the real Anthropic API. Not exported —
// keeps the SDK's option.RequestOption type out of the package's public
// surface.
func probeLiveness(ctx context.Context, credential string, useBearer bool, extraOpts ...option.RequestOption) error {
	if credential == "" {
		return fmt.Errorf("anthro: ProbeLiveness: empty credential")
	}
	var opts []option.RequestOption
	params := anthropic.ModelListParams{Limit: anthropic.Int(1)}
	if useBearer {
		opts = append(opts, option.WithAuthToken(credential))
		params.Betas = []anthropic.AnthropicBeta{oauthBetaHeader}
	} else {
		opts = append(opts, option.WithAPIKey(credential))
	}
	opts = append(opts, extraOpts...)

	client := anthropic.NewClient(opts...)
	if _, err := client.Models.List(ctx, params); err != nil {
		return fmt.Errorf("anthro: liveness probe failed (credential invalid, expired, or revoked?): %w", err)
	}
	return nil
}
