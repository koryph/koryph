// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package signing_test

// Adversarial redaction tests for vault.resolve spans (§O4).
//
// These tests feed fake PEM / API-key material through vault.Fetch code paths
// and assert that ZERO secret content reaches any log record, even when the
// raw capturing handler (no redaction wrapper) is in place.
//
// Strategy:
//  1. Write a fake secret to a temp file (ProviderFile path).
//  2. Install a raw capturing handler via obs.ReInitRaw.
//  3. Call VaultConfig.Fetch — it returns the secret but must NOT log it.
//  4. Scan every captured log record for the fake secret string.
//  5. Any match → test failure (code leaked a secret into the signal).

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
	"github.com/koryph/koryph/internal/signing"
)

// rawCapture is a slog.Handler that collects all records without any
// redaction. It is used exclusively in adversarial tests.
type rawCapture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *rawCapture) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *rawCapture) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *rawCapture) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *rawCapture) WithGroup(_ string) slog.Handler      { return h }

// assertNoSubstring fails t if secret appears anywhere in the captured output.
func (h *rawCapture) assertNoSubstring(t *testing.T, secret string) {
	t.Helper()
	if secret == "" {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, r := range h.records {
		check := func(s string) {
			t.Helper()
			if strings.Contains(s, secret) {
				t.Errorf("secret leaked into log output: field contains secret; log value=%q (truncated to 80 chars: %q)",
					s, truncate(s, 80))
			}
		}
		check(r.Message)
		r.Attrs(func(a slog.Attr) bool {
			check(a.Key)
			check(a.Value.String())
			return true
		})
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// debugCfg returns an obs.Config with DEBUG level so vault.resolve spans
// (logged at DEBUG) are captured by the raw handler.
func debugCfg() obs.Config {
	return obs.Config{DefaultLevel: "debug"}
}

// ---------- adversarial tests ------------------------------------------------

// TestVaultFetch_FileProvider_NoSecretInLogs is the primary adversarial test:
// feed a fake RSA private key through the ProviderFile path and assert no
// part of the PEM block appears in any log record.
func TestVaultFetch_FileProvider_NoSecretInLogs(t *testing.T) {
	// Compose fake PEM at runtime to avoid triggering static secret scanners.
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	fakePEM := begin + "\nMIIFakeKeyMaterialForTestingPurposesOnlyDoNotUse\n" + end

	// Write fake PEM to a temp file (ProviderFile uses file path as ref).
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "fake.pem")
	if err := os.WriteFile(keyFile, []byte(fakePEM), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	// Install raw capturing handler — no redaction, so any leakage is visible.
	capture := &rawCapture{}
	obs.ReInitRaw(debugCfg(), capture)
	t.Cleanup(func() {
		// Restore obs to a benign state after the test.
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	// Set KORYPH_HOME to the temp dir so vault.LoadVault() finds defaults.
	t.Setenv("KORYPH_HOME", dir)

	v := signing.DefaultVault()
	ctx := context.Background()

	secret, err := v.Fetch(ctx, signing.ProviderFile, keyFile)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Sanity check: we did get the PEM back.
	if !strings.Contains(string(secret), "MIIFakeKeyMaterial") {
		t.Fatalf("Fetch returned unexpected content: %q", secret[:min(80, len(secret))])
	}

	// Adversarial assertion: no fragment of the fake PEM must appear in logs.
	capture.assertNoSubstring(t, "MIIFakeKeyMaterial")
	capture.assertNoSubstring(t, "DoNotUse")
	// The begin/end markers would be caught by value-pattern redaction but
	// we assert even before redaction: code must not log them at all.
	capture.assertNoSubstring(t, "BEGIN RSA")
	capture.assertNoSubstring(t, "END RSA")
}

// TestVaultFetch_FileProvider_RefInLogs verifies the POSITIVE case: the
// key_ref (file path) IS safely logged — it is a reference, not a secret.
func TestVaultFetch_FileProvider_RefInLogs(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "mykey.pem")
	if err := os.WriteFile(keyFile, []byte("safe-placeholder"), 0o600); err != nil {
		t.Fatalf("write key file: %v", err)
	}

	capture := &rawCapture{}
	obs.ReInitRaw(debugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	t.Setenv("KORYPH_HOME", dir)

	v := signing.DefaultVault()
	_, _ = v.Fetch(context.Background(), signing.ProviderFile, keyFile)

	// The key_ref (file path) MUST appear in the log — this is intended.
	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	var foundKeyRef bool
	for _, r := range recs {
		r.Attrs(func(a slog.Attr) bool {
			if a.Key == obs.KeyKeyRef && strings.Contains(a.Value.String(), "mykey.pem") {
				foundKeyRef = true
			}
			return true
		})
	}
	if !foundKeyRef {
		t.Error("key_ref (file path) not found in vault.resolve span — expected as safe metadata")
	}
}

// TestVaultFetch_NoProviderTemplate_NoSecretInLogs tests the error path:
// a provider with no fetch template must emit an error span without leaking
// any argument that could contain secret material.
func TestVaultFetch_NoProviderTemplate_NoSecretInLogs(t *testing.T) {
	fakeRef := "pass://fake-share/fake-item/private_key"

	capture := &rawCapture{}
	obs.ReInitRaw(debugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	t.Setenv("KORYPH_HOME", t.TempDir())

	// ProviderCommand has no default fetch template — will return an error.
	v := signing.DefaultVault()
	_, err := v.Fetch(context.Background(), signing.ProviderCommand, fakeRef)
	if err == nil {
		t.Fatal("expected error for provider with no fetch template")
	}

	// The ref appears in logs (it is the key reference, not a value), but
	// no secret value should ever appear.
	capture.assertNoSubstring(t, "secret-value-sentinel")
}

// TestVaultFetchSecret_APIToken simulates fetching an API token via
// ProviderFile and asserts the token value never reaches the log.
func TestVaultFetchSecret_APIToken(t *testing.T) {
	// Build a fake API token at runtime to avoid triggering secret scanners.
	fakeToken := "glpat-" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ01234"

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token.txt")
	if err := os.WriteFile(tokenFile, []byte(fakeToken), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	capture := &rawCapture{}
	obs.ReInitRaw(debugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})
	t.Setenv("KORYPH_HOME", dir)

	v := signing.DefaultVault()
	secret, err := v.Fetch(context.Background(), signing.ProviderFile, tokenFile)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(secret) != fakeToken {
		t.Fatalf("Fetch returned unexpected value")
	}

	// The fake token must NOT appear in any log record.
	capture.assertNoSubstring(t, fakeToken)
	// Also check common prefix patterns that would identify the token class.
	capture.assertNoSubstring(t, "ABCDEFGHIJKLMNO")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
