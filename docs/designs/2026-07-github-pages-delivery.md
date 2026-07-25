<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# GitHub Pages delivery: docs publishing, custom domains, and Cloudflare DNS

## Problem

Koryph projects need a secure, ejectable path from repository documentation to
a published GitHub Pages site with a custom domain. The workflow spans three
ownership boundaries—forge configuration, CI assets, and DNS—and previously
existed only as terse epic prose. That left implementation agents to infer
provider semantics and security posture.

## Goals

- Provide in-binary Pages, DNS, and docs-setup capabilities.
- Keep GitHub operations behind `internal/forge`.
- Keep Cloudflare tokens behind the signing vault abstraction.
- Reconcile only the documented GitHub Pages DNS records.
- Install a least-privilege GitHub Actions Pages workflow.
- Support MkDocs and Zensical presets without SaaS lock-in.
- Diagnose Pages, HTTPS, workflow, and DNS drift through doctor.

## Non-goals

- General-purpose DNS management.
- Cloudflare proxying, load balancing, redirects, or certificate products.
- A hosted Koryph documentation service.
- Provider credentials in project files, command arguments, or logs.
- Live-provider calls in the unit gate.

## Current state

- `internal/forge` owns forge-native services and GitHub implementations.
- `internal/dns/cloudflare.go` owns the native Cloudflare v4 client.
- `cmd/koryph/dns.go` exposes the current DNS setup surface.
- `internal/ciinstall` installs rendered workflow assets.
- `docs/designs/2026-07-enhancement-roadmap.md` section F records the original
  strict Pages workflow direction.

## Decision ledger

| Decision | Rejected alternative | Invariant / consumers |
|---|---|---|
| Native Cloudflare REST v4 client | `flarectl` maintenance-mode dependency or preview Node `cf` CLI | Capability ships in the Koryph binary |
| Vault reference is required; token stays memory-only | Raw token flag/environment persistence | Credentials never enter argv, project state, logs, or generated assets |
| Manage exactly four apex A, four apex AAAA, and one `www` CNAME | General-purpose DNS reconciliation | Unrelated records are never deleted or modified |
| Records are DNS-only with automatic TTL | Cloudflare proxying | GitHub controls Pages TLS validation and serving |
| Find the nearest active parent zone | Require the requested hostname to equal the zone apex | Subdomains work without guessing a fixed account zone |
| GitHub Pages operations live behind `PagesService` | Direct `gh` or API calls from CLI/doctor | Forge portability remains enforceable |
| Workflow deploys only trusted default-branch pushes/manual dispatches | Deploy arbitrary refs or pull requests | Untrusted code cannot publish Pages |
| Publishing permissions are job-scoped | Workflow-global write/OIDC permissions | Build jobs remain read-only |
| MkDocs/Zensical presets are data | Engine-specific command branches | Adding an engine does not rewrite setup orchestration |

## Design

### Forge Pages service

`internal/forge` exposes custom-domain configuration, Pages health/certificate
state, and HTTPS enforcement. The GitHub provider owns API details. CLI and
doctor consume only this interface.

### Cloudflare DNS reconciler

The client resolves a scoped token through the vault fallback ladder, discovers
the nearest active parent zone, and reconciles the exact GitHub Pages record
set. Existing correct records are retained; wrong TTL/proxy state is patched;
missing desired content is created; unrelated and stale non-managed records are
left untouched. Responses are bounded and provider errors are normalized
without leaking payloads or credentials.

### Documentation workflow renderer

The GitHub renderer produces an installable docs asset with:

- dependency installation and strict MkDocs/Zensical build;
- `configure-pages` and `upload-pages-artifact`;
- deployment only from the configured default branch or explicit manual
  dispatch;
- `pages: write` and `id-token: write` only on the deploy job;
- concurrency cancellation appropriate for Pages deployments.

The renderer remains behind the existing forge/CI asset seam and keeps GitLab
capability behavior explicit.

### Operator workflows

`koryph dns setup` reconciles DNS and Pages custom-domain state, with dry-run
evidence. `koryph docs setup --engine mkdocs|zensical [--domain ...]` seeds the
minimal docs/config files, installs the workflow asset, configures Pages
through the forge service, and delegates domain work to the DNS service.

Doctor compares rendered asset drift, Pages source/domain/HTTPS state, and the
managed DNS record set. Findings identify the owning command and never reveal
tokens.

## Implementation units

| Unit | Owned paths | Dependencies | Acceptance |
|---|---|---|---|
| Pages service | `internal/forge/**` | none | GitHub implementation and seam tests |
| Cloudflare client | `internal/dns/**`, DNS CLI/docs | vault abstraction | Exact record reconciliation, bounded responses, no raw token |
| Docs renderer | forge renderer/assets and tests | Pages service | Trusted-ref and least-privilege workflow tests |
| DNS/doctor workflow | CLI + doctor consumers | Pages service, Cloudflare client | Dry-run, reconciliation, and drift findings |
| Docs setup | CLI/docs/presets | Pages service, docs renderer | Both engines install strict workflow and configure Pages |

## Acceptance criteria

- The exact nine-record Pages DNS set reconciles idempotently without touching
  unrelated records.
- Tokens resolve only through the vault abstraction and never appear in
  persisted/project output.
- GitHub Pages custom-domain and HTTPS operations go through `PagesService`.
- Rendered docs workflows cannot publish non-default refs automatically and
  grant write/OIDC permissions only to deployment.
- MkDocs and Zensical setup paths install working strict-build assets.
- Doctor reports workflow, Pages, HTTPS, and managed-DNS drift.
- Unit/fake-provider tests and `make gate-agent` pass; live validation remains
  an explicit operator canary, not a normal gate dependency.

## Open questions / assumptions

- GitHub's published Pages IP set remains a reviewed source constant and is
  updated through a normal schema-free code change when GitHub changes it.
- Cloudflare accounts may contain more than 100 exact-name records only in
  pathological cases; pagination can be added if provider evidence requires
  it.
