// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/engine"
	"github.com/koryph/koryph/internal/project"
	"github.com/koryph/koryph/internal/signing"
	"github.com/koryph/koryph/internal/signing/signingtest"
)

const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAITESTONLY koryph-test"

// testValidPubKey has a well-formed base64 blob so KeyFingerprint can produce
// a SHA256 fingerprint. testPubKey's blob is intentionally fake for brevity;
// tests that check fingerprints must use this constant instead.
const testValidPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GkZU koryph-test"

// setupProject registers a git project "demo" and returns its root.
func setupProject(t *testing.T) string {
	t.Helper()
	isolate(t)
	t.Setenv("SSH_AUTH_SOCK", "") // hermetic: never touch a real agent
	root := gitRepo(t)
	code, _, errb := runCmd("project", "add", root,
		"--account", "personal", "--identity", "me@example.com", "--id", "demo")
	if code != 0 {
		t.Fatalf("project add: code %d stderr=%s", code, errb)
	}
	return root
}

func TestSigningSetupWritesPolicyAndAudits(t *testing.T) {
	root := setupProject(t)
	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errb := runCmd("signing", "setup",
		"--project", "demo", "--provider", "file", "--key-ref", keyRef,
		"--identity", "dev@example.com", "--public-key", testPubKey, "--artifacts")
	if code != 0 {
		t.Fatalf("signing setup: code %d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "signing policy written") {
		t.Errorf("setup output = %q", out)
	}

	cfg, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Signing
	if sc == nil {
		t.Fatal("signing block not written")
	}
	if !sc.Required || sc.EffectiveMode() != signing.ModeSSH || sc.Provider != "file" ||
		sc.KeyRef != keyRef || sc.Identity != "dev@example.com" || !sc.Artifacts {
		t.Errorf("signing block = %+v", sc)
	}
	if !strings.HasPrefix(sc.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public key = %q", sc.PublicKey)
	}

	audit, err := os.ReadFile(filepath.Join(os.Getenv("KORYPH_HOME"), "audit.jsonl"))
	if err != nil {
		t.Fatalf("audit log: %v", err)
	}
	if !strings.Contains(string(audit), `"signing setup"`) || !strings.Contains(string(audit), `"kind":"update"`) {
		t.Errorf("audit log missing signing update event:\n%s", audit)
	}
}

func TestSigningSetupUsage(t *testing.T) {
	isolate(t)
	if code, _, _ := runCmd("signing", "setup", "--provider", "file"); code != engine.ExitUsage {
		t.Errorf("missing --project: code %d, want usage", code)
	}
	if code, _, _ := runCmd("signing", "setup", "--project", "demo"); code != engine.ExitUsage {
		t.Errorf("missing --identity: code %d, want usage", code)
	}
	if code, _, _ := runCmd("signing", "bogus"); code != engine.ExitUsage {
		t.Errorf("unknown subcommand: code %d, want usage", code)
	}
}

func TestSigningStatusUnconfiguredAndConfigured(t *testing.T) {
	root := setupProject(t)

	code, out, _ := runCmd("signing", "status", "--project", "demo")
	if code != 0 || !strings.Contains(out, "not configured") {
		t.Errorf("unconfigured status: code %d out=%q", code, out)
	}

	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runCmd("signing", "setup", "--project", "demo", "--provider", "file",
		"--key-ref", keyRef, "--identity", "dev@example.com", "--public-key", testPubKey); code != 0 {
		t.Fatalf("setup: code %d stderr=%s", code, errb)
	}
	code, out, errb := runCmd("signing", "status", "--project", "demo")
	if code != 0 {
		t.Fatalf("status: code %d stderr=%s", code, errb)
	}
	for _, want := range []string{"mode:            ssh", "provider:        file", "agent ready:     no", "allowed_signers"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}
	_ = root
}

func TestSigningVerifyFlagsUnsignedBranch(t *testing.T) {
	signingtest.IsolateGit(t)
	root := setupProject(t)

	// Seed main and an unsigned feature branch.
	mustRunGit(t, root, "config", "user.email", "t@example.com")
	mustRunGit(t, root, "config", "user.name", "T")
	mustRunGit(t, root, "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, root, "add", "-A")
	mustRunGit(t, root, "commit", "-qm", "seed")
	mustRunGit(t, root, "checkout", "-qb", "feat")
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustRunGit(t, root, "add", "b.txt")
	mustRunGit(t, root, "commit", "-qm", "unsigned work")
	mustRunGit(t, root, "checkout", "-q", "main")

	code, out, _ := runCmd("signing", "verify", "--project", "demo", "--branch", "feat")
	if code != engine.ExitFatal {
		t.Fatalf("verify code = %d, want %d\n%s", code, engine.ExitFatal, out)
	}
	if !strings.Contains(out, "no signature") {
		t.Errorf("verify output = %q, want unsigned commit listed", out)
	}
}

func TestSignBlobRequiresArtifactsFlag(t *testing.T) {
	root := setupProject(t)
	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runCmd("signing", "setup", "--project", "demo", "--provider", "file",
		"--key-ref", keyRef, "--identity", "dev@example.com", "--public-key", testPubKey); code != 0 {
		t.Fatalf("setup: code %d stderr=%s", code, errb)
	}
	blob := filepath.Join(root, "artifact.bin")
	if err := os.WriteFile(blob, []byte("bits"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errb := runCmd("sign", "blob", "--project", "demo", blob)
	if code != engine.ExitFatal || !strings.Contains(errb, "artifact") {
		t.Errorf("sign blob without artifacts: code %d stderr=%q", code, errb)
	}
}

func TestSignBlobWithFakeCosign(t *testing.T) {
	root := setupProject(t)
	dir := t.TempDir()
	keyRef := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyRef, []byte("COSIGN-PRIV"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, errb := runCmd("signing", "setup", "--project", "demo", "--provider", "file",
		"--key-ref", keyRef, "--identity", "dev@example.com", "--public-key", testPubKey, "--artifacts"); code != 0 {
		t.Fatalf("setup: code %d stderr=%s", code, errb)
	}

	fake := filepath.Join(dir, "fake-cosign")
	script := `#!/bin/sh
[ -n "$KORYPH_COSIGN_KEY" ] || exit 9
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-signature" ]; then printf 'sig' > "$a"; fi
  prev="$a"
done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KORYPH_COSIGN_BIN", fake)

	blob := filepath.Join(root, "artifact.bin")
	if err := os.WriteFile(blob, []byte("bits"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errb := runCmd("sign", "blob", "--project", "demo", blob)
	if code != 0 {
		t.Fatalf("sign blob: code %d stderr=%s", code, errb)
	}
	if !strings.Contains(out, blob+".sig") {
		t.Errorf("sign blob output = %q", out)
	}
	if _, err := os.Stat(blob + ".sig"); err != nil {
		t.Errorf("signature not written: %v", err)
	}
}

// mustRunGit runs git in dir, failing the test on error.
func mustRunGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ── deterministic key association tests ───────────────────────────────────────

// itemViewJSON returns a Proton Pass item JSON with a single SSH public key.
func itemViewJSON(pub string) []byte {
	return []byte(`{
		"id":       "item-001",
		"title":    "SSH Signing Key",
		"vault":    {"id": "share-001", "name": "Engineering"},
		"category": "SSH_KEY",
		"fields": [
			{"label": "public key",  "value": "` + pub + `"},
			{"label": "private key (enc)", "value": "base64-encoded-encrypted-blob"}
		]
	}`)
}

// setupVaultCLI wires a fake pass-cli that serves fixedJSON for any item view
// call. It writes the JSON to a temp file (avoiding shell quoting issues) and
// returns the KORYPH_HOME dir that contains the custom vault.json.
func setupVaultCLI(t *testing.T, fixedJSON []byte, exitCode int) {
	t.Helper()
	dir := t.TempDir()

	// Write the canned JSON response to a file.
	jsonFile := filepath.Join(dir, "vault-response.json")
	if err := os.WriteFile(jsonFile, fixedJSON, 0o644); err != nil {
		t.Fatal(err)
	}

	// Fake pass-cli: logs argv, cats the JSON, exits with exitCode.
	argvLog := filepath.Join(dir, "argv.log")
	fakeBin := filepath.Join(dir, "fake-pass-cli")
	exitStmt := "exit 0"
	if exitCode != 0 {
		exitStmt = "echo 'session expired' >&2; exit 1"
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + argvLog + "\"\n" +
		exitStmt + "\n" +
		"cat \"" + jsonFile + "\"\n"
	if exitCode == 0 {
		// Reorder: log then output then exit.
		script = "#!/bin/sh\n" +
			"printf '%s\\n' \"$*\" >> \"" + argvLog + "\"\n" +
			"cat \"" + jsonFile + "\"\n"
	}
	if err := os.WriteFile(fakeBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	// Write vault.json pointing at our fake CLI.
	vaultJSON := `{"schema_version":1,"providers":{"protonpass":{` +
		`"view":["` + fakeBin + `","item","view","{ref}","--output","json"],` +
		`"view_by_title":["` + fakeBin + `","item","view","--vault-name","{vault}","--item-title","{title}","--output","json"],` +
		`"fetch":["` + fakeBin + `","item","view","{ref}"],` +
		`"agent_load":["` + fakeBin + `","ssh-agent","load"],` +
		`"login_hint":"pass-cli login"}}}`
	vaultPath := filepath.Join(os.Getenv("KORYPH_HOME"), "vault.json")
	if err := os.WriteFile(vaultPath, []byte(vaultJSON), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestSigningSetupWithVaultNameResolvesKey tests --vault-name + --item-title
// form: a fake pass-cli returns item JSON with one SSH key, setup persists
// the resolved public key and the vault selector as provenance.
func TestSigningSetupWithVaultNameResolvesKey(t *testing.T) {
	root := setupProject(t)
	// testPubKey is our canned "resolved" public key embedded in the vault JSON.
	setupVaultCLI(t, itemViewJSON(testPubKey), 0)

	code, out, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "protonpass",
		"--vault-name", "Engineering",
		"--item-title", "SSH Signing Key",
		"--identity", "dev@example.com")
	if code != 0 {
		t.Fatalf("signing setup vault-name form: code %d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "resolved public key") {
		t.Errorf("output should mention resolved key:\n%s", out)
	}
	if !strings.Contains(out, "signing policy written") {
		t.Errorf("output should confirm policy written:\n%s", out)
	}

	// Persisted config must carry the public key and provenance.
	cfg, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Signing
	if sc == nil {
		t.Fatal("signing block not written")
	}
	if !strings.HasPrefix(sc.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public_key = %q, want ssh-ed25519 prefix", sc.PublicKey)
	}
	if sc.VaultName != "Engineering" {
		t.Errorf("vault_name = %q, want Engineering", sc.VaultName)
	}
	if sc.ItemTitle != "SSH Signing Key" {
		t.Errorf("item_title = %q, want SSH Signing Key", sc.ItemTitle)
	}
	if sc.KeyRef != "" {
		t.Errorf("key_ref should be empty for vault-name form, got %q", sc.KeyRef)
	}
	_ = root
}

// TestSigningSetupWithKeyRefURIResolvesKey tests --key-ref pass:// URI form:
// the fake pass-cli's view template is called with the URI, the public key is
// extracted from the JSON, and key_ref is persisted.
func TestSigningSetupWithKeyRefURIResolvesKey(t *testing.T) {
	root := setupProject(t)
	setupVaultCLI(t, itemViewJSON(testPubKey), 0)

	const uri = "pass://share-001/item-001"
	code, out, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "protonpass",
		"--key-ref", uri,
		"--identity", "dev@example.com")
	if code != 0 {
		t.Fatalf("signing setup key-ref form: code %d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "resolved public key") {
		t.Errorf("output should mention resolved key:\n%s", out)
	}

	cfg, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	sc := cfg.Signing
	if sc == nil {
		t.Fatal("signing block not written")
	}
	if !strings.HasPrefix(sc.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public_key = %q, want ssh-ed25519 prefix", sc.PublicKey)
	}
	if sc.KeyRef != uri {
		t.Errorf("key_ref = %q, want %q", sc.KeyRef, uri)
	}
	if sc.VaultName != "" || sc.ItemTitle != "" {
		t.Errorf("vault_name/item_title should be empty for key-ref form, got %q/%q", sc.VaultName, sc.ItemTitle)
	}
	_ = root
}

// TestSigningSetupWithAtFilePublicKey tests --public-key @path where path
// points to a file containing the public key.
func TestSigningSetupWithAtFilePublicKey(t *testing.T) {
	root := setupProject(t)
	pubKeyFile := filepath.Join(t.TempDir(), "id_ed25519.pub")
	if err := os.WriteFile(pubKeyFile, []byte(testPubKey+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, out, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "file",
		"--key-ref", keyRef,
		"--public-key", "@"+pubKeyFile,
		"--identity", "dev@example.com")
	if code != 0 {
		t.Fatalf("signing setup @file form: code %d stderr=%s", code, errb)
	}
	if !strings.Contains(out, "signing policy written") {
		t.Errorf("output = %q", out)
	}

	cfg, err := project.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.Signing.PublicKey, "ssh-ed25519 ") {
		t.Errorf("public_key = %q, want ssh-ed25519 prefix", cfg.Signing.PublicKey)
	}
	_ = root
}

// TestSigningSetupAtFileMissing verifies a clear error when @path doesn't exist.
func TestSigningSetupAtFileMissing(t *testing.T) {
	setupProject(t)
	code, _, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "file",
		"--key-ref", "/dev/null",
		"--public-key", "@/nonexistent/key.pub",
		"--identity", "dev@example.com")
	if code == 0 {
		t.Fatal("want error for missing @file")
	}
	if !strings.Contains(errb, "@/nonexistent/key.pub") {
		t.Errorf("stderr = %q, want path in error", errb)
	}
}

// TestSigningSetupConflictingPublicKeySources verifies that specifying both
// --public-key and --vault-name/--item-title is rejected.
func TestSigningSetupConflictingPublicKeySources(t *testing.T) {
	setupProject(t)
	code, _, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "protonpass",
		"--public-key", testPubKey,
		"--vault-name", "Engineering",
		"--item-title", "My Key",
		"--identity", "dev@example.com")
	if code != engine.ExitUsage {
		t.Errorf("conflicting key sources: code %d, want usage; stderr=%s", code, errb)
	}
	if !strings.Contains(errb, "conflicts") {
		t.Errorf("stderr = %q, want 'conflicts' message", errb)
	}
}

// TestSigningSetupVaultNameWithoutItemTitle verifies that --vault-name alone
// (without --item-title) is rejected.
func TestSigningSetupVaultNameWithoutItemTitle(t *testing.T) {
	setupProject(t)
	code, _, errb := runCmd("signing", "setup",
		"--project", "demo",
		"--provider", "protonpass",
		"--vault-name", "Engineering",
		"--identity", "dev@example.com")
	if code != engine.ExitUsage {
		t.Errorf("vault-name without item-title: code %d, want usage", code)
	}
	if !strings.Contains(errb, "--vault-name") && !strings.Contains(errb, "--item-title") {
		t.Errorf("stderr = %q, want flag mention", errb)
	}
}

// TestSigningStatusShowsFingerprint verifies that 'koryph signing status'
// prints both the key source selector and the SHA256 fingerprint of the
// configured public key.
func TestSigningStatusShowsFingerprint(t *testing.T) {
	root := setupProject(t)
	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Use a key with a valid base64 blob so KeyFingerprint can produce SHA256:.
	if code, _, errb := runCmd("signing", "setup", "--project", "demo",
		"--provider", "file", "--key-ref", keyRef,
		"--identity", "dev@example.com", "--public-key", testValidPubKey); code != 0 {
		t.Fatalf("setup: code %d stderr=%s", code, errb)
	}

	code, out, errb := runCmd("signing", "status", "--project", "demo")
	if code != 0 {
		t.Fatalf("status: code %d stderr=%s", code, errb)
	}

	// Must show the key source selector.
	if !strings.Contains(out, "key source:") {
		t.Errorf("status missing 'key source:' line:\n%s", out)
	}
	// Must show a SHA256 fingerprint.
	if !strings.Contains(out, "SHA256:") {
		t.Errorf("status missing 'SHA256:' fingerprint:\n%s", out)
	}
	// Must show the pubkey fp line.
	if !strings.Contains(out, "pubkey fp:") {
		t.Errorf("status missing 'pubkey fp:' line:\n%s", out)
	}
	_ = root
}

// TestSigningSetupPerProjectIndependence verifies that two projects can each
// hold their own public key without interference — a guard against any "single
// global key" assumption.
func TestSigningSetupPerProjectIndependence(t *testing.T) {
	signingtest.IsolateGit(t)
	isolate(t)
	t.Setenv("SSH_AUTH_SOCK", "")

	root1 := gitRepo(t)
	root2 := gitRepo(t)
	keyRef1 := filepath.Join(t.TempDir(), "key1")
	keyRef2 := filepath.Join(t.TempDir(), "key2")
	for _, p := range []string{keyRef1, keyRef2} {
		if err := os.WriteFile(p, []byte("k"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{
		{"project", "add", root1, "--account", "personal", "--identity", "a@a.com", "--id", "proj1"},
		{"project", "add", root2, "--account", "personal", "--identity", "b@b.com", "--id", "proj2"},
	} {
		if code, _, errb := runCmd(args...); code != 0 {
			t.Fatalf("project add: code %d stderr=%s", code, errb)
		}
	}

	const pub1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAAPROJECT1key"
	const pub2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAAPROJECT2key"

	if code, _, errb := runCmd("signing", "setup",
		"--project", "proj1", "--provider", "file", "--key-ref", keyRef1,
		"--identity", "a@a.com", "--public-key", pub1); code != 0 {
		t.Fatalf("setup proj1: code %d stderr=%s", code, errb)
	}
	if code, _, errb := runCmd("signing", "setup",
		"--project", "proj2", "--provider", "file", "--key-ref", keyRef2,
		"--identity", "b@b.com", "--public-key", pub2); code != 0 {
		t.Fatalf("setup proj2: code %d stderr=%s", code, errb)
	}

	cfg1, _ := project.Load(root1)
	cfg2, _ := project.Load(root2)

	if !strings.HasPrefix(cfg1.Signing.PublicKey, "ssh-ed25519 ") {
		t.Errorf("proj1 public_key = %q, want ssh-ed25519 prefix", cfg1.Signing.PublicKey)
	}
	if !strings.HasPrefix(cfg2.Signing.PublicKey, "ssh-ed25519 ") {
		t.Errorf("proj2 public_key = %q, want ssh-ed25519 prefix", cfg2.Signing.PublicKey)
	}
	// Each project pins its own key — must not be the same value.
	if cfg1.Signing.PublicKey == cfg2.Signing.PublicKey {
		t.Errorf("per-project independence: both projects share public_key %q", cfg1.Signing.PublicKey)
	}
	// The signing package's KeyFingerprint must also return distinct values.
	fp1 := signing.KeyFingerprint(cfg1.Signing.PublicKey)
	fp2 := signing.KeyFingerprint(cfg2.Signing.PublicKey)
	if fp1 == fp2 {
		t.Errorf("fingerprints must differ when public keys differ: fp=%q", fp1)
	}
}

// TestSigningStatusJSONUnconfigured proves `signing status --json` emits a
// JSON object with configured:false when signing has not been set up.
func TestSigningStatusJSONUnconfigured(t *testing.T) {
	setupProject(t)

	code, out, errb := runCmd("signing", "status", "--project", "demo", "--json")
	if code != 0 {
		t.Fatalf("signing status --json (unconfigured): code %d stderr=%s", code, errb)
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if configured, _ := obj["configured"].(bool); configured {
		t.Errorf("expected configured=false, got %v", obj["configured"])
	}
}

// TestSigningStatusJSONConfigured proves `signing status --json` emits a
// well-formed signingStatusJSON with the expected fields after setup.
func TestSigningStatusJSONConfigured(t *testing.T) {
	root := setupProject(t)
	keyRef := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyRef, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Use a key with a valid blob so KeyFingerprint produces a real SHA256 fp.
	if code, _, errb := runCmd("signing", "setup",
		"--project", "demo", "--provider", "file", "--key-ref", keyRef,
		"--identity", "dev@example.com", "--public-key", testValidPubKey); code != 0 {
		t.Fatalf("setup: code %d stderr=%s", code, errb)
	}

	code, out, errb := runCmd("signing", "status", "--project", "demo", "--json")
	if code != 0 {
		t.Fatalf("signing status --json: code %d stderr=%s", code, errb)
	}

	var st signingStatusJSON
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if st.ProjectID != "demo" {
		t.Errorf("project_id = %q, want demo", st.ProjectID)
	}
	if st.Mode != signing.ModeSSH {
		t.Errorf("mode = %q, want %q", st.Mode, signing.ModeSSH)
	}
	if st.Provider != "file" {
		t.Errorf("provider = %q, want file", st.Provider)
	}
	if st.Identity != "dev@example.com" {
		t.Errorf("identity = %q, want dev@example.com", st.Identity)
	}
	if !st.Required {
		t.Errorf("required = false, want true")
	}
	if st.PubkeyFP == "" || !strings.HasPrefix(st.PubkeyFP, "SHA256:") {
		t.Errorf("pubkey_fp = %q, want SHA256: prefix", st.PubkeyFP)
	}
	if st.AgentReady == nil {
		t.Errorf("agent_ready should be set for SSH mode")
	}
	if st.AllowedSignersPath == "" {
		t.Errorf("allowed_signers_path should be non-empty")
	}
	_ = root
}
