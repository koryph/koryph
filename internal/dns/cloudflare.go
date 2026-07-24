// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

// Package dns contains the narrowly scoped DNS clients used by koryph.
package dns

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/signing"
)

const cloudflareAPIBase = "https://api.cloudflare.com/client/v4"

// maxCloudflareResponseBytes bounds a response before decoding it. The
// Cloudflare endpoints used here return small JSON documents; accepting an
// unlimited response would let a faulty or hostile intermediary exhaust the
// process's memory.
const maxCloudflareResponseBytes = 1 << 20

// GitHubPagesARecords and GitHubPagesAAAARecords are GitHub's published
// addresses for apex custom domains. Keep them in one place so callers cannot
// accidentally configure only a subset of the required records.
var (
	GitHubPagesARecords = []string{
		"185.199.108.153",
		"185.199.109.153",
		"185.199.110.153",
		"185.199.111.153",
	}
	GitHubPagesAAAARecords = []string{
		"2606:50c0:8000::153",
		"2606:50c0:8001::153",
		"2606:50c0:8002::153",
		"2606:50c0:8003::153",
	}
)

// CloudflareConfig configures a Cloudflare DNS client. VaultRef must identify
// a narrowly scoped Cloudflare API token with Zone:Read and DNS:Edit access to
// the target zone; the token is fetched only when a request is made and is
// never persisted or logged.
//
// VaultProvider is optional. When absent, the normal vault fallback ladder is
// used: project vault, legacy project signing vault, global vault, then the OS
// default provider.
type CloudflareConfig struct {
	ProjectRoot   string
	VaultProvider string
	VaultRef      string
}

// CloudflareClient manages exactly the DNS record shapes GitHub Pages needs.
// It intentionally does not expose a general Cloudflare API surface.
type CloudflareClient struct {
	projectRoot string
	provider    string
	vaultRef    string
	baseURL     string
	httpClient  *http.Client
}

// NewCloudflareClient returns a client that retrieves its Cloudflare API token
// from the configured vault source. Raw API tokens are deliberately not an
// accepted configuration field.
func NewCloudflareClient(cfg CloudflareConfig) (*CloudflareClient, error) {
	if strings.TrimSpace(cfg.VaultRef) == "" {
		return nil, fmt.Errorf("dns: Cloudflare vault_ref is required")
	}
	return &CloudflareClient{
		projectRoot: cfg.ProjectRoot,
		provider:    strings.TrimSpace(cfg.VaultProvider),
		vaultRef:    strings.TrimSpace(cfg.VaultRef),
		baseURL:     cloudflareAPIBase,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}, nil
}

// EnsureGitHubPages creates any missing GitHub Pages records for domain and
// fixes DNS-only/automatic-TTL settings on records that already have the
// required target. It leaves unrelated and stale records untouched; deleting
// them could silently break another service sharing the zone.
//
// pagesDomain is the GitHub Pages default domain (for example,
// "octo-org.github.io") and is used as the www CNAME target.
func (c *CloudflareClient) EnsureGitHubPages(ctx context.Context, domain, pagesDomain string) error {
	domain = normalizeName(domain)
	pagesDomain = normalizeName(pagesDomain)
	if domain == "" {
		return fmt.Errorf("dns: GitHub Pages domain is required")
	}
	if pagesDomain == "" || !strings.HasSuffix(pagesDomain, ".github.io") {
		return fmt.Errorf("dns: GitHub Pages default domain must end in .github.io")
	}
	token, err := c.resolveToken(ctx)
	if err != nil {
		return err
	}
	zoneID, err := c.zoneID(ctx, token, domain)
	if err != nil {
		return err
	}
	for _, record := range githubPagesRecords(domain, pagesDomain) {
		if err := c.ensureRecord(ctx, token, zoneID, record); err != nil {
			return err
		}
	}
	return nil
}

type dnsRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

func githubPagesRecords(domain, pagesDomain string) []dnsRecord {
	records := make([]dnsRecord, 0, len(GitHubPagesARecords)+len(GitHubPagesAAAARecords)+1)
	for _, ip := range GitHubPagesARecords {
		records = append(records, dnsRecord{Type: "A", Name: domain, Content: ip, TTL: 1})
	}
	for _, ip := range GitHubPagesAAAARecords {
		records = append(records, dnsRecord{Type: "AAAA", Name: domain, Content: ip, TTL: 1})
	}
	return append(records, dnsRecord{Type: "CNAME", Name: "www." + domain, Content: pagesDomain, TTL: 1})
}

func (c *CloudflareClient) resolveToken(ctx context.Context) ([]byte, error) {
	provider := c.provider
	if provider == "" {
		defaults, err := signing.ResolveVaultDefaults(c.projectRoot)
		if err != nil {
			return nil, fmt.Errorf("dns: resolve Cloudflare vault defaults: %w", err)
		}
		provider = defaults.Provider
	}
	if provider == "" {
		provider = signing.ResolveDefaultProvider()
	}
	token, err := signing.FetchSecret(ctx, provider, c.vaultRef)
	if err != nil {
		return nil, fmt.Errorf("dns: fetch Cloudflare token from %q: %w", provider, err)
	}
	token = bytes.TrimSpace(token)
	if len(token) == 0 {
		return nil, fmt.Errorf("dns: Cloudflare token from %q is empty", provider)
	}
	return token, nil
}

func (c *CloudflareClient) zoneID(ctx context.Context, token []byte, name string) (string, error) {
	for _, candidate := range zoneCandidates(name) {
		// Cloudflare permits a per_page value from 5 through 50. The name
		// filter is exact, so five is sufficient while remaining valid.
		query := url.Values{"name": {candidate}, "status": {"active"}, "per_page": {"5"}}
		var result []struct {
			ID string `json:"id"`
		}
		if err := c.call(ctx, token, http.MethodGet, "/zones?"+query.Encode(), nil, &result); err != nil {
			return "", fmt.Errorf("dns: find Cloudflare zone %q: %w", candidate, err)
		}
		if len(result) > 0 && result[0].ID != "" {
			return result[0].ID, nil
		}
	}
	return "", fmt.Errorf("dns: active Cloudflare zone containing %q was not found", name)
}

// zoneCandidates returns a name and each parent in order, so a delegated
// hostname such as docs.example.com resolves to the closest accessible zone
// (normally example.com) without needing a public-suffix dependency.
func zoneCandidates(name string) []string {
	labels := strings.Split(normalizeName(name), ".")
	candidates := make([]string, 0, len(labels))
	for i := range labels {
		candidate := strings.Join(labels[i:], ".")
		if candidate != "" {
			candidates = append(candidates, candidate)
		}
	}
	return candidates
}

func (c *CloudflareClient) ensureRecord(ctx context.Context, token []byte, zoneID string, wanted dnsRecord) error {
	query := url.Values{"type": {wanted.Type}, "name.exact": {wanted.Name}, "per_page": {"100"}}
	var existing []dnsRecord
	endpoint := "/zones/" + url.PathEscape(zoneID) + "/dns_records"
	if err := c.call(ctx, token, http.MethodGet, endpoint+"?"+query.Encode(), nil, &existing); err != nil {
		return fmt.Errorf("dns: list %s record %q: %w", wanted.Type, wanted.Name, err)
	}
	for _, record := range existing {
		if record.Type == wanted.Type && normalizeName(record.Name) == wanted.Name && equalContent(record, wanted) {
			if record.Proxied || record.TTL != wanted.TTL {
				patch := struct {
					TTL     int  `json:"ttl"`
					Proxied bool `json:"proxied"`
				}{TTL: wanted.TTL, Proxied: false}
				if err := c.call(ctx, token, http.MethodPatch, endpoint+"/"+url.PathEscape(record.ID), patch, nil); err != nil {
					return fmt.Errorf("dns: make %s record %q DNS-only: %w", wanted.Type, wanted.Name, err)
				}
			}
			return nil
		}
	}
	if err := c.call(ctx, token, http.MethodPost, endpoint, wanted, nil); err != nil {
		return fmt.Errorf("dns: create %s record %q: %w", wanted.Type, wanted.Name, err)
	}
	return nil
}

func equalContent(got, want dnsRecord) bool {
	if want.Type == "CNAME" {
		return normalizeName(got.Content) == want.Content
	}
	return got.Content == want.Content
}

func normalizeName(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}

type cloudflareEnvelope struct {
	Success bool            `json:"success"`
	Errors  []cloudflareErr `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

type cloudflareErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (c *CloudflareClient) call(ctx context.Context, token []byte, method, endpoint string, payload, result any) error {
	var body io.Reader = http.NoBody
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+endpoint, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+string(token))
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path.Clean(endpoint), err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxCloudflareResponseBytes+1))
	if readErr != nil {
		return fmt.Errorf("read response: %w", readErr)
	}
	if len(raw) > maxCloudflareResponseBytes {
		return fmt.Errorf("read response: exceeds %d-byte limit", maxCloudflareResponseBytes)
	}
	var envelope cloudflareEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("decode response (HTTP %d): %w", resp.StatusCode, err)
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices || !envelope.Success {
		return fmt.Errorf("cloudflare API HTTP %d: %s", resp.StatusCode, cloudflareErrors(envelope.Errors))
	}
	if result != nil && len(envelope.Result) > 0 {
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

func cloudflareErrors(errs []cloudflareErr) string {
	if len(errs) == 0 {
		return "request failed"
	}
	parts := make([]string, 0, len(errs))
	for _, err := range errs {
		if err.Code == 0 {
			parts = append(parts, err.Message)
			continue
		}
		parts = append(parts, fmt.Sprintf("%d: %s", err.Code, err.Message))
	}
	return strings.Join(parts, "; ")
}
