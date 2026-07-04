// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/koryph/koryph/internal/fsx"
	"github.com/koryph/koryph/internal/paths"
)

// VaultDefaults is the vault block shared by koryph.project.json and the
// global ~/.koryph/config.json. It records the provider-native container
// (Proton Pass vault name, 1Password vault, file directory, etc.) alongside
// the provider so that every command that stores or fetches a secret can derive
// sensible defaults without repeating the same flags each time.
type VaultDefaults struct {
	// Provider names the vault backend. Must be one of the recognized
	// signing.Provider* constants when non-empty.
	Provider string `json:"provider,omitempty" jsonschema:"enum=protonpass,enum=onepassword,enum=file,enum=encrypted-file,enum=keychain,enum=command,enum=aws_secretsmanager,enum=azure_keyvault,enum=gcp_secretmanager,enum=keepassxc,enum=openbao,enum=vault"`

	// Container is the provider-native grouping:
	//   protonpass       vault name (e.g. "Engineering")
	//   onepassword      vault (e.g. "Personal")
	//   file / encrypted-file  directory under which key files are stored
	//   aws_secretsmanager     secret path prefix
	//   keepassxc              KeePassXC database path / group
	//   HashiCorp / OpenBao    KV mount path
	// Empty means "use the provider's default"; not all providers have a
	// meaningful container concept.
	Container string `json:"container,omitempty"`
}

// Validate returns an error if the VaultDefaults are internally inconsistent.
// An empty struct (Provider == "") is always valid — it means "no defaults set".
func (v *VaultDefaults) Validate() error {
	if v.Provider == "" {
		return nil
	}
	ok := false
	for _, p := range VaultProviders {
		if p == v.Provider {
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("vault.provider %q is not a recognized provider (valid: %s)",
			v.Provider, validProvidersStr())
	}
	return nil
}

// validProvidersStr returns all VaultProviders joined by "|".
func validProvidersStr() string {
	s := ""
	for i, p := range VaultProviders {
		if i > 0 {
			s += "|"
		}
		s += p
	}
	return s
}

// --- GlobalConfig -----------------------------------------------------------

// GlobalConfigFileName is the machine-wide koryph config at ~/.koryph/config.json.
const GlobalConfigFileName = "config.json"

// GlobalConfigPath returns the path to the global koryph config file (honours
// KORYPH_HOME via paths.KoryphHome).
func GlobalConfigPath() string {
	return filepath.Join(paths.KoryphHome(), GlobalConfigFileName)
}

// GlobalConfig is the machine-wide koryph operator config stored at
// ~/.koryph/config.json. It provides per-machine defaults that apply whenever
// no project-level overrides are found.
//
// Add only operator-scoped preferences here — nothing project-specific.
type GlobalConfig struct {
	// Vault sets the operator's default vault provider and container. Used by
	// any command that stores or fetches a secret when no project-level vault
	// block is configured (signing setup, bot create, …).
	Vault *VaultDefaults `json:"vault,omitempty"`
}

// LoadGlobalConfig reads ~/.koryph/config.json, returning an empty
// GlobalConfig when the file is absent. A malformed file is returned as an error.
func LoadGlobalConfig() (*GlobalConfig, error) {
	p := GlobalConfigPath()
	var cfg GlobalConfig
	err := fsx.ReadJSON(p, &cfg)
	if os.IsNotExist(err) {
		return &GlobalConfig{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("global config: %w", err)
	}
	return &cfg, nil
}

// SaveGlobalConfig writes cfg to ~/.koryph/config.json atomically, creating
// the directory if necessary.
func SaveGlobalConfig(cfg *GlobalConfig) error {
	if cfg.Vault != nil {
		if err := cfg.Vault.Validate(); err != nil {
			return fmt.Errorf("global config: %w", err)
		}
	}
	return fsx.WriteJSONAtomic(GlobalConfigPath(), cfg)
}

// --- Unified resolution seam ------------------------------------------------

// ResolveVaultDefaults walks the precedence ladder and returns the first
// non-empty vault defaults:
//
//  1. project.vault block (koryph.project.json — the new explicit block)
//  2. project.signing block provider + vault_name (legacy proxy: keeps existing
//     projects working without a vault block)
//  3. global ~/.koryph/config.json vault block
//  4. Empty VaultDefaults{} — caller falls through to its own OS default.
//
// The returned error is non-nil only when a config file exists but cannot be
// parsed; absent files are silently treated as "not configured".
//
// Callers apply explicit CLI flags BEFORE calling this function:
//
//	provider := flagProvider           // from --vault-provider or --provider
//	container := flagVaultName         // from --vault-name
//	if provider == "" {
//	    d, _ := signing.ResolveVaultDefaults(rec.Root)
//	    provider = d.Provider
//	    if container == "" { container = d.Container }
//	}
func ResolveVaultDefaults(projectRoot string) (VaultDefaults, error) {
	// 1. Project vault block.
	if projectRoot != "" {
		if d, ok, err := readProjectVaultBlock(projectRoot); err != nil {
			return VaultDefaults{}, err
		} else if ok && d.Provider != "" {
			return d, nil
		}
	}

	// 2. Project signing block (legacy proxy).
	if projectRoot != "" {
		if d, ok := readProjectSigningProxy(projectRoot); ok {
			return d, nil
		}
	}

	// 3. Global config.
	if d, ok, err := readGlobalVaultDefaults(); err != nil {
		return VaultDefaults{}, err
	} else if ok && d.Provider != "" {
		return d, nil
	}

	return VaultDefaults{}, nil
}

// readProjectVaultBlock reads the vault block from koryph.project.json.
// Returns (defaults, found, error). found=false when the file or block is absent.
func readProjectVaultBlock(projectRoot string) (VaultDefaults, bool, error) {
	cfgPath := filepath.Join(projectRoot, "koryph.project.json")
	data, err := os.ReadFile(cfgPath)
	if os.IsNotExist(err) {
		return VaultDefaults{}, false, nil
	}
	if err != nil {
		return VaultDefaults{}, false, nil // unreadable — safe to skip
	}
	var min struct {
		Vault *VaultDefaults `json:"vault,omitempty"`
	}
	if err := json.Unmarshal(data, &min); err != nil {
		return VaultDefaults{}, false, nil // malformed — safe to skip
	}
	if min.Vault == nil || min.Vault.Provider == "" {
		return VaultDefaults{}, false, nil
	}
	return *min.Vault, true, nil
}

// readProjectSigningProxy reads the signing block from koryph.project.json and
// returns a VaultDefaults derived from it for backwards compatibility.
// The signing.provider maps to VaultDefaults.Provider; signing.vault_name maps
// to VaultDefaults.Container (it is the provider-native vault grouping captured
// during signing setup).
func readProjectSigningProxy(projectRoot string) (VaultDefaults, bool) {
	cfgPath := filepath.Join(projectRoot, "koryph.project.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return VaultDefaults{}, false
	}
	var min struct {
		Signing *struct {
			Provider  string `json:"provider"`
			VaultName string `json:"vault_name"`
		} `json:"signing,omitempty"`
	}
	if err := json.Unmarshal(data, &min); err != nil {
		return VaultDefaults{}, false
	}
	if min.Signing == nil || min.Signing.Provider == "" {
		return VaultDefaults{}, false
	}
	return VaultDefaults{
		Provider:  min.Signing.Provider,
		Container: min.Signing.VaultName,
	}, true
}

// readGlobalVaultDefaults reads the vault block from ~/.koryph/config.json.
func readGlobalVaultDefaults() (VaultDefaults, bool, error) {
	cfg, err := LoadGlobalConfig()
	if err != nil {
		return VaultDefaults{}, false, err
	}
	if cfg.Vault == nil || cfg.Vault.Provider == "" {
		return VaultDefaults{}, false, nil
	}
	return *cfg.Vault, true, nil
}
