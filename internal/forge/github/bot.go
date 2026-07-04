// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// githubBotSvc implements [forge.BotService] for GitHub Apps.
//
// It wraps the same HTTP calls used by internal/bot (the App manifest
// exchange, installation token minting, and secret wiring via the gh CLI)
// behind the forge.BotService seam. All network behaviour is identical to
// the internal/bot package — the extraction is behaviour-identical.
//
// The gh CLI binary path is controlled by the KORYPH_GH_BIN environment
// variable (default: "gh").
type githubBotSvc struct{}

func (s *githubBotSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

// ExchangeManifest exchanges a GitHub App manifest code for bot credentials.
// This is step 5 of the App Manifest flow:
//
//	POST /app-manifests/{code}/conversions
//
// The returned [forge.BotConfig] carries the App ID, slug, and PEM private
// key. The PEM is in memory only; callers are responsible for secure storage
// (vault or encrypted file via internal/bot.storeKeyAfterCreate).
func (s *githubBotSvc) ExchangeManifest(ctx context.Context, code string) (forge.BotConfig, error) {
	apiURL := fmt.Sprintf("https://api.github.com/app-manifests/%s/conversions",
		url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		return forge.BotConfig{}, fmt.Errorf("github bot: build exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return forge.BotConfig{}, fmt.Errorf("github bot: exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return forge.BotConfig{}, fmt.Errorf(
			"github bot: exchange failed (HTTP %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var conv struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
		PEM  string `json:"pem"`
	}
	if err := json.Unmarshal(body, &conv); err != nil {
		return forge.BotConfig{}, fmt.Errorf("github bot: parse exchange response: %w", err)
	}
	if conv.PEM == "" {
		return forge.BotConfig{}, fmt.Errorf(
			"github bot: exchange response contained no PEM (code may already have been used)")
	}

	return forge.BotConfig{
		AppID:         conv.ID,
		Slug:          conv.Slug,
		PrivateKeyPEM: conv.PEM,
	}, nil
}

// ListInstallations returns all GitHub App installations. jwtOrToken must be
// a GitHub App JWT (Bearer authentication against /app/installations).
func (s *githubBotSvc) ListInstallations(ctx context.Context, jwtOrToken string) ([]forge.Installation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/app/installations", http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("github bot: build installations request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtOrToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("github bot: GET /app/installations: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf(
			"github bot: GET /app/installations returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("github bot: parse installations response: %w", err)
	}

	installs := make([]forge.Installation, 0, len(raw))
	for _, i := range raw {
		installs = append(installs, forge.Installation{
			ID:           i.ID,
			AccountLogin: i.Account.Login,
		})
	}
	return installs, nil
}

// MintInstallationToken creates a short-lived installation access token.
// jwtOrToken must be a GitHub App JWT; installID is the installation numeric
// identifier returned by [ListInstallations].
func (s *githubBotSvc) MintInstallationToken(ctx context.Context, jwtOrToken string, installID int64) (string, error) {
	apiURL := fmt.Sprintf("https://api.github.com/app/installations/%d/access_tokens", installID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		return "", fmt.Errorf("github bot: build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtOrToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("github bot: POST /app/installations/%d/access_tokens: %w", installID, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf(
			"github bot: POST /app/installations/%d/access_tokens returned HTTP %d",
			installID, resp.StatusCode)
	}

	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
		return "", fmt.Errorf("github bot: parse installation token response: %w", err)
	}
	return tok.Token, nil
}

// SetSecrets sets RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY as GitHub
// Actions secrets on the target repository (or org level when ownerRepo is
// an org name without a slash). The cfg.PrivateKeyPEM is the operational PEM
// passed directly to GitHub Actions regardless of local vault storage.
//
// ownerRepo must be "owner/repo" for repository-level secrets. Org-level
// secrets are not supported by this method; use the gh CLI directly.
func (s *githubBotSvc) SetSecrets(_ context.Context, cfg forge.BotConfig, ownerRepo string) error {
	ghBin := s.ghBin()
	appIDVal := fmt.Sprintf("%d", cfg.AppID)

	for _, sec := range []struct{ name, val string }{
		{"RELEASE_BOT_APP_ID", appIDVal},
		{"RELEASE_BOT_PRIVATE_KEY", cfg.PrivateKeyPEM},
	} {
		cmd := exec.Command(ghBin, "secret", "set", sec.name, //nolint:gosec
			"--repo", ownerRepo,
			"--body", sec.val)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("github bot: gh secret set %s --repo %s: %w\n%s",
				sec.name, ownerRepo, err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}
