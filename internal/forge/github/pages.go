// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/koryph/koryph/internal/forge"
)

// githubPagesSvc implements [forge.PagesService] through GitHub's Pages REST
// endpoints, invoked via the gh CLI. The health endpoint starts an asynchronous
// check and reports HTTP 202 until its DNS result is ready.
type githubPagesSvc struct{}

func (s *githubPagesSvc) ghBin() string {
	if v := os.Getenv("KORYPH_GH_BIN"); v != "" {
		return v
	}
	return "gh"
}

func (s *githubPagesSvc) ghAPI(ctx context.Context, args []string, input []byte) ([]byte, int, error) {
	finalArgs := args
	if input != nil {
		f, err := os.CreateTemp("", "koryph-pages-*.json")
		if err != nil {
			return nil, -1, fmt.Errorf("github pages: create temp: %w", err)
		}
		defer os.Remove(f.Name()) //nolint:errcheck
		if _, err := f.Write(input); err != nil {
			f.Close()
			return nil, -1, fmt.Errorf("github pages: write temp: %w", err)
		}
		if err := f.Close(); err != nil {
			return nil, -1, fmt.Errorf("github pages: close temp: %w", err)
		}
		finalArgs = append(append([]string{}, args...), "--input", f.Name())
	}
	cmd := exec.CommandContext(ctx, s.ghBin(), finalArgs...) //nolint:gosec
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if runErr := cmd.Run(); runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			return out.Bytes(), ee.ExitCode(), nil
		}
		return nil, -1, fmt.Errorf("github pages: exec gh: %w: %s", runErr, errb.String())
	}
	return out.Bytes(), 0, nil
}

func pagesEndpoint(owner, repo string) string {
	return fmt.Sprintf("repos/%s/%s/pages", owner, repo)
}

// Get fetches the current Pages site settings.
func (s *githubPagesSvc) Get(ctx context.Context, owner, repo string) (*forge.PagesSite, error) {
	raw, code, err := s.ghAPI(ctx, []string{"api", pagesEndpoint(owner, repo)}, nil)
	if err != nil {
		return nil, fmt.Errorf("github pages: get site: %w", err)
	}
	if code != 0 {
		return nil, fmt.Errorf("github pages: get site for %q/%q: gh exited %d: %s", owner, repo, code, strings.TrimSpace(string(raw)))
	}
	var site struct {
		HTMLURL       string `json:"html_url"`
		CNAME         string `json:"cname"`
		HTTPSEnforced bool   `json:"https_enforced"`
	}
	if err := json.Unmarshal(raw, &site); err != nil {
		return nil, fmt.Errorf("github pages: parse site: %w", err)
	}
	return &forge.PagesSite{URL: site.HTMLURL, CustomDomain: site.CNAME, HTTPSEnforced: site.HTTPSEnforced}, nil
}

// SetCustomDomain updates the Pages CNAME. An empty domain clears it.
func (s *githubPagesSvc) SetCustomDomain(ctx context.Context, owner, repo, domain string) error {
	var cname *string
	if domain != "" {
		cname = &domain
	}
	payload, err := json.Marshal(struct {
		CNAME *string `json:"cname"`
	}{CNAME: cname})
	if err != nil {
		return fmt.Errorf("github pages: marshal custom domain: %w", err)
	}
	return s.update(ctx, owner, repo, payload, "set custom domain")
}

// CheckHealth returns the DNS health result. GitHub reports the initial
// asynchronous calculation as a successful gh invocation with HTTP 202, so
// --include lets us distinguish it from the completed HTTP 200 response.
func (s *githubPagesSvc) CheckHealth(ctx context.Context, owner, repo string) (*forge.PagesHealth, bool, error) {
	raw, code, err := s.ghAPI(ctx, []string{"api", "--include", pagesEndpoint(owner, repo) + "/health"}, nil)
	if err != nil {
		return nil, false, fmt.Errorf("github pages: check health: %w", err)
	}
	if code != 0 {
		return nil, false, fmt.Errorf("github pages: check health for %q/%q: gh exited %d: %s", owner, repo, code, strings.TrimSpace(string(raw)))
	}
	status, body, err := splitIncludedResponse(raw)
	if err != nil {
		return nil, false, fmt.Errorf("github pages: parse health response: %w", err)
	}
	if status == 202 {
		return nil, true, nil
	}
	if status != 200 {
		return nil, false, fmt.Errorf("github pages: check health for %q/%q: unexpected HTTP %d: %s", owner, repo, status, strings.TrimSpace(string(body)))
	}
	var rawHealth githubPagesHealth
	if err := json.Unmarshal(body, &rawHealth); err != nil {
		return nil, false, fmt.Errorf("github pages: parse health: %w", err)
	}
	health := forge.PagesHealth{Domain: rawHealth.Domain.forgeHealth()}
	if rawHealth.AltDomain != nil {
		alt := rawHealth.AltDomain.forgeHealth()
		health.AltDomain = &alt
	}
	return &health, false, nil
}

// WaitForHealth polls GitHub until its asynchronous DNS health check completes.
func (s *githubPagesSvc) WaitForHealth(ctx context.Context, owner, repo string, interval time.Duration) (*forge.PagesHealth, error) {
	if interval <= 0 {
		return nil, fmt.Errorf("github pages: health poll interval must be positive")
	}
	for {
		health, pending, err := s.CheckHealth(ctx, owner, repo)
		if err != nil || !pending {
			return health, err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// SetHTTPSEnforced updates GitHub Pages' HTTPS redirect setting.
func (s *githubPagesSvc) SetHTTPSEnforced(ctx context.Context, owner, repo string, enabled bool) error {
	payload, err := json.Marshal(struct {
		HTTPSEnforced bool `json:"https_enforced"`
	}{HTTPSEnforced: enabled})
	if err != nil {
		return fmt.Errorf("github pages: marshal HTTPS setting: %w", err)
	}
	return s.update(ctx, owner, repo, payload, "set HTTPS enforcement")
}

func (s *githubPagesSvc) update(ctx context.Context, owner, repo string, payload []byte, action string) error {
	raw, code, err := s.ghAPI(ctx, []string{"api", "-X", "PUT", pagesEndpoint(owner, repo)}, payload)
	if err != nil {
		return fmt.Errorf("github pages: %s: %w", action, err)
	}
	if code != 0 {
		return fmt.Errorf("github pages: %s for %q/%q: gh exited %d: %s", action, owner, repo, code, strings.TrimSpace(string(raw)))
	}
	return nil
}

func splitIncludedResponse(raw []byte) (int, []byte, error) {
	normalized := strings.ReplaceAll(string(raw), "\r\n", "\n")
	head, body, ok := strings.Cut(normalized, "\n\n")
	if !ok {
		return 0, nil, fmt.Errorf("missing header separator")
	}
	lines := strings.Split(head, "\n")
	if len(lines) == 0 {
		return 0, nil, fmt.Errorf("missing status line")
	}
	fields := strings.Fields(lines[0])
	if len(fields) < 2 || !strings.HasPrefix(fields[0], "HTTP/") {
		return 0, nil, fmt.Errorf("invalid status line %q", lines[0])
	}
	var status int
	if _, err := fmt.Sscanf(fields[1], "%d", &status); err != nil {
		return 0, nil, fmt.Errorf("parse status line %q: %w", lines[0], err)
	}
	return status, []byte(body), nil
}

type githubPagesHealth struct {
	Domain    githubPagesDomainHealth  `json:"domain"`
	AltDomain *githubPagesDomainHealth `json:"alt_domain"`
}

type githubPagesDomainHealth struct {
	Host            string `json:"host"`
	DNSResolves     bool   `json:"dns_resolves"`
	Valid           bool   `json:"is_valid"`
	ServedByPages   bool   `json:"is_served_by_pages"`
	RespondsToHTTPS bool   `json:"responds_to_https"`
	EnforcesHTTPS   bool   `json:"enforces_https"`
	HTTPSEligible   bool   `json:"is_https_eligible"`
	Reason          string `json:"reason"`
	HTTPSError      string `json:"https_error"`
	CAAError        string `json:"caa_error"`
}

func (h githubPagesDomainHealth) forgeHealth() forge.PagesDomainHealth {
	return forge.PagesDomainHealth{
		Host: h.Host, DNSResolves: h.DNSResolves, Valid: h.Valid,
		ServedByPages: h.ServedByPages, RespondsToHTTPS: h.RespondsToHTTPS,
		EnforcesHTTPS: h.EnforcesHTTPS, HTTPSEligible: h.HTTPSEligible,
		Reason: h.Reason, HTTPSError: h.HTTPSError, CAAError: h.CAAError,
	}
}

var _ forge.PagesService = (*githubPagesSvc)(nil)
