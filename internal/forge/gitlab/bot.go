// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// gitlabBotSvc implements [forge.BotService] for GitLab access tokens.
//
// GitLab has no App manifest concept; ExchangeManifest returns
// [forge.ErrUnsupported]. The token flow is handled by the higher-level
// internal/bot package (see gitlab.go in that package).
//
// Token scope validation uses GET /personal_access_tokens/self or
// GET /projects/{id}/access_tokens/{token_id} depending on token type.
//
// CI variable management uses PUT /projects/{id}/variables (per-project)
// or PUT /groups/{id}/variables (per-group).
type gitlabBotSvc struct{}

// glHost returns the GitLab host to use (default: gitlab.com).
func glHost() string {
	if v := os.Getenv("KORYPH_GITLAB_HOST"); v != "" {
		return v
	}
	return "gitlab.com"
}

// glAPIBase returns the base API URL for the configured host.
func glAPIBase() string {
	return "https://" + glHost() + "/api/v4"
}

// ExchangeManifest is not supported for GitLab (no App manifest concept).
func (s *gitlabBotSvc) ExchangeManifest(_ context.Context, _ string) (forge.BotConfig, error) {
	return forge.BotConfig{}, fmt.Errorf("gitlab bot: %w: GitLab uses access tokens, not App manifests; use `koryph bot create --forge gitlab`", forge.ErrUnsupported)
}

// ListInstallations synthesises a single installation record from the token's
// user/namespace information via GET /personal_access_tokens/self.
// jwtOrToken must be a GitLab personal or project access token.
func (s *gitlabBotSvc) ListInstallations(ctx context.Context, jwtOrToken string) ([]forge.Installation, error) {
	info, err := fetchTokenSelf(ctx, jwtOrToken)
	if err != nil {
		return nil, fmt.Errorf("gitlab bot: list installations: %w", err)
	}
	return []forge.Installation{{
		ID:           int64(info.ID),
		AccountLogin: info.Name,
	}}, nil
}

// MintInstallationToken returns the existing token unchanged — GitLab access
// tokens are long-lived and do not have an installation-token equivalent.
// installID is ignored.
func (s *gitlabBotSvc) MintInstallationToken(_ context.Context, jwtOrToken string, _ int64) (string, error) {
	return jwtOrToken, nil
}

// SetSecrets sets the bot token as GitLab CI variables on the target project.
//
// ownerRepo must be "namespace/project" (URL-encoded as needed). Two variables
// are created or updated:
//   - KORYPH_BOT_TOKEN — the access token value
//   - KORYPH_BOT_TOKEN_EXPIRY — the expiry date (YYYY-MM-DD) or "never"
//
// Variables are set as "masked" to prevent accidental log exposure and
// "protected" so they are only available to protected branches and tags.
func (s *gitlabBotSvc) SetSecrets(ctx context.Context, cfg forge.BotConfig, ownerRepo string) error {
	token := cfg.PrivateKeyPEM // reuse field — holds the token value
	if token == "" {
		return fmt.Errorf("gitlab bot: SetSecrets: token value is empty")
	}

	// Validate the token and get expiry information.
	info, err := fetchTokenSelf(ctx, token)
	if err != nil {
		return fmt.Errorf("gitlab bot: SetSecrets: validate token: %w", err)
	}

	expiry := "never"
	if info.ExpiresAt != "" {
		expiry = info.ExpiresAt
	}

	projectID := url.PathEscape(ownerRepo)

	for _, v := range []struct{ key, val string }{
		{"KORYPH_BOT_TOKEN", token},
		{"KORYPH_BOT_TOKEN_EXPIRY", expiry},
	} {
		if err := SetProjectVariable(ctx, token, projectID, v.key, v.val); err != nil {
			return fmt.Errorf("gitlab bot: SetSecrets: set %s: %w", v.key, err)
		}
	}
	return nil
}

// ---------- GitLab API helpers -----------------------------------------------

// tokenSelfInfo is the relevant subset of GET /personal_access_tokens/self.
type tokenSelfInfo struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expires_at"` // "YYYY-MM-DD" or null/empty
	Revoked   bool     `json:"revoked"`
	Active    bool     `json:"active"`
}

// fetchTokenSelf calls GET /personal_access_tokens/self with the given token
// and returns the parsed response.
func fetchTokenSelf(ctx context.Context, token string) (*tokenSelfInfo, error) {
	apiURL := glAPIBase() + "/personal_access_tokens/self"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /personal_access_tokens/self: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, fmt.Errorf("token is invalid or revoked (HTTP 401)")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /personal_access_tokens/self returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var info tokenSelfInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return &info, nil
}

// SetProjectVariable creates or updates a GitLab CI/CD variable on the
// project identified by projectID (URL-encoded "namespace/project").
// It attempts PUT (update) first; falls back to POST (create) on 404.
//
// Variables are created as masked+protected to prevent log exposure and to
// restrict availability to protected branches/tags.
func SetProjectVariable(ctx context.Context, token, projectID, key, value string) error {
	apiBase := glAPIBase() + "/projects/" + projectID + "/variables/" + url.PathEscape(key)

	body := url.Values{}
	body.Set("value", value)
	body.Set("masked", "true")
	body.Set("protected", "true")
	body.Set("variable_type", "env_var")

	// Try PUT (update existing variable).
	if err := doVariableRequest(ctx, token, http.MethodPut, apiBase, body); err == nil {
		return nil
	}

	// Variable may not exist yet — try POST (create).
	createURL := glAPIBase() + "/projects/" + projectID + "/variables"
	body.Set("key", key)
	return doVariableRequest(ctx, token, http.MethodPost, createURL, body)
}

// doVariableRequest executes an HTTP request to the GitLab variables API.
func doVariableRequest(ctx context.Context, token, method, rawURL string, form url.Values) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("not found (HTTP 404)")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s returned HTTP %d: %s", method, resp.StatusCode,
			strings.TrimSpace(string(b)))
	}
	return nil
}

// ValidateToken calls GET /personal_access_tokens/self and checks that:
//  1. The token is active (not revoked).
//  2. All required scopes are present.
//  3. The token has not expired.
//  4. If it expires within warnDays, a warning is returned.
//
// Returns (info, nil, warning_message) on success (warning may be empty).
// Returns an error when the token is invalid, revoked, missing required scopes,
// or already expired.
func ValidateToken(ctx context.Context, token string, requiredScopes []string, warnDays int) (*tokenSelfInfo, string, error) {
	info, err := fetchTokenSelf(ctx, token)
	if err != nil {
		return nil, "", fmt.Errorf("validate token: %w", err)
	}

	if info.Revoked || !info.Active {
		return nil, "", fmt.Errorf("token %q is revoked or inactive", info.Name)
	}

	// Check scopes.
	present := make(map[string]bool, len(info.Scopes))
	for _, s := range info.Scopes {
		present[s] = true
	}
	var missing []string
	for _, want := range requiredScopes {
		if !present[want] {
			missing = append(missing, want)
		}
	}
	if len(missing) > 0 {
		return nil, "", fmt.Errorf("token %q is missing required scopes: %s (present: %s)",
			info.Name, strings.Join(missing, ", "), strings.Join(info.Scopes, ", "))
	}

	// Check expiry.
	var warning string
	if info.ExpiresAt != "" {
		expiry, parseErr := time.Parse("2006-01-02", info.ExpiresAt)
		if parseErr != nil {
			// Non-fatal: just skip expiry check on unexpected format.
			warning = fmt.Sprintf("token expiry %q has unexpected format; cannot check expiry", info.ExpiresAt)
		} else {
			now := time.Now().UTC()
			if expiry.Before(now) {
				return nil, "", fmt.Errorf("token %q expired on %s — create a new token and re-run `koryph bot create --forge gitlab`",
					info.Name, info.ExpiresAt)
			}
			daysLeft := int(expiry.Sub(now).Hours() / 24)
			if daysLeft <= warnDays {
				warning = fmt.Sprintf("token %q expires in %d day(s) (on %s) — rotate soon with `koryph bot create --forge gitlab`",
					info.Name, daysLeft, info.ExpiresAt)
			}
		}
	}

	return info, warning, nil
}

// VariableInfo is the subset of a GitLab CI/CD variable we care about.
type VariableInfo struct {
	Key   string `json:"key"`
	Value string `json:"-"` // never echoed; present in API but not used by callers
}

// ListProjectVariables returns the keys of CI/CD variables defined on the
// project identified by projectID. Token must have api or read_api scope.
func ListProjectVariables(ctx context.Context, token, projectID string) ([]string, error) {
	apiURL := glAPIBase() + "/projects/" + url.PathEscape(projectID) + "/variables"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET /projects/.../variables: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET /projects/.../variables returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var vars []VariableInfo
	if err := json.Unmarshal(body, &vars); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}

	keys := make([]string, 0, len(vars))
	for _, v := range vars {
		keys = append(keys, v.Key)
	}
	return keys, nil
}
