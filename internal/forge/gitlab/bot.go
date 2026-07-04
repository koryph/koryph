// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
	"github.com/koryph/koryph/internal/obs"
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

// glAPIBaseOverride is the full API base URL used by tests to redirect HTTP
// calls to a local httptest.Server. When non-empty it takes precedence over
// glAPIBase(). Tests in package gitlab set this directly; production code
// never sets it.
var glAPIBaseOverride string

// glAPIBase returns the base API URL for the configured host. Tests may set
// glAPIBaseOverride to redirect calls to a local test server.
func glAPIBase() string {
	if glAPIBaseOverride != "" {
		return glAPIBaseOverride
	}
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
	info, err := fetchTokenSelf(ctx, jwtOrToken, "")
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
	info, err := fetchTokenSelf(ctx, token, "")
	if err != nil {
		return fmt.Errorf("gitlab bot: SetSecrets: validate token: %w", err)
	}

	expiry := "never"
	if info.ExpiresAt != "" {
		expiry = info.ExpiresAt
	}

	// Pass the raw "namespace/project" slug; SetProjectVariable applies
	// url.PathEscape internally. Pre-escaping here would produce %252F (double-
	// encoded) and a 404 for any namespaced project.

	for _, v := range []struct {
		key    string
		val    string
		masked bool
	}{
		{"KORYPH_BOT_TOKEN", token, true},
		{"KORYPH_BOT_TOKEN_EXPIRY", expiry, false},
	} {
		if err := SetProjectVariable(ctx, token, ownerRepo, v.key, v.val, v.masked); err != nil {
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
// and returns the parsed response. host overrides the default host when
// non-empty (avoids mutating the process-global env).
//
// Span: provider=gitlab, endpoint_class=token_self.
// Redaction contract: token is set only as a PRIVATE-TOKEN header; it is
// NEVER logged. The response body (token metadata, never the raw value) is
// parsed and only token.Name and token.ID are used for logging.
func fetchTokenSelf(ctx context.Context, token, host string) (*tokenSelfInfo, error) {
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "gitlab"),
		slog.String(obs.KeyEndpointClass, "token_self"),
	)

	base := glAPIBase()
	if host != "" && host != "gitlab.com" {
		base = "https://" + host + "/api/v4"
	}
	apiURL := base + "/personal_access_tokens/self"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("GET /personal_access_tokens/self: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusUnauthorized {
		callErr := fmt.Errorf("token is invalid or revoked (HTTP 401)")
		sp.End(resp.StatusCode, callErr)
		return nil, callErr
	}
	if resp.StatusCode != http.StatusOK {
		callErr := fmt.Errorf("GET /personal_access_tokens/self returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
		sp.End(resp.StatusCode, callErr)
		return nil, callErr
	}

	var info tokenSelfInfo
	if err := json.Unmarshal(body, &info); err != nil {
		sp.End(resp.StatusCode, err)
		return nil, fmt.Errorf("parse response: %w", err)
	}

	sp.End(resp.StatusCode, nil)
	return &info, nil
}

// errVariableNotFound is returned by doVariableRequest when the API returns
// HTTP 404 (variable does not exist yet). SetProjectVariable uses this sentinel
// to distinguish a missing-variable 404 from auth, network, or 5xx failures —
// only a 404 should trigger the PUT→POST fallback.
var errVariableNotFound = fmt.Errorf("gitlab variable: not found (HTTP 404)")

// SetProjectVariable creates or updates a GitLab CI/CD variable on the
// project identified by projectID ("namespace/project" slug — URL-encoding is
// applied internally). It attempts PUT (update) first; falls back to POST
// (create) only on HTTP 404. Any other error (auth, network, 5xx) is returned
// directly so the caller sees the real failure, not a misleading create attempt.
//
// masked controls whether the variable is masked in job logs. Only set
// masked=true for actual secrets; GitLab requires masked values to be ≥8
// characters and match [a-zA-Z0-9@:.~] — non-secret or short values must use
// masked=false. Variables are always created as protected to restrict them to
// protected branches/tags.
func SetProjectVariable(ctx context.Context, token, projectID, key, value string, masked bool) error {
	escapedID := url.PathEscape(projectID)
	apiBase := glAPIBase() + "/projects/" + escapedID + "/variables/" + url.PathEscape(key)

	maskedStr := "false"
	if masked {
		maskedStr = "true"
	}

	body := url.Values{}
	body.Set("value", value)
	body.Set("masked", maskedStr)
	body.Set("protected", "true")
	body.Set("variable_type", "env_var")

	// Try PUT (update existing variable). Fall back to POST only on 404; any
	// other error (auth, network, 5xx) is propagated so the caller sees the
	// real failure rather than a misleading "create" attempt.
	putErr := doVariableRequest(ctx, token, http.MethodPut, apiBase, body)
	if putErr == nil {
		return nil
	}
	if putErr != errVariableNotFound {
		return putErr
	}

	// Variable does not exist yet — create it.
	createURL := glAPIBase() + "/projects/" + escapedID + "/variables"
	body.Set("key", key)
	return doVariableRequest(ctx, token, http.MethodPost, createURL, body)
}

// doVariableRequest executes an HTTP request to the GitLab variables API.
// It returns [errVariableNotFound] on HTTP 404 so callers can distinguish
// a missing-variable response from other failures.
//
// Span: provider=gitlab, endpoint_class=variable_{method}.
// Redaction contract: token is set only as a PRIVATE-TOKEN header; the form
// body (which may contain a variable value) is NEVER logged. Only the HTTP
// method, status code, and latency are recorded.
func doVariableRequest(ctx context.Context, token, method, rawURL string, form url.Values) error {
	endpointClass := "variable_" + strings.ToLower(method)
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "gitlab"),
		slog.String(obs.KeyEndpointClass, endpointClass),
	)

	req, err := http.NewRequestWithContext(ctx, method, rawURL,
		strings.NewReader(form.Encode()))
	if err != nil {
		sp.End(0, err)
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return fmt.Errorf("%s: %w", method, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		sp.End(resp.StatusCode, errVariableNotFound)
		return errVariableNotFound
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		callErr := fmt.Errorf("%s returned HTTP %d: %s", method, resp.StatusCode,
			strings.TrimSpace(string(b)))
		sp.End(resp.StatusCode, callErr)
		return callErr
	}

	sp.End(resp.StatusCode, nil)
	return nil
}

// ValidateToken calls GET /personal_access_tokens/self and checks that:
//  1. The token is active (not revoked).
//  2. All required scopes are present.
//  3. The token has not expired.
//  4. If it expires within warnDays, a warning is returned.
//
// host overrides the default GitLab host when non-empty (pass cfg.Host or ""
// for gitlab.com). This avoids mutating the process-global KORYPH_GITLAB_HOST.
//
// Returns (info, warning, nil) on success (warning may be empty).
// Returns an error when the token is invalid, revoked, missing required scopes,
// or already expired.
func ValidateToken(ctx context.Context, token, host string, requiredScopes []string, warnDays int) (*tokenSelfInfo, string, error) {
	info, err := fetchTokenSelf(ctx, token, host)
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
//
// Span: provider=gitlab, endpoint_class=list_project_variables.
// Redaction contract: token is set only as a PRIVATE-TOKEN header; variable
// values in the response are parsed but NEVER logged (only key names).
func ListProjectVariables(ctx context.Context, token, projectID string) ([]string, error) {
	sp := obs.StartSpan(ctx, obs.For("forge"), slog.LevelDebug, "forge.api",
		slog.String(obs.KeyProvider, "gitlab"),
		slog.String(obs.KeyEndpointClass, "list_project_variables"),
	)

	apiURL := glAPIBase() + "/projects/" + url.PathEscape(projectID) + "/variables"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, http.NoBody)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		sp.End(0, err)
		return nil, fmt.Errorf("GET /projects/.../variables: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		callErr := fmt.Errorf("GET /projects/.../variables returned HTTP %d: %s",
			resp.StatusCode, strings.TrimSpace(string(body)))
		sp.End(resp.StatusCode, callErr)
		return nil, callErr
	}

	var vars []VariableInfo
	if err := json.Unmarshal(body, &vars); err != nil {
		sp.End(resp.StatusCode, err)
		return nil, fmt.Errorf("parse response: %w", err)
	}

	sp.End(resp.StatusCode, nil)

	keys := make([]string, 0, len(vars))
	for _, v := range vars {
		keys = append(keys, v.Key)
	}
	return keys, nil
}
