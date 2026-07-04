// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"fmt"
	"io"

	"github.com/koryph/koryph/internal/forge"
	glpkg "github.com/koryph/koryph/internal/forge/gitlab"
)

// GitLabAttachOptions configures 'koryph bot attach --forge gitlab'.
type GitLabAttachOptions struct {
	// Name is the GitLab bot credential name (required).
	Name string
	// Project is the "namespace/project" slug to attach (required).
	Project string
	// Out receives progress messages.
	Out io.Writer
	// BotSvc is an optional [forge.BotService] override for testing.
	// nil = use the built-in gitlab BotService.
	BotSvc forge.BotService
}

// GitLabAttachResult summarises what 'koryph bot attach --forge gitlab' did.
type GitLabAttachResult struct {
	// VariablesSet is the list of CI variable keys that were written.
	VariablesSet []string
}

// AttachGitLab implements 'koryph bot attach --forge gitlab'. It is
// idempotent: safe to re-run.
//
// Steps:
//  1. Load the GitLab bot credential and resolve the token.
//  2. Validate the token (scopes, expiry).
//  3. Set CI variables on the project:
//     - KORYPH_BOT_TOKEN        (masked, protected)
//     - KORYPH_BOT_TOKEN_EXPIRY (not masked — value may be "never", protected)
func AttachGitLab(ctx context.Context, cfg *GitLabConfig, opts GitLabAttachOptions) (*GitLabAttachResult, error) {
	if opts.Project == "" {
		return nil, fmt.Errorf("gitlab bot attach: --project is required")
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	// Step 1: resolve token.
	token, err := ResolveGLToken(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("gitlab bot attach: resolve token: %w", err)
	}
	fmt.Fprintf(out, "  token resolved (provider=%s)\n", cfg.Provider)

	// Step 2: validate token via API (scopes + expiry).
	info, warning, err := glpkg.ValidateToken(ctx, token, cfg.Host, defaultGLScopes, ExpiryWarnDays)
	if err != nil {
		return nil, fmt.Errorf("gitlab bot attach: token validation: %w", err)
	}
	fmt.Fprintf(out, "  token validated: name=%q scopes=%s\n", info.Name, joinScopes(info.Scopes))
	if warning != "" {
		fmt.Fprintf(out, "  WARNING: %s\n", warning)
	}

	// Step 3: set CI variables.
	var keys []string
	if opts.BotSvc != nil {
		fCfg := forge.BotConfig{
			PrivateKeyPEM: token,
		}
		if err := opts.BotSvc.SetSecrets(ctx, fCfg, opts.Project); err != nil {
			return nil, fmt.Errorf("gitlab bot attach: set CI variables via forge: %w", err)
		}
		keys = []string{"KORYPH_BOT_TOKEN", "KORYPH_BOT_TOKEN_EXPIRY"}
		fmt.Fprintf(out, "  CI variables set via forge seam: %v\n", keys)
	} else {
		expiry := "never"
		if info.ExpiresAt != "" {
			expiry = info.ExpiresAt
		}
		for _, v := range []struct {
			key    string
			val    string
			masked bool
		}{
			{"KORYPH_BOT_TOKEN", token, true},
			// KORYPH_BOT_TOKEN_EXPIRY is not a secret; masked=false avoids the
			// GitLab ≥8-char constraint ("never" is only 5 chars).
			{"KORYPH_BOT_TOKEN_EXPIRY", expiry, false},
		} {
			if err := glpkg.SetProjectVariable(ctx, token, opts.Project, v.key, v.val, v.masked); err != nil {
				return nil, fmt.Errorf("gitlab bot attach: set variable %s: %w", v.key, err)
			}
			fmt.Fprintf(out, "  CI variable %s set on %s\n", v.key, opts.Project)
			keys = append(keys, v.key)
		}
	}

	return &GitLabAttachResult{VariablesSet: keys}, nil
}

func joinScopes(scopes []string) string {
	s := ""
	for i, sc := range scopes {
		if i > 0 {
			s += ", "
		}
		s += sc
	}
	return s
}
