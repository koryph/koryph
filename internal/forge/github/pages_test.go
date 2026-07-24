// SPDX-License-Identifier: Apache-2.0
// Copyright (c) 2026 The Koryph Developers

package github_test

import (
	"context"
	"strings"
	"testing"

	githubforge "github.com/koryph/koryph/internal/forge/github"
)

func TestPagesServiceGet(t *testing.T) {
	fakeGhBin(t, `
if [ "$1" = api ] && [ "$2" = repos/acme/proj/pages ]; then
  printf '{"html_url":"https://docs.example.com","cname":"docs.example.com","https_enforced":true}\n'
  exit 0
fi
exit 1`)

	site, err := githubforge.New().Pages().Get(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if site.URL != "https://docs.example.com" || site.CustomDomain != "docs.example.com" || !site.HTTPSEnforced {
		t.Errorf("Get = %+v, want public URL, domain, and HTTPS enforcement", site)
	}
}

func TestPagesServiceSetCustomDomainAndHTTPS(t *testing.T) {
	fakeGhBin(t, `
if [ "$1" != api ] || [ "$2" != -X ] || [ "$3" != PUT ] || [ "$4" != repos/acme/proj/pages ]; then exit 1; fi
if [ "$5" != --input ]; then exit 1; fi
case "$(cat "$6")" in
  '{"cname":"docs.example.com"}'|'{"https_enforced":true}') exit 0 ;;
  *) exit 1 ;;
esac`)

	svc := githubforge.New().Pages()
	if err := svc.SetCustomDomain(context.Background(), "acme", "proj", "docs.example.com"); err != nil {
		t.Fatalf("SetCustomDomain: %v", err)
	}
	if err := svc.SetHTTPSEnforced(context.Background(), "acme", "proj", true); err != nil {
		t.Fatalf("SetHTTPSEnforced: %v", err)
	}
}

func TestPagesServiceHealth(t *testing.T) {
	fakeGhBin(t, `
if [ "$1" != api ] || [ "$2" != --include ] || [ "$3" != repos/acme/proj/pages/health ]; then exit 1; fi
printf 'HTTP/2 200 OK\r\ncontent-type: application/json\r\n\r\n{"domain":{"host":"docs.example.com","dns_resolves":true,"is_valid":true,"is_served_by_pages":true,"responds_to_https":true,"enforces_https":true,"is_https_eligible":true}}\n'`)

	health, pending, err := githubforge.New().Pages().CheckHealth(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if pending || !health.Domain.Valid || !health.Domain.RespondsToHTTPS || health.Domain.Host != "docs.example.com" {
		t.Errorf("CheckHealth = %+v, pending=%v; want completed healthy result", health, pending)
	}
}

func TestPagesServiceHealthPendingAndInvalidInterval(t *testing.T) {
	fakeGhBin(t, `
if [ "$1" != api ] || [ "$2" != --include ]; then exit 1; fi
printf 'HTTP/2 202 Accepted\r\n\r\n'`)

	svc := githubforge.New().Pages()
	health, pending, err := svc.CheckHealth(context.Background(), "acme", "proj")
	if err != nil {
		t.Fatalf("CheckHealth: %v", err)
	}
	if health != nil || !pending {
		t.Errorf("CheckHealth = (%+v, %v), want (nil, true)", health, pending)
	}
	if _, err := svc.WaitForHealth(context.Background(), "acme", "proj", 0); err == nil || !strings.Contains(err.Error(), "positive") {
		t.Errorf("WaitForHealth(interval=0) error = %v, want positive-interval error", err)
	}
}
