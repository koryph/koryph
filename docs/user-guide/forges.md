<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Choosing a forge

A **forge** is the hosted service wrapped around git: repository settings,
branch protection, pull/merge requests, CI pipelines, secrets, releases, pages,
and bot identity. Koryph's core loop — clones, worktrees, branches,
fast-forward merges, commit signing, the green gate — is entirely git-native
and never talks to a forge. Only the *edges* do, and each of those edges is
served by a pluggable **forge provider**.

Koryph ships two providers:

- **GitHub** — the reference implementation. Everything koryph does is
  implemented and battle-tested here first.
- **GitLab** — a first-class alternative (gitlab.com and self-managed). It
  implements the same contract, with a small number of honest capability gaps
  called out below.

Any other git service (Gitea, Forgejo, Bitbucket, …) can be added by
implementing the provider contract in `internal/forge/`; those providers are
not shipped yet.

---

## The `forge` field

Each project records its forge in `koryph.project.json`:

```json
{
  "name": "widgets",
  "forge": "github"
}
```

- The field is **optional and defaults to `github`**, so existing projects
  need no change — full back-compat.
- Set it to `"gitlab"` to select the GitLab provider.
- `koryph doctor` reports the resolved forge for the project.

### Remote-URL sniffing (suggests, never decides)

When you onboard a project, koryph inspects the git remote URL and *suggests* a
forge:

| Remote URL contains | Suggested forge |
|---|---|
| `github.com` | `github` |
| `gitlab.com` or `gitlab.` (self-managed) | `gitlab` |
| anything else | *(none — you choose)* |

Sniffing is an onboarding assist only. The `forge` field in
`koryph.project.json` is always authoritative; koryph never overrides it from
the remote URL.

---

## Capability differences

Providers advertise **capabilities** so features are gated by what a forge can
do, not by its name. The table below is the honest state of the two shipped
providers. Where GitLab lacks a native feature, koryph either selects a
different mechanism that satisfies the same invariant, or documents the gap.

| Capability | GitHub | GitLab | What koryph does when absent |
|---|---|---|---|
| Draft releases | ✅ | ❌ | **Assemble-then-create**: stage every asset in the package registry first, create the Release last. Same "nothing visible until complete" invariant. |
| Structured rulesets | ✅ | ❌ | GitLab uses protected branches + push rules + approval rules, compiled from the same posture *intents*. |
| Org/group-cascading rulesets | ✅ | ❌ | Protection is applied per-project on GitLab. |
| App bot identity | ✅ (GitHub App) | ❌ | GitLab uses a **project/group access token** flow (see below). |
| Workflow dispatch | ✅ | ❌ | On-demand pipeline triggers use a different mechanism on GitLab (v1: not wired). |
| Pages hosting | ✅ | ✅ | Both host docs; DNS/CNAME mechanics differ. |
| Immutable releases | ✅ | ❌ | GitLab releases can be updated/deleted; re-runs probe before create. |
| Secret scanning toggle | ✅ | ❌ | Out of GitLab API scope in v1. |
| Vulnerability alerts toggle | ✅ | ❌ | Advisory-database integration differs; not toggled via koryph on GitLab. |
| SLSA build provenance | ✅ (SLSA generator) | ⚠️ reduced | See **Supply chain** below. |

The design rationale for this split lives in
[the forge-providers design](https://github.com/koryph/koryph/blob/main/docs/designs/2026-07-forge-providers.md)
(design docs are kept in-repo, outside the published book).

---

## GitLab setup, end to end

This walks a GitLab project from zero to a working release train. GitHub setup
is covered by [Release bot](release-bot.md) and
[Releasing projects](releasing-projects.md); the steps below are the GitLab
equivalents.

### 1. Select the forge

```json
// koryph.project.json
{ "name": "widgets", "forge": "gitlab" }
```

For a self-managed GitLab instance, point koryph at the host:

```bash
export KORYPH_GITLAB_HOST=gitlab.example.com   # default: gitlab.com
```

Koryph shells out to the `glab` CLI where a CLI is convenient; override the
binary with `KORYPH_GLAB_BIN`. Operations that need fine-grained control use
GitLab's REST API (`/api/v4`) directly with a token resolved through the vault
layer (the same keychain/encrypted-file providers used for signing and bot
keys).

### 2. Create the bot token

GitLab has no App identity — tokens cannot self-create. `koryph bot create
--forge gitlab` runs a **guided access-token flow**:

1. Koryph opens the correct project (or group) **Settings → Access Tokens** URL.
2. You create a **project or group access token** with the scopes koryph
   validates for: **`api`** and **`write_repository`**. Set an expiry you are
   willing to rotate.
3. Paste the token back. Koryph validates its scopes via
   `GET /personal_access_tokens/self` and stores it through the vault layer
   with the same fallback ladder as signing keys.

### 3. Attach the bot to CI

`koryph bot attach` sets the project CI/CD variables koryph's pipelines expect
(**Settings → CI/CD → Variables**, created `protected`):

| Variable | Purpose |
|---|---|
| `KORYPH_BOT_TOKEN` | the access-token value |
| `KORYPH_BOT_TOKEN_EXPIRY` | expiry date (`YYYY-MM-DD`) or `never` |
| `KORYPH_RELEASE_TOKEN` | token used by the release pipeline to push the release branch, open the Release MR, and push the semver tag (`api` + `write_repository`). `CI_JOB_TOKEN` cannot push branches or tags in most project configs. |

### 4. Install the release pipeline

`koryph release setup` renders a `.gitlab-ci.yml` release pipeline (the GitLab
CIService asset) instead of GitHub Actions YAML. It implements the same
release-train contract with GitLab-native mechanics:

- **Version computation** uses koryph's own conventional-commit detection —
  release-please is GitHub-bound and is not used on GitLab.
- **Release MR** is authored by the project access token. GitLab runs pipelines
  for bot-authored MRs by default, so there is **no close/reopen trap** that
  GitHub App bots must work around.
- **Gate before tag**: the green gate runs on the MR pipeline; the tag is
  pushed only after a release merge commit.
- **Assemble-then-create**: all artifacts upload to the generic package
  registry first; the GitLab Release (with asset links) is created as the final
  step — the draft-release invariant, achieved without draft releases.
- GoReleaser publishes to GitLab natively (`gitlab_urls`, `CI_JOB_TOKEN`/PAT).

### 5. Supply chain (honest gap)

Cosign **keyless** signing works on GitLab CI via OIDC `id_tokens` (audience
`sigstore`), giving artifact authenticity and a verifiable build identity
(the GitLab CI OIDC subject). Requires cosign v2.2+ and GitLab 15.7+.

However, the **SLSA GitHub Generator is GitHub-specific and is not available on
GitLab**. Keyless signing is *not* equivalent to SLSA Build Level 3 provenance.
GitLab 16.1+ artifact attestations provide a lighter in-toto record. If you
require SLSA L3, use the GitHub forge or supplement the pipeline with a custom
in-toto signing step. See [Supply chain](supply-chain.md) for verification.

### 6. Rotate before expiry

GitLab access tokens expire. `koryph bot check` and `koryph doctor` validate
that the token is present, carries the required scopes, and has not expired —
and **warn before** the expiry date so you can rotate ahead of an outage.
Re-run `koryph bot create --forge gitlab` to mint and store a replacement.

---

## The seam is enforced

Forge-specific shell-outs (`gh`, `glab`) are only allowed inside
`internal/forge/**`. A build-time test (`TestForgeSeamSealed`) fails if any
other package invokes a forge CLI directly, so the boundary cannot erode as the
codebase grows. If you are adding a forge-specific behavior, add it to the
provider — not to a call site elsewhere.
