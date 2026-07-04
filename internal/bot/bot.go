// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package bot implements the GitHub App Manifest flow for the 'koryph bot'
// command family.  It handles app creation (the localhost-redirect manifest
// dance), credential persistence, and installation URL generation.
//
// Credentials are stored at ~/.koryph/bots/<name>.json (mode 0600) and are
// never printed to the terminal.  The private key is the only secret in the
// file; App ID, slug, and owner are also recorded so that downstream commands
// (release setup, doctor) can validate the installation without re-reading
// the PEM.
//
// # Key storage modes
//
// Pointer mode (Provider + KeyRef set): the private key lives in the vault
// specified by Provider; the JSON file is a credential pointer only. Use
// ResolveKey to fetch the key at JWT-mint time.
//
// Inline mode (Provider empty, PEM set): the private key is stored directly
// in the JSON file (legacy / back-compat).  New bots created by
// 'koryph bot create' always use a vault-backed or protected-file provider via
// the no-vault fallback ladder; use --plaintext to opt into inline storage.
package bot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/signing"
)

// nameRe validates a bot name: lowercase letters, digits, and hyphens only,
// 1–39 characters (GitHub App slug constraints).
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,38}$`)

// Config is the credential record persisted to ~/.koryph/bots/<name>.json.
// It holds only the fields required by downstream consumers; the webhook
// secret (not requested) and client_id/secret are intentionally omitted to
// keep the surface minimal.
//
// Key storage: either inline (PEM) or pointer (Provider + KeyRef).
// Use IsPointer to distinguish and ResolveKey to fetch the key at use time.
type Config struct {
	Name   string `json:"name"`
	AppID  int64  `json:"app_id"`
	Slug   string `json:"slug"`
	Owner  string `json:"owner"`
	Public bool   `json:"public"`

	// PEM is the RSA private key in inline mode. It is stored at 0600 and must
	// never be printed to a terminal. Empty when Provider is set (pointer mode).
	// Preserved for back-compat: existing bot files with only a "pem" field
	// continue to load and operate without any migration.
	PEM string `json:"pem,omitempty"`

	// Provider names the vault backend for pointer-mode credentials. Supported
	// values mirror signing.VaultProviders (protonpass, onepassword,
	// encrypted-file, keychain, file, aws_secretsmanager, …). Empty = inline
	// mode (PEM field contains the key).
	Provider string `json:"provider,omitempty"`

	// KeyRef is the provider-specific reference for the private key:
	//   protonpass    pass:// URI
	//   onepassword   op:// reference
	//   keychain      macOS Keychain service name (e.g. "koryph-bot-mybot")
	//   encrypted-file file path of the age-encrypted file
	//   file          plaintext file path (back-compat / --plaintext)
	//   cloud providers: provider-native ARN or secret name
	// Required when Provider is non-empty.
	KeyRef string `json:"key_ref,omitempty"`
}

// IsPointer reports whether the credentials use vault-backed or protected-file
// key storage (Provider and KeyRef set) rather than an inline PEM.
// Pointer-mode configs must go through ResolveKey to obtain the key material.
func (c *Config) IsPointer() bool {
	return c.Provider != ""
}

// BotsDir returns the directory that stores bot credential files.
// It honours KORYPH_HOME via paths.KoryphHome().
func BotsDir() string { return filepath.Join(paths.KoryphHome(), "bots") }

// BotPath returns the credential file path for the given bot name.
func BotPath(name string) string { return filepath.Join(BotsDir(), name+".json") }

// Save persists cfg to BotPath(cfg.Name) with mode 0600, creating BotsDir
// if necessary.
func Save(cfg *Config) error {
	if err := os.MkdirAll(BotsDir(), 0o700); err != nil {
		return fmt.Errorf("bot save: mkdir bots dir: %w", err)
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("bot save: marshal: %w", err)
	}
	path := BotPath(cfg.Name)
	// Write to a sibling temp file then rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("bot save: write: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("bot save: rename: %w", err)
	}
	return nil
}

// Load reads and parses the credential file for the named bot.
func Load(name string) (*Config, error) {
	data, err := os.ReadFile(BotPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("bot %q not found (run `koryph bot create --name %s` first)", name, name)
	}
	if err != nil {
		return nil, fmt.Errorf("bot load: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("bot load: unmarshal: %w", err)
	}
	return &cfg, nil
}

// List returns the names of all stored bots (alphabetical).
func List() ([]string, error) {
	entries, err := os.ReadDir(BotsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("bot list: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		n := e.Name()
		// Exclude *.gitlab.json — those are GitLab bot credentials read by
		// ListGitLab() and must not be parsed as GitHub App configs.
		if strings.HasSuffix(n, ".json") && !strings.HasSuffix(n, ".gitlab.json") {
			names = append(names, strings.TrimSuffix(n, ".json"))
		}
	}
	return names, nil
}

// ValidateName returns an error if name is not a valid bot name.
func ValidateName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("invalid bot name %q: must match [a-z0-9][a-z0-9-]{0,38}", name)
	}
	return nil
}

// CreateOptions controls the manifest flow for 'koryph bot create'.
type CreateOptions struct {
	// Name is the GitHub App name.  If empty, Create returns an error.
	Name string
	// Org is the owning organization.  If empty the app is created under the
	// authenticated user's account.
	Org string
	// Public controls whether the app is installable by any user (true) or
	// only by the creating account (false).
	Public bool
	// Headless suppresses browser opening when true; the manifest URL is
	// printed to Out instead.
	Headless bool
	// Out receives progress messages.
	Out io.Writer
	// Timeout caps the entire flow (browser open → redirect caught).
	// Defaults to 5 minutes.
	Timeout time.Duration

	// VaultProvider selects where the private key is stored after creation.
	// When empty, Create applies the no-vault fallback ladder:
	//   configured vault (project signing block) > keychain (darwin) > encrypted-file.
	// Override with a specific signing.Provider* constant or the --vault-provider flag.
	// Use Plaintext=true to bypass the ladder and write a plaintext PEM file.
	VaultProvider string

	// KeyRef is the provider-specific reference for the new key.
	// Required for CLI-backed vault providers (protonpass, onepassword, etc.).
	// Auto-derived for keychain ("koryph-bot-<name>") and encrypted-file
	// ("<BotsDir>/<name>.age") when left empty.
	KeyRef string

	// Plaintext, when true, bypasses the fallback ladder and stores the private
	// key as a plaintext PEM in the credential JSON (inline mode).
	// This is the legacy behavior and requires an explicit opt-in.
	// Only use for environments where no vault or encrypted storage is feasible.
	Plaintext bool

	// ProjectRoot is the managed project root; used by resolveVaultDefaults to
	// read the project signing block for provider/key_ref defaults.
	// Empty = no project context (personal bot not associated with a project).
	ProjectRoot string

	// Passphrase is the age encryption passphrase used when VaultProvider is
	// "encrypted-file". If empty, the passphrase is read from KORYPH_PASSPHRASE
	// or prompted on the controlling TTY.
	Passphrase string
}

// Create runs the GitHub App Manifest flow:
//
//  1. Starts a localhost HTTP server on an ephemeral port.
//  2. Serves an HTML page that auto-submits the manifest form to GitHub.
//  3. Opens the browser (or prints the URL when headless).
//  4. Waits for the GitHub redirect with code=XXX.
//  5. Exchanges the code via POST /app-manifests/{code}/conversions.
//  6. Saves the credentials to ~/.koryph/bots/<name>.json (0600).
//
// Returns the saved *Config on success.
func Create(ctx context.Context, opts CreateOptions) (*Config, error) {
	if opts.Name == "" {
		return nil, errors.New("bot create: name is required")
	}
	if err := ValidateName(opts.Name); err != nil {
		return nil, err
	}
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Minute
	}
	out := opts.Out
	if out == nil {
		out = io.Discard
	}

	// Bind an ephemeral port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("bot create: listen: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	redirectURL := fmt.Sprintf("http://127.0.0.1:%d/callback", port)

	manifest := buildManifest(opts.Name, opts.Public, redirectURL)
	manifestJSON, err := json.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("bot create: marshal manifest: %w", err)
	}

	// GitHub target URL for the form POST.
	var ghTarget string
	if opts.Org != "" {
		ghTarget = fmt.Sprintf("https://github.com/organizations/%s/settings/apps/new", url.PathEscape(opts.Org))
	} else {
		ghTarget = "https://github.com/settings/apps/new"
	}

	// codeCh receives the redirect code (or an error string prefixed with "err:").
	codeCh := make(chan string, 1)

	mux := http.NewServeMux()
	// "/" serves an auto-submitting form that POSTs the manifest to GitHub.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// JSON must be HTML-escaped when embedded in a value attribute.
		escaped := htmlEscape(string(manifestJSON))
		fmt.Fprintf(w, autoFormHTML, ghTarget, escaped)
	})
	// "/callback" catches GitHub's redirect after the user clicks Create.
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error_description")
			if errMsg == "" {
				errMsg = "no code in callback"
			}
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, callbackErrorHTML)
			codeCh <- "err:" + errMsg
			return
		}
		fmt.Fprint(w, callbackSuccessHTML)
		codeCh <- code
	})

	srv := &http.Server{
		Handler:     mux,
		ReadTimeout: 10 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	localURL := fmt.Sprintf("http://127.0.0.1:%d/", port)
	if opts.Headless {
		fmt.Fprintf(out, "Open this URL in your browser to create the GitHub App:\n  %s\n", localURL)
	} else {
		fmt.Fprintf(out, "Opening browser to create the GitHub App…\n")
		fmt.Fprintf(out, "(If the browser does not open, visit: %s)\n", localURL)
		openBrowser(localURL)
	}
	fmt.Fprintf(out, "Waiting for GitHub callback (timeout %s)…\n", opts.Timeout)

	timer := time.NewTimer(opts.Timeout)
	defer timer.Stop()

	var code string
	select {
	case v := <-codeCh:
		if strings.HasPrefix(v, "err:") {
			return nil, fmt.Errorf("bot create: GitHub callback error: %s", strings.TrimPrefix(v, "err:"))
		}
		code = v
	case <-timer.C:
		return nil, fmt.Errorf("bot create: timed out after %s waiting for GitHub callback", opts.Timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	fmt.Fprintf(out, "Exchanging code with GitHub…\n")
	cfg, err := exchangeCode(ctx, code, opts.Name, opts.Public)
	if err != nil {
		return nil, err
	}

	// Store the private key via the selected provider (vault, protected file, or
	// inline plaintext).  exchangeCode returns cfg with PEM set; storeKey either
	// moves it into the vault (clearing cfg.PEM and setting Provider/KeyRef) or
	// leaves it inline when Plaintext=true.
	if err := storeKeyAfterCreate(ctx, cfg, opts, out); err != nil {
		return nil, fmt.Errorf("bot create: store private key: %w", err)
	}

	if err := Save(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// storeKeyAfterCreate resolves the vault provider and stores the private key
// material from cfg.PEM.  On success it mutates cfg: pointer-mode configs have
// cfg.PEM cleared and cfg.Provider/cfg.KeyRef set; inline mode is unchanged.
func storeKeyAfterCreate(ctx context.Context, cfg *Config, opts CreateOptions, out io.Writer) error {
	if opts.Plaintext {
		// Explicit inline mode: keep cfg.PEM as-is, no vault interaction.
		fmt.Fprintf(out, "  ⚠  storing private key inline (--plaintext); "+
			"consider `koryph bot vault-migrate --name %s` to protect it later\n", cfg.Name)
		return nil
	}

	// Resolve provider from: explicit flag > project vault block > project
	// signing block (legacy) > global config > OS default.
	provider := opts.VaultProvider
	if provider == "" {
		projProvider, _, _ := resolveVaultDefaults(opts.ProjectRoot)
		if projProvider != "" {
			provider = projProvider
		} else {
			provider = signing.ResolveDefaultProvider()
		}
	}

	// Resolve key_ref from: explicit flag > auto-derive per provider.
	keyRef := opts.KeyRef
	if keyRef == "" {
		keyRef = defaultKeyRef(provider, cfg.Name)
	}

	pemBytes := []byte(cfg.PEM)

	var storeErr error
	switch provider {
	case signing.ProviderEncryptedFile:
		passphrase := opts.Passphrase
		if passphrase == "" {
			// Prompt interactively; signing.PromptPassphraseOnce reads from /dev/tty.
			passphrase, storeErr = signing.PromptPassphraseOnce(
				fmt.Sprintf("Passphrase for %s (new encrypted key): ", keyRef))
			if storeErr != nil {
				return fmt.Errorf("encrypted-file: %w", storeErr)
			}
		}
		storeErr = signing.StoreEncryptedFile(keyRef, pemBytes, passphrase)
		if storeErr == nil {
			fmt.Fprintf(out, "  ✓ private key encrypted and stored at %s\n", keyRef)
			fmt.Fprintf(out, "    (same posture as a passphrase-protected ~/.ssh key — keep it backed up, never commit it)\n")
		}

	case signing.ProviderKeychain:
		storeErr = signing.StoreKeychain(keyRef, pemBytes)
		if storeErr == nil {
			fmt.Fprintf(out, "  ✓ private key stored in macOS Keychain (%s)\n", keyRef)
			fmt.Fprintf(out, "    (same posture as a passphrase-protected ~/.ssh key — keep it backed up)\n")
		}

	default:
		// Generic CLI-backed vault provider.
		storeErr = signing.StoreSecret(ctx, provider, keyRef, pemBytes, "")
		if storeErr == nil {
			fmt.Fprintf(out, "  ✓ private key stored in %s (ref: %s)\n", provider, keyRef)
		}
	}

	if storeErr != nil {
		return fmt.Errorf("provider %s: %w", provider, storeErr)
	}

	// Switch cfg to pointer mode: clear inline PEM, set Provider/KeyRef.
	cfg.Provider = provider
	cfg.KeyRef = keyRef
	cfg.PEM = ""
	return nil
}

// DefaultKeyRef returns a provider-appropriate key reference when the user
// has not supplied --key-ref. Exported so the CLI and tests can call it
// without duplicating the per-provider defaults.
func DefaultKeyRef(provider, botName string) string {
	return defaultKeyRef(provider, botName)
}

// defaultKeyRef is the internal implementation of DefaultKeyRef.
func defaultKeyRef(provider, botName string) string {
	switch provider {
	case signing.ProviderKeychain:
		return "koryph-bot-" + botName
	case signing.ProviderEncryptedFile:
		return filepath.Join(BotsDir(), botName+".age")
	case signing.ProviderFile:
		return filepath.Join(BotsDir(), botName+".pem")
	default:
		// For CLI-backed providers, there is no universal default — the caller
		// must supply --key-ref.  Return a placeholder that will fail fast.
		return "koryph-bot-" + botName
	}
}

// InstallURL returns the browser URL for installing the named bot.
func InstallURL(cfg *Config) string {
	return fmt.Sprintf("https://github.com/apps/%s/installations/new", cfg.Slug)
}

// --- internal helpers -------------------------------------------------------

// appManifest is the JSON manifest posted to the GitHub App Manifest endpoint.
// Only the fields the task specifies are included.
type appManifest struct {
	Name        string            `json:"name"`
	URL         string            `json:"url"`
	RedirectURL string            `json:"redirect_url"`
	Public      bool              `json:"public"`
	Permissions map[string]string `json:"default_permissions"`
	Events      []string          `json:"default_events"`
}

func buildManifest(name string, public bool, redirectURL string) appManifest {
	return appManifest{
		Name:        name,
		URL:         "https://github.com/koryph/koryph",
		RedirectURL: redirectURL,
		Public:      public,
		Permissions: map[string]string{
			"contents":      "write",
			"pull_requests": "write",
		},
		Events: []string{},
	}
}

// conversionResponse is the shape returned by
// POST /app-manifests/{code}/conversions.
type conversionResponse struct {
	ID    int64  `json:"id"`
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	Owner struct {
		Login string `json:"login"`
	} `json:"owner"`
	PEM string `json:"pem"`
}

func exchangeCode(ctx context.Context, code, name string, public bool) (*Config, error) {
	apiURL := fmt.Sprintf("https://api.github.com/app-manifests/%s/conversions", url.PathEscape(code))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, http.NoBody)
	if err != nil {
		return nil, fmt.Errorf("bot create: build exchange request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("bot create: exchange request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("bot create: exchange failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var conv conversionResponse
	if err := json.Unmarshal(body, &conv); err != nil {
		return nil, fmt.Errorf("bot create: parse exchange response: %w", err)
	}
	if conv.PEM == "" {
		return nil, errors.New("bot create: exchange response contained no PEM (code may already have been used)")
	}

	return &Config{
		Name:   name,
		AppID:  conv.ID,
		Slug:   conv.Slug,
		Owner:  conv.Owner.Login,
		Public: public,
		PEM:    conv.PEM,
	}, nil
}

// openBrowser opens url in the default OS browser. Errors are silently
// swallowed because the caller already printed a fallback manual URL.
func openBrowser(rawURL string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", rawURL)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", rawURL)
	default: // Linux, BSD, etc.
		cmd = exec.Command("xdg-open", rawURL)
	}
	_ = cmd.Start()
}

// htmlEscape performs minimal HTML attribute escaping for the manifest JSON
// embedded in the auto-form value attribute.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	s = strings.ReplaceAll(s, "'", "&#39;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// autoFormHTML is the page served at "/" — it auto-submits the manifest form
// to GitHub one second after load.  The %s substitutions are:
//  1. GitHub target URL (form action)
//  2. HTML-escaped manifest JSON (hidden input value)
const autoFormHTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Creating GitHub App…</title></head>
<body>
<p>Redirecting you to GitHub to create the App. If nothing happens,
<a id="btn" href="#">click here</a>.</p>
<form id="f" method="post" action="%s">
  <input type="hidden" name="manifest" value="%s">
</form>
<script>
document.getElementById('btn').addEventListener('click', function(e) {
  e.preventDefault(); document.getElementById('f').submit();
});
setTimeout(function() { document.getElementById('f').submit(); }, 500);
</script>
</body>
</html>
`

const callbackSuccessHTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>GitHub App created</title></head>
<body><h2>&#x2705; GitHub App created!</h2>
<p>You can close this tab and return to the terminal.</p>
</body>
</html>
`

const callbackErrorHTML = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Error</title></head>
<body><h2>&#x274C; Something went wrong</h2>
<p>Return to the terminal for details.</p>
</body>
</html>
`
