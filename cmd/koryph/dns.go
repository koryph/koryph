// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/koryph/koryph/internal/dns"
	"github.com/koryph/koryph/internal/project"
)

// githubPagesDNSClient is the deliberately small DNS surface used by the CLI.
// Keeping it to this operation means the command cannot grow into a general
// Cloudflare administration frontend.
type githubPagesDNSClient interface {
	EnsureGitHubPages(ctx context.Context, domain, pagesDomain string) error
}

var newGitHubPagesDNSClient = func(cfg dns.CloudflareConfig) (githubPagesDNSClient, error) {
	return dns.NewCloudflareClient(cfg)
}

func init() {
	registerCmd(command{
		name:    "dns",
		summary: "reconcile narrowly scoped DNS records for hosted pages",
		run:     cmdDNS,
		DocLinks: []string{
			"user-guide/github-pages.md",
		},
		subs: []command{
			{
				name:     "github-pages",
				summary:  "reconcile Cloudflare DNS-only records for a GitHub Pages domain",
				run:      cmdDNSGitHubPages,
				DocLinks: []string{"user-guide/github-pages.md"},
			},
		},
	})
}

func cmdDNS(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || isHelpArg(args[0]) {
		parentHelp(stdout, "dns", "reconcile narrowly scoped DNS records for hosted pages", []subVerb{
			{"github-pages --domain DOMAIN --pages-domain NAME.github.io --vault-ref REF [--project ID]", "reconcile GitHub Pages records through Cloudflare"},
		})
		return 0
	}
	if args[0] == "github-pages" {
		return cmdDNSGitHubPages(args[1:], stdout, stderr)
	}
	return usageErr(stderr, fmt.Sprintf("unknown dns subcommand %q", args[0]))
}

// cmdDNSGitHubPages implements `koryph dns github-pages`. It uses the
// project's vault defaults when --vault-provider is omitted; the token itself
// is fetched only in memory by the DNS client and never enters CLI state.
func cmdDNSGitHubPages(args []string, stdout, stderr io.Writer) int {
	fs := newFlagSet("dns github-pages", stderr)
	flagProject := fs.String("project", "", "project id")
	flagDomain := fs.String("domain", "", "apex custom domain to configure (for example, docs.example.com)")
	flagPagesDomain := fs.String("pages-domain", "", "GitHub Pages default domain (for example, owner.github.io)")
	flagVaultRef := fs.String("vault-ref", "", "reference to the scoped Cloudflare API token in the selected vault")
	flagVaultProvider := fs.String("vault-provider", "", "vault provider for --vault-ref (uses the project/global fallback ladder when omitted)")
	setUsage(fs, stdout,
		"reconcile Cloudflare DNS-only records for a GitHub Pages custom domain",
		"[--project ID] --domain DOMAIN --pages-domain NAME.github.io --vault-ref REF [--vault-provider PROVIDER]")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return flagExit(err)
	}
	posVal := ""
	if len(pos) > 0 {
		posVal = pos[0]
	}
	if len(pos) > 1 {
		return usageErr(stderr, "dns github-pages: accepts at most one positional project id")
	}
	projectID, code := mergeProjectID(stderr, "dns github-pages", posVal, *flagProject)
	if code != 0 {
		return code
	}
	if strings.TrimSpace(*flagDomain) == "" {
		return usageErr(stderr, "dns github-pages: --domain is required")
	}
	if strings.TrimSpace(*flagPagesDomain) == "" {
		return usageErr(stderr, "dns github-pages: --pages-domain is required")
	}
	if strings.TrimSpace(*flagVaultRef) == "" {
		return usageErr(stderr, "dns github-pages: --vault-ref is required; raw API tokens are not accepted")
	}
	if *flagVaultProvider != "" && !isKnownProvider(*flagVaultProvider) {
		return usageErr(stderr, fmt.Sprintf("dns github-pages: unknown --vault-provider %q", *flagVaultProvider))
	}

	ctx := context.Background()
	store, err := openStore(ctx)
	if err != nil {
		return fail(stderr, err)
	}
	rec, code := resolveProjectRecordCwd(stderr, store, projectID, "dns github-pages")
	if code != 0 {
		return code
	}
	if _, err := project.Load(rec.Root); err != nil {
		return fail(stderr, err)
	}

	client, err := newGitHubPagesDNSClient(dns.CloudflareConfig{
		ProjectRoot:   rec.Root,
		VaultProvider: strings.TrimSpace(*flagVaultProvider),
		VaultRef:      strings.TrimSpace(*flagVaultRef),
	})
	if err != nil {
		return fail(stderr, fmt.Errorf("dns github-pages: %w", err))
	}
	if err := client.EnsureGitHubPages(ctx, *flagDomain, *flagPagesDomain); err != nil {
		return fail(stderr, fmt.Errorf("dns github-pages: %w", err))
	}
	fmt.Fprintf(stdout, "dns github-pages: reconciled DNS-only records for %s\n", strings.TrimSpace(*flagDomain))
	return 0
}
