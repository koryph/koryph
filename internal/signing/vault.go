// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/paths"
	"github.com/koryph/koryph/internal/schemaver"
)

// VaultFileName is the vault adapter file under KoryphHome.
const VaultFileName = "vault.json"

// RefPlaceholder is substituted with Config.KeyRef (or an explicit ref) in
// every argv template token.
const RefPlaceholder = "{ref}"

// VaultPlaceholder and TitlePlaceholder are substituted in view_by_title
// template tokens with the vault name and item title, respectively.
const (
	VaultPlaceholder = "{vault}"
	TitlePlaceholder = "{title}"
)

// fetchTimeout bounds one vault CLI invocation.
const fetchTimeout = 60 * time.Second

// ProviderTemplates are the argv templates for one vault provider. Templates
// exist so CLI drift (a renamed verb, a new flag) is a config edit in
// ~/.koryph/vault.json, not a code change.
type ProviderTemplates struct {
	// Fetch prints an arbitrary secret value to stdout. {ref} tokens are
	// substituted with the item reference. The fetched bytes are held in
	// memory only and never written to disk or logged (errors carry provider
	// stderr, never stdout). Works for SSH private keys, API tokens,
	// passphrases, or any other secret material.
	Fetch []string `json:"fetch,omitempty"`

	// Store writes a secret to the provider. The secret is passed via stdin
	// to avoid process-listing leaks; {ref} tokens are substituted with the
	// item reference. Built-in providers (keychain, encrypted-file) override
	// this with native Go code and ignore the template.
	Store []string `json:"store,omitempty"`

	// AgentLoad loads the provider's SSH keys into the system SSH agent
	// (e.g. `pass-cli ssh-agent load`). Empty = no native agent integration:
	// EnsureAgent falls back to Fetch piped to `ssh-add -`.
	AgentLoad []string `json:"agent_load,omitempty"`

	// View runs an item-view command and prints the item JSON to stdout.
	// {ref} tokens are substituted with the key-ref URI. Used by
	// ResolvePublicKey to extract the SSH public key from the vault item.
	//
	// Default (protonpass): ["pass-cli", "item", "view", "{ref}", "--output", "json"]
	View []string `json:"view,omitempty"`

	// ViewByTitle is like View but selects the item by vault name and title.
	// {vault} is substituted with the vault name; {title} with the item title.
	//
	// Default (protonpass): ["pass-cli", "item", "view", "--vault-name", "{vault}",
	//   "--item-title", "{title}", "--output", "json"]
	ViewByTitle []string `json:"view_by_title,omitempty"`

	// LoginHint is appended to provider errors ("run `pass-cli login` first").
	LoginHint string `json:"login_hint,omitempty"`
}

// VaultConfig is ~/.koryph/vault.json: per-provider argv templates.
type VaultConfig struct {
	SchemaVersion int                          `json:"schema_version"`
	Providers     map[string]ProviderTemplates `json:"providers"`
}

// VaultPath returns the vault adapter file location (honors KORYPH_HOME).
func VaultPath() string { return filepath.Join(paths.KoryphHome(), VaultFileName) }

// DefaultVault returns the built-in provider templates, derived from the real
// CLIs:
//
//   - protonpass: `pass-cli item view pass://SHARE_ID/ITEM_ID[/FIELD]` for
//     secret fetch; `pass-cli ssh-agent load` to load Proton Pass SSH keys
//     into the system agent (scope with `--vault-name {ref}` via a template
//     edit). `pass-cli ssh-agent start` — running Proton Pass AS the agent —
//     is the alternative: point agent_load at a no-op and SSH_AUTH_SOCK at
//     its --socket-path.
//   - onepassword: `op read op://vault/item/field`.
//   - file: handled natively (KeyRef is a path); no templates.
//   - command: intentionally empty — the operator supplies argv in vault.json.
//   - keepassxc: `keepassxc-cli show --key-file KEY --attributes Password DB {ref}`.
//     {ref} is the entry path within the database (e.g. "Engineering/GitHub Token").
//     Headless constraint: keepassxc-cli prompts for the master password interactively
//     unless --key-file is supplied and the database is configured for key-file-only
//     authentication. The DB path and key-file path in the default template are
//     placeholders — override them in ~/.koryph/vault.json. For SSH private keys
//     stored as KeePassXC file attachments, override fetch with:
//     ["keepassxc-cli","attachment-export","--key-file","KEY","DB","{ref}","ATTACH","-"]
//   - openbao: `bao kv get -field=value {ref}`. {ref} is the KV secret path.
//     Auth is ambient (VAULT_TOKEN / VAULT_ADDR env vars, or `bao login`).
//     Override fetch in vault.json to use a different field name.
//   - vault: `vault kv get -field=value {ref}`. {ref} is the KV secret path.
//     Auth is ambient (VAULT_TOKEN / VAULT_ADDR env vars, or `vault login`).
//     Override fetch in vault.json to use a different field name.
func DefaultVault() *VaultConfig {
	return &VaultConfig{
		SchemaVersion: schemaver.Current(schemaver.SigningVault),
		Providers: map[string]ProviderTemplates{
			ProviderProtonPass: {
				Fetch:       []string{"pass-cli", "item", "view", RefPlaceholder},
				AgentLoad:   []string{"pass-cli", "ssh-agent", "load"},
				View:        []string{"pass-cli", "item", "view", RefPlaceholder, "--output", "json"},
				ViewByTitle: []string{"pass-cli", "item", "view", "--vault-name", VaultPlaceholder, "--item-title", TitlePlaceholder, "--output", "json"},
				LoginHint:   "pass-cli login",
			},
			ProviderOnePassword: {
				Fetch:     []string{"op", "read", RefPlaceholder},
				LoginHint: "op signin",
			},
			ProviderCommand: {},

			// Cloud CLI providers — auth is ambient (aws configure / az login /
			// gcloud auth login). The {ref} placeholder carries the provider-native
			// secret identifier (see documentation for each provider below).
			//
			// AWS Secrets Manager: {ref} is the secret ARN or name.
			//   Required IAM: secretsmanager:GetSecretValue on the target secret.
			ProviderAWSSecretsManager: {
				Fetch: []string{
					"aws", "secretsmanager", "get-secret-value",
					"--secret-id", RefPlaceholder,
					"--query", "SecretString",
					"--output", "text",
				},
				LoginHint: "aws configure",
			},

			// Azure Key Vault: {ref} is the secret ID URI
			//   (https://VAULT.vault.azure.net/secrets/NAME).
			//   Required RBAC: Key Vault Secrets User (or legacy "Get" access policy).
			ProviderAzureKeyVault: {
				Fetch: []string{
					"az", "keyvault", "secret", "show",
					"--id", RefPlaceholder,
					"--query", "value",
					"-o", "tsv",
				},
				LoginHint: "az login",
			},

			// GCP Secret Manager: {ref} is the secret name (projects/PROJECT/secrets/NAME
			// or just NAME when gcloud project is already configured).
			//   Required IAM: roles/secretmanager.secretAccessor on the target secret.
			ProviderGCPSecretManager: {
				Fetch: []string{
					"gcloud", "secrets", "versions", "access", "latest",
					"--secret", RefPlaceholder,
				},
				LoginHint: "gcloud auth login",
			},

			// KeePassXC: {ref} is the KeePass entry path within the database
			//   (e.g. "Engineering/GitHub Token").
			//
			// Headless constraint: keepassxc-cli prompts for the master password
			// interactively unless --key-file is supplied and the database is
			// configured for key-file-only authentication (no master password).
			// The /path/to/ values below are placeholders — override them in
			// ~/.koryph/vault.json before use.
			//
			// Alternative for SSH private keys stored as KeePassXC file attachments:
			//   ["keepassxc-cli","attachment-export","--key-file","/path/to/database.keyx",
			//    "/path/to/database.kdbx","{ref}","private_key","-"]
			ProviderKeePassXC: {
				Fetch: []string{
					"keepassxc-cli", "show",
					"--key-file", "/path/to/database.keyx",
					"--attributes", "Password",
					"/path/to/database.kdbx",
					RefPlaceholder,
				},
				LoginHint: "keepassxc-cli --key-file /path/to/database.keyx /path/to/database.kdbx",
			},

			// OpenBao: {ref} is the KV secret path
			//   (e.g. "secret/myapp" or "kv/myapp/credentials").
			// Auth is ambient — VAULT_TOKEN and VAULT_ADDR env vars must be set,
			// or run `bao login` first. The "value" field is fetched by default;
			// override fetch in vault.json to use a different field name.
			ProviderOpenBao: {
				Fetch: []string{
					"bao", "kv", "get", "-field=value",
					RefPlaceholder,
				},
				LoginHint: "bao login",
			},

			// HashiCorp Vault: {ref} is the KV secret path
			//   (e.g. "secret/myapp" or "kv/myapp/credentials").
			// Auth is ambient — VAULT_TOKEN and VAULT_ADDR env vars must be set,
			// or run `vault login` first. The "value" field is fetched by default;
			// override fetch in vault.json to use a different field name.
			ProviderHashiCorpVault: {
				Fetch: []string{
					"vault", "kv", "get", "-field=value",
					RefPlaceholder,
				},
				LoginHint: "vault login",
			},
		},
	}
}

// LoadVault reads ~/.koryph/vault.json layered over the defaults: a
// provider entry on disk replaces that provider's defaults wholesale; absent
// providers keep their defaults. A missing file yields pure defaults.
func LoadVault() (*VaultConfig, error) {
	v := DefaultVault()
	var onDisk VaultConfig
	err := fsx.ReadJSON(VaultPath(), &onDisk)
	if os.IsNotExist(err) {
		return v, nil
	}
	if err != nil {
		return nil, fmt.Errorf("signing: %w", err)
	}
	if verr := schemaver.CheckRead(schemaver.SigningVault, onDisk.SchemaVersion); verr != nil {
		return nil, verr
	}
	for name, pt := range onDisk.Providers {
		v.Providers[name] = pt
	}
	return v, nil
}

// SaveVault writes the vault adapter file atomically. Templates only — never
// secret material.
func SaveVault(v *VaultConfig) error {
	return fsx.WriteJSONAtomic(VaultPath(), v)
}

// ExpandArgv substitutes {ref} in every template token.
func ExpandArgv(argv []string, ref string) []string {
	out := make([]string, len(argv))
	for i, tok := range argv {
		out[i] = strings.ReplaceAll(tok, RefPlaceholder, ref)
	}
	return out
}

// FetchSecret loads the vault adapter config from the default path
// (~/.koryph/vault.json, layered over built-in defaults) and fetches an
// arbitrary secret value from provider using ref.
//
// This is the primary entry point for callers that do not already hold a
// *VaultConfig (e.g. the intake package fetching an API token, or any
// package needing a generic secret). The underlying *VaultConfig.Fetch
// semantics apply: memory only, never written to disk, never logged.
func FetchSecret(ctx context.Context, provider, ref string) ([]byte, error) {
	v, err := LoadVault()
	if err != nil {
		return nil, err
	}
	return v.Fetch(ctx, provider, ref)
}

// Fetch resolves an arbitrary secret value from a provider using the provider's
// fetch template. The secret is returned in memory only: it is NEVER written
// to disk and NEVER logged (errors carry provider stderr, never stdout). Works
// for SSH private keys, API tokens, passphrases, or any other secret material.
//
// Callers that do not already hold a *VaultConfig should use FetchSecret
// instead, which loads vault config automatically.
//
// Span: vault.resolve with provider and key_ref (the reference URI/path, NEVER
// the resolved secret value). Passphrase prompts are never emitted to any log.
func (v *VaultConfig) Fetch(ctx context.Context, provider, ref string) ([]byte, error) {
	// Emit a vault.resolve span. ref is the KEY REFERENCE (e.g. a file path,
	// a pass:// URI, or a Keychain service name) — safe to log. The returned
	// bytes (the actual secret) are NEVER included in any attribute.
	sp := obs.StartSpan(ctx, obs.For("vault"), slog.LevelDebug, "vault.resolve",
		slog.String(obs.KeyProvider, provider),
		slog.String(obs.KeyKeyRef, ref),
	)

	secret, err := v.fetch(ctx, provider, ref)
	sp.End(0, err)
	return secret, err
}

// fetch is the internal implementation of Fetch without span emission.
// Keeping span logic out of the switch makes each branch simpler.
func (v *VaultConfig) fetch(ctx context.Context, provider, ref string) ([]byte, error) {
	switch provider {
	case ProviderFile:
		if ref == "" {
			return nil, fmt.Errorf("signing: provider file needs a key_ref path")
		}
		data, err := os.ReadFile(ref)
		if err != nil {
			return nil, fmt.Errorf("signing: %w", err)
		}
		return data, nil

	case ProviderEncryptedFile:
		return FetchEncryptedFile(ref)

	case ProviderKeychain:
		return FetchKeychain(ref)
	}

	pt, ok := v.Providers[provider]
	if !ok || len(pt.Fetch) == 0 {
		return nil, fmt.Errorf("signing: provider %q has no fetch template — add providers.%s.fetch to %s",
			provider, provider, VaultPath())
	}
	argv := ExpandArgv(pt.Fetch, ref)
	res, err := execx.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Timeout: fetchTimeout})
	if err != nil {
		return nil, fmt.Errorf("signing: fetch via %s: %w", argv[0], err)
	}
	if res.ExitCode != 0 {
		hint := ""
		if pt.LoginHint != "" {
			hint = fmt.Sprintf(" (not logged in? run `%s` first)", pt.LoginHint)
		}
		return nil, fmt.Errorf("signing: %s exited %d%s: %s",
			argv[0], res.ExitCode, hint, strings.TrimSpace(res.Stderr))
	}
	if strings.TrimSpace(res.Stdout) == "" {
		return nil, fmt.Errorf("signing: provider %q returned an empty secret for ref %q", provider, ref)
	}
	return []byte(res.Stdout), nil
}

// StoreSecret stores a secret value with the named provider at the given ref.
// This is the primary entry point for callers that need to persist a new key.
// For built-in providers (keychain, encrypted-file) the secret is written
// natively in Go. For CLI-backed providers the Store template is used with the
// secret on stdin to avoid process-listing leaks.
func StoreSecret(ctx context.Context, provider, ref string, secret []byte, passphrase string) error {
	v, err := LoadVault()
	if err != nil {
		return err
	}
	return v.Store(ctx, provider, ref, secret, passphrase)
}

// Store writes a secret for a provider. passphrase is consumed by the
// encrypted-file provider; it is ignored for CLI-backed providers.
func (v *VaultConfig) Store(ctx context.Context, provider, ref string, secret []byte, passphrase string) error {
	switch provider {
	case ProviderEncryptedFile:
		return StoreEncryptedFile(ref, secret, passphrase)

	case ProviderKeychain:
		return StoreKeychain(ref, secret)

	case ProviderFile:
		if ref == "" {
			return fmt.Errorf("signing: provider file needs a key_ref path")
		}
		if err := os.WriteFile(ref, secret, 0o600); err != nil {
			return fmt.Errorf("signing: %w", err)
		}
		return nil
	}

	// Generic CLI-backed store: pass the secret on stdin.
	pt, ok := v.Providers[provider]
	if !ok || len(pt.Store) == 0 {
		return fmt.Errorf("signing: provider %q has no store template — cannot store key via this provider", provider)
	}
	argv := ExpandArgv(pt.Store, ref)
	res, err := execx.Run(ctx, execx.Cmd{
		Name: argv[0], Args: argv[1:],
		Stdin:   string(secret),
		Timeout: fetchTimeout,
	})
	if err != nil {
		return fmt.Errorf("signing: store via %s: %w", argv[0], err)
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("signing: %s exited %d: %s", argv[0], res.ExitCode, strings.TrimSpace(res.Stderr))
	}
	return nil
}
