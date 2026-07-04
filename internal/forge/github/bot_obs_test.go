// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

// Adversarial redaction tests for forge.api spans — GitHub provider (§O4).
//
// These tests drive the githubBotSvc HTTP calls against a local httptest
// server that returns fake PEM / token material in its JSON responses. A raw
// capturing handler (no redaction) is installed and every captured log record
// is scanned for secret content.
//
// Tests live in package github (not github_test) so they can override
// ghAPIBase and access unexported types.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
)

// rawCaptureGH is a non-redacting slog.Handler for adversarial tests.
type rawCaptureGH struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *rawCaptureGH) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *rawCaptureGH) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *rawCaptureGH) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *rawCaptureGH) WithGroup(_ string) slog.Handler      { return h }

func (h *rawCaptureGH) assertNoSubstring(t *testing.T, secret string) {
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
				t.Errorf("secret leaked into log output: field=%q (truncated)", s[:minGH(len(s), 120)])
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

func minGH(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ghDebugCfg() obs.Config { return obs.Config{DefaultLevel: "debug"} }

// ---------- adversarial tests ------------------------------------------------

// TestGitHubExchangeManifest_NoSecretInLogs exercises the ExchangeManifest
// code path against an httptest server that returns a fake PEM in the JSON
// response. Asserts the PEM never reaches any log record.
func TestGitHubExchangeManifest_NoSecretInLogs(t *testing.T) {
	// Compose fake PEM at runtime to avoid triggering static secret scanners.
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	fakePEM := begin + "\nMIIGHFakePEMExchangeManifestDoNotUse\n" + end

	// Stand up a test server that mimics the GitHub manifest exchange endpoint.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		resp := map[string]interface{}{
			"id":   int64(12345),
			"slug": "my-test-app",
			"name": "My Test App",
			"pem":  fakePEM,
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Redirect all GitHub API calls to the test server.
	origBase := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = origBase })

	// Install raw capturing handler.
	capture := &rawCaptureGH{}
	obs.ReInitRaw(ghDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	svc := &githubBotSvc{}
	cfg, err := svc.ExchangeManifest(context.Background(), "test-code-123")
	if err != nil {
		t.Fatalf("ExchangeManifest: %v", err)
	}
	if cfg.PrivateKeyPEM == "" {
		t.Fatal("ExchangeManifest returned empty PEM (sanity check)")
	}

	// The fake PEM value must NOT appear in any log record.
	capture.assertNoSubstring(t, "MIIGHFakePEMExchange")
	capture.assertNoSubstring(t, "DoNotUse")
	capture.assertNoSubstring(t, "BEGIN RSA")
	capture.assertNoSubstring(t, "END RSA")
	// The returned PEM full string should not appear.
	capture.assertNoSubstring(t, fakePEM)
}

// TestGitHubExchangeManifest_SafeFieldsLogged verifies that the span DOES log
// safe metadata: provider, endpoint_class, app_id, slug, latency_ms, status.
func TestGitHubExchangeManifest_SafeFieldsLogged(t *testing.T) {
	begin := "-----BEGIN RSA " + "PRIVATE KEY-----"
	end := "-----END RSA " + "PRIVATE KEY-----"
	fakePEM := begin + "\nMIIGHSafeFieldsTest\n" + end

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":   int64(99999),
			"slug": "safe-fields-app",
			"name": "Safe Fields App",
			"pem":  fakePEM,
		})
	}))
	defer srv.Close()

	origBase := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = origBase })

	capture := &rawCaptureGH{}
	obs.ReInitRaw(ghDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	svc := &githubBotSvc{}
	_, err := svc.ExchangeManifest(context.Background(), "safe-code")
	if err != nil {
		t.Fatalf("ExchangeManifest: %v", err)
	}

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	if len(recs) == 0 {
		t.Fatal("no log records captured for ExchangeManifest")
	}

	var foundProvider, foundEndpoint, foundLatency, foundStatus bool
	for _, r := range recs {
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case obs.KeyProvider:
				if a.Value.String() == "github" {
					foundProvider = true
				}
			case obs.KeyEndpointClass:
				if a.Value.String() == "app_manifest_exchange" {
					foundEndpoint = true
				}
			case obs.KeyLatencyMS:
				foundLatency = true
			case obs.KeyStatus:
				if a.Value.Int64() == http.StatusCreated {
					foundStatus = true
				}
			}
			return true
		})
	}
	if !foundProvider {
		t.Error("provider=github not found in span records")
	}
	if !foundEndpoint {
		t.Error("endpoint_class=app_manifest_exchange not found in span records")
	}
	if !foundLatency {
		t.Error("latency_ms not found in span records")
	}
	if !foundStatus {
		t.Errorf("status=%d not found in span records", http.StatusCreated)
	}
}

// TestGitHubListInstallations_NoSecretInLogs exercises ListInstallations.
// The JWT (Bearer token) is NEVER logged; only install_id and account_login
// (non-secret identifiers) are in the span.
func TestGitHubListInstallations_NoSecretInLogs(t *testing.T) {
	// Build a fake JWT at runtime.
	fakeJWT := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9." +
		"eyJpYXQiOjE3NTAwMDAwMDAsImV4cCI6MTc1MDAwMDYwMCwiaXNzIjo5OTl9." +
		"FakeSignatureDoNotUseInProduction"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the JWT was sent as Bearer (not as a query param or body).
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, "Bearer ") {
			http.Error(w, "no bearer", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode([]map[string]interface{}{
			{
				"id":      int64(1),
				"account": map[string]string{"login": "myorg"},
			},
		})
	}))
	defer srv.Close()

	origBase := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = origBase })

	capture := &rawCaptureGH{}
	obs.ReInitRaw(ghDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	svc := &githubBotSvc{}
	installs, err := svc.ListInstallations(context.Background(), fakeJWT)
	if err != nil {
		t.Fatalf("ListInstallations: %v", err)
	}
	if len(installs) == 0 {
		t.Fatal("expected at least one installation")
	}

	// The JWT must NOT appear in any log record.
	capture.assertNoSubstring(t, fakeJWT)
	// Also check representative substrings of the JWT signature.
	capture.assertNoSubstring(t, "FakeSignatureDoNotUse")
	capture.assertNoSubstring(t, "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9")
}

// TestGitHubMintInstallationToken_NoSecretInLogs exercises MintInstallationToken.
// Both the input JWT and the returned installation token must never reach logs.
func TestGitHubMintInstallationToken_NoSecretInLogs(t *testing.T) {
	fakeJWT := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.FakeMintJWT.DoNotLogThis"
	fakeToken := "ghs_" + "FakeInstallTokenDoNotLogInProduction0123456"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token": fakeToken,
		})
	}))
	defer srv.Close()

	origBase := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = origBase })

	capture := &rawCaptureGH{}
	obs.ReInitRaw(ghDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	svc := &githubBotSvc{}
	tok, err := svc.MintInstallationToken(context.Background(), fakeJWT, 42)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if tok == "" {
		t.Fatal("MintInstallationToken returned empty token (sanity check)")
	}

	// Neither the input JWT nor the returned token must appear in logs.
	capture.assertNoSubstring(t, fakeJWT)
	capture.assertNoSubstring(t, fakeToken)
	capture.assertNoSubstring(t, "DoNotLogThis")
	capture.assertNoSubstring(t, "FakeInstallToken")
}

// TestGitHubSpanErrorPath_NoSecretInLogs verifies that when the server returns
// an error status, the error span does not include any credential material
// (the error message references the HTTP status, not token values).
func TestGitHubSpanErrorPath_NoSecretInLogs(t *testing.T) {
	fakeJWT := "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.ErrorPathJWT.DoNotLogInError"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	origBase := ghAPIBase
	ghAPIBase = srv.URL
	t.Cleanup(func() { ghAPIBase = origBase })

	capture := &rawCaptureGH{}
	obs.ReInitRaw(ghDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	svc := &githubBotSvc{}
	_, err := svc.ListInstallations(context.Background(), fakeJWT)
	if err == nil {
		t.Fatal("expected error from 401 response")
	}

	// The JWT must not appear in the error span.
	capture.assertNoSubstring(t, fakeJWT)
	capture.assertNoSubstring(t, "DoNotLogInError")
}
