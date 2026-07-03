// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeCosign is a script standing in for cosign v3: it asserts the key
// arrived via the environment, records argv, and writes the signature file
// named by --output-signature.
const fakeCosignBody = `[ -n "$KORYPH_COSIGN_KEY" ] || { echo "no key in env" >&2; exit 9; }
printf '%s' "$KORYPH_COSIGN_KEY" > "$FAKE_COSIGN_DIR/key.txt"
prev=""
for a in "$@"; do
  if [ "$prev" = "--output-signature" ]; then printf 'fake-signature' > "$a"; fi
  prev="$a"
done`

func TestSignBlobFakeCosign(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("FAKE_COSIGN_DIR", dir)
	script := fakeCLI(t, dir, fakeCosignBody)
	t.Setenv("KORYPH_COSIGN_BIN", script)

	keyFile := filepath.Join(dir, "cosign.key")
	if err := os.WriteFile(keyFile, []byte("PRIVATE-COSIGN-KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	blob := filepath.Join(dir, "artifact.tar")
	if err := os.WriteFile(blob, []byte("bits"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{Mode: ModeGitsign, Provider: ProviderFile, KeyRef: keyFile, Artifacts: true}
	sig, err := SignBlob(context.Background(), DefaultVault(), cfg, blob)
	if err != nil {
		t.Fatalf("SignBlob: %v", err)
	}
	if sig != blob+".sig" {
		t.Errorf("sigPath = %q, want %q", sig, blob+".sig")
	}
	if data, rerr := os.ReadFile(sig); rerr != nil || string(data) != "fake-signature" {
		t.Errorf("signature file = %q, %v", data, rerr)
	}

	// The key traveled via the env var, newline-trimmed, never via argv.
	key, err := os.ReadFile(filepath.Join(dir, "key.txt"))
	if err != nil {
		t.Fatalf("fake cosign never saw the env key: %v", err)
	}
	if string(key) != "PRIVATE-COSIGN-KEY" {
		t.Errorf("env key = %q", key)
	}
	argv := argvLog(t, dir)
	if !strings.Contains(argv, "sign-blob --yes --key env://"+CosignKeyEnv+" --output-signature "+sig+" "+blob) {
		t.Errorf("argv = %q, want cosign sign-blob shape", argv)
	}
	if strings.Contains(argv, "PRIVATE-COSIGN-KEY") {
		t.Errorf("argv leaks the key: %q", argv)
	}
}

func TestSignBlobRequiresArtifacts(t *testing.T) {
	cfg := &Config{Mode: ModeGitsign, Provider: ProviderFile, KeyRef: "x"}
	if _, err := SignBlob(context.Background(), DefaultVault(), cfg, "nope"); err == nil {
		t.Errorf("want error when artifacts are not enabled")
	}
	if _, err := SignBlob(context.Background(), DefaultVault(), nil, "nope"); err == nil {
		t.Errorf("want error for nil config")
	}
}

func TestSignBlobMissingBlob(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "k")
	if err := os.WriteFile(keyFile, []byte("k"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &Config{Mode: ModeGitsign, Provider: ProviderFile, KeyRef: keyFile, Artifacts: true}
	if _, err := SignBlob(context.Background(), DefaultVault(), cfg, filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Errorf("want error for a missing blob")
	}
}
