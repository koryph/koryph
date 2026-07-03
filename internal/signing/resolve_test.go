// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ── JSON scanning ─────────────────────────────────────────────────────────────

// TestScanSSHKeysFromJSONClean verifies that exactly one key is found in a
// well-formed vault item JSON with a single SSH public key.
func TestScanSSHKeysFromJSONClean(t *testing.T) {
	json := `{
		"id":       "abc123",
		"title":    "SSH Signing Key",
		"vault":    {"id": "vault1", "name": "Engineering"},
		"category": "SSH_KEY",
		"fields": [
			{"id": "f1", "label": "public key",  "value": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5test koryph-test"},
			{"id": "f2", "label": "private key (enc)", "value": "base64-encoded-encrypted-blob"}
		]
	}`
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	unique := dedupKeys(keys)
	if len(unique) != 1 {
		t.Fatalf("want 1 unique key, got %d: %v", len(unique), unique)
	}
	if !strings.HasPrefix(unique[0], "ssh-ed25519 ") {
		t.Errorf("key = %q, want ssh-ed25519 prefix", unique[0])
	}
	// Comment must be stripped.
	if strings.Contains(unique[0], "koryph-test") {
		t.Errorf("key retains comment: %q", unique[0])
	}
}

// TestScanSSHKeysFromJSONAmbiguous verifies that two DISTINCT keys are both
// returned (caller must fail closed).
func TestScanSSHKeysFromJSONAmbiguous(t *testing.T) {
	json := `{
		"fields": [
			{"value": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA first-key"},
			{"value": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5BBBB second-key"}
		]
	}`
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	unique := dedupKeys(keys)
	if len(unique) != 2 {
		t.Fatalf("want 2 unique keys, got %d: %v", len(unique), unique)
	}
}

// TestScanSSHKeysFromJSONKeyless verifies that a JSON with no SSH keys returns
// an empty list (caller must fail closed with a helpful error).
func TestScanSSHKeysFromJSONKeyless(t *testing.T) {
	json := `{"title": "Database credentials", "password": "s3cr3t", "notes": "no keys here"}`
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	if len(keys) != 0 {
		t.Errorf("want 0 keys, got %d: %v", len(keys), keys)
	}
}

// TestScanSSHKeysFromJSONDeduplicateSameKey verifies that if the same key
// appears multiple times (e.g. in "public key" and "notes") it counts as one.
func TestScanSSHKeysFromJSONDeduplicateSameKey(t *testing.T) {
	key := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5test"
	json := fmt.Sprintf(`{"a": %q, "b": %q, "nested": {"c": %q}}`, key, key, key)
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	// Three raw hits but only one unique after dedup.
	if len(keys) != 3 {
		t.Errorf("want 3 raw hits, got %d", len(keys))
	}
	if unique := dedupKeys(keys); len(unique) != 1 {
		t.Errorf("want 1 unique key, got %d", len(unique))
	}
}

// TestScanSSHKeysFromJSONNestedArray verifies keys inside JSON arrays are found.
func TestScanSSHKeysFromJSONNestedArray(t *testing.T) {
	json := `{"sections": [{"rows": [{"value": "ssh-rsa AAAAB3NzaC1yc2EAAAADtest rsa-key"}]}]}`
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	if len(keys) == 0 {
		t.Errorf("want at least one key in nested array")
	}
	if !strings.HasPrefix(keys[0], "ssh-rsa ") {
		t.Errorf("key = %q, want ssh-rsa prefix", keys[0])
	}
}

// TestScanSSHKeysFromJSONEcdsaAndSK verifies ecdsa and sk-ssh types are matched.
func TestScanSSHKeysFromJSONEcdsaAndSK(t *testing.T) {
	json := `{
		"a": "ecdsa-sha2-nistp256 AAAAE2VjZHNhtest ecdsa-key",
		"b": "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5test sk-key"
	}`
	keys, err := scanSSHKeysFromJSON([]byte(json))
	if err != nil {
		t.Fatalf("scanSSHKeysFromJSON: %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("want 2 keys, got %d: %v", len(keys), keys)
	}
}

// TestScanSSHKeysFromJSONInvalidJSON verifies that malformed JSON returns an error.
func TestScanSSHKeysFromJSONInvalidJSON(t *testing.T) {
	_, err := scanSSHKeysFromJSON([]byte(`{not valid json`))
	if err == nil {
		t.Errorf("want error for invalid JSON")
	}
}

// ── ResolvePublicKey (fake pass-cli) ─────────────────────────────────────────

// itemJSON returns a JSON payload simulating a Proton Pass item view response
// containing the given SSH public key in a fields array.
func itemJSON(pubKey string) string {
	return fmt.Sprintf(`{
		"id":       "item-id-001",
		"title":    "My Signing Key",
		"vault":    {"id": "share-id-001", "name": "Engineering"},
		"category": "SSH_KEY",
		"fields": [
			{"id": "f1", "label": "public key",  "value": %q},
			{"id": "f2", "label": "private key (enc)", "value": "base64-encoded-encrypted-blob"},
			{"id": "f3", "label": "notes",        "value": "managed by koryph"}
		]
	}`, pubKey)
}

// fakeViewCLI writes a script that prints the contents of a fixed JSON file to
// stdout and appends its argv to argv.log. The JSON is written to a file on
// disk rather than embedded in the script to avoid shell quoting issues.
func fakeViewCLI(t *testing.T, dir, fixedOutput string) string {
	t.Helper()
	outFile := filepath.Join(dir, "fake-output.json")
	if err := os.WriteFile(outFile, []byte(fixedOutput), 0o644); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "fake-pass-cli")
	content := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + filepath.Join(dir, "argv.log") + "\"\n" +
		"cat \"" + outFile + "\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// newFakeVault returns a VaultConfig with the given script wired as both view
// and view_by_title templates for the protonpass provider.
func newFakeVault(script string) *VaultConfig {
	v := DefaultVault()
	v.Providers[ProviderProtonPass] = ProviderTemplates{
		View:        []string{script, "item", "view", RefPlaceholder, "--output", "json"},
		ViewByTitle: []string{script, "item", "view", "--vault-name", VaultPlaceholder, "--item-title", TitlePlaceholder, "--output", "json"},
		Fetch:       []string{script, "item", "view", RefPlaceholder},
		AgentLoad:   []string{script, "ssh-agent", "load"},
		LoginHint:   "pass-cli login",
	}
	return v
}

// TestResolvePublicKeyByURI tests URI-based resolution (view template).
func TestResolvePublicKeyByURI(t *testing.T) {
	dir := t.TempDir()
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI1 koryph-signing"
	script := fakeViewCLI(t, dir, itemJSON(pub))
	vault := newFakeVault(script)

	got, err := vault.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"pass://share-id/item-id", "", "")
	if err != nil {
		t.Fatalf("ResolvePublicKey: %v", err)
	}
	if !strings.HasPrefix(got, "ssh-ed25519 ") {
		t.Errorf("key = %q, want ssh-ed25519 prefix", got)
	}
	// Comment stripped.
	if strings.Contains(got, "koryph-signing") {
		t.Errorf("comment not stripped from key: %q", got)
	}

	// The view template must receive the URI (not title flags).
	log, _ := os.ReadFile(filepath.Join(dir, "argv.log"))
	if !strings.Contains(string(log), "item view pass://share-id/item-id --output json") {
		t.Errorf("argv log = %q, want URI form invoked", log)
	}
}

// TestResolvePublicKeyByTitle tests vault-name+item-title resolution (view_by_title template).
func TestResolvePublicKeyByTitle(t *testing.T) {
	dir := t.TempDir()
	const pub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI2 work-key"
	script := fakeViewCLI(t, dir, itemJSON(pub))
	vault := newFakeVault(script)

	got, err := vault.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"", "Engineering", "My Signing Key")
	if err != nil {
		t.Fatalf("ResolvePublicKey: %v", err)
	}
	if !strings.HasPrefix(got, "ssh-ed25519 ") {
		t.Errorf("key = %q, want ssh-ed25519 prefix", got)
	}

	// The view_by_title template must use --vault-name and --item-title.
	log, _ := os.ReadFile(filepath.Join(dir, "argv.log"))
	if !strings.Contains(string(log), "--vault-name Engineering --item-title My Signing Key --output json") {
		t.Errorf("argv log = %q, want vault-name/title form", log)
	}
}

// TestResolvePublicKeyAmbiguous verifies fail-closed when two distinct keys are found.
func TestResolvePublicKeyAmbiguous(t *testing.T) {
	dir := t.TempDir()
	twoKeyJSON := `{
		"fields": [
			{"value": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAA key-one"},
			{"value": "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5BBBB key-two"}
		]
	}`
	script := fakeViewCLI(t, dir, twoKeyJSON)
	vault := newFakeVault(script)

	_, err := vault.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"pass://s/i", "", "")
	if err == nil {
		t.Fatal("want error for ambiguous keys")
	}
	msg := err.Error()
	if !strings.Contains(msg, "ambiguous") || !strings.Contains(msg, "2") {
		t.Errorf("err = %q, want 'ambiguous' with count", msg)
	}
	// Both keys must be listed so the user knows what was found.
	if !strings.Contains(msg, "AAAA") || !strings.Contains(msg, "BBBB") {
		t.Errorf("err = %q, want both key blobs listed", msg)
	}
}

// TestResolvePublicKeyKeyless verifies fail-closed when no SSH key is found.
func TestResolvePublicKeyKeyless(t *testing.T) {
	dir := t.TempDir()
	noKeyJSON := `{"title": "Database Credentials", "password": "hunter2"}`
	script := fakeViewCLI(t, dir, noKeyJSON)
	vault := newFakeVault(script)

	_, err := vault.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"pass://s/i", "", "")
	if err == nil {
		t.Fatal("want error when no SSH key found")
	}
	if !strings.Contains(err.Error(), "no SSH public key") {
		t.Errorf("err = %q, want 'no SSH public key' message", err.Error())
	}
}

// TestResolvePublicKeyNoViewTemplate verifies a helpful error when the view
// template is absent for a provider.
func TestResolvePublicKeyNoViewTemplate(t *testing.T) {
	v := DefaultVault()
	// Remove view from protonpass defaults.
	pp := v.Providers[ProviderProtonPass]
	pp.View = nil
	v.Providers[ProviderProtonPass] = pp

	_, err := v.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"pass://s/i", "", "")
	if err == nil {
		t.Fatal("want error when view template is absent")
	}
	if !strings.Contains(err.Error(), "view template") {
		t.Errorf("err = %q, want template guidance", err.Error())
	}
}

// TestResolvePublicKeyNoViewByTitleTemplate verifies a helpful error when the
// view_by_title template is absent for a provider.
func TestResolvePublicKeyNoViewByTitleTemplate(t *testing.T) {
	v := DefaultVault()
	pp := v.Providers[ProviderProtonPass]
	pp.ViewByTitle = nil
	v.Providers[ProviderProtonPass] = pp

	_, err := v.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"", "Engineering", "My Key")
	if err == nil {
		t.Fatal("want error when view_by_title template is absent")
	}
	if !strings.Contains(err.Error(), "view_by_title template") {
		t.Errorf("err = %q, want template guidance", err.Error())
	}
}

// TestResolvePublicKeyViewCLIFails verifies that a non-zero exit from the view
// CLI carries the login hint.
func TestResolvePublicKeyViewCLIFails(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail-pass-cli")
	content := "#!/bin/sh\necho 'session expired' >&2; exit 1\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	vault := newFakeVault(script)

	_, err := vault.ResolvePublicKey(context.Background(), ProviderProtonPass,
		"pass://s/i", "", "")
	if err == nil {
		t.Fatal("want error on CLI failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "pass-cli login") {
		t.Errorf("err = %q, want login hint", msg)
	}
	if !strings.Contains(msg, "session expired") {
		t.Errorf("err = %q, want CLI stderr detail", msg)
	}
}

// ── expandArgvVars ────────────────────────────────────────────────────────────

func TestExpandArgvVars(t *testing.T) {
	argv := []string{"pass-cli", "item", "view", "--vault-name", "{vault}", "--item-title", "{title}", "--output", "json"}
	got := expandArgvVars(argv, map[string]string{
		VaultPlaceholder: "Engineering",
		TitlePlaceholder: "My Key",
	})
	want := []string{"pass-cli", "item", "view", "--vault-name", "Engineering", "--item-title", "My Key", "--output", "json"}
	for i, tok := range got {
		if tok != want[i] {
			t.Errorf("got[%d] = %q, want %q", i, tok, want[i])
		}
	}
	// Original slice must not be mutated.
	if argv[4] != VaultPlaceholder {
		t.Errorf("original mutated: argv[4] = %q", argv[4])
	}
}

// ── KeyFingerprint ─────────────────────────────────────────────────────────────

func TestKeyFingerprintFormat(t *testing.T) {
	// A real ed25519 public key blob (base64 portion is valid base64 of 51 bytes).
	// We just need the fingerprint to be SHA256:<base64> shaped.
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GkZU"
	fp := KeyFingerprint(pub)
	if !strings.HasPrefix(fp, "SHA256:") {
		t.Errorf("fingerprint = %q, want SHA256: prefix", fp)
	}
	// The SHA256: section should be 43 chars of unpadded base64 (256-bit → 32 bytes → 43 base64 chars).
	suffix := strings.TrimPrefix(fp, "SHA256:")
	if len(suffix) != 43 {
		t.Errorf("fingerprint suffix length = %d, want 43: %q", len(suffix), suffix)
	}
}

func TestKeyFingerprintInvalid(t *testing.T) {
	cases := []string{"", "not a key", "ssh-ed25519"}
	for _, c := range cases {
		if got := KeyFingerprint(c); got != "(invalid key)" {
			t.Errorf("KeyFingerprint(%q) = %q, want (invalid key)", c, got)
		}
	}
}

// TestKeyFingerprintDeterministic verifies same key → same fingerprint.
func TestKeyFingerprintDeterministic(t *testing.T) {
	pub := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GkZU koryph-test"
	fp1 := KeyFingerprint(pub)
	fp2 := KeyFingerprint(pub)
	if fp1 != fp2 {
		t.Errorf("non-deterministic: %q != %q", fp1, fp2)
	}
	// Comment variation must not change fingerprint.
	pubNoComment := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GkZU"
	fp3 := KeyFingerprint(pubNoComment)
	if fp1 != fp3 {
		t.Errorf("comment affects fingerprint: %q != %q", fp1, fp3)
	}
}

// ── DefaultVault view templates ───────────────────────────────────────────────

func TestDefaultVaultViewTemplates(t *testing.T) {
	v := DefaultVault()
	pp := v.Providers[ProviderProtonPass]
	if len(pp.View) == 0 {
		t.Errorf("protonpass view template is empty")
	}
	if len(pp.ViewByTitle) == 0 {
		t.Errorf("protonpass view_by_title template is empty")
	}
	// View must include --output json.
	if !containsAll(pp.View, "pass-cli", "--output", "json", RefPlaceholder) {
		t.Errorf("view template = %v, want pass-cli with --output json and {ref}", pp.View)
	}
	// ViewByTitle must include --vault-name and --item-title placeholders.
	if !containsAll(pp.ViewByTitle, "pass-cli", "--vault-name", VaultPlaceholder, "--item-title", TitlePlaceholder, "--output", "json") {
		t.Errorf("view_by_title template = %v, want vault-name+title placeholders and --output json", pp.ViewByTitle)
	}
}

func containsAll(slice []string, elems ...string) bool {
	set := make(map[string]bool, len(slice))
	for _, s := range slice {
		set[s] = true
	}
	for _, e := range elems {
		if !set[e] {
			return false
		}
	}
	return true
}
