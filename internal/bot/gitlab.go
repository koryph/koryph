// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// GitLab bot support: guided access-token creation flow, credential storage,
// attach (CI variable wiring), and check/doctor validators.
//
// # Credential schema
//
// GitLab bots use the same pointer-credential schema as GitHub bots:
//
//	GitLabConfig fields:
//	  Name       string   — bot name (matches BotPath key)
//	  Forge      string   — always "gitlab"
//	  Host       string   — GitLab host, e.g. "gitlab.com"
//	  Project    string   — "namespace/project" the token covers (or "" for PAT)
//	  TokenID    int64    — GitLab token numeric ID
//	  TokenName  string   — human-readable name on GitLab
//	  ExpiresAt  string   — YYYY-MM-DD expiry date or "" for no expiry
//	  Provider   string   — vault provider (same as Config.Provider)
//	  KeyRef     string   — vault key reference
//	  Token      string   — inline token value (legacy / --plaintext)
//
// The file is saved to ~/.koryph/bots/<name>.gitlab.json (mode 0600).
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/forge/gitlab"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/signing"
)

// GitLabConfig is the credential record persisted to
// ~/.koryph/bots/<name>.gitlab.json for GitLab bots.
//
// Key storage mirrors [Config] (GitHub): either inline (Token) or pointer
// (Provider + KeyRef). Use [IsPointerGL] to distinguish and [ResolveGLToken]
// to fetch the token at use time.
type GitLabConfig struct {
	Name      string `json:"name"`
	Forge     string `json:"forge"` // always "gitlab"
	Host      string `json:"host"`  // e.g. "gitlab.com"
	Project   string `json:"project"`
	TokenID   int64  `json:"token_id,omitempty"`
	TokenName string `json:"token_name,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"` // YYYY-MM-DD or ""

	// Token is the access-token value in inline mode (mode 0600; never printed).
	// Empty when Provider is set (pointer mode). Preserved for back-compat.
	Token string `json:"token,omitempty"`

	// Provider names the vault backend (same values as [Config].Provider).
	// Empty = inline mode.
	Provider string `json:"provider,omitempty"`

	// KeyRef is the provider-specific reference. Required when Provider is
	// non-empty.
	KeyRef string `json:"key_ref,omitempty"`
}

// IsPointerGL reports whether the credentials use vault-backed key storage.
func (c *GitLabConfig) IsPointerGL() bool {
	return c.Provider != ""
}

// GitLabBotPath returns the credential file path for the given GitLab bot name.
func GitLabBotPath(name string) string {
	return filepath.Join(BotsDir(), name+".gitlab.json")
}

// SaveGitLab persists cfg to GitLabBotPath(cfg.Name) with mode 0600.
func SaveGitLab(cfg *GitLabConfig) error {
	if err := os.MkdirAll(BotsDir(), 0o700); err != nil {
		return fmt.Errorf("gitlab bot save: mkdir bots dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("gitlab bot save: marshal: %w", err)
	}
	path := GitLabBotPath(cfg.Name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("gitlab bot save: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("gitlab bot save: rename: %w", err)
	}
	return nil
}

// LoadGitLab reads and parses the credential file for the named GitLab bot.
func LoadGitLab(name string) (*GitLabConfig, error) {
	data, err := os.ReadFile(GitLabBotPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("gitlab bot %q not found (run `koryph bot create --forge gitlab --name %s` first)", name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("gitlab bot load: %w", err)
	}
	var cfg GitLabConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("gitlab bot load: unmarshal: %w", err)
	}
	return &cfg, nil
}

// ListGitLab returns the names of all stored GitLab bots (alphabetical).
func ListGitLab() ([]string, error) {
	entries, err := os.ReadDir(BotsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitlab bot list: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		if strings.HasSuffix(n, ".gitlab.json") {
			names = append(names, strings.TrimSuffix(n, ".gitlab.json"))
		}
	}
	return names, nil
}

// ResolveGLToken returns the access-token value for a GitLab bot.
//
// Pointer mode: fetches from the vault via signing.FetchSecret.
// Inline mode:  returns cfg.Token directly (back-compat).
func ResolveGLToken(ctx context.Context, cfg *GitLabConfig) (string, error) {
	if cfg.Provider == "" {
		return cfg.Token, nil
	}
	raw, err := signing.FetchSecret(ctx, cfg.Provider, cfg.KeyRef)
	if err != nil {
		return "", classifyVaultErr(cfg.Provider, err)
	}
	return string(raw), nil
}

// GitLabCreateOptions controls the guided access-token creation flow.
type GitLabCreateOptions struct {
	// Name is the bot name used for the credential file.
	Name string
	// Host is the GitLab host (default: "gitlab.com").
	Host string
	// Project is the "namespace/project" path the token should cover.
	// When empty, a personal access token settings URL is used.
	Project string
	// TokenName is the display name to give the GitLab access token.
	// Defaults to "koryph-bot-<Name>".
	TokenName string
	// RequiredScopes lists the scopes the token must have.
	// Defaults to [api, read_api, write_repository].
	RequiredScopes []string
	// ExpiryWarnDays is the number of days before expiry at which to emit a
	// WARN. Defaults to 30.
	ExpiryWarnDays int
	// Headless suppresses browser opening; the settings URL is printed instead.
	Headless bool
	// Out receives progress messages.
	Out io.Writer
	// VaultProvider and KeyRef mirror [CreateOptions].
	VaultProvider string
	KeyRef        string
	// Plaintext bypasses vault storage (token stored inline).
	Plaintext bool
	// ProjectRoot is used to resolve vault defaults from the project signing block.
	ProjectRoot string
	// Passphrase for encrypted-file vault provider.
	Passphrase string
}

// defaultGLScopes are the scopes required for koryph release automation on
// GitLab: create/update MRs (api), read pipeline status (read_api), push
// commits and tags (write_repository).
var defaultGLScopes = []string{"api", "read_api", "write_repository"}

// GLSettingsURL returns the GitLab access-token settings URL for the given
// host and project. When project is empty, the personal-access-token settings
// URL is returned instead.
func GLSettingsURL(host, project string) string {
	if project != "" {
		encoded := strings.ReplaceAll(project, "/", "%2F")
		return fmt.Sprintf("https://%s/%s/-/settings/access_tokens", host, encoded)
	}
	return fmt.Sprintf("https://%s/-/user_settings/personal_access_tokens", host)
}

// GLBotInstallURL returns a human-readable "installation" URL for a GitLab
// bot — the project's CI/CD variables settings page.
func GLBotInstallURL(cfg *GitLabConfig) string {
	if cfg.Project != "" {
		encoded := strings.ReplaceAll(cfg.Project, "/", "%2F")
		return fmt.Sprintf("https://%s/%s/-/settings/ci_cd#js-cicd-variables-settings",
			cfg.Host, encoded)
	}
	return fmt.Sprintf("https://%s/-/user_settings/personal_access_tokens", cfg.Host)
}

// ExpiryWarnDays is the default number of days before expiry at which to
// emit a WARN in bot check / doctor.
const ExpiryWarnDays = 30

// CreateGitLab runs the guided access-token creation flow for GitLab:
//
//  1. Determines the correct settings URL (project or personal access tokens).
//  2. Opens the URL in the browser (or prints it in headless mode).
//  3. Prompts the user to paste the newly-created token into the terminal.
//  4. Validates the token via GET /personal_access_tokens/self, checking
//     that all required scopes are present and the token is not expired.
//  5. Stores the token via the vault ladder (same logic as GitHub Create).
//  6. Persists a GitLabConfig to ~/.koryph/bots/<name>.gitlab.json (0600).
//
// Returns the saved *GitLabConfig on success.
func CreateGitLab(ctx context.Context, opts GitLabCreateOptions) (*GitLabConfig, error) {
	if opts.Name == "" {
		return nil, errors.New("gitlab bot create: name is required")
	}
	if err := ValidateName(opts.Name); err != nil {
		return nil, err
	}
	if opts.Host == "" {
		opts.Host = "gitlab.com"
	}
	if opts.TokenName == "" {
		opts.TokenName = "koryph-bot-" + opts.Name
	}
	if len(opts.RequiredScopes) == 0 {
		opts.RequiredScopes = defaultGLScopes
	}
	if opts.ExpiryWarnDays == 0 {
		opts.ExpiryWarnDays = ExpiryWarnDays
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	setURL := GLSettingsURL(opts.Host, opts.Project)

	// Step 1: display guidance and open browser.
	fmt.Fprintf(out, "\nGitLab access-token creation\n")
	fmt.Fprintf(out, "============================\n\n")
	if opts.Project != "" {
		fmt.Fprintf(out, "Project: %s\n", opts.Project)
	} else {
		fmt.Fprintf(out, "Scope:   personal access token (no --project specified)\n")
	}
	fmt.Fprintf(out, "Host:    %s\n\n", opts.Host)
	fmt.Fprintf(out, "Required scopes: %s\n\n", strings.Join(opts.RequiredScopes, ", "))
	fmt.Fprintf(out, "Settings URL:\n  %s\n\n", setURL)

	if opts.Headless {
		fmt.Fprintf(out, "Open the URL above in your browser to create the access token.\n")
	} else {
		fmt.Fprintf(out, "Opening your browser to the token settings page...\n")
		fmt.Fprintf(out, "(If the browser does not open, visit: %s)\n\n", setURL)
		openBrowser(setURL)
	}

	fmt.Fprintf(out, "Instructions:\n")
	fmt.Fprintf(out, "  1. Click \"Add new token\".\n")
	fmt.Fprintf(out, "  2. Set Token Name: %q\n", opts.TokenName)
	fmt.Fprintf(out, "  3. Set expiry date (recommended: 365 days from today).\n")
	fmt.Fprintf(out, "  4. Select scopes: %s\n", strings.Join(opts.RequiredScopes, ", "))
	fmt.Fprintf(out, "  5. Click \"Create project access token\".\n")
	fmt.Fprintf(out, "  6. Copy the token value shown (it is only displayed once).\n\n")

	// Step 2: read token from TTY (echo-suppressed, consistent with signing).
	token, err := signing.PromptPassphraseOnce("Paste the access token (input will not be echoed): ")
	if err != nil {
		return nil, fmt.Errorf("gitlab bot create: read token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, errors.New("gitlab bot create: no token entered")
	}

	// Step 3: validate token via GitLab API.
	fmt.Fprintf(out, "\nValidating token with GitLab API (%s)...\n", opts.Host)
	if opts.Host != "gitlab.com" {
		// Point the gitlab package at the correct host for this call.
		oldHost := os.Getenv("KORYPH_GITLAB_HOST")
		if err := os.Setenv("KORYPH_GITLAB_HOST", opts.Host); err != nil {
			return nil, fmt.Errorf("gitlab bot create: set KORYPH_GITLAB_HOST: %w", err)
		}
		defer func() { _ = os.Setenv("KORYPH_GITLAB_HOST", oldHost) }()
	}

	info, warning, err := gitlab.ValidateToken(ctx, token, opts.RequiredScopes, opts.ExpiryWarnDays)
	if err != nil {
		return nil, fmt.Errorf("gitlab bot create: token validation failed: %w\n  Ensure the token has scopes: %s",
			err, strings.Join(opts.RequiredScopes, ", "))
	}
	fmt.Fprintf(out, "  Token validated: name=%q scopes=%s\n",
		info.Name, strings.Join(info.Scopes, ", "))
	if warning != "" {
		fmt.Fprintf(out, "  WARNING: %s\n", warning)
	}

	cfg := &GitLabConfig{
		Name:      opts.Name,
		Forge:     "gitlab",
		Host:      opts.Host,
		Project:   opts.Project,
		TokenID:   int64(info.ID),
		TokenName: info.Name,
		ExpiresAt: info.ExpiresAt,
		Token:     token,
	}

	// Step 4: store token via vault ladder.
	if err := storeGLTokenAfterCreate(ctx, cfg, opts, out); err != nil {
		return nil, fmt.Errorf("gitlab bot create: store token: %w", err)
	}

	// Step 5: persist credential file.
	if err := SaveGitLab(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// storeGLTokenAfterCreate resolves the vault provider and stores the token
// from cfg.Token. On success it mutates cfg to pointer mode.
func storeGLTokenAfterCreate(ctx context.Context, cfg *GitLabConfig, opts GitLabCreateOptions, out io.Writer) error {
	if opts.Plaintext {
		fmt.Fprintf(out, "  WARNING: storing access token inline (--plaintext); "+
			"consider vault migration to protect it later\n")
		return nil
	}

	provider := opts.VaultProvider
	if provider == "" {
		projProvider, _, _ := resolveVaultDefaults(opts.ProjectRoot)
		if projProvider != "" {
			provider = projProvider
		} else {
			provider = signing.ResolveDefaultProvider()
		}
	}

	keyRef := opts.KeyRef
	if keyRef == "" {
		keyRef = defaultGLKeyRef(provider, cfg.Name)
	}

	tokenBytes := []byte(cfg.Token)

	var storeErr error
	switch provider {
	case signing.ProviderEncryptedFile:
		passphrase := opts.Passphrase
		if passphrase == "" {
			passphrase, storeErr = signing.PromptPassphraseOnce(
				fmt.Sprintf("Passphrase for %s (new encrypted token): ", keyRef))
			if storeErr != nil {
				return fmt.Errorf("encrypted-file: %w", storeErr)
			}
		}
		storeErr = signing.StoreEncryptedFile(keyRef, tokenBytes, passphrase)
		if storeErr == nil {
			fmt.Fprintf(out, "  token encrypted and stored at %s\n", keyRef)
		}
	case signing.ProviderKeychain:
		storeErr = signing.StoreKeychain(keyRef, tokenBytes)
		if storeErr == nil {
			fmt.Fprintf(out, "  token stored in macOS Keychain (%s)\n", keyRef)
		}
	default:
		storeErr = signing.StoreSecret(ctx, provider, keyRef, tokenBytes, "")
		if storeErr == nil {
			fmt.Fprintf(out, "  token stored in %s (ref: %s)\n", provider, keyRef)
		}
	}

	if storeErr != nil {
		return fmt.Errorf("provider %s: %w", provider, storeErr)
	}

	cfg.Provider = provider
	cfg.KeyRef = keyRef
	cfg.Token = ""
	return nil
}

// defaultGLKeyRef derives a vault key reference for a GitLab bot.
func defaultGLKeyRef(provider, botName string) string {
	switch provider {
	case signing.ProviderKeychain:
		return "koryph-gl-bot-" + botName
	case signing.ProviderEncryptedFile:
		return filepath.Join(paths.KoryphHome(), "bots", botName+".gl.age")
	case signing.ProviderFile:
		return filepath.Join(paths.KoryphHome(), "bots", botName+".gl.token")
	default:
		return "koryph-gl-bot-" + botName
	}
}

// RunGlabBin runs the glab CLI with the given args and returns combined output.
// Binary path is controlled by KORYPH_GLAB_BIN (default: "glab").
func RunGlabBin(args ...string) (string, error) {
	bin := "glab"
	if v := os.Getenv("KORYPH_GLAB_BIN"); v != "" {
		bin = v
	}
	cmd := exec.Command(bin, args...) //nolint:gosec
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("glab %s: %w\n%s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
