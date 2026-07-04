// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/koryph/koryph/internal/signing"
)

// ResolveKey returns the PEM-encoded RSA private key material for a bot.
//
// Two modes:
//
//   - Pointer mode (cfg.Provider != ""): the key is fetched from the vault via
//     signing.FetchSecret. Vault errors are wrapped as *VaultErr with the
//     provider-exact remediation command. The PEM is held in memory only and
//     never written to disk.
//
//   - Inline mode (cfg.Provider == ""): cfg.PEM is returned as-is. Existing
//     bot credential files (pre-vault retrofit) use this path unconditionally,
//     preserving full backward compatibility.
func ResolveKey(ctx context.Context, cfg *Config) (string, error) {
	if cfg.Provider == "" {
		// Inline/back-compat mode — return the stored PEM directly.
		return cfg.PEM, nil
	}
	raw, err := signing.FetchSecret(ctx, cfg.Provider, cfg.KeyRef)
	if err != nil {
		return "", classifyVaultErr(cfg.Provider, err)
	}
	return string(raw), nil
}

// resolveVaultDefaults reads the project's signing block (koryph.project.json)
// to derive a default vault provider and key reference for bot key storage.
// This lets `koryph bot create` (when no --vault-provider flag is given)
// default to the same provider the project already uses for commit signing.
//
// Precedence ladder in callers:
//
//	explicit --vault-provider flag > project signing.provider > signing.ResolveDefaultProvider()
//
// Returns ("", "", nil) when projectRoot is empty, the project file is absent,
// or the signing block is not configured. The empty-provider case is not an
// error: callers fall through to the OS-appropriate default.
func resolveVaultDefaults(projectRoot string) (provider, keyRef string, err error) {
	if projectRoot == "" {
		return "", "", nil
	}
	cfgPath := filepath.Join(projectRoot, "koryph.project.json")
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		return "", "", nil
	}
	if err != nil {
		// Unreadable project file — not a fatal error; fall through to defaults.
		return "", "", nil
	}

	// Minimal parse: we only need the signing.provider and signing.key_ref
	// fields; a full project.Load would pull in unrelated config dependencies.
	var min struct {
		Signing *struct {
			Provider string `json:"provider"`
			KeyRef   string `json:"key_ref"`
		} `json:"signing,omitempty"`
	}
	if err := json.Unmarshal(data, &min); err != nil {
		return "", "", nil // malformed project file — safe to ignore here
	}
	if min.Signing == nil || min.Signing.Provider == "" {
		return "", "", nil
	}
	return min.Signing.Provider, min.Signing.KeyRef, nil
}

// classifyVaultErr inspects the error from signing.FetchSecret and returns a
// *VaultErr with the most precise VaultErrClass and provider-exact remediation.
//
// Classification is heuristic (based on common CLI error messages) and errs on
// the side of VaultErrNotAuthenticated (the most common transient failure).
func classifyVaultErr(provider string, err error) *VaultErr {
	msg := err.Error()
	low := strings.ToLower(msg)

	ve := &VaultErr{
		Provider: provider,
		Detail:   msg,
	}

	switch {
	// CLI binary not on PATH.
	case strings.Contains(low, "executable file not found") ||
		strings.Contains(low, "no such file or directory") && strings.Contains(low, "exec"):
		ve.Class = VaultErrNotInstalled
		ve.Remediation = installRemediation(provider)

	// Session expired / not logged in / wrong passphrase.
	case strings.Contains(low, "not logged in") ||
		strings.Contains(low, "not authenticated") ||
		strings.Contains(low, "unauthenticated") ||
		strings.Contains(low, "unauthorized") ||
		strings.Contains(low, "session") ||
		strings.Contains(low, "passphrase") ||
		strings.Contains(low, "wrong passphrase") ||
		strings.Contains(low, "bad password") ||
		strings.Contains(low, "decrypt"):
		ve.Class = VaultErrNotAuthenticated
		ve.Remediation = authRemediation(provider)

	// Vault sealed or database locked.
	case strings.Contains(low, "sealed") ||
		strings.Contains(low, "locked") && !strings.Contains(low, "file"):
		ve.Class = VaultErrSealedOrLocked
		ve.Remediation = unsealRemediation(provider)

	// Permission denied.
	case strings.Contains(low, "permission denied") ||
		strings.Contains(low, "access denied") ||
		strings.Contains(low, "forbidden") ||
		strings.Contains(low, "unauthorized"):
		ve.Class = VaultErrPermissionDenied
		ve.Remediation = fmt.Sprintf("grant the current session read access to the key item in %q", provider)

	// Ref not found / empty secret.
	case strings.Contains(low, "not found") ||
		strings.Contains(low, "no such item") ||
		strings.Contains(low, "empty secret"):
		ve.Class = VaultErrRefNotFound
		ve.Remediation = fmt.Sprintf(
			"check key_ref in %s — the item may have been deleted or moved in %q",
			BotsDir(), provider)

	default:
		// Default: treat as authentication failure (most common transient cause).
		ve.Class = VaultErrNotAuthenticated
		ve.Remediation = authRemediation(provider)
	}

	return ve
}

func installRemediation(provider string) string {
	switch provider {
	case signing.ProviderProtonPass:
		return "install pass-cli: https://proton.me/pass/download"
	case signing.ProviderOnePassword:
		return "install 1Password CLI: https://developer.1password.com/docs/cli/get-started"
	case signing.ProviderAWSSecretsManager:
		return "install the AWS CLI: https://docs.aws.amazon.com/cli/latest/userguide/install-cliv2.html"
	case signing.ProviderAzureKeyVault:
		return "install the Azure CLI: https://docs.microsoft.com/en-us/cli/azure/install-azure-cli"
	case signing.ProviderGCPSecretManager:
		return "install the gcloud CLI: https://cloud.google.com/sdk/docs/install"
	case signing.ProviderKeePassXC:
		return "install keepassxc-cli: https://keepassxc.org/download/"
	case signing.ProviderOpenBao:
		return "install the OpenBao CLI: https://openbao.org/downloads/"
	case signing.ProviderHashiCorpVault:
		return "install the Vault CLI: https://developer.hashicorp.com/vault/downloads"
	default:
		return fmt.Sprintf("install the CLI for provider %q and ensure it is on PATH", provider)
	}
}

func authRemediation(provider string) string {
	switch provider {
	case signing.ProviderProtonPass:
		return "pass-cli login"
	case signing.ProviderOnePassword:
		return "op signin"
	case signing.ProviderEncryptedFile:
		return "set KORYPH_PASSPHRASE or ensure you have the correct passphrase for the encrypted key file"
	case signing.ProviderAWSSecretsManager:
		return "aws configure  (or set AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY)"
	case signing.ProviderAzureKeyVault:
		return "az login"
	case signing.ProviderGCPSecretManager:
		return "gcloud auth login"
	case signing.ProviderKeePassXC:
		return "open your KeePassXC database and supply the correct master password / key file"
	case signing.ProviderOpenBao:
		return "bao login"
	case signing.ProviderHashiCorpVault:
		return "vault login"
	case signing.ProviderKeychain:
		return "unlock your macOS login Keychain (Keychain Access → unlock)"
	default:
		return fmt.Sprintf("authenticate with the %q provider", provider)
	}
}

func unsealRemediation(provider string) string {
	switch provider {
	case signing.ProviderOpenBao:
		return "bao operator unseal"
	case signing.ProviderHashiCorpVault:
		return "vault operator unseal"
	case signing.ProviderKeePassXC:
		return "open your KeePassXC database"
	default:
		return fmt.Sprintf("unlock or unseal the %q vault", provider)
	}
}
