<!-- SPDX-License-Identifier: Apache-2.0 -->
<!-- Copyright (c) 2026 The Koryph Developers -->

# Release pipeline setup

## Opt-in: koryph never blocks you

The release pipeline is **fully optional**. If a project has no
`release` block in `koryph.project.json` and no `.github/workflows/release.yml`
caller workflow installed, koryph never creates, touches, or expects release
infrastructure. `koryph doctor` reports "release not configured" for such
projects (LevelOK — it is a valid, intentional state).

You opt in by running `koryph release setup`. You opt out by removing (or
never adding) the release block and workflow file.

---

`koryph release setup` wires a project's release pipeline by rendering three files from koryph's embedded templates and installing them into your repository:

| File | Purpose |
|---|---|
| `.github/workflows/release.yml` | Caller workflow that invokes koryph's reusable `release-train.yml` |
| `release-please-config.json` | release-please package configuration |
| `.release-please-manifest.json` | Initial version manifest (written once, managed by release-please thereafter) |

The generated caller workflow invokes `koryph/koryph`'s reusable `.github/workflows/release-train.yml` (koryph-0vf.3) via `koryph/koryph/.github/workflows/release-train.yml@main`. koryph's own release pipeline (`.github/workflows/release-please.yml`) is the first caller and uses the same-repository local-path form instead (`./.github/workflows/release-train.yml`) — see `docs/developer-guide/releasing.md`.

## Prerequisites

- A project registered with koryph (`koryph project add`).
- The project's `koryph.project.json` must be loadable (run `koryph validate --project ID` to check).

## Quickstart

```sh
# Goreleaser build mode (mode A — cross-platform binaries via .goreleaser.yaml)
koryph release setup --project myproject --mode goreleaser

# Commands build mode (mode B — custom shell commands stage artifacts)
koryph release setup --project myproject --mode commands

# Specify the initial version (default: 0.0.0)
koryph release setup --project myproject --mode goreleaser --version 0.3.0

# Also provision the release bot GitHub App (requires scripts/provision-release-bot.sh)
koryph release setup --project myproject --mode goreleaser --bot
```

### If the project already has a release block

If `koryph.project.json` already has a `release` block, `--mode` is optional. It overrides the build mode in the existing block if supplied:

```sh
# Re-render templates after editing koryph.project.json by hand
koryph release setup --project myproject

# Switch an existing goreleaser project to commands mode
koryph release setup --project myproject --mode commands
```

## Build modes

### Mode A — GoReleaser

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

Requires a `.goreleaser.yaml` at the repo root. GoReleaser manages cross-platform binary builds, checksums, cosign signing, and per-archive SBOMs. Set `sbom: true` to enable `anchore/sbom-action` and `provenance: true` for SLSA level-3 provenance.

### Mode B — Commands

```json
"release": {
  "type": "simple",
  "artifacts_dir": "dist",
  "build": {
    "commands": ["make build", "make package"]
  }
}
```

An ordered list of shell commands (each run via `sh -c`) that build and stage artifacts into `artifacts_dir`. Use this for non-Go projects or bespoke build systems.

## Release block reference (`koryph.project.json`)

| Field | Type | Required | Description |
|---|---|---|---|
| `type` | string | yes | release-please release type (`go`, `simple`, `node`, …) |
| `extra_files` | `[]string` | no | Additional files whose version strings release-please bumps |
| `artifacts_dir` | string | no | Build artifact directory (default: `dist`) |
| `build.goreleaser` | object | one of | Mode A: GoReleaser config (`version` field) |
| `build.commands` | `[]string` | one of | Mode B: ordered shell commands |
| `sbom` | bool | no | Enable SBOM generation via `anchore/sbom-action` |
| `provenance` | bool | no | Enable SLSA provenance via `slsa-framework/slsa-github-generator` |

Exactly one of `build.goreleaser` or `build.commands` must be set. `koryph validate` enforces this.

## Remaining HUMAN steps

After `koryph release setup` prints "Remaining HUMAN steps:", you need to:

1. **Provision a GitHub App** (release bot): if no release bot app exists, create one and note its App ID and private key. Use `--bot` to run `scripts/provision-release-bot.sh --attach` automatically.
2. **Set repository secrets**: `RELEASE_BOT_APP_ID` and `RELEASE_BOT_PRIVATE_KEY` — required by the reusable `release-train.yml`.
3. **Review branch-protection rulesets**: add the release bot's GitHub App identity to the "Bypass pull request requirements" list on `main`.
4. **Commit and push** the generated files to trigger the first release-please run.
5. **GoReleaser users**: verify `.goreleaser.yaml` is present at the repo root.
6. **Provenance users**: confirm `id-token: write` permission is available in your GitHub org.

## Bot-less mode (rung 2)

Installing a GitHub App is optional. The caller workflow falls back to
`GITHUB_TOKEN` when `RELEASE_BOT_APP_ID` / `RELEASE_BOT_PRIVATE_KEY` are
absent. **Release PRs are opened and updated correctly**, but check workflows
do NOT fire on release-please-authored events because GitHub prevents
`GITHUB_TOKEN`-caused events from triggering workflows (platform rule, not a
koryph limitation).

To trigger checks without a bot, run once per release:

```bash
koryph release kick --repo OWNER/REPO
```

This closes then reopens the Release PR under **your** `gh` auth token (a real
actor), which causes GitHub to fire all required check workflows. Add `--wait`
to poll until checks conclude:

```bash
koryph release kick --repo OWNER/REPO --wait
```

See [release-bot.md](release-bot.md) for a complete fallback ladder (PAT
alternative, admin-merge escape hatch) and their trade-offs.

## Re-running setup

`koryph release setup` is idempotent for the workflow and config files (they are always overwritten with the latest render). The manifest file (`.release-please-manifest.json`) is **never overwritten** after the first write — release-please manages it from that point on.
