// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

//go:build darwin

package signing

import (
	"encoding/base64"
	"strings"
	"testing"
)

// TestKeychainBase64Encoding verifies that multi-line key material
// base64-encodes to a single whitespace-free line, and that decoding
// round-trips the original bytes. This guards against the command-injection
// path where raw PEM lines would be interpreted as security(1) subcommands.
func TestKeychainBase64Encoding(t *testing.T) {
	// Simulate multi-line key material without triggering the detect-private-key
	// pre-commit hook. The content uses synthetic test-only marker strings.
	multilineKey := strings.Join([]string{
		"koryph-test-key-header-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"koryph-test-key-body-line-one-BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB",
		"koryph-test-key-body-line-two-CCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCCC",
		"koryph-test-key-footer-DDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDDD",
	}, "\n") + "\n"

	secret := []byte(multilineKey)

	// Encode (mirrors StoreKeychain).
	encoded := base64.StdEncoding.EncodeToString(secret)

	// The encoded form must be a single line with no whitespace (safe for -w).
	if strings.ContainsAny(encoded, " \t\n\r") {
		t.Errorf("base64 output contains whitespace; would break security -i parser:\n%s", encoded)
	}

	// Decode (mirrors FetchKeychain).
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if string(decoded) != string(secret) {
		t.Errorf("round-trip mismatch:\ngot:  %q\nwant: %q", decoded, secret)
	}
}

// TestKeychainBase64BinaryRoundTrip verifies that arbitrary binary content
// (not just printable ASCII) survives the encode/decode cycle. SSH keys can
// contain arbitrary bytes when decoded.
func TestKeychainBase64BinaryRoundTrip(t *testing.T) {
	// Binary blob with all byte values 0-255.
	binary := make([]byte, 256)
	for i := range binary {
		binary[i] = byte(i)
	}

	encoded := base64.StdEncoding.EncodeToString(binary)
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(decoded) != len(binary) {
		t.Fatalf("length mismatch: got %d, want %d", len(decoded), len(binary))
	}
	for i := range binary {
		if decoded[i] != binary[i] {
			t.Errorf("byte %d: got %x, want %x", i, decoded[i], binary[i])
		}
	}
}
