// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package bot

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"strconv"
	"time"
)

// MintJWT creates a GitHub App JWT from the stored PEM key in cfg.
//
// The JWT format follows GitHub's App authentication contract:
//   - Algorithm: RS256 (RSASSA-PKCS1-v1_5 with SHA-256)
//   - iss: App ID (numeric string)
//   - iat: current time minus 60 s (clock-skew buffer required by GitHub)
//   - exp: current time plus 10 minutes (GitHub maximum is 10 minutes)
//
// The returned token string is ready for use as a Bearer token in the
// Authorization header of any GitHub App API call.
func MintJWT(cfg *Config) (string, error) {
	return MintJWTAt(cfg, time.Now())
}

// MintJWTAt is the injectable-time variant of MintJWT (useful in tests).
func MintJWTAt(cfg *Config, now time.Time) (string, error) {
	key, err := parseRSAKey(cfg.PEM)
	if err != nil {
		return "", fmt.Errorf("bot jwt: %w", err)
	}

	iat := now.Add(-60 * time.Second).Unix()
	exp := now.Add(10 * time.Minute).Unix()

	header := jwtSegment(map[string]string{"alg": "RS256", "typ": "JWT"})
	payload := jwtSegment(map[string]any{
		"iat": iat,
		"exp": exp,
		"iss": strconv.FormatInt(cfg.AppID, 10),
	})
	signingInput := header + "." + payload

	h := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, h[:])
	if err != nil {
		return "", fmt.Errorf("bot jwt: sign: %w", err)
	}

	return signingInput + "." + base64RawURL(sig), nil
}

// ValidatePEM reports whether the PEM stored in cfg can be parsed as a valid
// RSA private key. This is a pure offline check — no network call is made.
func ValidatePEM(cfg *Config) error {
	_, err := parseRSAKey(cfg.PEM)
	return err
}

// parseRSAKey decodes a PEM-encoded RSA private key, supporting both
// PKCS#1 ("RSA PRIVATE KEY") and PKCS#8 ("PRIVATE KEY") encodings.
func parseRSAKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, fmt.Errorf("no PEM block found in credentials")
	}

	// Try PKCS#1 first (traditional RSA key format).
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}

	// Fall back to PKCS#8 (GitHub occasionally issues keys in this format).
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse RSA private key: %w", err)
	}
	rsaKey, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key is not RSA (got %T)", parsed)
	}
	return rsaKey, nil
}

// jwtSegment base64url-encodes the JSON serialisation of v.
func jwtSegment(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		// json.Marshal only fails on un-marshalable types (funcs, channels);
		// map[string]string and map[string]any with int64/string values are safe.
		panic("bot jwt: marshal: " + err.Error())
	}
	return base64RawURL(b)
}

// base64RawURL encodes data using standard base64url without padding.
func base64RawURL(data []byte) string {
	return base64.RawURLEncoding.EncodeToString(data)
}
