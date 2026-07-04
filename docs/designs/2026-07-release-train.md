<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Reusable release train: bot IaC + language-agnostic release pipeline (2026-07-04)

Status: approved for implementation.
Origin: koryph-q35.4/q35.5 first-release validation (v0.4.0 post-mortem,
v0.4.1 redesign) + operator requirement: every koryph-managed project must be
able to opt into the same release infrastructure, regardless of language or
build tool, with provisioning captured as code.

## 1. What generalizes and what does not

The koryph release pipeline (release-please → tag → build → draft release →
provenance → publish) has exactly one project-specific stage: **producing the
artifacts**. Everything else is language-agnostic:

| Stage | Generic? |
|---|---|
| Release PR upkeep (release-please) | Yes — parameterized by `release-type` (go, node, python, rust, simple, …) + `extra-files` version stamps |
| Bot identity (GitHub App) so PR checks trigger | Yes — one app per account, installed per repo |
| Release-merge detection + tag push | Yes — commit-subject regex, gate-before-tag |
| **Build artifacts** | **No — the seam** |
| Draft release + asset upload | Yes — given a directory of files |
| SBOM + SLSA provenance | Yes — syft/generator operate on any artifacts via sha256 subjects |
| Publish last (immutability-safe) | Yes |

## 2. The release build contract (the seam)

A project declares, in `koryph.project.json`:

```jsonc
"release": {
  "type": "go",                        // release-please release-type
  "extra_files": ["internal/version/version.go"],
  "artifacts_dir": "dist",
  "build": {                            // exactly one of:
    "goreleaser": true,                 // MODE B: tool owns the draft release
    "commands": ["make release-artifacts"] // MODE A: generic contract
  },
  "sbom": true,                         // syft SPDX over artifacts (mode A)
  "provenance": true                    // SLSA generic generator
}
```

**Mode A — generic contract.** The pipeline runs the declared commands at the
release tag; the commands' only obligation is to fill `artifacts_dir` (a
`checksums.txt` is generated if the build didn't). The pipeline then creates
the DRAFT release itself (`gh release create --draft` with the release-please
changelog section as body), uploads `dist/*`, optionally attaches syft SBOMs,
runs the SLSA generator over the checksums, and publishes last. Works for
npm packs, Python wheels, tarballs, containers-by-digest-manifest, or a
docs-only "simple" release with no artifacts at all (`build: {"commands": []}`
→ tag + changelog release only).

**Mode B — tool-owned.** Tools that already manage GitHub releases correctly
(GoReleaser with `release.draft: true`) keep ownership of draft creation and
uploads; the pipeline contributes detection/tag, provenance attach, and the
final publish. koryph itself uses mode B.

Both modes share the invariant learned from v0.4.0: **nothing publishes until
every asset is attached** — publication is the last step, so immutable
releases lock a complete release.

## 3. The reusable workflow

`.github/workflows/release-train.yml` in koryph/koryph gains `workflow_call`
with inputs mirroring the release block (release-type, extra-files,
build-mode, artifacts-dir, sbom, provenance, bot-secrets names). Consumer
projects get a ~20-line caller workflow (installed by koryph — see §5) that
just forwards their release block. koryph's own release-please.yml becomes
the first caller (dogfood; behavior-identical to the q35.5 design).

Reusable-workflow constraints honored: the called workflow lives in a public
repo; SLSA generator remains a by-tag reusable call from within the train;
caller grants the superset permissions the train declares (the q35.5
startup_failure lesson, documented inline).

## 4. Bot provisioning as code

GitHub Apps cannot be created headlessly (no REST endpoint — deliberate).
The **App Manifest flow** reduces creation to one browser click, and
everything else scripts:

`scripts/provision-release-bot.sh`:
- `--bootstrap` (once per GitHub account): serves a localhost redirect
  catcher, POSTs the app manifest (name, Contents:RW + PullRequests:RW, no
  webhook) to `github.com/settings/apps/new`, opens the browser for the one
  confirmation click, exchanges the redirect code via
  `gh api /app-manifests/{code}/conversions`, captures App ID + private key,
  and prints/stores them. Guides the one-click app installation.
- `--attach <owner/repo>` (per project, zero clicks): resolves the
  installation id, `gh api -X PUT /user/installations/{iid}/repositories/{rid}`,
  `gh secret set RELEASE_BOT_APP_ID / RELEASE_BOT_PRIVATE_KEY` on the target,
  and (recorded IaC for settings we once clicked)
  `gh api -X PUT repos/{o}/{r}/actions/permissions/workflow
  -F can_approve_pull_request_reviews=true`.
- Idempotent; `--check` mode reports drift (doctor consumes it).

The workflow consumes the bot via `actions/create-github-app-token` with a
`GITHUB_TOKEN` fallback when the secrets are absent — projects without the
bot still work, they just keep the close/reopen limitation. App-authored
Release PRs trigger checks normally AND remain approvable by the operator
(a PAT would make the operator the author, who cannot approve their own PR —
the trap that rules out the simpler fix).

## 5. koryph integration

- `koryph release setup --project ID [--mode a|b] [--bot]`: renders and
  installs the caller workflow + release-please-config.json + manifest into
  the project, updates its `release` block + schema, runs
  `provision-release-bot.sh --attach` when `--bot`, and prints the remaining
  HUMAN steps (app bootstrap if never done; ruleset requirements).
- `koryph doctor`: release-infra checks — caller workflow present/current,
  bot secrets present, release block valid, Actions PR-creation toggle on.
- Assets ship like every other koryph asset (embedded, `commands install`
  drift-checked), so `koryph project add` offers releases to every project.

## 6. Sequencing

1. **R1** `scripts/provision-release-bot.sh` + provisioning doc (loop-safe).
2. **R2** Extract `release-train.yml` reusable workflow; koryph's own
   workflow becomes the first caller; app-token-with-fallback wired
   (.github → orchestrator; AFTER the v0.4.1 release PR merges, to avoid
   check churn on the open Release PR).
3. **R3** `koryph release setup` + project-config release block + caller
   asset (loop-safe Go work).
4. **R4** doctor release-infra checks (loop-safe).
5. **R5** HUMAN: run `--bootstrap` (one click), attach koryph, validate a
   bot-authored Release PR triggers checks without close/reopen.
6. **R6** docs: user-guide releases chapter covering both modes (loop-safe).
