// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/obs"
)

// ghAPIBase is the GitHub API root URL. Tests override this to redirect HTTP
// calls to a local httptest.Server without touching production defaults.
var ghAPIBase = "https://api.github.com"

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

// CurrentUser returns the login associated with the current gh credentials.
func (s *githubBotSvc) CurrentUser(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, s.ghBin(), "api", "user", "--jq", ".login") //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("github bot: current user: %w: %s", err, strings.TrimSpace(string(out)))
	}
	login := strings.TrimSpace(string(out))
	if login == "" {
		return "", fmt.Errorf("github bot: current user: empty login")
	}
	return login, nil
}

// ExchangeManifest exchanges a GitHub App manifest code for bot credentials.
// This is step 5 of the App Manifest flow:
//
//	POST /app-manifests/{code}/conversions
//
// The returned [forge.BotConfig] carries the App ID, slug, and PEM private
// key. The PEM is in memory only; callers are responsible for secure storage
// (vault or encrypted file via internal/bot.storeKeyAfterCreate).
//
// Span: provider=github, endpoint_class=app_manifest_exchange.
// Redaction contract: the response PEM is NEVER logged; only app_id and slug
// are recorded after a successful exchange.
func (s *githubBotSvc) ExchangeManifest(ctx context.Context, code string) (forge.BotConfig, error) {
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "github"),
		slog.String(obs.KeyEndpointClass, "app_manifest_exchange"),
	)

	apiURL := fmt.Sprintf("%s/app-manifests/%s/conversions", ghAPIBase, url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		sp.End(0, err)
		return forge.BotConfig{}, fmt.Errorf("github bot: build exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return forge.BotConfig{}, fmt.Errorf("github bot: exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		callErr := fmt.Errorf("github bot: exchange failed (HTTP %d): %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
		sp.End(resp.StatusCode, callErr)
		return forge.BotConfig{}, callErr
	}

	var conv struct {
		ID   int64  `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
		PEM  string `json:"pem"`
	}
	if err := json.Unmarshal(body, &conv); err != nil {
		sp.End(resp.StatusCode, err)
		return forge.BotConfig{}, fmt.Errorf("github bot: parse exchange response: %w", err)
	}
	if conv.PEM == "" {
		noKeyErr := fmt.Errorf("github bot: exchange response contained no PEM (code may already have been used)")
		sp.End(resp.StatusCode, noKeyErr)
		return forge.BotConfig{}, noKeyErr
	}

	// Emit span before returning — log only safe identifiers (app_id, slug),
	// NEVER PrivateKeyPEM or any credential value.
	sp.End(resp.StatusCode, nil)
	obs.For("forge").LogAttrs(ctx, slog.LevelDebug, "bot.lifecycle",
		slog.String(obs.KeyLifecycle, "app_manifest_exchanged"),
		slog.String(obs.KeyProvider, "github"),
		slog.Int64("app_id", conv.ID),
		slog.String("slug", conv.Slug),
	)

	return forge.BotConfig{
		AppID:         conv.ID,
		Slug:          conv.Slug,
		PrivateKeyPEM: conv.PEM,
	}, nil
}

// ListInstallations returns all GitHub App installations. jwtOrToken must be
// a GitHub App JWT (Bearer authentication against /app/installations).
//
// Span: provider=github, endpoint_class=list_installations.
// Redaction contract: jwtOrToken (the JWT) is set only as an Authorization
// header and is NEVER logged.
func (s *githubBotSvc) ListInstallations(ctx context.Context, jwtOrToken string) ([]forge.Installation, error) {
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "github"),
		slog.String(obs.KeyEndpointClass, "list_installations"),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		ghAPIBase+"/app/installations", http.NoBody)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("github bot: build installations request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtOrToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("github bot: GET /app/installations: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		callErr := fmt.Errorf("github bot: GET /app/installations returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
		sp.End(resp.StatusCode, callErr)
		return nil, callErr
	}

	var raw []struct {
		ID      int64 `json:"id"`
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		sp.End(resp.StatusCode, err)
		return nil, fmt.Errorf("github bot: parse installations response: %w", err)
	}

	sp.End(resp.StatusCode, nil)

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
//
// Span: provider=github, endpoint_class=mint_installation_token, install_id logged.
// Redaction contract: jwtOrToken and the returned token are NEVER logged.
func (s *githubBotSvc) MintInstallationToken(ctx context.Context, jwtOrToken string, installID int64) (string, error) {
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "github"),
		slog.String(obs.KeyEndpointClass, "mint_installation_token"),
		slog.Int64("install_id", installID),
	)

	apiURL := fmt.Sprintf("%s/app/installations/%d/access_tokens", ghAPIBase, installID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		sp.End(0, err)
		return "", fmt.Errorf("github bot: build token request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+jwtOrToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return "", fmt.Errorf("github bot: POST /app/installations/%d/access_tokens: %w", installID, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		callErr := fmt.Errorf("github bot: POST /app/installations/%d/access_tokens returned HTTP %d",
			installID, resp.StatusCode)
		sp.End(resp.StatusCode, callErr)
		return "", callErr
	}

	var tok struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(body, &tok); err != nil || tok.Token == "" {
		sp.End(resp.StatusCode, err)
		return "", fmt.Errorf("github bot: parse installation token response: %w", err)
	}

	// Log success — the token value itself is never emitted.
	sp.End(resp.StatusCode, nil)
	return tok.Token, nil
}

// AttachRepository adds ownerRepo to a selected GitHub App installation.
func (s *githubBotSvc) AttachRepository(ctx context.Context, ownerRepo string, installID int64) (forge.RepositoryAttachment, error) {
	cmd := exec.CommandContext(ctx, s.ghBin(), "api", "/repos/"+ownerRepo) //nolint:gosec
	raw, err := cmd.Output()
	if err != nil {
		return forge.RepositoryAttachment{}, fmt.Errorf("github bot: resolve repository %s: %w", ownerRepo, err)
	}
	var repository struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &repository); err != nil || repository.ID == 0 {
		return forge.RepositoryAttachment{}, fmt.Errorf("github bot: parse repository %s: %w", ownerRepo, err)
	}

	cmd = exec.CommandContext(ctx, s.ghBin(), "api", //nolint:gosec
		fmt.Sprintf("/user/installations/%d/repositories", installID))
	raw, err = cmd.Output()
	if err == nil {
		var installations struct {
			Repositories []struct {
				ID int64 `json:"id"`
			} `json:"repositories"`
		}
		if json.Unmarshal(raw, &installations) == nil {
			for _, repo := range installations.Repositories {
				if repo.ID == repository.ID {
					return forge.RepositoryAttachment{RepositoryID: repository.ID}, nil
				}
			}
		}
	}

	cmd = exec.CommandContext(ctx, s.ghBin(), "api", "-X", "PUT", //nolint:gosec
		fmt.Sprintf("/user/installations/%d/repositories/%d", installID, repository.ID))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return forge.RepositoryAttachment{RepositoryID: repository.ID}, fmt.Errorf("github bot: attach %s to installation %d: %w: %s",
			ownerRepo, installID, err, strings.TrimSpace(string(out)))
	}
	return forge.RepositoryAttachment{RepositoryID: repository.ID, Added: true}, nil
}

// SetSecrets sets RELEASE_BOT_APP_ID and RELEASE_BOT_PRIVATE_KEY as GitHub
// Actions secrets on the target repository (or org level when ownerRepo is
// an org name without a slash). The cfg.PrivateKeyPEM is the operational PEM
// passed directly to GitHub Actions regardless of local vault storage.
//
// ownerRepo must be "owner/repo" for repository-level secrets. Org-level
// secrets are not supported by this method; use the gh CLI directly.
//
// Span: provider=github, endpoint_class=set_secrets. Secret names (not values)
// are logged; cfg.PrivateKeyPEM and cfg.AppID value are NEVER emitted.
func (s *githubBotSvc) SetSecrets(ctx context.Context, cfg forge.BotConfig, ownerRepo string) error {
	ghBin := s.ghBin()
	appIDVal := fmt.Sprintf("%d", cfg.AppID)

	secrets := []struct{ name, val string }{
		{"RELEASE_BOT_APP_ID", appIDVal},
		{"RELEASE_BOT_PRIVATE_KEY", cfg.PrivateKeyPEM},
	}

	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "github"),
		slog.String(obs.KeyEndpointClass, "set_secrets"),
		slog.String("repo", ownerRepo),
		slog.Int("secret_count", len(secrets)),
	)

	for _, sec := range secrets {
		cmd := exec.Command(ghBin, "secret", "set", sec.name, //nolint:gosec
			"--repo", ownerRepo,
			"--body", sec.val)
		if out, err := cmd.CombinedOutput(); err != nil {
			callErr := fmt.Errorf("github bot: gh secret set %s --repo %s: %w\n%s",
				sec.name, ownerRepo, err, strings.TrimSpace(string(out)))
			sp.End(0, callErr)
			return callErr
		}
	}

	sp.EndOK()
	return nil
}
