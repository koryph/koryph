// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package dns

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestEnsureGitHubPages_CreatesAllDNSOnlyRecords(t *testing.T) {
	tokenFile := writeToken(t, "scoped-token\n")
	var created []dnsRecord
	c := newTestCloudflareClient(t, tokenFile, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer scoped-token" {
			t.Errorf("Authorization = %q", got)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if got := r.URL.Query().Get("per_page"); got != "5" {
				t.Errorf("zone per_page = %q, want 5", got)
			}
			writeCF(t, w, []map[string]string{{"id": "zone-id"}})
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-id/dns_records":
			writeCF(t, w, []dnsRecord{})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-id/dns_records":
			var record dnsRecord
			if err := json.NewDecoder(r.Body).Decode(&record); err != nil {
				t.Fatal(err)
			}
			created = append(created, record)
			writeCF(t, w, record)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	if err := c.EnsureGitHubPages(context.Background(), "example.com", "octo.github.io"); err != nil {
		t.Fatalf("EnsureGitHubPages: %v", err)
	}
	if len(created) != 9 {
		t.Fatalf("created %d records, want 9", len(created))
	}
	sort.Slice(created, func(i, j int) bool { return created[i].Type+created[i].Content < created[j].Type+created[j].Content })
	for _, record := range created {
		if record.Proxied || record.TTL != 1 {
			t.Errorf("record %+v is not DNS-only automatic TTL", record)
		}
	}
	if got := created[len(created)-1]; got.Type != "CNAME" || got.Name != "www.example.com" || got.Content != "octo.github.io" {
		t.Errorf("CNAME = %+v", got)
	}
}

func TestEnsureGitHubPages_FindsClosestParentZone(t *testing.T) {
	tokenFile := writeToken(t, "token")
	var lookedUp []string
	c := newTestCloudflareClient(t, tokenFile, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			if got := r.URL.Query().Get("per_page"); got != "5" {
				t.Errorf("zone per_page = %q, want 5", got)
			}
			name := r.URL.Query().Get("name")
			lookedUp = append(lookedUp, name)
			if name == "example.com" {
				writeCF(t, w, []map[string]string{{"id": "zone-id"}})
				return
			}
			writeCF(t, w, []dnsRecord{})
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-id/dns_records":
			writeCF(t, w, []dnsRecord{})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-id/dns_records":
			writeCF(t, w, map[string]string{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	if err := c.EnsureGitHubPages(context.Background(), "docs.example.com", "octo.github.io"); err != nil {
		t.Fatalf("EnsureGitHubPages: %v", err)
	}
	if got, want := strings.Join(lookedUp, ","), "docs.example.com,example.com"; got != want {
		t.Errorf("zone lookups = %q, want %q", got, want)
	}
}

func TestEnsureGitHubPages_ReconcilesProxiedRecordWithoutDuplicates(t *testing.T) {
	tokenFile := writeToken(t, "token")
	var patched dnsRecord
	posts := 0
	c := newTestCloudflareClient(t, tokenFile, "", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/zones":
			writeCF(t, w, []map[string]string{{"id": "zone-id"}})
		case r.Method == http.MethodGet && r.URL.Path == "/zones/zone-id/dns_records":
			name, kind := r.URL.Query().Get("name"), r.URL.Query().Get("type")
			if name == "example.com" && kind == "A" {
				writeCF(t, w, []dnsRecord{{ID: "a-id", Type: "A", Name: name, Content: "185.199.108.153", TTL: 120, Proxied: true}})
				return
			}
			writeCF(t, w, []dnsRecord{})
		case r.Method == http.MethodPatch && r.URL.Path == "/zones/zone-id/dns_records/a-id":
			if err := json.NewDecoder(r.Body).Decode(&patched); err != nil {
				t.Fatal(err)
			}
			writeCF(t, w, map[string]string{})
		case r.Method == http.MethodPost && r.URL.Path == "/zones/zone-id/dns_records":
			posts++
			writeCF(t, w, map[string]string{})
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	if err := c.EnsureGitHubPages(context.Background(), "example.com", "octo.github.io"); err != nil {
		t.Fatalf("EnsureGitHubPages: %v", err)
	}
	if patched.TTL != 1 || patched.Proxied {
		t.Errorf("patch = %+v, want DNS-only automatic TTL", patched)
	}
	if posts != 8 {
		t.Errorf("POST count = %d, want 8 (no duplicate required A record)", posts)
	}
}

func TestEnsureGitHubPages_UsesProjectVaultFallback(t *testing.T) {
	t.Setenv("KORYPH_HOME", t.TempDir())
	tokenFile := writeToken(t, "fallback-token")
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "koryph.project.json"), []byte(`{"vault":{"provider":"file"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var authorization string
	c := newTestCloudflareClient(t, tokenFile, project, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		if r.URL.Path == "/zones" {
			writeCF(t, w, []map[string]string{{"id": "zone-id"}})
			return
		}
		writeCF(t, w, []dnsRecord{})
	}))
	if err := c.EnsureGitHubPages(context.Background(), "example.com", "octo.github.io"); err != nil {
		t.Fatalf("EnsureGitHubPages: %v", err)
	}
	if authorization != "Bearer fallback-token" {
		t.Errorf("vault fallback Authorization = %q", authorization)
	}
}

func TestNewCloudflareClient_RequiresVaultRef(t *testing.T) {
	if _, err := NewCloudflareClient(CloudflareConfig{}); err == nil || !strings.Contains(err.Error(), "vault_ref") {
		t.Fatalf("NewCloudflareClient error = %v, want vault_ref error", err)
	}
}

func TestCloudflareClient_RejectsOversizedResponse(t *testing.T) {
	tokenFile := writeToken(t, "token")
	c := newTestCloudflareClient(t, tokenFile, "", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte(`{"success":true,"result":"`))
		w.Write(make([]byte, maxCloudflareResponseBytes))
		w.Write([]byte(`"}`))
	}))

	err := c.EnsureGitHubPages(context.Background(), "example.com", "octo.github.io")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("EnsureGitHubPages error = %v, want response limit error", err)
	}
}

func TestNewCloudflareClient_UsesCloudflareHTTPSAPI(t *testing.T) {
	c, err := NewCloudflareClient(CloudflareConfig{VaultRef: "ref"})
	if err != nil {
		t.Fatal(err)
	}
	if c.baseURL != cloudflareAPIBase {
		t.Errorf("baseURL = %q, want %q", c.baseURL, cloudflareAPIBase)
	}
}

func writeToken(t *testing.T, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cloudflare-token")
	if err := os.WriteFile(p, []byte(token), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func writeCF(t *testing.T, w http.ResponseWriter, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{"success": true, "result": result}); err != nil {
		t.Fatal(err)
	}
}

func newTestCloudflareClient(t *testing.T, vaultRef, projectRoot string, handler http.Handler) *CloudflareClient {
	t.Helper()
	c, err := NewCloudflareClient(CloudflareConfig{ProjectRoot: projectRoot, VaultProvider: "file", VaultRef: vaultRef})
	if err != nil {
		t.Fatal(err)
	}
	c.httpClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Scheme != "https" || r.URL.Host != "api.cloudflare.com" {
			t.Errorf("request URL = %s, want Cloudflare HTTPS API", r.URL)
		}
		request := r.Clone(r.Context())
		requestURL := *r.URL
		requestURL.Path = strings.TrimPrefix(requestURL.Path, "/client/v4")
		request.URL = &requestURL
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	return c
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
