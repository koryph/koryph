// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package obs

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestRedactSecretKeyNames asserts that attributes with secret-shaped key names
// have their values replaced with [REDACTED]. This test MUST FAIL if any new
// field with a secret-suggestive name flows through unredacted.
func TestRedactSecretKeyNames(t *testing.T) {
	secretKeys := []string{
		"token",
		"api_key",
		"api-key",
		"password",
		"passwd",
		"secret",
		"bearer",
		"authorization",
		"auth",
		"credential",
		"private_key",
		"vault",
		"passphrase",
	}
	for _, key := range secretKeys {
		a := RedactAttr(slog.String(key, "super-secret-value-12345"))
		if a.Value.String() != Redacted {
			t.Errorf("key %q: value not redacted, got %q", key, a.Value.String())
		}
	}
}

// TestRedactPEMBlock asserts that PEM-shaped values are scrubbed even on
// innocuous-named keys. The string is built at runtime to avoid triggering
// static secret-scanner rules in the test file itself.
func TestRedactPEMBlock(t *testing.T) {
	// Compose the PEM fixture at runtime so gitleaks does not flag this file.
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	pem := begin + "\nMIIEpAIBAAKCAQEA1234AAAABBBBCCCC\n" + end
	got := RedactValue(pem)
	if got == pem {
		t.Error("PEM block not redacted")
	}
	if got == "" {
		t.Error("RedactValue returned empty string")
	}
}

// TestRedactBearerToken asserts that Authorization header patterns are scrubbed.
func TestRedactBearerToken(t *testing.T) {
	cases := []string{
		"Bearer eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.payload.signature",
		"bearer sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",
		"Basic dXNlcjpwYXNzd29yZA==",
		"Token ghp_abcdefghijklmnopqrstuvwxyz01234",
	}
	for _, c := range cases {
		got := RedactValue(c)
		if got == c {
			t.Errorf("bearer/basic/token not redacted: %q", c)
		}
	}
}

// TestRedactAPIKeys asserts that well-known API key prefixes are redacted.
// Strings are composed at runtime to avoid triggering static secret scanners.
func TestRedactAPIKeys(t *testing.T) {
	// Build prefixed key fixtures at runtime so gitleaks does not flag them.
	keys := []string{
		"sk-ant-" + "api03-verylongsecretkeyvaluethatismorethan32chars",
		"ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ012345",
		"glpat" + "-abcdefghijklmnopqrstu",
		"xoxb" + "-123456789-abcdefghijklmnopqrst",
	}
	for _, k := range keys {
		got := RedactValue(k)
		if got == k {
			t.Errorf("API key not redacted: %q", k)
		}
	}
}

// TestSafeValuesNotRedacted ensures non-secret strings pass through unchanged.
func TestSafeValuesNotRedacted(t *testing.T) {
	cases := []string{
		"hello world",
		"engine",
		"run-2026-07-04-001",
		"bead-abc123",
	}
	for _, c := range cases {
		got := RedactValue(c)
		if got != c {
			t.Errorf("safe value unexpectedly redacted: input=%q got=%q", c, got)
		}
	}
}

// TestRedactAttrGroup asserts that nested group attrs are recursively redacted.
func TestRedactAttrGroup(t *testing.T) {
	// Build token at runtime so gitleaks does not flag this file.
	fakeToken := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ01234"
	inner := slog.Group("forge",
		slog.String("token", fakeToken),
		slog.String("model", "claude-opus-4"),
	)
	cleaned := RedactAttr(inner)
	for _, a := range cleaned.Value.Group() {
		if a.Key == "token" && a.Value.String() != Redacted {
			t.Errorf("nested token not redacted: %q", a.Value.String())
		}
		if a.Key == "model" && a.Value.String() != "claude-opus-4" {
			t.Errorf("safe model attr altered: %q", a.Value.String())
		}
	}
}

// TestRedactRecord asserts that RedactRecord scrubs a full slog.Record.
func TestRedactRecord(t *testing.T) {
	var rec slog.Record
	rec.Add(slog.String("authorization", "Bearer supersecret12345678901234567"))
	rec.Add(slog.String("component", "engine"))
	out := RedactRecord(rec)

	out.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "authorization":
			if a.Value.String() != Redacted {
				t.Errorf("authorization not redacted: %q", a.Value.String())
			}
		case "component":
			if a.Value.String() != "engine" {
				t.Errorf("component altered: %q", a.Value.String())
			}
		}
		return true
	})
}

// TestRedactRecordScansMessage guards the fix for the audit finding that
// RedactRecord copied the message verbatim while the engine formats raw
// subprocess errors (wrapping git/gh/gate stderr) straight into it.
func TestRedactRecordScansMessage(t *testing.T) {
	secret := "Bearer supersecret12345678901234567"
	rec := slog.NewRecord(time.Time{}, slog.LevelWarn,
		"engine.slot.blocked: gate failed: "+secret, 0)
	out := RedactRecord(rec)
	if strings.Contains(out.Message, secret) {
		t.Errorf("secret survived in redacted message: %q", out.Message)
	}
	if !strings.Contains(out.Message, Redacted) {
		t.Errorf("redacted message missing the redaction marker: %q", out.Message)
	}
	// A clean message must pass through unchanged.
	clean := RedactRecord(slog.NewRecord(time.Time{}, slog.LevelInfo, "engine.run.start", 0))
	if clean.Message != "engine.run.start" {
		t.Errorf("clean message altered: %q", clean.Message)
	}
}

// TestIsSecretKey covers key detection without touching handlers.
func TestIsSecretKey(t *testing.T) {
	mustMatch := []string{"token", "password", "api_key", "secret", "authorization", "vault"}
	for _, k := range mustMatch {
		if !IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = false, want true", k)
		}
	}
	mustNotMatch := []string{"run_id", "project", "bead_id", "model", "provider", "component"}
	for _, k := range mustNotMatch {
		if IsSecretKey(k) {
			t.Errorf("IsSecretKey(%q) = true, want false", k)
		}
	}
}
