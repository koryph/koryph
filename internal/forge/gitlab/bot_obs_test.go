// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package gitlab

// Adversarial redaction tests for forge.api spans — GitLab provider (§O4).
//
// These tests drive the gitlabBotSvc HTTP calls against a local httptest
// server that returns fake token / credential material in its JSON responses.
// A raw capturing handler (no redaction) is installed and every captured log
// record is scanned for secret content.
//
// Tests live in package gitlab (not gitlab_test) so they can set the
// KORYPH_GITLAB_HOST environment variable to redirect API calls and access
// unexported helpers.

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/koryph/koryph/internal/obs"
)

// Ensure net/url is used (url.Values in doVariableRequest test).
var _ = url.Values{}

// rawCaptureGL is a non-redacting slog.Handler for adversarial tests.
type rawCaptureGL struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *rawCaptureGL) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (h *rawCaptureGL) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.records = append(h.records, r.Clone())
	h.mu.Unlock()
	return nil
}

func (h *rawCaptureGL) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *rawCaptureGL) WithGroup(_ string) slog.Handler      { return h }

func (h *rawCaptureGL) assertNoSubstring(t *testing.T, secret string) {
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
				n := len(s)
				if n > 120 {
					n = 120
				}
				t.Errorf("secret leaked into log output: field=%q (truncated)", s[:n])
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

func glDebugCfg() obs.Config { return obs.Config{DefaultLevel: "debug"} }

// redirectToTestServer sets glAPIBaseOverride to the test server's URL so all
// gitlab HTTP calls go to the local httptest.Server (HTTP, not HTTPS).
func redirectToTestServer(t *testing.T, srvURL string) {
	t.Helper()
	origOverride := glAPIBaseOverride
	glAPIBaseOverride = srvURL + "/api/v4"
	t.Cleanup(func() { glAPIBaseOverride = origOverride })
}

// ---------- adversarial tests ------------------------------------------------

// TestGitLabFetchTokenSelf_NoSecretInLogs exercises fetchTokenSelf against a
// test server returning a fake token response. The token value (passed as
// PRIVATE-TOKEN header) must never appear in any log record.
func TestGitLabFetchTokenSelf_NoSecretInLogs(t *testing.T) {
	fakeToken := "glpat-" + "FakeTokenSelfAdversarialTest0123456"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify token is in header, not URL.
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id":         42,
			"name":       "test-token",
			"scopes":     []string{"api"},
			"expires_at": "",
			"revoked":    false,
			"active":     true,
		})
	}))
	defer srv.Close()

	redirectToTestServer(t, srv.URL)

	capture := &rawCaptureGL{}
	obs.ReInitRaw(glDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	ctx := context.Background()
	info, err := fetchTokenSelf(ctx, fakeToken, "")
	if err != nil {
		t.Fatalf("fetchTokenSelf: %v", err)
	}
	if info.Name == "" {
		t.Fatal("fetchTokenSelf returned empty name (sanity check)")
	}

	// The token value must NOT appear in any log record.
	capture.assertNoSubstring(t, fakeToken)
	capture.assertNoSubstring(t, "FakeTokenSelf")
}

// TestGitLabFetchTokenSelf_SafeFieldsLogged verifies the span emits safe
// metadata: provider=gitlab, endpoint_class=token_self, latency_ms, status.
func TestGitLabFetchTokenSelf_SafeFieldsLogged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": 1, "name": "tok", "scopes": []string{"api"},
			"active": true, "revoked": false,
		})
	}))
	defer srv.Close()

	redirectToTestServer(t, srv.URL)

	capture := &rawCaptureGL{}
	obs.ReInitRaw(glDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	_, err := fetchTokenSelf(context.Background(), "safe-test-token", "")
	if err != nil {
		t.Fatalf("fetchTokenSelf: %v", err)
	}

	capture.mu.Lock()
	recs := capture.records
	capture.mu.Unlock()

	if len(recs) == 0 {
		t.Fatal("no log records captured for fetchTokenSelf")
	}

	var foundProvider, foundEndpoint, foundLatency bool
	for _, r := range recs {
		r.Attrs(func(a slog.Attr) bool {
			switch a.Key {
			case obs.KeyProvider:
				if a.Value.String() == "gitlab" {
					foundProvider = true
				}
			case obs.KeyEndpointClass:
				if a.Value.String() == "token_self" {
					foundEndpoint = true
				}
			case obs.KeyLatencyMS:
				foundLatency = true
			}
			return true
		})
	}
	if !foundProvider {
		t.Error("provider=gitlab not found in span")
	}
	if !foundEndpoint {
		t.Error("endpoint_class=token_self not found in span")
	}
	if !foundLatency {
		t.Error("latency_ms not found in span")
	}
}

// TestGitLabDoVariableRequest_NoSecretInLogs exercises doVariableRequest.
// The token in the PRIVATE-TOKEN header and the variable value in the form
// body must NEVER reach any log record.
func TestGitLabDoVariableRequest_NoSecretInLogs(t *testing.T) {
	fakeToken := "glpat-" + "FakeVarRequestTokenDoNotLog0123456"
	fakeVarValue := "super-secret-variable-value-do-not-log"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Parse form to verify value was transmitted (but server must not echo it).
		if err := r.ParseForm(); err == nil {
			// Server silently consumes the value — it is NOT reflected back.
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	redirectToTestServer(t, srv.URL)

	capture := &rawCaptureGL{}
	obs.ReInitRaw(glDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	form := url.Values{}
	form.Set("value", fakeVarValue)
	form.Set("masked", "true")

	ctx := context.Background()
	err := doVariableRequest(ctx, fakeToken, http.MethodPut, srv.URL+"/projects/ns%2Fproj/variables/MY_KEY", form)
	if err != nil {
		t.Fatalf("doVariableRequest: %v", err)
	}

	// Neither the token nor the variable value must appear in logs.
	capture.assertNoSubstring(t, fakeToken)
	capture.assertNoSubstring(t, fakeVarValue)
	capture.assertNoSubstring(t, "FakeVarRequest")
}

// TestGitLabListProjectVariables_NoSecretInLogs exercises ListProjectVariables.
// The token and variable VALUES in the API response must never appear in logs.
// Variable KEYS are safe to log (they are CI variable names, not secrets).
func TestGitLabListProjectVariables_NoSecretInLogs(t *testing.T) {
	fakeToken := "glpat-" + "FakeListVarsTokenDoNotLog01234567"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Response includes values — they must NOT be logged by our code.
		_ = json.NewEncoder(w).Encode([]map[string]string{
			{"key": "KORYPH_BOT_TOKEN", "value": "SecretBotTokenValueDoNotLog"},
			{"key": "KORYPH_BOT_TOKEN_EXPIRY", "value": "2026-12-31"},
		})
	}))
	defer srv.Close()

	redirectToTestServer(t, srv.URL)

	capture := &rawCaptureGL{}
	obs.ReInitRaw(glDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	keys, err := ListProjectVariables(context.Background(), fakeToken, "ns/proj")
	if err != nil {
		t.Fatalf("ListProjectVariables: %v", err)
	}
	if len(keys) == 0 {
		t.Fatal("expected at least one variable key")
	}

	// Token must not appear in logs.
	capture.assertNoSubstring(t, fakeToken)
	capture.assertNoSubstring(t, "FakeListVars")
	// Variable VALUES must not appear in logs.
	capture.assertNoSubstring(t, "SecretBotTokenValueDoNotLog")
}

// TestGitLabErrorPath_NoSecretInLogs verifies that error spans (e.g. 401)
// do not include credential material — the error references only the HTTP
// status, not the token value.
func TestGitLabErrorPath_NoSecretInLogs(t *testing.T) {
	fakeToken := "glpat-" + "FakeErrorPathTokenDoNotLog0123456"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer srv.Close()

	redirectToTestServer(t, srv.URL)

	capture := &rawCaptureGL{}
	obs.ReInitRaw(glDebugCfg(), capture)
	t.Cleanup(func() {
		obs.ReInitRaw(obs.Config{DefaultLevel: "info"}, slog.NewTextHandler(os.Stderr, nil))
	})

	_, err := fetchTokenSelf(context.Background(), fakeToken, "")
	if err == nil {
		t.Fatal("expected error from 401 response")
	}

	capture.assertNoSubstring(t, fakeToken)
	capture.assertNoSubstring(t, "FakeErrorPath")
}
