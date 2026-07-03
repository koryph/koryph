// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package signing provides vault-backed commit and artifact signing for
// managed projects: a per-project policy (Config, embedded in
// koryph.project.json), a vault adapter layer (~/.koryph/vault.json,
// per-provider argv templates so CLI drift is a config edit), an SSH-agent
// bridge (private keys never touch disk — they live in the vault or the
// agent), idempotent git repo configuration, commit-signature verification
// for the merge path, and cosign blob signing for artifacts.
//
// Security invariants:
//   - Fetched secrets are held in memory only; never written to disk and
//     never logged.
//   - The engine consumes only ConfigureRepo / AgentReady / Verify; secret
//     fetching happens in the operator-facing CLI and the cosign path.
package signing

import (
	"fmt"
)

// Signing modes.
const (
	ModeSSH     = "ssh"     // SSH commit signing via an agent-held key (default)
	ModeGitsign = "gitsign" // sigstore keyless signing (OIDC; browser on first use)
)

// Vault providers.
const (
	ProviderProtonPass        = "protonpass"         // Proton Pass CLI (pass-cli)
	ProviderOnePassword       = "onepassword"        // 1Password CLI (op)
	ProviderFile              = "file"               // KeyRef is a filesystem path
	ProviderCommand           = "command"            // user-supplied argv template in vault.json
	ProviderAWSSecretsManager = "aws_secretsmanager" // AWS Secrets Manager CLI (aws)
	ProviderAzureKeyVault     = "azure_keyvault"     // Azure Key Vault CLI (az)
	ProviderGCPSecretManager  = "gcp_secretmanager"  // GCP Secret Manager CLI (gcloud)
	ProviderKeePassXC         = "keepassxc"          // KeePassXC CLI (keepassxc-cli)
	ProviderOpenBao           = "openbao"            // OpenBao CLI (bao)
	ProviderHashiCorpVault    = "vault"              // HashiCorp Vault CLI (vault)
)

// Config is the per-project signing policy, stored as the "signing" block of
// koryph.project.json.
type Config struct {
	// Required makes the engine fail closed: repo signing config is applied
	// at run setup, the SSH agent must hold the key before dispatch, and
	// every merge verifies commit signatures.
	Required bool `json:"required"`

	// Mode is "ssh" (default when empty) or "gitsign".
	Mode string `json:"mode,omitempty" jsonschema:"enum=ssh,enum=gitsign"`

	// Provider names the vault backend: protonpass|onepassword|file|command.
	Provider string `json:"provider,omitempty" jsonschema:"enum=protonpass,enum=onepassword,enum=file,enum=command"`

	// KeyRef is the provider-specific reference for the signing key: a
	// pass:// URI (protonpass), an op:// reference (onepassword), a file
	// path (file), or whatever the command template consumes ({ref}).
	KeyRef string `json:"key_ref,omitempty"`

	// Identity is the signer email; it becomes the allowed-signers principal.
	Identity string `json:"identity,omitempty"`

	// PublicKey is the SSH public key literal ("ssh-ed25519 AAAA..."),
	// captured at setup. It configures user.signingkey and .allowed_signers.
	PublicKey string `json:"public_key,omitempty"`

	// VaultName and ItemTitle record the vault selector used to resolve
	// PublicKey when --vault-name/--item-title was used at setup. They are
	// provenance only — koryph signing status displays them as the key source.
	// Each project pins its own vault item independently; no assumption is made
	// about a single global key.
	VaultName string `json:"vault_name,omitempty"`
	ItemTitle string `json:"item_title,omitempty"`

	// Artifacts enables cosign blob signing (`koryph sign blob`).
	Artifacts bool `json:"artifacts,omitempty"`
}

// EffectiveMode resolves the mode, defaulting to ssh.
func (c *Config) EffectiveMode() string {
	if c.Mode == "" {
		return ModeSSH
	}
	return c.Mode
}

// Validate enforces internal consistency of the signing block.
func (c *Config) Validate() error {
	switch c.Mode {
	case "", ModeSSH, ModeGitsign:
	default:
		return fmt.Errorf("mode must be ssh|gitsign, got %q", c.Mode)
	}
	switch c.Provider {
	case "", ProviderProtonPass, ProviderOnePassword, ProviderFile, ProviderCommand,
		ProviderAWSSecretsManager, ProviderAzureKeyVault, ProviderGCPSecretManager,
		ProviderKeePassXC, ProviderOpenBao, ProviderHashiCorpVault:
	default:
		return fmt.Errorf("provider must be protonpass|onepassword|file|command|aws_secretsmanager|azure_keyvault|gcp_secretmanager|keepassxc|openbao|vault, got %q", c.Provider)
	}
	if c.Required && c.Identity == "" {
		return fmt.Errorf("identity is required when signing is required")
	}
	if c.EffectiveMode() == ModeSSH {
		if c.Provider == "" {
			return fmt.Errorf("provider is required for mode ssh")
		}
		if (c.Provider == ProviderOnePassword || c.Provider == ProviderFile) && c.KeyRef == "" {
			return fmt.Errorf("key_ref is required for provider %s", c.Provider)
		}
		if c.Required && c.PublicKey == "" {
			return fmt.Errorf("public_key is required when signing is required in mode ssh (run `koryph signing setup`)")
		}
	}
	if c.Artifacts {
		if c.Provider == "" {
			return fmt.Errorf("artifacts signing requires a provider")
		}
		if c.Provider != ProviderCommand && c.KeyRef == "" {
			return fmt.Errorf("artifacts signing requires key_ref for provider %s", c.Provider)
		}
	}
	return nil
}
