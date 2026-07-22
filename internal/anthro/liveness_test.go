// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package anthro

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestProbeLivenessHeaderScheme proves the two auth-mode header schemes
// (design docs/designs/2026-07-api-key-auth.md §5): api-key uses x-api-key
// only, oauth-token uses Authorization: Bearer plus the oauth-2025-04-20
// anthropic-beta header — never both, never the wrong one for the mode.
func TestProbeLivenessHeaderScheme(t *testing.T) {
	cases := []struct {
		name       string
		useBearer  bool
		credential string
		check      func(t *testing.T, r *http.Request)
	}{
		{
			name:       "api-key uses x-api-key",
			useBearer:  false,
			credential: "sk-ant-test-key",
			check: func(t *testing.T, r *http.Request) {
				if got := r.Header.Get("x-api-key"); got != "sk-ant-test-key" {
					t.Errorf("x-api-key = %q, want sk-ant-test-key", got)
				}
				if got := r.Header.Get("Authorization"); got != "" {
					t.Errorf("Authorization present for api-key mode: %q", got)
				}
				if got := r.Header.Get("anthropic-beta"); strings.Contains(got, oauthBetaHeader) {
					t.Errorf("anthropic-beta carries the oauth beta value for api-key mode: %q", got)
				}
			},
		},
		{
			name:       "oauth-token uses bearer + beta header",
			useBearer:  true,
			credential: "oauth-test-token",
			check: func(t *testing.T, r *http.Request) {
				if got := r.Header.Get("Authorization"); got != "Bearer oauth-test-token" {
					t.Errorf("Authorization = %q, want Bearer oauth-test-token", got)
				}
				if got := r.Header.Get("x-api-key"); got != "" {
					t.Errorf("x-api-key present for oauth-token mode: %q", got)
				}
				if got := r.Header.Get("anthropic-beta"); !strings.Contains(got, oauthBetaHeader) {
					t.Errorf("anthropic-beta = %q, want it to contain %q", got, oauthBetaHeader)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath, gotMethod = r.URL.Path, r.Method
				tc.check(t, r)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "has_more": false})
			}))
			defer srv.Close()

			err := probeLiveness(context.Background(), tc.credential, tc.useBearer,
				option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
			if err != nil {
				t.Fatalf("probeLiveness: %v", err)
			}
			if gotMethod != http.MethodGet {
				t.Errorf("method = %q, want GET (free, no token spend)", gotMethod)
			}
			if gotPath != "/v1/models" {
				t.Errorf("path = %q, want /v1/models", gotPath)
			}
		})
	}
}

// TestProbeLivenessFailsOnAuthError proves an invalid/expired/revoked
// credential fails closed and the credential is never echoed into the
// error message.
func TestProbeLivenessFailsOnAuthError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"},
		})
	}))
	defer srv.Close()

	err := probeLiveness(context.Background(), "sk-should-not-leak", false,
		option.WithBaseURL(srv.URL), option.WithMaxRetries(0))
	if err == nil {
		t.Fatal("probeLiveness succeeded against a 401 response; want error (fail closed)")
	}
	if strings.Contains(err.Error(), "sk-should-not-leak") {
		t.Errorf("error message leaks the credential: %q", err.Error())
	}
}

// TestProbeLivenessEmptyCredential proves the empty-credential guard fires
// before any network call.
func TestProbeLivenessEmptyCredential(t *testing.T) {
	if err := probeLiveness(context.Background(), "", false); err == nil {
		t.Fatal("probeLiveness succeeded with an empty credential; want error")
	}
}

// TestProbeLiveness is the exported entry point's smoke test — a thin
// wrapper, so one passing case plus the unexported table above is enough.
func TestProbeLiveness(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "has_more": false})
	}))
	defer srv.Close()

	// ProbeLiveness itself takes no base-URL override, so exercise it only
	// far enough to prove the empty-credential guard (its one
	// network-independent behavior); the header-scheme/network behavior is
	// covered via probeLiveness above.
	if err := ProbeLiveness(context.Background(), "", true); err == nil {
		t.Fatal("ProbeLiveness succeeded with an empty credential; want error")
	}
}
