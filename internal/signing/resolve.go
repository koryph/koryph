// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/execx"
)

// sshPubKeyRe matches the type+blob prefix of an SSH public key. Supported
// key types (per IANA / OpenSSH):
//
//	ssh-ed25519, ssh-rsa
//	ecdsa-sha2-* (e.g. ecdsa-sha2-nistp256)
//	sk-ssh-*     (hardware security keys, e.g. sk-ssh-ed25519@openssh.com)
var sshPubKeyRe = regexp.MustCompile(
	`^(ssh-(ed25519|rsa)|ecdsa-sha2-[A-Za-z0-9-]+|sk-ssh-[A-Za-z0-9-]+(@[A-Za-z0-9.-]+)?)\s+[A-Za-z0-9+/=]+`,
)

// viewTimeout bounds one vault item-view invocation.
const viewTimeout = 60 * time.Second

// ResolvePublicKey resolves the SSH public key deterministically from a vault
// item. Exactly one of (keyRef) or (vaultName + itemTitle) must be non-empty.
//
// The provider's "view" or "view_by_title" template is invoked; the JSON
// output is parsed and ALL string values at every nesting depth are scanned
// for an SSH public key shaped value. Exactly one distinct key must be found:
//
//   - Zero matches → error (fail closed)
//   - Multiple distinct matches → error listing all found
//   - Exactly one match → that key is returned ("type blob" only, comment stripped)
//
// Per-project keys: each project independently pins its own public_key; the
// repo-level user.signingkey selects which key signs commits in that repo.
// ResolvePublicKey makes no assumption about a "global" or "single" key.
func (v *VaultConfig) ResolvePublicKey(ctx context.Context, provider, keyRef, vaultName, itemTitle string) (string, error) {
	var jsonData []byte
	var err error

	switch {
	case keyRef != "":
		jsonData, err = v.viewItem(ctx, provider, keyRef)
	case vaultName != "" && itemTitle != "":
		jsonData, err = v.viewItemByTitle(ctx, provider, vaultName, itemTitle)
	default:
		return "", fmt.Errorf("signing: ResolvePublicKey requires key_ref or vault_name+item_title")
	}
	if err != nil {
		return "", err
	}

	keys, err := scanSSHKeysFromJSON(jsonData)
	if err != nil {
		return "", fmt.Errorf("signing: resolving public key from vault item JSON: %w", err)
	}
	unique := dedupKeys(keys)
	switch len(unique) {
	case 0:
		return "", fmt.Errorf("signing: no SSH public key found in vault item (scanned %d string field(s), none matched)", len(keys))
	case 1:
		return unique[0], nil
	default:
		return "", fmt.Errorf("signing: ambiguous — found %d distinct SSH public keys in vault item: %s",
			len(unique), strings.Join(unique, " | "))
	}
}

// viewItem invokes the provider's "view" template with the given ref (URI).
func (v *VaultConfig) viewItem(ctx context.Context, provider, ref string) ([]byte, error) {
	pt, ok := v.Providers[provider]
	if !ok || len(pt.View) == 0 {
		return nil, fmt.Errorf("signing: provider %q has no view template — add providers.%s.view to %s",
			provider, provider, VaultPath())
	}
	argv := ExpandArgv(pt.View, ref)
	return v.runView(ctx, argv, pt.LoginHint)
}

// viewItemByTitle invokes the provider's "view_by_title" template.
func (v *VaultConfig) viewItemByTitle(ctx context.Context, provider, vaultName, itemTitle string) ([]byte, error) {
	pt, ok := v.Providers[provider]
	if !ok || len(pt.ViewByTitle) == 0 {
		return nil, fmt.Errorf("signing: provider %q has no view_by_title template — add providers.%s.view_by_title to %s",
			provider, provider, VaultPath())
	}
	argv := expandArgvVars(pt.ViewByTitle, map[string]string{
		VaultPlaceholder: vaultName,
		TitlePlaceholder: itemTitle,
	})
	return v.runView(ctx, argv, pt.LoginHint)
}

// runView executes a view command and returns the stdout bytes.
func (v *VaultConfig) runView(ctx context.Context, argv []string, loginHint string) ([]byte, error) {
	res, err := execx.Run(ctx, execx.Cmd{Name: argv[0], Args: argv[1:], Timeout: viewTimeout})
	if err != nil {
		return nil, fmt.Errorf("signing: item view via %s: %w", argv[0], err)
	}
	if res.ExitCode != 0 {
		hint := ""
		if loginHint != "" {
			hint = fmt.Sprintf(" (not logged in? run `%s` first)", loginHint)
		}
		return nil, fmt.Errorf("signing: %s exited %d%s: %s",
			argv[0], res.ExitCode, hint, strings.TrimSpace(res.Stderr))
	}
	return []byte(res.Stdout), nil
}

// expandArgvVars substitutes multiple named placeholders in argv tokens.
func expandArgvVars(argv []string, vars map[string]string) []string {
	out := make([]string, len(argv))
	for i, tok := range argv {
		s := tok
		for placeholder, value := range vars {
			s = strings.ReplaceAll(s, placeholder, value)
		}
		out[i] = s
	}
	return out
}

// scanSSHKeysFromJSON unmarshals data as JSON and walks all string values at
// every nesting depth, collecting those that match sshPubKeyRe. Returns the
// list of matched "type blob" strings (may contain duplicates; caller deduplicates).
func scanSSHKeysFromJSON(data []byte) ([]string, error) {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse JSON: %w", err)
	}
	var found []string
	walkJSON(raw, &found)
	return found, nil
}

// walkJSON recursively visits all values in v, appending SSH key blobs to keys.
func walkJSON(v interface{}, keys *[]string) {
	switch val := v.(type) {
	case string:
		if k := extractSSHKey(val); k != "" {
			*keys = append(*keys, k)
		}
	case map[string]interface{}:
		for _, child := range val {
			walkJSON(child, keys)
		}
	case []interface{}:
		for _, child := range val {
			walkJSON(child, keys)
		}
	}
}

// extractSSHKey returns the "type blob" prefix of s if it looks like an SSH
// public key, or "" otherwise. The comment field (third token) is stripped so
// two keys that differ only in comment are treated as equal.
func extractSSHKey(s string) string {
	s = strings.TrimSpace(s)
	if !sshPubKeyRe.MatchString(s) {
		return ""
	}
	return keyBlob(s)
}

// dedupKeys returns unique strings preserving order of first occurrence.
func dedupKeys(keys []string) []string {
	seen := make(map[string]bool, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// KeyFingerprint returns the SHA256 fingerprint of pubKey in the canonical
// OpenSSH format "SHA256:<base64>" (no padding, matching `ssh-keygen -lf`).
//
// The fingerprint is computed by SHA256-hashing the raw key bytes (the
// base64-decoded blob from the second field of the public key line), matching
// what openssh itself displays.
//
// Returns "(invalid key)" when pubKey cannot be parsed.
func KeyFingerprint(pubKey string) string {
	fields := strings.Fields(strings.TrimSpace(pubKey))
	if len(fields) < 2 {
		return "(invalid key)"
	}
	raw, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil {
		return "(invalid key)"
	}
	sum := sha256.Sum256(raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}
