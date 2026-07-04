<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Releasing projects

Koryph ships a reusable release pipeline that any koryph-managed project can
opt into with a single command, regardless of language or build tool. The
pipeline handles everything after you merge a Release PR: tagging, building
and staging artifacts, attaching SBOMs and SLSA provenance, and publishing —
in that order, with nothing published until every asset is attached.

This chapter is for operators setting up or managing release pipelines for
projects they own. For koryph's own release internals (the reusable
`release-train.yml` workflow, the GoReleaser config, provenance pipeline, and
the first-release validation checklist), see
[Releasing & versioning](../developer-guide/releasing.md).

!!! note "Forge coverage"
    The pipeline contract is forge-neutral, but the rendered assets differ by
    forge. On **GitHub** (the reference forge, and what this chapter shows)
    `koryph release setup` renders a GitHub Actions caller workflow driven by
    release-please. On **GitLab** it renders a `.gitlab-ci.yml` that computes
    versions from conventional commits directly and uses the assemble-then-create
    release strategy. See [Choosing a forge](forges.md#gitlab-setup-end-to-end).

---

## How the release lifecycle works

Every release starts from a stream of Conventional Commits landing on `main`.
Release-please monitors that stream and maintains a standing **Release PR** —
a pull request that accumulates the changelog and version bump for the next
release. You merge it when you're ready to ship.

```
feat/fix commits → main → release-please opens/updates Release PR
                                              ↓
                          you squash-merge the Release PR
                                              ↓
                          release-train detects merge, creates tag
                                              ↓
                          build step fills artifacts_dir (mode A)
                          or GoReleaser creates draft release (mode B)
                                              ↓
                          draft release created; assets uploaded
                                              ↓
                          SLSA provenance attached to draft
                                              ↓
                          release published (last step — locks asset set)
```

Publication is always the last step. This invariant — learned from a v0.4.0
post-mortem where GoReleaser tried to upload to an already-published release
and hit HTTP 422 — means immutable releases always lock a **complete** release:
no partial uploads, no missing provenance.

---

## The release build contract

Every project's release pipeline has exactly one project-specific stage:
**building and staging the artifacts**. Everything else (version detection,
tagging, draft release management, provenance, publication) is
language-agnostic.

Koryph provides two modes for the build stage:

### Mode A — generic commands (any language or tool)

```json
"release": {
  "type": "simple",
  "artifacts_dir": "dist",
  "build": {
    "commands": ["make build", "make package"]
  }
}
```

`build.commands` is an ordered list of shell commands (each run via `sh -c`)
executed at the release tag. Their **only obligation is to fill
`artifacts_dir`** — the pipeline generates `checksums.txt` if the build
didn't, creates the draft release itself, uploads `dist/*`, optionally
attaches syft SBOMs (`"sbom": true`), and publishes last.

Mode A works for any language or tool that can produce files:

| Ecosystem | Typical commands |
|-----------|-----------------|
| Node.js | `["npm ci", "npm pack --pack-destination dist"]` |
| Python | `["python -m build --outdir dist"]` |
| Docker | `["docker build -t myapp:$RELEASE_TAG .", "docker save myapp:$RELEASE_TAG -o dist/myapp.tar"]` |
| Rust / other | `["cargo build --release", "cp target/release/myapp dist/"]` |
| Docs-only | `[]` — empty list produces a tag-and-changelog release with no artifacts |

An empty `commands` list is a valid "simple" release: the pipeline creates the
tag and a changelog release with no binary assets attached.

### Mode B — tool-owned releases (GoReleaser-class)

```json
"release": {
  "type": "go",
  "extra_files": ["internal/version/version.go"],
  "artifacts_dir": "dist",
  "build": {
    "goreleaser": { "version": "~> v2.16" }
  },
  "sbom": true,
  "provenance": true
}
```

Tools that already manage GitHub releases correctly (GoReleaser with
`release.draft: true` in `.goreleaser.yaml`) keep ownership of draft creation
and asset upload. The pipeline contributes detection, tagging, the optional
pre-tag gate, SLSA provenance, and the final publish.

Koryph itself uses mode B. Requires a `.goreleaser.yaml` at the repo root.

### Release block reference

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `type` | string | yes | release-please release type (`go`, `node`, `python`, `rust`, `simple`, …) |
| `extra_files` | `[]string` | no | Files whose version strings release-please bumps alongside the primary version file |
| `artifacts_dir` | string | no | Build artifact directory (default: `dist`) |
| `build.commands` | `[]string` | one of | Mode A: ordered shell commands filling `artifacts_dir` |
| `build.goreleaser` | object | one of | Mode B: GoReleaser config; `version` field selects the GoReleaser version |
| `sbom` | bool | no | Attach a syft SPDX SBOM per artifact (mode A) or per-archive (mode B) |
| `provenance` | bool | no | Generate and attach a SLSA Build L3 provenance attestation |

Exactly one of `build.commands` or `build.goreleaser` must be set.
`koryph validate --project ID` enforces this.

---

## Forge-specific release pipelines

The release pipeline is forge-specific: koryph renders the appropriate CI
asset for your forge when you run `koryph release setup`.

### GitHub

GitHub uses the reusable `release-train.yml` workflow (hosted in
`koryph/koryph`) together with release-please for version management and
GitHub Releases for publishing. Full details in the section below.

### GitLab

GitLab projects get a self-contained `.gitlab-ci.yml` that implements the same
release train contract without any GitHub-specific tooling:

| Feature | GitLab implementation |
|---|---|
| Version management | koryph-native conventional-commit scanner — no release-please (GitHub-bound) |
| Release MR | Pipeline creates/updates an MR via the GitLab API using `KORYPH_RELEASE_TOKEN` |
| CI check trigger | GitLab pipelines fire for bot-authored MRs by default — no close/reopen workaround |
| Artifact staging | Generic package registry (all assets uploaded before Release is created) |
| Release creation | GitLab Release API — created as the final step (assemble-then-create) |
| Artifact signing | cosign keyless via GitLab CI OIDC `id_tokens` (sigstore audience) |
| Docs publish | GitLab Pages via a separate `pages:` job |

#### GitLab quickstart

```sh
# Set forge in koryph.project.json first
koryph project set-forge --project myproject gitlab

# Render the .gitlab-ci.yml release pipeline
koryph release setup --project myproject --mode goreleaser   # GoReleaser
koryph release setup --project myproject --mode commands     # generic commands
```

`koryph release setup` for a GitLab project writes:

| File | Purpose |
|---|---|
| `.gitlab-ci.yml` | Full release pipeline (version compute, Release MR, build, sign, publish) |

#### Required CI/CD variable

Set one project-level CI/CD variable in **Settings → CI/CD → Variables**:

| Variable | Scopes | Description |
|---|---|---|
| `KORYPH_RELEASE_TOKEN` | Protected, Masked | Project or group access token with `api` + `write_repository` scopes. Used to push the release branch, manage the Release MR, push semver tags, and create the GitLab Release. `CI_JOB_TOKEN` is insufficient for branch/tag pushes in most project configurations. |

#### How a GitLab release happens

```
conventional commits land on main
        ↓
pipeline (upkeep kind) runs compute-version
        ↓
koryph computes next semver from commits since last tag
        ↓
release-mr job creates / updates the Release MR
  title: "chore(release): vX.Y.Z"
  label: koryph-release
        ↓
CI gate runs on the MR source branch
        ↓
operator merges the Release MR (squash merge)
        ↓
pipeline (release kind) detects commit title /^chore\(release\): v/
        ↓
tag-release: pushes annotated semver tag
        ↓
build: GoReleaser --skip=publish (archives in dist/) or custom commands
        ↓
sign: cosign keyless sign-blob (checksums.txt) using OIDC id_tokens
        ↓
upload-packages: all dist/* → generic package registry
        ↓
release-create: GitLab Release created with asset links (last step)
  → nothing user-visible until this job completes
```

#### SLSA posture on GitLab (honest capability statement)

The SLSA GitHub Generator is GitHub-specific and is not available on GitLab.
The cosign keyless signing step provides **artifact authenticity** — a
verifiable link between the binary and the GitLab CI pipeline that produced
it — but it does **not** constitute SLSA Build Level 3 provenance.

GitLab 16.1+ artifact attestations (the `--attest` flag on job artifact
declarations) generate a lighter in-toto record per artifact; enabling them
requires no pipeline changes but does require a supported GitLab tier. The
release pipeline template includes a comment describing how to add them.

Projects that require SLSA Build L3 should use the GitHub forge.

---

## Setting up your project's release pipeline (GitHub)

`koryph release setup` renders three files from koryph's embedded templates
and installs them into your repository:

| File | Purpose |
|------|---------|
| `.github/workflows/release.yml` | Caller workflow that invokes the reusable `release-train.yml` |
| `release-please-config.json` | release-please package configuration |
| `.release-please-manifest.json` | Initial version manifest (written once; release-please manages it thereafter) |

### Quickstart

```sh
# Generic commands mode (mode A) — any language
koryph release setup --project myproject --mode commands

# GoReleaser mode (mode B) — Go projects with .goreleaser.yaml
koryph release setup --project myproject --mode goreleaser

# Start at a specific version (default: 0.0.0)
koryph release setup --project myproject --mode commands --version 1.2.0

# Also provision the release bot in one step
koryph release setup --project myproject --mode commands --bot
```

If `koryph.project.json` already has a `release` block, `--mode` is optional
— it overrides the existing build mode if supplied. Run without `--mode` to
re-render templates after editing the project config by hand.

### After running setup

`koryph release setup` prints the remaining HUMAN steps:

1. **Provision the release bot** (once per GitHub account) — run
   `scripts/provision-release-bot.sh --bootstrap`, or pass `--bot` to
   have `release setup` run it for you. See [The release bot](#the-release-bot) below.
2. **Set repository secrets**: `RELEASE_BOT_APP_ID` and
   `RELEASE_BOT_PRIVATE_KEY`. The `--bot` flag runs `--attach` which sets
   these automatically.
3. **Review branch-protection rulesets**: add the release bot's GitHub App
   identity to the "Bypass pull request requirements" list on `main`.
4. **Commit and push** the generated files to trigger the first release-please
   run (which opens the first Release PR).
5. **GoReleaser users**: verify `.goreleaser.yaml` is present at the repo root.
6. **Provenance users**: confirm `id-token: write` permission is available in
   your GitHub organisation.

`koryph release setup` is **idempotent** for the workflow and config files —
they are always overwritten with the latest render. The manifest
(`.release-please-manifest.json`) is **never overwritten** after the first
write; release-please manages it from that point on.

`koryph doctor --project myproject` checks for configuration drift: missing
secrets, outdated caller workflow, missing release block fields, and the
Actions PR-approval toggle.

---

## The release bot

The pipeline uses a **GitHub App** (the "release bot") to open and update
Release PRs. This section explains why an App is required and how to set one
up.

### Why a GitHub App, not a PAT?

Release-please opens a Release PR from the workflow token. If a personal
access token (PAT) is used, the PAT owner becomes the PR author — and GitHub
prevents authors from approving their own pull requests. This creates a
permanent blockage: the operator who owns the PAT cannot approve the Release
PR they need to merge.

A GitHub App sidesteps this completely:

| Mechanism | PR author | Operator can approve? |
|-----------|-----------|----------------------|
| PAT | The PAT owner | **No** — author cannot self-approve |
| GitHub App | The App (bot identity) | **Yes** — operator is not the author |

The App only needs two permissions (`Contents: write`, `Pull requests: write`)
and has no webhook — a narrow-scope, no-inbound-listener identity.

**Without the bot**, the workflow falls back to `GITHUB_TOKEN`. Releases still
work — you just need to close and reopen the Release PR with a different token
to trigger required status checks. It is a friction issue, not a correctness
one.

### One-click bootstrap (once per GitHub account)

GitHub requires exactly one browser click to create an App (the API cannot do
this headlessly). The bootstrap script reduces everything else to a script:

```sh
scripts/provision-release-bot.sh --bootstrap
```

What happens:

1. A local HTTP server starts on port 3737 to catch the OAuth redirect.
2. Your browser opens with a form pre-filled with the App manifest (name,
   permissions, redirect URL).
3. **Click "Create GitHub App"** — this is the single human action.
4. GitHub redirects back to `localhost:3737/callback` with a one-time code.
5. The script exchanges the code for an App ID and private key PEM and stores
   them in `~/.config/koryph/release-bot/`.
6. GitHub opens the installation page; grant access to your account or
   organisation.

Credentials land at:
```
~/.config/koryph/release-bot/app-id          # plain text App ID
~/.config/koryph/release-bot/private-key.pem # RSA private key (mode 600)
```

The bootstrap script is **idempotent** — re-running when credentials exist
prints their location and exits without creating a second App.

### Zero-click attach (once per repository)

After the App is bootstrapped and installed on the account that owns the
repository, attach each repository with one command:

```sh
scripts/provision-release-bot.sh --attach owner/myrepo
```

Five idempotent steps run automatically:

| Step | What it does |
|------|-------------|
| Resolve installation ID | Finds the App installation for the repo's owner |
| Add repo to installation | Grants the App access to this specific repo |
| Set `RELEASE_BOT_APP_ID` | Writes the App ID as a repository secret |
| Set `RELEASE_BOT_PRIVATE_KEY` | Writes the private key PEM as a repository secret |
| Enable Actions PR-approval | Sets `can_approve_pull_request_reviews=true` on the repo |

The last step captures as code a setting that would otherwise require a manual
click in GitHub UI under _Settings → Actions → General → Allow GitHub Actions
to create and approve pull requests_.

### Replicating to additional projects

The App is created once per GitHub account. For each new project:

1. If the App is not yet installed on the new project's owner account:
   ```sh
   open "https://github.com/settings/apps/<app-name>/installations"
   # Click "Install" and grant access
   ```
2. Attach the repository (zero clicks):
   ```sh
   scripts/provision-release-bot.sh --attach owner/new-repo
   ```
3. Verify:
   ```sh
   scripts/provision-release-bot.sh --check owner/new-repo
   ```

Or use `koryph release setup --bot` to run `--attach` in one step as part of
release pipeline setup.

For detailed troubleshooting and security notes, see
[Release Bot: GitHub App provisioning](release-bot.md).

---

## Conventional-commit versioning

Version numbers are computed **automatically** from the Conventional Commits
that land on `main` since the previous release. You do not set version numbers
by hand.

### How commit types map to version bumps

| Commit type | Version bump | Example |
|-------------|-------------|---------|
| `feat:` | **minor** (`0.x.0`) | `feat(api): add batch endpoint` |
| `fix:`, `perf:`, `docs:`, etc. | **patch** (`0.0.x`) | `fix(cli): handle missing config` |
| `feat!:` or `BREAKING CHANGE:` footer | **major** (`x.0.0`) | `feat!: rename --project to --id` |

Pre-1.0 projects (version `0.x`) follow the same rules, with the exception
that breaking changes still produce a major bump (from `0.x` to `1.0.0`) when
a `BREAKING CHANGE:` footer is present.

### Commit format

```
type(scope): subject in imperative mood, lowercase, ≤72 chars

Optional longer body explaining context and motivation.

BREAKING CHANGE: describe what broke and how to migrate.
Signed-off-by: Your Name <you@example.com>
```

Accepted types: `feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `ci`,
`build`, `perf`, `style`. Only `feat` and `fix` (plus breaking changes) drive
version bumps; the others are grouped in the changelog but do not bump the
version alone.

All commits must carry a `Signed-off-by:` trailer (DCO) and be
cryptographically signed. See [Signing](signing.md) for setup.

### The Release PR

Release-please opens or updates a **Release PR** automatically on every push
to `main`. The PR:

- Carries the title `chore(main): release X.Y.Z`
- Bumps version strings in the files configured under `extra_files`
- Updates `CHANGELOG.md` with a grouped changelog since the previous release
- Requires no manual version-number entry — the version is computed from the
  commit stream

Review the Release PR like any normal PR: check that the computed version
bump matches what you intended (a `feat!:` in the batch should produce a
major bump; a pure `fix:` batch should produce a patch bump). When it looks
right, **squash-merge it** — the squash-merge title shape is load-bearing for
the detection step (see below).

### release-please release types

The `type` field in the release block tells release-please which version file
format to manage:

| `type` | Primary version file | Notes |
|--------|---------------------|-------|
| `go` | Constant with `// x-release-please-version` comment | Use `extra_files` for non-standard locations |
| `node` | `package.json` | Also bumps `package-lock.json` |
| `python` | `setup.cfg` / `pyproject.toml` | |
| `rust` | `Cargo.toml` | |
| `simple` | `version.txt` | Fallback; good for shell scripts, containers, docs |

When your project has no natural primary version file (e.g. a pure Docker
image), use `"type": "simple"` with `"artifacts_dir": "dist"` and list any
files you want bumped under `extra_files`.

---

## The immutability lifecycle

GitHub supports **immutable releases** — once a release is published, no
asset can be added, replaced, or removed (uploads return HTTP 422). This is a
desirable security property, but it requires the pipeline to get asset
attachment ordering right.

The koryph pipeline was designed around this invariant after a v0.4.0
post-mortem where the original design published the release first and then
tried to attach build artifacts — every upload failed with 422.

### How the pipeline preserves immutability

The pipeline creates a **draft release** during the build stage. Draft
releases are mutable: assets can be uploaded to them without restriction.
Publication — the step that locks the asset set — is always last:

```
1. Build stage fills artifacts_dir (mode A) or GoReleaser creates draft (mode B)
2. Checksum manifest generated (if not already present)
3. All artifacts uploaded to the draft release
4. SLSA provenance generated and attached to the still-draft release
5. gh release edit --draft=false   ← publication; this locks the asset set
```

Nothing is published until every asset is attached. From the perspective of
anyone watching the Releases page normally (published releases only), the
release appears fully-formed the moment it becomes visible.

### What immutability means for operators

- You cannot hot-patch a published release by uploading a replacement binary.
  If a release has a critical bug, cut a new patch release (`fix:` commit →
  Release PR → merge).
- The draft phase is invisible to normal users. You can monitor it in the
  GitHub UI under _Releases → Drafts_ while the pipeline runs.
- If the pipeline fails mid-run (after draft creation but before publication),
  the draft is left open. Delete it manually from the Releases page before
  re-running, or the re-run's GoReleaser step (mode B) will attempt to reuse
  the existing draft.

---

## Release artifacts and supply-chain verification

Releases in mode B (GoReleaser) automatically include:

- Platform binary archives
- `checksums.txt` (SHA-256 manifest)
- `checksums.txt.sigstore.json` (keyless cosign signature bundle)
- Per-archive SPDX SBOMs (when `"sbom": true`)
- `checksums.txt.intoto.jsonl` (SLSA Build L3 provenance, when
  `"provenance": true`)

Mode A releases include `checksums.txt` and, when enabled, per-artifact syft
SBOMs and SLSA provenance over the checksum manifest.

For instructions on verifying these assets after downloading a release, see
[Verifying a release](supply-chain.md).

---

## See also

- [Release pipeline setup](release.md) — `koryph release setup` reference,
  build mode JSON examples, and the remaining HUMAN steps list
- [Release Bot: GitHub App provisioning](release-bot.md) — detailed bot
  bootstrap and attach instructions, troubleshooting, and security notes
- [Verifying a release](supply-chain.md) — consumer-side supply-chain
  verification (cosign, slsa-verifier, sha256sum)
- [Releasing & versioning](../developer-guide/releasing.md) — koryph's own
  release pipeline internals, the reusable `release-train.yml` workflow, the
  v0.4.0 post-mortem, and the first-release validation checklist
