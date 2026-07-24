// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"strings"
	"testing"

	"github.com/koryph/koryph/internal/dns"
)

func TestDNSGitHubPages_ReconcilesWithVaultReference(t *testing.T) {
	isolate(t)
	root := gitRepo(t)
	registerProjectForCI(t, "dnstest", root)

	oldNewClient := newGitHubPagesDNSClient
	t.Cleanup(func() { newGitHubPagesDNSClient = oldNewClient })
	var gotConfig dns.CloudflareConfig
	var gotDomain, gotPagesDomain string
	newGitHubPagesDNSClient = func(cfg dns.CloudflareConfig) (githubPagesDNSClient, error) {
		gotConfig = cfg
		return githubPagesDNSClientFunc(func(_ context.Context, domain, pagesDomain string) error {
			gotDomain, gotPagesDomain = domain, pagesDomain
			return nil
		}), nil
	}

	code, out, errb := runCmd("dns", "github-pages", "--project", "dnstest",
		"--domain", "docs.example.com", "--pages-domain", "acme.github.io", "--vault-ref", "op://infra/cloudflare-token")
	if code != 0 {
		t.Fatalf("dns github-pages: code=%d stdout=%s stderr=%s", code, out, errb)
	}
	if gotConfig.ProjectRoot != root || gotConfig.VaultRef != "op://infra/cloudflare-token" || gotConfig.VaultProvider != "" {
		t.Errorf("CloudflareConfig = %+v, want project root + vault reference and fallback provider", gotConfig)
	}
	if gotDomain != "docs.example.com" || gotPagesDomain != "acme.github.io" {
		t.Errorf("EnsureGitHubPages(%q, %q), want docs.example.com, acme.github.io", gotDomain, gotPagesDomain)
	}
	if !strings.Contains(out, "reconciled DNS-only records") {
		t.Errorf("stdout = %q, want reconciliation confirmation", out)
	}
}

func TestDNSGitHubPages_RejectsMissingVaultReference(t *testing.T) {
	code, _, errb := runCmd("dns", "github-pages", "--domain", "docs.example.com", "--pages-domain", "acme.github.io")
	if code == 0 || !strings.Contains(errb, "raw API tokens are not accepted") {
		t.Errorf("code=%d stderr=%q, want vault reference validation", code, errb)
	}
}

type githubPagesDNSClientFunc func(context.Context, string, string) error

func (f githubPagesDNSClientFunc) EnsureGitHubPages(ctx context.Context, domain, pagesDomain string) error {
	return f(ctx, domain, pagesDomain)
}
