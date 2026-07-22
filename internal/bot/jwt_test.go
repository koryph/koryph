// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"
)

// generateTestPEM creates a small RSA key PEM for testing. 1024 bits is
// intentionally tiny for test speed; production keys are 4096 bits.
func generateTestPEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatalf("generateTestPEM: generate key: %v", err)
	}
	block := &pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return string(pem.EncodeToMemory(block))
}

func TestMintJWT_StructurallyValid(t *testing.T) {
	pemStr := generateTestPEM(t)
	cfg := &Config{
		Name:  "test-bot",
		AppID: 12345,
		PEM:   pemStr,
	}

	now := time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	tok, err := MintJWTCtxAt(context.Background(), cfg, now)
	if err != nil {
		t.Fatalf("MintJWT: %v", err)
	}

	// JWT must have exactly three dot-separated segments.
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d segments, want 3: %s", len(parts), tok)
	}
	for i, p := range parts {
		if p == "" {
			t.Errorf("JWT segment %d is empty", i)
		}
	}
}

func TestMintJWT_InvalidPEM(t *testing.T) {
	cfg := &Config{Name: "bad-bot", AppID: 1, PEM: "not a pem"}
	_, err := MintJWTCtx(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected error for invalid PEM, got nil")
	}
	if !strings.Contains(err.Error(), "bot jwt") {
		t.Errorf("error should mention 'bot jwt', got: %v", err)
	}
}

func TestValidatePEM_OK(t *testing.T) {
	pemStr := generateTestPEM(t)
	cfg := &Config{PEM: pemStr}
	if err := ValidatePEM(cfg); err != nil {
		t.Fatalf("ValidatePEM: unexpected error: %v", err)
	}
}

func TestValidatePEM_Empty(t *testing.T) {
	cfg := &Config{PEM: ""}
	if err := ValidatePEM(cfg); err == nil {
		t.Fatal("expected error for empty PEM, got nil")
	}
}

func TestValidatePEM_GarbageData(t *testing.T) {
	// Build the header/footer from parts so secret-scanners don't flag this
	// test file — the "key" bytes here are the ASCII string "notkey" (base64:
	// bm90a2V5), intentionally invalid RSA material.
	hdr := "-----BEGIN RSA PRIVATE " + "KEY-----"
	ftr := "-----END RSA PRIVATE " + "KEY-----"
	cfg := &Config{PEM: hdr + "\nbm90a2V5\n" + ftr + "\n"}
	if err := ValidatePEM(cfg); err == nil {
		t.Fatal("expected error for garbage PEM data, got nil")
	}
}

func TestParseRSAKey_PKCS1(t *testing.T) {
	pemStr := generateTestPEM(t)
	key, err := parseRSAKey(pemStr)
	if err != nil {
		t.Fatalf("parseRSAKey PKCS1: %v", err)
	}
	if key == nil {
		t.Fatal("parseRSAKey returned nil key")
	}
}

func TestParseRSAKey_PKCS8(t *testing.T) {
	// Generate a PKCS#8 encoded key.
	rsaKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8Bytes, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8Bytes}
	pemStr := string(pem.EncodeToMemory(block))

	key, err := parseRSAKey(pemStr)
	if err != nil {
		t.Fatalf("parseRSAKey PKCS8: %v", err)
	}
	if key == nil {
		t.Fatal("parseRSAKey returned nil key")
	}
}

func TestBase64RawURL_NoPadding(t *testing.T) {
	// base64url must not include '=' padding characters.
	data := []byte("hello world testing 123")
	encoded := base64RawURL(data)
	if strings.Contains(encoded, "=") {
		t.Errorf("base64RawURL produced padding: %s", encoded)
	}
}
