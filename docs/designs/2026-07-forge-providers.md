<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Pluggable git forges: GitHub, GitLab, and the provider seam (2026-07-04)

Status: approved for implementation; epic + GitLab children filed from §8.
Origin: operator direction — koryph supports only GitHub today. GitLab must
be a first-class alternative, and any git service must be pluggable by
implementing a provider. Hygiene, CI/action frameworks, and release
solutions all become forge-specific provider concerns.

## 1. Scope: what "forge" covers (and what it does not)

A **forge** is the hosted service around git: repo settings, branch
protection, PRs/MRs, CI pipelines, secrets, releases, pages, bot identity.
Everything *git-native* (clones, worktrees, branches, ff-merges, commit
signing, the green gate) is already forge-neutral and stays untouched —
koryph's core loop never talks to a forge; only the edges do.

Today those edges are GitHub-shaped in five places:

| Domain | Today | Forge-specific because |
|---|---|---|
| Hygiene (`repo`/`posture`) | GitHub rulesets + repo-settings JSON via `gh api` | rulesets/settings schemas are GitHub's |
| PR operations (`review-pr`, `pr-sync`, `land`, ufy) | GitHub PRs via `gh` | MRs differ in states, approvals, merge methods |
| Release train | release-please + GitHub Releases + Actions reusable workflow | releases API, CI framework, token-event rules all differ |
| Bot identity | GitHub App (manifest flow, JWT, installations) | GitLab has no Apps; access tokens instead |
| CI assets (`docs.yml`, caller workflow, scanner fragments) | GitHub Actions YAML | pipeline format is the forge's |

The same pattern as `internal/runtime` (agent plurality) applies: a
contract package, per-forge providers, capability discovery, and
behavior-identical extraction of the incumbent.

## 2. The contract: `internal/forge`

```go
type Forge interface {
    Name() string                  // "github", "gitlab"
    Capabilities() Capabilities    // what this forge can do
    Repo() RepoService             // settings, description/homepage
    Protection() ProtectionService // branch protection / rulesets
    PRs() PRService                // PR/MR list, create, approve, labels, merge
    Secrets() SecretsService       // Actions secrets / CI variables
    Releases() ReleaseService      // create/upload/publish
    CI() CIService                 // render pipeline assets for this forge
    Bot() BotService               // bot identity lifecycle
}
```

- **Capabilities** gate features instead of if-github-else chains:
  `{DraftReleases, Rulesets, AppIdentity, WorkflowDispatch, PagesHosting,
  ImmutableReleases, ...}` — a provider without draft releases gets the
  assemble-then-create strategy (§5) selected by capability, not by name.
- **Registry + config**: `koryph.project.json` gains `"forge": "github"`
  (default `github` — full back-compat; zero config change for existing
  projects). The registry record carries the resolved forge; `doctor`
  reports it. Remote-URL sniffing suggests-but-never-decides.
- **Auth**: each provider names its CLI/token source. GitHub keeps `gh`
  (`KORYPH_GH_BIN`). GitLab uses `glab` (`KORYPH_GLAB_BIN`) or a PAT
  resolved through the **vault layer** (same providers as signing/bot
  keys — keychain/encrypted-file fallbacks included). Providers shell to
  their forge CLI argv-template style first (matches today's `gh` usage);
  REST-native is a later optimization, not a blocker.
- **Enforcement**: a CI lint forbids invoking `gh`/`glab` outside
  `internal/forge/…` once extraction completes — the seam stays sealed.

## 3. Hygiene: the posture intent model

Today's desired state is raw GitHub JSON (`.github/rulesets/*.json`,
`repo-settings.json`) — meaningless to GitLab. Posture profiles split into:

- **Intents (forge-neutral core)**: `require_approvals: 1`,
  `require_signed_commits`, `required_checks: [...]`, `no_force_push`,
  `secret_scanning: on`, `default_branch_protected`, … Each provider
  **compiles** intents to its native config: GitHub → rulesets + settings
  (the compiler must reproduce today's `oss-solo-maintainer` output
  byte-identically — fixture-locked); GitLab → protected branches, push
  rules, approval rules, project settings.
- **Native passthrough (per-forge fidelity)**: a profile may carry
  `github/rulesets/*.json` or `gitlab/*.json` sections applied verbatim on
  the matching forge — full-fidelity escape hatch, clearly marked
  non-portable. Repo-local native IaC keeps overriding profiles entirely
  (ejectability unchanged).
- `describe`, snapshots, and `rollback` (koryph-bxe/vud) operate on the
  provider's managed-field set, so they extend to GitLab for free once the
  provider implements get/patch.

## 4. PRs/MRs and the ufy dependency

`PRService` abstracts: list/create, approve, labels, merge (method +
message), checks state, close/reopen. GitLab mapping: MRs, approval rules,
`merge_when_pipeline_succeeds`, squash options. **The koryph-ufy epic
(PR-based merge flow for protected branches) MUST build on `PRService`
from day one** — it is the first consumer that would otherwise hard-code
GitHub a second time.

## 5. Releases per forge

The train's stage table (release-train design §1) already isolates the
forge-specific stages. Per-forge release solutions:

- **GitHub** — unchanged: release-please, Actions reusable train,
  draft-until-complete immutable releases, SLSA generator, App bot.
- **GitLab** — a rendered `.gitlab-ci.yml` pipeline (CIService asset)
  implementing the same contract: conventional-commit version computation
  (koryph's own detection — release-please is GitHub-bound), Release MR
  authored via a **project access token** (no close/reopen trap: GitLab
  pipelines run for bot-authored MRs by default), gate-before-tag, then
  artifacts. GoReleaser publishes to GitLab natively (`gitlab_urls`,
  `CI_JOB_TOKEN`/PAT), including the tap-style casks question (out of
  scope v1). **No draft releases on GitLab** → the draft-until-complete
  invariant maps to **assemble-then-create**: upload all assets to the
  generic package registry first, create the Release (with asset links)
  as the last step. Same invariant — nothing user-visible until complete —
  different mechanics, selected by `Capabilities.DraftReleases`.
  Provenance: cosign keyless works against GitLab CI OIDC (`id_tokens`);
  SLSA's GitHub generator is replaced by GitLab's artifact attestations
  where available, else documented as reduced (honest capability gap).
- **Bot identity on GitLab** — `koryph bot create --forge gitlab` becomes
  a guided **project/group access token** flow (tokens can't self-create;
  koryph opens the right settings URL, validates the pasted token's
  scopes, stores it via the vault layer with the same fallback ladder,
  and `attach` sets CI variables). `bot check`/doctor validate scope,
  expiry (GitLab tokens expire — WARN before), and variable presence.

## 6. CI assets

`CIService.Render(kind)` produces forge-appropriate assets: docs publish
pipeline, release pipeline/caller, scanner fragments, badge snippets.
Installers (`release setup`, `docs setup`, posture fragments) ask the
project's forge for its rendering instead of embedding GitHub YAML
directly. GitLab Pages replaces the Pages API + CNAME story (bbr's DNS
records differ; noted there, not blocking here).

## 7. Non-goals (v1)

- Bitbucket/Gitea/Forgejo providers — the seam is the deliverable; they
  are follow-on providers (Gitea likely first: GitHub-compatible API).
- Cross-forge mirroring or migration tooling.
- GitLab self-managed quirks beyond `--hostname` passthrough.
- Homebrew-tap publishing from GitLab (needs cross-forge push; later).

## 8. Sequencing (the epic's children)

F1 contract → F2a/F2b/F2c GitHub extraction (behavior-identical,
fixture-locked) → F3 intent compiler → F4 GitLab hygiene → F5 GitLab MRs
→ F6 GitLab release pipeline → F7 GitLab bot/token flow → F8 docs +
runbook + enforcement lint → F9 live GitLab validation (HUMAN: needs a
real GitLab project). ufy consumes `PRService`; bbr/rdc consume
`CIService`/Pages capabilities; 0wv (issue intake) stays a separate
provider family.
